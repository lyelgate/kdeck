package quick

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/lyelgate/kdeck/internal/match"
)

// TroubleCommands are the troubleshooting commands.
func TroubleCommands(d Deps) []*cobra.Command {
	cmds := []*cobra.Command{whyCmd(d), traceCmd(d), fitCmd(d), timelineCmd(d), nodeShCmd(d), editSecretCmd(d)}
	for _, c := range cmds {
		c.GroupID = "shortcuts"
	}
	return cmds
}

// --- why -------------------------------------------------------------------------

// Diagnosis is the outcome of "kdeck why".
type Diagnosis struct {
	Verdict  string
	Evidence []string
	Next     []string
}

var exitMeanings = map[int32]string{
	1:   "the application exited with an error",
	2:   "the application exited with a usage error (bad arguments or config)",
	126: "the command is not executable (permissions or wrong binary format)",
	127: "the command was not found in the image",
	134: "the process aborted (SIGABRT)",
	137: "the process was killed (SIGKILL), usually the out-of-memory killer",
	139: "the process crashed with a segmentation fault",
	143: "the process was terminated (SIGTERM), often a failing liveness probe or shutdown",
}

// Diagnose explains why a pod is not healthy, from its status and events.
func Diagnose(p corev1.Pod, events []corev1.Event, now time.Time) Diagnosis {
	var d Diagnosis
	warn := func(reason string) []string {
		var out []string
		for _, e := range events {
			if e.Type == corev1.EventTypeWarning && (reason == "" || e.Reason == reason) {
				out = append(out, fmt.Sprintf("%s: %s", e.Reason, strings.TrimSpace(e.Message)))
			}
		}
		return out
	}
	if p.DeletionTimestamp != nil && now.Sub(p.DeletionTimestamp.Time) > 5*time.Minute {
		d.Verdict = fmt.Sprintf("stuck terminating for %s", age(p.DeletionTimestamp.Time))
		d.Evidence = append(d.Evidence, fmt.Sprintf("finalizers: %v, node: %s", p.Finalizers, dash(p.Spec.NodeName)))
		d.Next = append(d.Next, "If the node is gone or NotReady, the kubelet cannot confirm the stop; check kdeck nodes", "Remove finalizers only once you know what they protect")
		return d
	}
	if p.Status.Phase == corev1.PodFailed && p.Status.Reason == "Evicted" {
		d.Verdict = "evicted by the kubelet"
		d.Evidence = append(d.Evidence, p.Status.Message)
		d.Next = append(d.Next, "The node ran short of memory or disk; set requests close to real usage (kdeck fit) and clean up with kdeck clean --yes")
		return d
	}
	if c, ok := podCond(p, corev1.PodScheduled); ok && c.Status == corev1.ConditionFalse {
		d.Verdict = "not scheduled: " + c.Reason
		d.Evidence = append(d.Evidence, c.Message)
		msg := strings.ToLower(c.Message)
		switch {
		case strings.Contains(msg, "insufficient"):
			d.Next = append(d.Next, "No node has enough free CPU or memory for its requests: lower requests, add nodes, or check kdeck nodes")
		case strings.Contains(msg, "taint"):
			d.Next = append(d.Next, "Nodes have taints the pod does not tolerate: add tolerations or schedule elsewhere")
		case strings.Contains(msg, "affinity") || strings.Contains(msg, "selector"):
			d.Next = append(d.Next, "No node matches its nodeSelector or affinity rules")
		case strings.Contains(msg, "persistentvolumeclaim") || strings.Contains(msg, "volume"):
			d.Next = append(d.Next, "Its PVC is not bound or its volume zone has no node: kubectl get pvc -n "+p.Namespace)
		default:
			d.Next = append(d.Next, "kubectl describe pod "+p.Name+" -n "+p.Namespace+" for the scheduler's full message")
		}
		return d
	}
	all := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, s := range all {
		if w := s.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				d.Verdict = fmt.Sprintf("container %s cannot pull its image", s.Name)
				d.Evidence = append(d.Evidence, "image: "+s.Image, w.Message)
				lm := strings.ToLower(w.Message + strings.Join(warn("Failed"), " "))
				switch {
				case strings.Contains(lm, "not found") || strings.Contains(lm, "manifest unknown"):
					d.Next = append(d.Next, "The image or tag does not exist: check the name and tag")
				case strings.Contains(lm, "unauthorized") || strings.Contains(lm, "denied") || strings.Contains(lm, "authentication"):
					d.Next = append(d.Next, "The registry refused access: add or fix imagePullSecrets on the pod or its ServiceAccount")
				case strings.Contains(lm, "no such host") || strings.Contains(lm, "timeout") || strings.Contains(lm, "i/o"):
					d.Next = append(d.Next, "The node cannot reach the registry: DNS, egress firewall or proxy")
				default:
					d.Next = append(d.Next, "kdeck desc "+p.Name+" shows the pull error in events")
				}
			case "CreateContainerConfigError", "CreateContainerError":
				d.Verdict = fmt.Sprintf("container %s cannot be created: %s", s.Name, w.Reason)
				d.Evidence = append(d.Evidence, w.Message)
				d.Next = append(d.Next, "Usually a missing ConfigMap, Secret or key it references; create it or fix the reference")
			case "CrashLoopBackOff":
				d.Verdict = fmt.Sprintf("container %s keeps crashing (%d restarts)", s.Name, s.RestartCount)
				if t := s.LastTerminationState.Terminated; t != nil {
					meaning := exitMeanings[t.ExitCode]
					if t.Reason == "OOMKilled" {
						meaning = "killed for exceeding its memory limit"
					}
					d.Evidence = append(d.Evidence, fmt.Sprintf("last exit code %d (%s) %s ago: %s", t.ExitCode, t.Reason, age(t.FinishedAt.Time), dash(meaning)))
					switch {
					case t.Reason == "OOMKilled" || t.ExitCode == 137:
						d.Next = append(d.Next, "Raise its memory limit or fix the leak; compare usage with kdeck fit "+p.Name)
					case t.ExitCode == 127 || t.ExitCode == 126:
						d.Next = append(d.Next, "Fix command/args or the image entrypoint")
					case t.ExitCode == 143:
						d.Next = append(d.Next, "Check the liveness probe: it may kill the app before it is ready (raise initialDelaySeconds or use a startupProbe)")
					default:
						d.Next = append(d.Next, "Read the crash output: kdeck log "+p.Name+" -p -c "+s.Name)
					}
				} else {
					d.Next = append(d.Next, "kdeck log "+p.Name+" -p -c "+s.Name)
				}
			case "ContainerCreating":
				d.Verdict = fmt.Sprintf("container %s stuck creating", s.Name)
				d.Evidence = append(d.Evidence, warn("FailedMount")...)
				d.Evidence = append(d.Evidence, warn("FailedAttachVolume")...)
				d.Next = append(d.Next, "Usually a volume that cannot attach or mount, or a CNI problem; see the events above and kdeck silent --check storage")
			default:
				d.Verdict = fmt.Sprintf("container %s waiting: %s", s.Name, w.Reason)
				d.Evidence = append(d.Evidence, w.Message)
				d.Next = append(d.Next, "kdeck desc "+p.Name)
			}
			d.Evidence = append(d.Evidence, warn("")...)
			return d
		}
	}
	// Between restarts a crash-looping container is briefly Running; recent
	// non-zero exits still mean it is crashing.
	for _, st := range all {
		if t := st.LastTerminationState.Terminated; t != nil && t.ExitCode != 0 && st.RestartCount >= 2 && now.Sub(t.FinishedAt.Time) < 10*time.Minute {
			d.Verdict = fmt.Sprintf("container %s keeps crashing (%d restarts)", st.Name, st.RestartCount)
			meaning := exitMeanings[t.ExitCode]
			if t.Reason == "OOMKilled" {
				meaning = "killed for exceeding its memory limit"
			}
			d.Evidence = append(d.Evidence, fmt.Sprintf("last exit code %d (%s) %s ago: %s", t.ExitCode, t.Reason, age(t.FinishedAt.Time), dash(meaning)))
			if t.Reason == "OOMKilled" || t.ExitCode == 137 {
				d.Next = append(d.Next, "Raise its memory limit or fix the leak; compare usage with kdeck fit "+p.Name)
			} else {
				d.Next = append(d.Next, "Read the crash output: kdeck log "+p.Name+" -p -c "+st.Name)
			}
			return d
		}
	}
	if p.Status.Phase == corev1.PodRunning {
		if c, ok := podCond(p, corev1.PodReady); ok && c.Status != corev1.ConditionTrue {
			d.Verdict = "running but not Ready: it receives no Service traffic"
			if u := warn("Unhealthy"); len(u) > 0 {
				d.Evidence = append(d.Evidence, u...)
				d.Next = append(d.Next, "The readiness probe fails: check its path, port and timeout against what the app serves")
			} else {
				d.Evidence = append(d.Evidence, c.Message)
				d.Next = append(d.Next, "Check readiness gates and the readiness probe in kdeck desc "+p.Name)
			}
			return d
		}
		var restarts int32
		for _, s := range all {
			restarts += s.RestartCount
		}
		d.Verdict = "healthy (running and ready)"
		if restarts > 0 {
			d.Verdict = fmt.Sprintf("running and ready, but restarted %d times", restarts)
			for _, s := range all {
				if t := s.LastTerminationState.Terminated; t != nil {
					d.Evidence = append(d.Evidence, fmt.Sprintf("%s last exit %d (%s) %s ago", s.Name, t.ExitCode, t.Reason, age(t.FinishedAt.Time)))
				}
			}
			d.Next = append(d.Next, "kdeck log "+p.Name+" -p shows the output before the last restart")
		}
		return d
	}
	if p.Status.Phase == corev1.PodSucceeded {
		d.Verdict = "completed successfully"
		return d
	}
	d.Verdict = "phase " + string(p.Status.Phase)
	d.Evidence = append(d.Evidence, warn("")...)
	d.Next = append(d.Next, "kdeck desc "+p.Name)
	return d
}

func podCond(p corev1.Pod, t corev1.PodConditionType) (corev1.PodCondition, bool) {
	for _, c := range p.Status.Conditions {
		if c.Type == t {
			return c, true
		}
	}
	return corev1.PodCondition{}, false
}

func whyCmd(d Deps) *cobra.Command {
	var allNS, every bool
	cmd := &cobra.Command{
		Use:   "why POD-REGEX",
		Short: "Explain why a pod is unhealthy: scheduling, image pulls, crashes (exit codes), config, probes, mounts",
		Long:  "Plain-language diagnosis of a pod from its status and events, with next steps. Deterministic: no AI, nothing sent anywhere.\n" + regexHelp,
		Example: `  kdeck why api
  kdeck why '^worker-' --all`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pods, err := targets(d, cmd, args[0], allNS, every)
			if err != nil {
				return err
			}
			cs, _, err := d.client()
			if err != nil {
				return err
			}
			for i, p := range pods {
				if i > 0 {
					fmt.Fprintln(d.Out)
				}
				evs, _ := cs.CoreV1().Events(p.Namespace).List(cmd.Context(), metav1.ListOptions{FieldSelector: "involvedObject.kind=Pod,involvedObject.name=" + p.Name})
				var items []corev1.Event
				if evs != nil {
					items = evs.Items
					sort.Slice(items, func(a, b int) bool { return eventTime(items[a]).After(eventTime(items[b])) })
				}
				dg := Diagnose(p, items, time.Now())
				fmt.Fprintf(d.Out, "%s/%s: %s\n", p.Namespace, p.Name, dg.Verdict)
				seen := map[string]bool{}
				for _, e := range dg.Evidence {
					if e == "" || seen[e] {
						continue
					}
					seen[e] = true
					fmt.Fprintf(d.Out, "  evidence: %s\n", e)
				}
				for _, n := range dg.Next {
					fmt.Fprintf(d.Out, "  next:     %s\n", n)
				}
			}
			return nil
		},
	}
	singleFlags(cmd, &allNS, &every)
	return cmd
}

// --- trace ------------------------------------------------------------------------

type step struct {
	name, detail string
	ok           bool
}

var gvrHTTPRoute = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}

func hostMatches(pattern, host string) bool {
	if pattern == "" || pattern == host {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return strings.HasSuffix(host, suffix) && strings.Count(host, ".") == strings.Count(pattern, ".")
	}
	return false
}

func pathMatches(pathType string, rulePath, path string) (bool, int) {
	if rulePath == "" {
		rulePath = "/"
	}
	switch pathType {
	case "Exact":
		return path == rulePath, len(rulePath) + 1000
	default: // Prefix and ImplementationSpecific: element-wise prefix
		rp := strings.TrimRight(rulePath, "/")
		if rp == "" || path == rp || strings.HasPrefix(path, rp+"/") {
			return true, len(rp)
		}
	}
	return false, 0
}

func traceCmd(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trace URL",
		Short: "Follow a URL through Ingress or HTTPRoute, TLS, Service, endpoints and pods; points at the broken link",
		Example: `  kdeck trace https://shop.example.com/api/orders
  kdeck trace shop.example.com/            # scheme optional`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw := args[0]
			if !strings.Contains(raw, "://") {
				raw = "http://" + raw
			}
			u, err := url.Parse(raw)
			if err != nil {
				return err
			}
			host, path := u.Hostname(), u.Path
			if path == "" {
				path = "/"
			}
			cs, _, err := d.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var steps []step
			add := func(name string, ok bool, f string, a ...interface{}) {
				steps = append(steps, step{name, fmt.Sprintf(f, a...), ok})
			}
			// Find the best matching Ingress rule across namespaces.
			ings, err := cs.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			var best *networkingv1.Ingress
			var bestBackend *networkingv1.IngressBackend
			bestScore, bestPath, bestType := -1, "", ""
			for i := range ings.Items {
				ing := &ings.Items[i]
				for _, r := range ing.Spec.Rules {
					if !hostMatches(r.Host, host) || r.HTTP == nil {
						continue
					}
					for j := range r.HTTP.Paths {
						p := r.HTTP.Paths[j]
						pt := "Prefix"
						if p.PathType != nil {
							pt = string(*p.PathType)
						}
						if ok, score := pathMatches(pt, p.Path, path); ok {
							if r.Host != "" {
								score += 10000 // a host rule beats a catch-all
							}
							if score > bestScore {
								best, bestBackend, bestScore, bestPath, bestType = ing, &r.HTTP.Paths[j].Backend, score, p.Path, pt
							}
						}
					}
				}
			}
			var svcNS, svcName, svcPort string
			if best != nil {
				class := "(default class)"
				if best.Spec.IngressClassName != nil {
					class = *best.Spec.IngressClassName
				}
				add("Route", true, "Ingress %s/%s, class %s, path %s (%s)", best.Namespace, best.Name, class, bestPath, bestType)
				if best.Spec.IngressClassName != nil {
					if _, err := cs.NetworkingV1().IngressClasses().Get(ctx, *best.Spec.IngressClassName, metav1.GetOptions{}); err != nil {
						add("Controller", false, "IngressClass %q not found: no controller will serve this Ingress", *best.Spec.IngressClassName)
					}
				}
				if u.Scheme == "https" {
					secret := ""
					for _, t := range best.Spec.TLS {
						for _, h := range t.Hosts {
							if hostMatches(h, host) {
								secret = t.SecretName
							}
						}
					}
					switch {
					case secret == "":
						add("TLS", false, "no tls entry covers %s: the controller serves its default certificate", host)
					default:
						if _, err := cs.CoreV1().Secrets(best.Namespace).Get(ctx, secret, metav1.GetOptions{}); apierrors.IsNotFound(err) {
							add("TLS", false, "secret %s/%s does not exist", best.Namespace, secret)
						} else if err != nil {
							add("TLS", true, "secret %s/%s (could not read: %v)", best.Namespace, secret, firstLineOf(err.Error()))
						} else {
							add("TLS", true, "secret %s/%s present (expiry: kdeck silent --sensitive --check SF-CERT-001)", best.Namespace, secret)
						}
					}
				}
				if bestBackend.Service == nil {
					add("Service", false, "backend is not a Service (resource backend)")
				} else {
					svcNS, svcName = best.Namespace, bestBackend.Service.Name
					svcPort = bestBackend.Service.Port.Name
					if bestBackend.Service.Port.Number != 0 {
						svcPort = strconv.Itoa(int(bestBackend.Service.Port.Number))
					}
				}
			} else if svcNS, svcName, svcPort = traceHTTPRoute(ctx, d, host, path, add); svcName == "" {
				add("Route", false, "no Ingress or HTTPRoute matches host %q path %q", host, path)
			}
			if svcName != "" {
				traceService(ctx, cs, svcNS, svcName, svcPort, add)
			}
			tw := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "URL\t%s\t\n", u.String())
			broken := ""
			for _, s := range steps {
				mark := "ok"
				if !s.ok {
					mark = "BROKEN"
					if broken == "" {
						broken = s.name + ": " + s.detail
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", s.name, s.detail, mark)
			}
			_ = tw.Flush()
			if broken != "" {
				fmt.Fprintf(d.Out, "\nFirst broken link -> %s\n", broken)
			} else {
				fmt.Fprintln(d.Out, "\nEvery link looks healthy inside the cluster; if it still fails, check DNS, the load balancer and the ingress controller's logs.")
			}
			return nil
		},
	}
	return cmd
}

func traceHTTPRoute(ctx context.Context, d Deps, host, path string, add func(string, bool, string, ...interface{})) (string, string, string) {
	rc, err := d.Flags.ToRESTConfig()
	if err != nil {
		return "", "", ""
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return "", "", ""
	}
	l, err := dyn.Resource(gvrHTTPRoute).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", ""
	}
	for _, r := range l.Items {
		hosts, _, _ := unstructured.NestedStringSlice(r.Object, "spec", "hostnames")
		hostOK := len(hosts) == 0
		for _, h := range hosts {
			if hostMatches(h, host) {
				hostOK = true
			}
		}
		if !hostOK {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(r.Object, "spec", "rules")
		for _, rr := range rules {
			rm, _ := rr.(map[string]interface{})
			matches, _, _ := unstructured.NestedSlice(rm, "matches")
			ok := len(matches) == 0
			for _, m := range matches {
				mm, _ := m.(map[string]interface{})
				pt, _, _ := unstructured.NestedString(mm, "path", "type")
				pv, _, _ := unstructured.NestedString(mm, "path", "value")
				if pt == "" {
					pt = "PathPrefix"
				}
				if pt == "PathPrefix" {
					pt = "Prefix"
				}
				if m2, _ := pathMatches(pt, pv, path); m2 {
					ok = true
				}
			}
			if !ok {
				continue
			}
			add("Route", true, "HTTPRoute %s/%s", r.GetNamespace(), r.GetName())
			parents, _, _ := unstructured.NestedSlice(r.Object, "status", "parents")
			for _, p := range parents {
				pm, _ := p.(map[string]interface{})
				conds, _, _ := unstructured.NestedSlice(pm, "conditions")
				for _, c := range conds {
					cm, _ := c.(map[string]interface{})
					if (cm["type"] == "Accepted" || cm["type"] == "ResolvedRefs") && cm["status"] == "False" {
						add("Gateway", false, "%v=False: %v", cm["type"], cm["message"])
					}
				}
			}
			backends, _, _ := unstructured.NestedSlice(rm, "backendRefs")
			if len(backends) == 0 {
				add("Service", false, "the matching rule has no backendRefs")
				return r.GetNamespace(), "", ""
			}
			bm, _ := backends[0].(map[string]interface{})
			name, _, _ := unstructured.NestedString(bm, "name")
			ns, _, _ := unstructured.NestedString(bm, "namespace")
			if ns == "" {
				ns = r.GetNamespace()
			}
			port, _, _ := unstructured.NestedInt64(bm, "port")
			return ns, name, strconv.Itoa(int(port))
		}
	}
	return "", "", ""
}

func traceService(ctx context.Context, cs kubernetes.Interface, ns, name, port string, add func(string, bool, string, ...interface{})) {
	svc, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		add("Service", false, "%s/%s: %s", ns, name, firstLineOf(err.Error()))
		return
	}
	var sp *corev1.ServicePort
	for i := range svc.Spec.Ports {
		p := &svc.Spec.Ports[i]
		if port == "" || p.Name == port || strconv.Itoa(int(p.Port)) == port {
			sp = p
			break
		}
	}
	if sp == nil {
		add("Service", false, "%s/%s has no port %q", ns, name, port)
		return
	}
	add("Service", true, "%s/%s port %d -> targetPort %s", ns, name, sp.Port, sp.TargetPort.String())
	if len(svc.Spec.Selector) == 0 {
		add("Endpoints", true, "selectorless Service (endpoints managed by hand)")
		return
	}
	slices, err := cs.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=" + name})
	ready, notReady := 0, 0
	if err == nil {
		for _, s := range slices.Items {
			for _, e := range s.Endpoints {
				if e.Conditions.Ready == nil || *e.Conditions.Ready {
					ready++
				} else {
					notReady++
				}
			}
		}
	}
	pods, _ := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	matched := 0
	if pods != nil {
		matched = len(pods.Items)
	}
	switch {
	case matched == 0:
		add("Endpoints", false, "no pods match selector %s", labels.SelectorFromSet(svc.Spec.Selector))
		return
	case ready == 0:
		add("Endpoints", false, "%d pods match, none ready (kdeck why %s)", matched, pods.Items[0].Name)
	default:
		add("Endpoints", true, "%d ready, %d not ready, %d pods match", ready, notReady, matched)
	}
	// Does the target port exist on the pods?
	if pods != nil && len(pods.Items) > 0 {
		p := pods.Items[0]
		tp := sp.TargetPort
		found := false
		where := ""
		for _, c := range p.Spec.Containers {
			for _, cp := range c.Ports {
				if (tp.Type == intstr.String && cp.Name == tp.StrVal) || (tp.Type == intstr.Int && cp.ContainerPort == tp.IntVal) {
					found, where = true, fmt.Sprintf("%s:%d (%s)", c.Name, cp.ContainerPort, dash(cp.Name))
				}
			}
		}
		switch {
		case found:
			add("Container", true, "targetPort %s = %s", tp.String(), where)
		case tp.Type == intstr.String:
			add("Container", false, "targetPort %q is not a named port on the pods' containers", tp.StrVal)
		default:
			add("Container", true, "targetPort %d not declared on containers (fine if the app listens on it)", tp.IntVal)
		}
	}
}

// --- fit ---------------------------------------------------------------------------

var gvrPodMetrics = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

// FitVerdict compares usage with requests and limits for one resource.
func FitVerdict(res string, use, req, lim *resource.Quantity) string {
	u := use.AsApproximateFloat64()
	switch {
	case lim != nil && !lim.IsZero() && res == "memory" && u >= 0.9*lim.AsApproximateFloat64():
		return "OOM risk: memory at " + pctOf(u, lim.AsApproximateFloat64()) + " of limit"
	case lim != nil && !lim.IsZero() && res == "cpu" && u >= 0.9*lim.AsApproximateFloat64():
		return "CPU throttled: at " + pctOf(u, lim.AsApproximateFloat64()) + " of limit"
	case req == nil || req.IsZero():
		return "no " + res + " request: scheduler cannot plan for it"
	case u > 1.5*req.AsApproximateFloat64():
		return res + " under-requested: using " + pctOf(u, req.AsApproximateFloat64()) + " of request"
	case u < 0.25*req.AsApproximateFloat64() && (res == "cpu" && req.MilliValue() >= 100 || res == "memory" && req.Value() >= 128<<20):
		return res + " over-requested: using " + pctOf(u, req.AsApproximateFloat64()) + " of request"
	}
	return ""
}

func pctOf(a, b float64) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", a/b*100)
}

func fitCmd(d Deps) *cobra.Command {
	var allNS, problems bool
	cmd := &cobra.Command{
		Use:   "fit [POD-REGEX]",
		Short: "Real CPU and memory use vs requests and limits per container: OOM risk, throttling, over/under-requested",
		Long:  "Compare live usage (metrics-server) with requests and limits. One sample, so treat it as a hint, not a sizing decision.\n" + regexHelp,
		Example: `  kdeck fit                 # current namespace
  kdeck fit api -A
  kdeck fit -A --problems   # only containers with a verdict`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := ""
			if len(args) == 1 {
				pattern = args[0]
			}
			pods, err := matchingPods(d, cmd, pattern, allNS, "")
			if err != nil {
				return err
			}
			rc, err := d.Flags.ToRESTConfig()
			if err != nil {
				return err
			}
			dyn, err := dynamic.NewForConfig(rc)
			if err != nil {
				return err
			}
			_, ns, _ := d.client()
			if allNS {
				ns = ""
			}
			pm, err := dyn.Resource(gvrPodMetrics).Namespace(ns).List(cmd.Context(), metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("metrics-server is not available (%s); install it to use kdeck fit", firstLineOf(err.Error()))
			}
			usage := map[string]map[string]corev1.ResourceList{}
			for _, item := range pm.Items {
				key := item.GetNamespace() + "/" + item.GetName()
				usage[key] = map[string]corev1.ResourceList{}
				ctrs, _, _ := unstructured.NestedSlice(item.Object, "containers")
				for _, c := range ctrs {
					cm, _ := c.(map[string]interface{})
					name, _, _ := unstructured.NestedString(cm, "name")
					cpu, _, _ := unstructured.NestedString(cm, "usage", "cpu")
					mem, _, _ := unstructured.NestedString(cm, "usage", "memory")
					rl := corev1.ResourceList{}
					if q, err := resource.ParseQuantity(cpu); err == nil {
						rl[corev1.ResourceCPU] = q
					}
					if q, err := resource.ParseQuantity(mem); err == nil {
						rl[corev1.ResourceMemory] = q
					}
					usage[key][name] = rl
				}
			}
			tw := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "POD\tCONTAINER\tCPU USE/REQ/LIM\tMEM USE/REQ/LIM\tVERDICT")
			rows := 0
			for _, p := range pods {
				for _, c := range p.Spec.Containers {
					u, ok := usage[p.Namespace+"/"+p.Name][c.Name]
					if !ok {
						continue
					}
					var verdicts []string
					for _, r := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
						use := u[r]
						var req, lim *resource.Quantity
						if q, ok := c.Resources.Requests[r]; ok {
							req = &q
						}
						if q, ok := c.Resources.Limits[r]; ok {
							lim = &q
						}
						if v := FitVerdict(string(r), &use, req, lim); v != "" {
							verdicts = append(verdicts, v)
						}
					}
					if problems && len(verdicts) == 0 {
						continue
					}
					rows++
					q := func(l corev1.ResourceList, r corev1.ResourceName) string {
						if v, ok := l[r]; ok {
							return v.String()
						}
						return "-"
					}
					cpuU, memU := u[corev1.ResourceCPU], u[corev1.ResourceMemory]
					fmt.Fprintf(tw, "%s\t%s\t%s/%s/%s\t%s/%s/%s\t%s\n", p.Name, c.Name,
						fmt.Sprintf("%dm", cpuU.MilliValue()), q(c.Resources.Requests, corev1.ResourceCPU), q(c.Resources.Limits, corev1.ResourceCPU),
						fmt.Sprintf("%dMi", memU.Value()>>20), q(c.Resources.Requests, corev1.ResourceMemory), q(c.Resources.Limits, corev1.ResourceMemory),
						dash(strings.Join(verdicts, "; ")))
				}
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if rows == 0 {
				fmt.Fprintln(d.Out, "No matching containers with metrics.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&allNS, "all-namespaces", "A", false, "all namespaces")
	cmd.Flags().BoolVar(&problems, "problems", false, "only containers with a verdict")
	return cmd
}

// --- timeline --------------------------------------------------------------------------

type tlEntry struct {
	at                 time.Time
	object, kind, what string
	warn               bool
}

func timelineCmd(d Deps) *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use:     "timeline TYPE/NAME | POD-REGEX",
		Aliases: []string{"tl-events", "history-of"},
		Short:   "What happened to a workload, in order: events, rollouts, restarts and crashes of it, its ReplicaSets and pods",
		Example: `  kdeck timeline deploy/api
  kdeck timeline deploy/api --since 30m
  kdeck timeline api               # pods matching a regex`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := d.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			window := 24 * time.Hour
			if since != "" {
				if window, err = time.ParseDuration(since); err != nil {
					return err
				}
			}
			names := map[string]bool{}
			var entries []tlEntry
			var pods []corev1.Pod
			if kind, name, ok := strings.Cut(args[0], "/"); ok && !strings.ContainsAny(args[0], "^$|()[]*+?\\") {
				names[name] = true
				sel, err := workloadSelector(d, cmd, args[0])
				if err != nil {
					return err
				}
				pl, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
				if err != nil {
					return err
				}
				pods = pl.Items
				if strings.HasPrefix(strings.ToLower(kind), "deploy") {
					rss, err := cs.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
					if err == nil {
						for _, rs := range rss.Items {
							names[rs.Name] = true
							rev := rs.Annotations["deployment.kubernetes.io/revision"]
							entries = append(entries, tlEntry{rs.CreationTimestamp.Time, "ReplicaSet/" + rs.Name, "Rollout", "revision " + rev + " created (" + rsImages(rs) + ")", false})
						}
					}
				}
			} else {
				pods, err = matchingPods(d, cmd, args[0], false, "")
				if err != nil {
					return err
				}
			}
			for _, p := range pods {
				names[p.Name] = true
				entries = append(entries, tlEntry{p.CreationTimestamp.Time, "Pod/" + p.Name, "Created", "on " + dash(p.Spec.NodeName), false})
				for _, s := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
					if t := s.LastTerminationState.Terminated; t != nil {
						entries = append(entries, tlEntry{t.FinishedAt.Time, "Pod/" + p.Name, "Restart", fmt.Sprintf("%s exited %d (%s), restart #%d", s.Name, t.ExitCode, t.Reason, s.RestartCount), t.ExitCode != 0})
					}
				}
			}
			evs, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
			if err == nil {
				for _, e := range evs.Items {
					if !names[e.InvolvedObject.Name] {
						continue
					}
					msg := strings.TrimSpace(e.Message)
					if e.Count > 1 {
						msg += fmt.Sprintf(" (x%d)", e.Count)
					}
					entries = append(entries, tlEntry{eventTime(e), e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name, e.Reason, msg, e.Type == corev1.EventTypeWarning})
				}
			}
			cut := time.Now().Add(-window)
			sort.Slice(entries, func(a, b int) bool { return entries[a].at.Before(entries[b].at) })
			tw := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TIME\tAGO\tOBJECT\tWHAT\tDETAIL")
			n := 0
			for _, e := range entries {
				if e.at.Before(cut) || e.at.IsZero() {
					continue
				}
				what := e.kind
				if e.warn {
					what = "! " + what
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.at.Local().Format("15:04:05"), age(e.at), e.object, what, e.what)
				n++
			}
			_ = tw.Flush()
			if n == 0 {
				fmt.Fprintf(d.Out, "Nothing in the last %s (events expire after about an hour by default).\n", window)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "time window (default 24h; events themselves expire after ~1h)")
	return cmd
}

func rsImages(rs appsv1.ReplicaSet) string {
	var imgs []string
	for _, c := range rs.Spec.Template.Spec.Containers {
		imgs = append(imgs, c.Image)
	}
	return strings.Join(imgs, ", ")
}

// --- node-sh -------------------------------------------------------------------------------

func nodeShCmd(d Deps) *cobra.Command {
	var image string
	cmd := &cobra.Command{
		Use:   "node-sh NODE-REGEX",
		Short: "Root shell on a node (kubectl debug, host filesystem at /), debug pod removed afterwards",
		Example: `  kdeck node-sh worker-3
  kdeck node-sh lke --image nicolaka/netshoot:v0.13`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := d.client()
			if err != nil {
				return err
			}
			m, err := match.Compile(args[0])
			if err != nil {
				return err
			}
			nodes, err := cs.CoreV1().Nodes().List(cmd.Context(), metav1.ListOptions{})
			if err != nil {
				return err
			}
			var names []string
			for _, n := range nodes.Items {
				if n.Name == args[0] {
					names = []string{n.Name}
					break
				}
				if m.Match(n.Name) {
					names = append(names, n.Name)
				}
			}
			node, err := pickOne("node", names)
			if err != nil {
				return err
			}
			start := time.Now().Add(-5 * time.Second)
			runErr := d.Kubectl("debug", "node/"+node, "-it", "--profile=sysadmin", "--image="+image, "--", "chroot", "/host", "sh", "-c", "command -v bash >/dev/null && exec bash || exec sh")
			// kubectl debug leaves the pod behind; remove ours.
			pods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{})
			if err == nil {
				for _, p := range pods.Items {
					if strings.HasPrefix(p.Name, "node-debugger-"+node+"-") && p.CreationTimestamp.After(start) {
						_ = cs.CoreV1().Pods(ns).Delete(context.Background(), p.Name, metav1.DeleteOptions{})
						fmt.Fprintf(d.ErrOut, "Removed debug pod %s/%s.\n", ns, p.Name)
					}
				}
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&image, "image", "busybox:1.36", "debug image")
	return cmd
}

// --- edit-secret ----------------------------------------------------------------------------

func editor() []string {
	for _, env := range []string{"KUBE_EDITOR", "EDITOR"} {
		if e := os.Getenv(env); e != "" {
			return strings.Fields(e)
		}
	}
	if runtime.GOOS == "windows" {
		return []string{"notepad"}
	}
	return []string{"vi"}
}

func editSecretCmd(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit-secret SECRET",
		Short: "Edit a Secret's values decoded in your editor; re-encoded on save (temp file wiped afterwards)",
		Example: `  kdeck edit-secret db-credentials
  KUBE_EDITOR="code --wait" kdeck edit-secret api-keys`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := d.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			sec, err := cs.CoreV1().Secrets(ns).Get(ctx, args[0], metav1.GetOptions{})
			if err != nil {
				return err
			}
			plain := map[string]string{}
			for k, v := range sec.Data {
				plain[k] = string(v)
			}
			body, err := yaml.Marshal(plain)
			if err != nil {
				return err
			}
			dir, err := os.MkdirTemp("", "kdeck-secret-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, sec.Name+".yaml")
			header := fmt.Sprintf("# Secret %s/%s, decoded. Save and close to apply; leave unchanged to cancel.\n", ns, sec.Name)
			if err := os.WriteFile(path, append([]byte(header), body...), 0o600); err != nil {
				return err
			}
			defer func() { _ = os.WriteFile(path, bytes.Repeat([]byte{0}, len(body)+len(header)), 0o600) }()
			ed := editor()
			c := exec.Command(ed[0], append(ed[1:], path)...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("editor: %w", err)
			}
			edited, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var next map[string]string
			if err := yaml.Unmarshal(edited, &next); err != nil {
				return fmt.Errorf("not valid YAML, nothing changed: %w", err)
			}
			var added, changed, removed []string
			newData := map[string][]byte{}
			for k, v := range next {
				newData[k] = []byte(v)
				old, ok := plain[k]
				switch {
				case !ok:
					added = append(added, k)
				case old != v:
					changed = append(changed, k)
				}
			}
			for k := range plain {
				if _, ok := next[k]; !ok {
					removed = append(removed, k)
				}
			}
			if len(added)+len(changed)+len(removed) == 0 {
				fmt.Fprintln(d.Out, "No changes.")
				return nil
			}
			sec.Data = newData
			if _, err := cs.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
				return err
			}
			sort.Strings(added)
			sort.Strings(changed)
			sort.Strings(removed)
			fmt.Fprintf(d.Out, "Updated secret %s/%s: added %v, changed %v, removed %v. Pods read it at start: kdeck restart if needed.\n", ns, sec.Name, added, changed, removed)
			return nil
		},
	}
	return cmd
}

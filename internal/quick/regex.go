package quick

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"

	"github.com/lyelgate/kubepilot/internal/match"
	"github.com/lyelgate/kubepilot/internal/stern"
)

// regexHelp is appended to commands that take a pattern.
const regexHelp = `
Patterns are regular expressions with smart case: lower-case patterns ignore
case, a pattern with an upper-case letter matches case exactly.
  api            any name containing "api"
  ^web-          names starting with "web-"
  api|worker     either
  ^db-[0-9]+$    db-0, db-1, ... exactly`

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// matchingPods lists pods whose name matches pattern, running and ready first.
func matchingPods(d Deps, cmd *cobra.Command, pattern string, all bool, selector string) ([]corev1.Pod, error) {
	m, err := match.Compile(pattern)
	if err != nil {
		return nil, err
	}
	pods, err := listPods(d, cmd, all, selector)
	if err != nil {
		return nil, err
	}
	var out []corev1.Pod
	for _, p := range pods {
		if m.Match(p.Name) {
			out = append(out, p)
		}
	}
	rank := func(p corev1.Pod) int {
		if p.DeletionTimestamp != nil {
			return 3
		}
		switch p.Status.Phase {
		case corev1.PodRunning:
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					return 0
				}
			}
			return 1
		case corev1.PodPending:
			return 2
		}
		return 3
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	return out, nil
}

// pickOne chooses among candidates: the only one, an fzf choice, or an error
// that lists them.
func pickOne(what string, names []string) (string, error) {
	switch len(names) {
	case 0:
		return "", fmt.Errorf("no %s matches", what)
	case 1:
		return names[0], nil
	}
	if fzf, err := exec.LookPath("fzf"); err == nil && os.Getenv("KP_NO_FZF") == "" && isTTY(os.Stdin) && isTTY(os.Stdout) {
		c := exec.Command(fzf, "--height=40%", "--reverse", "--prompt="+what+"> ")
		c.Stdin = strings.NewReader(strings.Join(names, "\n"))
		c.Stderr = os.Stderr
		var buf bytes.Buffer
		c.Stdout = &buf
		if err := c.Run(); err == nil && strings.TrimSpace(buf.String()) != "" {
			return strings.TrimSpace(buf.String()), nil
		}
		return "", fmt.Errorf("nothing selected")
	}
	shown := names
	if len(shown) > 10 {
		shown = append(append([]string{}, names[:10]...), fmt.Sprintf("... and %d more", len(names)-10))
	}
	return "", fmt.Errorf("%d %ss match; be more specific:\n  %s", len(names), what, strings.Join(shown, "\n  "))
}

// --- tail / stern -----------------------------------------------------------------

func tailCmd(d Deps) *cobra.Command {
	var (
		all, noFollow, timestamps, initC bool
		selector, container, since       string
		colorMode, excludePod            string
		include, exclude                 []string
		lines                            int64
	)
	cmd := &cobra.Command{
		Use:     "tail POD-REGEX | TYPE/NAME",
		Aliases: []string{"stern", "st", "logs-all", "lg", "kptail"},
		Short:   "Stern-style logs: every pod and container matching a regex, colour per pod, new pods picked up live",
		Long: `Follow logs from every pod whose name matches a regex, and every container
whose name matches -c. In follow mode the API is watched, so pods created
later (scale-ups, rollouts, restarts) join the stream automatically.
` + regexHelp,
		Example: `  kp tail api                       # all pods with "api" in the name
  kp tail '^web-' -c nginx          # only the nginx containers
  kp tail . -A -i error -e healthz  # every pod, lines with "error", not "healthz"
  kp tail deploy/api --since 10m    # pods of a workload
  kp tail api -l tier=backend       # regex and label selector together
  kp stern api                      # same command`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := d.client()
			if err != nil {
				return err
			}
			if all {
				ns = ""
			}
			query := args[0]
			sel := selector
			if strings.Contains(query, "/") && !strings.ContainsAny(query, "^$|()[]*+?\\") {
				s, err := workloadSelector(d, cmd, query)
				if err != nil {
					return err
				}
				if sel != "" {
					sel += ","
				}
				sel += s
				query = ""
			}
			pod, err := match.Compile(query)
			if err != nil {
				return err
			}
			ctr, err := match.Compile(container)
			if err != nil {
				return err
			}
			var exPod *match.Matcher
			if excludePod != "" {
				if exPod, err = match.Compile(excludePod); err != nil {
					return err
				}
			}
			compileAll := func(ps []string) ([]*match.Matcher, error) {
				var out []*match.Matcher
				for _, p := range ps {
					m, err := match.Compile(p)
					if err != nil {
						return nil, err
					}
					out = append(out, m)
				}
				return out, nil
			}
			inc, err := compileAll(include)
			if err != nil {
				return err
			}
			exc, err := compileAll(exclude)
			if err != nil {
				return err
			}
			var sinceDur time.Duration
			if since != "" {
				if sinceDur, err = time.ParseDuration(since); err != nil {
					return err
				}
			}
			color := colorMode == "always" || (colorMode == "auto" && isTTY(os.Stdout) && os.Getenv("NO_COLOR") == "")
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return stern.Run(ctx, cs, stern.Options{
				Namespace: ns, Selector: sel, Pod: pod, ExcludePod: exPod, Container: ctr,
				Include: inc, Exclude: exc, Since: sinceDur, Tail: lines, Follow: !noFollow,
				Timestamps: timestamps, InitContainers: initC, Color: color,
			}, d.Out)
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&all, "all-namespaces", "A", false, "all namespaces")
	f.StringVarP(&selector, "selector", "l", "", "label selector, combined with the regex")
	f.StringVarP(&container, "container", "c", "", "container name regex")
	f.StringVarP(&excludePod, "exclude-pod", "E", "", "skip pods matching this regex")
	f.StringArrayVarP(&include, "include", "i", nil, "only lines matching this regex (repeatable: all must match)")
	f.StringArrayVarP(&exclude, "exclude", "e", nil, "drop lines matching this regex (repeatable)")
	f.StringVar(&since, "since", "", "only logs newer than this (30s, 10m, 2h)")
	f.Int64Var(&lines, "tail", 10, "lines of history per container at start (-1 = all)")
	f.Int64Var(&lines, "lines", 10, "alias for --tail")
	f.BoolVar(&noFollow, "no-follow", false, "print existing logs and exit")
	f.BoolVarP(&timestamps, "timestamps", "t", false, "show timestamps")
	f.BoolVar(&initC, "init-containers", false, "include init containers")
	f.StringVar(&colorMode, "color", "auto", "auto, always or never")
	return cmd
}

// --- pf -----------------------------------------------------------------------------

func localPort(remote int32) int32 {
	if remote < 1024 {
		return remote + 8000 // 80 -> 8080, 443 -> 8443: no root needed
	}
	return remote
}

func pfCmd(d Deps) *cobra.Command {
	var address string
	var podsOnly bool
	cmd := &cobra.Command{
		Use:     "pf SVC-OR-POD-REGEX | TYPE/NAME [[LOCAL:]REMOTE...]",
		Aliases: []string{"fwd", "forward", "kppf"},
		Short:   "Port-forward by regex: finds the Service (or pod) and its ports for you",
		Long: `Port-forward to the Service or pod whose name matches a regex. Services are
preferred; --pod forwards to a pod instead. Without ports, every declared
port is forwarded; ports below 1024 map to +8000 locally (80 -> 8080).
When several match, fzf opens if installed, otherwise the matches are listed.
` + regexHelp,
		Example: `  kp pf grafana                 # svc/grafana, all its ports
  kp pf api 9000:8080           # svc matching "api", local 9000 -> 8080
  kp pf '^web-0$' --pod         # a specific pod
  kp pf deploy/api 8080         # TYPE/NAME passes straight to kubectl
  kp pf grafana --address 0.0.0.0`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := d.client()
			if err != nil {
				return err
			}
			query, ports := args[0], args[1:]
			target := ""
			var declared []int32
			if strings.Contains(query, "/") && !strings.ContainsAny(query, "^$|()[]*+?\\") {
				target = query
			} else {
				m, err := match.Compile(query)
				if err != nil {
					return err
				}
				if !podsOnly {
					svcs, err := cs.CoreV1().Services(ns).List(cmd.Context(), metav1.ListOptions{})
					if err != nil {
						return err
					}
					var names []string
					byName := map[string]corev1.Service{}
					for _, s := range svcs.Items {
						if m.Match(s.Name) && len(s.Spec.Selector) > 0 {
							names = append(names, s.Name)
							byName[s.Name] = s
						}
					}
					sort.Strings(names)
					if len(names) > 0 {
						name, err := pickOne("service", names)
						if err != nil {
							return err
						}
						target = "svc/" + name
						for _, p := range byName[name].Spec.Ports {
							declared = append(declared, p.Port)
						}
					}
				}
				if target == "" {
					pods, err := matchingPods(d, cmd, query, false, "")
					if err != nil {
						return err
					}
					var names []string
					byName := map[string]corev1.Pod{}
					for _, p := range pods {
						if p.Status.Phase == corev1.PodRunning {
							names = append(names, p.Name)
							byName[p.Name] = p
						}
					}
					name, err := pickOne("running pod", names)
					if err != nil {
						return err
					}
					target = "pod/" + name
					for _, c := range byName[name].Spec.Containers {
						for _, p := range c.Ports {
							declared = append(declared, p.ContainerPort)
						}
					}
				}
			}
			if len(ports) == 0 {
				if len(declared) == 0 {
					return fmt.Errorf("%s declares no ports; give them explicitly, e.g. kp pf %s 8080", target, query)
				}
				for _, p := range declared {
					ports = append(ports, fmt.Sprintf("%d:%d", localPort(p), p))
				}
			}
			host := address
			if host == "" || host == "localhost" || host == "127.0.0.1" {
				host = "localhost"
			}
			for _, p := range ports {
				local, remote, ok := strings.Cut(p, ":")
				if !ok {
					remote = local
				}
				if local == "" {
					local = "random"
				}
				fmt.Fprintf(d.ErrOut, "Forwarding http://%s:%s -> %s:%s\n", host, local, target, remote)
			}
			kargs := append([]string{"port-forward", target}, ports...)
			if address != "" {
				kargs = append(kargs, "--address", address)
			}
			return d.Kubectl(kargs...)
		},
	}
	cmd.Flags().StringVar(&address, "address", "", "local addresses to listen on (default localhost)")
	cmd.Flags().BoolVar(&podsOnly, "pod", false, "match pods instead of Services")
	return cmd
}

// --- pod details ----------------------------------------------------------------------

func podCmd(d Deps) *cobra.Command {
	var all, describe bool
	var selector, output string
	cmd := &cobra.Command{
		Use:     "pod POD-REGEX",
		Aliases: []string{"pd", "inspect", "kppod"},
		Short:   "Details of every pod matching a regex: node, IP, owner, containers, restarts, last crash, warnings",
		Long:    "Show a compact detail card for each pod whose name matches a regex.\n" + regexHelp,
		Example: `  kp pod api                  # every pod with "api" in the name
  kp pod '^web-[0-9]+$' -A
  kp pod api --describe       # full kubectl describe for each match
  kp pod api -o yaml          # kubectl get -o yaml for each match`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pods, err := matchingPods(d, cmd, args[0], all, selector)
			if err != nil {
				return err
			}
			if len(pods) == 0 {
				return fmt.Errorf("no pods match %q", args[0])
			}
			if describe || output != "" {
				for _, p := range pods {
					kargs := []string{"describe", "pod", p.Name, "-n", p.Namespace}
					if output != "" {
						kargs = []string{"get", "pod", p.Name, "-n", p.Namespace, "-o", output}
					}
					if err := d.Kubectl(kargs...); err != nil {
						return err
					}
				}
				return nil
			}
			cs, _, err := d.client()
			if err != nil {
				return err
			}
			for i, p := range pods {
				if i > 0 {
					fmt.Fprintln(d.Out)
				}
				printPodCard(d, cmd, cs, p)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "all namespaces")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "label selector, combined with the regex")
	cmd.Flags().BoolVar(&describe, "describe", false, "run kubectl describe for each match")
	cmd.Flags().StringVarP(&output, "output", "o", "", "kubectl output format for each match (yaml, json, wide)")
	return cmd
}

func printPodCard(d Deps, cmd *cobra.Command, cs kubernetes.Interface, p corev1.Pod) {
	w := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
	rows := podRows([]corev1.Pod{p})
	status := rows[0].status
	fmt.Fprintf(w, "Pod:\t%s/%s\n", p.Namespace, p.Name)
	fmt.Fprintf(w, "Status:\t%s (ready %s, %d restarts, age %s)\n", status, rows[0].ready, rows[0].restarts, age(p.CreationTimestamp.Time))
	fmt.Fprintf(w, "Node:\t%s\n", dash(p.Spec.NodeName))
	fmt.Fprintf(w, "Pod IP:\t%s\n", dash(p.Status.PodIP))
	for _, o := range p.OwnerReferences {
		fmt.Fprintf(w, "Owner:\t%s/%s\n", o.Kind, o.Name)
	}
	fmt.Fprintf(w, "QoS:\t%s\n", p.Status.QOSClass)
	sa := p.Spec.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	fmt.Fprintf(w, "ServiceAccount:\t%s\n", sa)
	for _, c := range p.Status.Conditions {
		if c.Status != corev1.ConditionTrue && c.Message != "" {
			fmt.Fprintf(w, "Not %s:\t%s\n", c.Type, c.Message)
		}
	}
	_ = w.Flush()

	statuses := map[string]corev1.ContainerStatus{}
	for _, s := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		statuses[s.Name] = s
	}
	cw := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(cw, "  CONTAINER\tREADY\tRESTARTS\tSTATE\tLAST TERMINATION\tREQUESTS\tLIMITS\tIMAGE")
	show := func(c corev1.Container, init bool) {
		s := statuses[c.Name]
		state := "waiting"
		switch {
		case s.State.Running != nil:
			state = "running " + age(s.State.Running.StartedAt.Time)
		case s.State.Waiting != nil:
			state = s.State.Waiting.Reason
		case s.State.Terminated != nil:
			state = fmt.Sprintf("%s (exit %d)", s.State.Terminated.Reason, s.State.Terminated.ExitCode)
		}
		last := "-"
		if t := s.LastTerminationState.Terminated; t != nil {
			last = fmt.Sprintf("%s exit %d, %s ago", t.Reason, t.ExitCode, age(t.FinishedAt.Time))
		}
		name := c.Name
		if init {
			name += " (init)"
		}
		fmt.Fprintf(cw, "  %s\t%v\t%d\t%s\t%s\t%s\t%s\t%s\n", name, s.Ready, s.RestartCount, state, last,
			resources(c.Resources.Requests), resources(c.Resources.Limits), c.Image)
	}
	for _, c := range p.Spec.InitContainers {
		show(c, true)
	}
	for _, c := range p.Spec.Containers {
		show(c, false)
	}
	_ = cw.Flush()

	evs, err := cs.CoreV1().Events(p.Namespace).List(cmd.Context(), metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Pod,involvedObject.name=" + p.Name + ",type=Warning",
	})
	if err == nil && len(evs.Items) > 0 {
		sort.Slice(evs.Items, func(i, j int) bool { return eventTime(evs.Items[i]).After(eventTime(evs.Items[j])) })
		fmt.Fprintln(d.Out, "  Recent warnings:")
		for i, e := range evs.Items {
			if i == 5 {
				break
			}
			fmt.Fprintf(d.Out, "    %s ago  %s  %s (x%d)\n", age(eventTime(e)), e.Reason, strings.TrimSpace(e.Message), max(e.Count, 1))
		}
	}
}

func eventTime(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func resources(r corev1.ResourceList) string {
	var parts []string
	if q, ok := r[corev1.ResourceCPU]; ok {
		parts = append(parts, "cpu="+q.String())
	}
	if q, ok := r[corev1.ResourceMemory]; ok {
		parts = append(parts, "mem="+q.String())
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

// --- find -----------------------------------------------------------------------------

// defaultFindKinds are searched when no kinds are given.
var defaultFindKinds = "pods,deployments,statefulsets,daemonsets,replicasets,services,ingresses,configmaps,secrets,jobs,cronjobs,persistentvolumeclaims,horizontalpodautoscalers,serviceaccounts,nodes,namespaces,persistentvolumes"

func findCmd(d Deps) *cobra.Command {
	var all, labels bool
	var kinds string
	cmd := &cobra.Command{
		Use:     "find REGEX [KIND[,KIND...]]",
		Aliases: []string{"search", "grep", "kpfind"},
		Short:   "Regex search over resource names (and labels) across many kinds at once",
		Long: `Search resource names by regex across kinds: pods, workloads, Services,
Ingresses, ConfigMaps, Secrets, jobs, PVCs, HPAs, nodes, namespaces and more.
Kinds accept kubectl names and short names (po, deploy, svc, cm, ...), and any
CRD. Only metadata is read, so Secret contents are never fetched.
` + regexHelp,
		Example: `  kp find payment -A                  # anything named like "payment", all namespaces
  kp find '^redis' svc,sts,pvc -A
  kp find 'team=core' --labels -A     # match labels (key=value) too
  kp find cert certificates.cert-manager.io -A`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := match.Compile(args[0])
			if err != nil {
				return err
			}
			if len(args) == 2 {
				kinds = args[1]
			}
			rc, err := d.Flags.ToRESTConfig()
			if err != nil {
				return err
			}
			mc, err := metadata.NewForConfig(rc)
			if err != nil {
				return err
			}
			mapper, err := d.Flags.ToRESTMapper()
			if err != nil {
				return err
			}
			_, ns, err := d.client()
			if err != nil {
				return err
			}
			if all {
				ns = ""
			}
			type hit struct{ kind, ns, name, age string }
			var hits []hit
			var skipped []string
			for _, k := range strings.Split(kinds, ",") {
				k = strings.TrimSpace(k)
				if k == "" {
					continue
				}
				gvr, err := mapper.ResourceFor(schema.ParseGroupResource(k).WithVersion(""))
				if err != nil {
					skipped = append(skipped, k)
					continue
				}
				mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gvr.Group, Kind: mustKind(mapper, gvr)}, gvr.Version)
				if err != nil {
					skipped = append(skipped, k)
					continue
				}
				var list *metav1.PartialObjectMetadataList
				if mapping.Scope.Name() == "namespace" {
					list, err = mc.Resource(gvr).Namespace(ns).List(cmd.Context(), metav1.ListOptions{})
				} else {
					list, err = mc.Resource(gvr).List(cmd.Context(), metav1.ListOptions{})
				}
				if err != nil {
					skipped = append(skipped, k+" ("+firstLineOf(err.Error())+")")
					continue
				}
				for _, o := range list.Items {
					ok := m.Match(o.Name)
					if !ok && labels {
						for lk, lv := range o.Labels {
							if m.Match(lk + "=" + lv) {
								ok = true
								break
							}
						}
					}
					if ok {
						hits = append(hits, hit{mapping.GroupVersionKind.Kind, o.Namespace, o.Name, age(o.CreationTimestamp.Time)})
					}
				}
			}
			if len(hits) == 0 {
				fmt.Fprintf(d.Out, "Nothing matches %q.\n", args[0])
			} else {
				tw := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "KIND\tNAMESPACE\tNAME\tAGE")
				for _, h := range hits {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", h.kind, dash(h.ns), h.name, h.age)
				}
				_ = tw.Flush()
			}
			if len(skipped) > 0 {
				fmt.Fprintf(d.ErrOut, "skipped: %s\n", strings.Join(skipped, ", "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "all namespaces")
	cmd.Flags().BoolVar(&labels, "labels", false, "also match labels as key=value")
	cmd.Flags().StringVar(&kinds, "kinds", defaultFindKinds, "comma-separated kinds to search")
	return cmd
}

func mustKind(mapper interface {
	KindFor(schema.GroupVersionResource) (schema.GroupVersionKind, error)
}, gvr schema.GroupVersionResource) string {
	gvk, err := mapper.KindFor(gvr)
	if err != nil {
		return ""
	}
	return gvk.Kind
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

var _ = strconv.Itoa

// matchingWorkloads returns TYPE/NAME for Deployments, StatefulSets and
// DaemonSets whose name matches pattern in the current namespace.
func matchingWorkloads(d Deps, cmd *cobra.Command, pattern string) ([]string, error) {
	m, err := match.Compile(pattern)
	if err != nil {
		return nil, err
	}
	cs, ns, err := d.client()
	if err != nil {
		return nil, err
	}
	ctx := cmd.Context()
	var out []string
	if l, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, o := range l.Items {
			if m.Match(o.Name) {
				out = append(out, "deployment/"+o.Name)
			}
		}
	} else {
		return nil, err
	}
	if l, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, o := range l.Items {
			if m.Match(o.Name) {
				out = append(out, "statefulset/"+o.Name)
			}
		}
	}
	if l, err := cs.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, o := range l.Items {
			if m.Match(o.Name) {
				out = append(out, "daemonset/"+o.Name)
			}
		}
	}
	return out, nil
}

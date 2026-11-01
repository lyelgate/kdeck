// Package quick holds everyday shortcuts that kubectl lacks or makes long.
// Where kubectl already does the job, a shortcut builds the kubectl
// arguments and runs the embedded kubectl, so behaviour stays identical.
package quick

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// Runner runs the embedded kubectl with the given arguments, writing its
// standard output to out.
type Runner func(out io.Writer, args ...string) error

// Deps is what the shortcuts need from the application.
type Deps struct {
	Flags  *genericclioptions.ConfigFlags
	Out    io.Writer
	ErrOut io.Writer
	Run    Runner
}

// Kubectl runs the embedded kubectl, writing to the normal output.
func (d Deps) Kubectl(args ...string) error { return d.Run(d.Out, args...) }

func (d Deps) client() (kubernetes.Interface, string, error) {
	rc, err := d.Flags.ToRESTConfig()
	if err != nil {
		return nil, "", err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, "", err
	}
	ns, _, err := d.Flags.ToRawKubeConfigLoader().Namespace()
	return cs, ns, err
}

// Commands returns every shortcut command, grouped under "Shortcuts".
func Commands(d Deps) []*cobra.Command {
	cmds := []*cobra.Command{
		whoCmd(d), podsCmd(d), badCmd(d), shCmd(d), tailCmd(d), restartCmd(d),
		imagesCmd(d), cleanCmd(d), neatCmd(d), decodeCmd(d), nodesCmd(d),
		netshootCmd(d), usageCmd(d), eachCmd(d),
	}
	for _, c := range cmds {
		c.GroupID = "shortcuts"
	}
	return cmds
}

func age(t time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(t))
}

// --- who ---------------------------------------------------------------

func whoCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:     "who",
		Aliases: []string{"whoami", "me"},
		Short:   "Show context, cluster, namespace, server and the identity the API server sees",
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := d.Flags.ToRawKubeConfigLoader().RawConfig()
			if err != nil {
				return err
			}
			ctxName := raw.CurrentContext
			if d.Flags.Context != nil && *d.Flags.Context != "" {
				ctxName = *d.Flags.Context
			}
			kc := raw.Contexts[ctxName]
			ns, _, _ := d.Flags.ToRawKubeConfigLoader().Namespace()
			w := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Context:\t%s\n", ctxName)
			if kc != nil {
				fmt.Fprintf(w, "Cluster:\t%s\n", kc.Cluster)
				fmt.Fprintf(w, "Kubeconfig user:\t%s\n", kc.AuthInfo)
				if cl := raw.Clusters[kc.Cluster]; cl != nil {
					fmt.Fprintf(w, "Server:\t%s\n", cl.Server)
				}
			}
			fmt.Fprintf(w, "Namespace:\t%s\n", ns)
			if cs, _, err := d.client(); err == nil {
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
				defer cancel()
				if ssr, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{}); err == nil {
					fmt.Fprintf(w, "Authenticated as:\t%s\n", ssr.Status.UserInfo.Username)
					fmt.Fprintf(w, "Groups:\t%s\n", strings.Join(ssr.Status.UserInfo.Groups, ", "))
				} else {
					fmt.Fprintf(w, "Authenticated as:\t(unavailable: %v)\n", err)
				}
				if v, err := cs.Discovery().ServerVersion(); err == nil {
					fmt.Fprintf(w, "Server version:\t%s\n", v.GitVersion)
				}
			}
			return w.Flush()
		},
	}
}

// --- pods / bad ---------------------------------------------------------

type podRow struct {
	ns, name, ready, status, node string
	restarts                      int32
	created                       time.Time
	healthy                       bool
}

func podRows(pods []corev1.Pod) []podRow {
	rows := make([]podRow, 0, len(pods))
	for _, p := range pods {
		var ready, total int
		var restarts int32
		status := string(p.Status.Phase)
		if p.Status.Reason != "" {
			status = p.Status.Reason
		}
		for _, cs := range p.Status.ContainerStatuses {
			total++
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				status = cs.State.Waiting.Reason
			} else if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && p.Status.Phase != corev1.PodSucceeded {
				status = cs.State.Terminated.Reason
			}
		}
		if p.DeletionTimestamp != nil {
			status = "Terminating"
		}
		healthy := (p.Status.Phase == corev1.PodRunning && ready == total && total > 0) || p.Status.Phase == corev1.PodSucceeded
		rows = append(rows, podRow{
			ns: p.Namespace, name: p.Name, ready: fmt.Sprintf("%d/%d", ready, total),
			status: status, node: p.Spec.NodeName, restarts: restarts,
			created: p.CreationTimestamp.Time, healthy: healthy,
		})
	}
	// Problems first, then most restarts, then name.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].healthy != rows[j].healthy {
			return !rows[i].healthy
		}
		if rows[i].restarts != rows[j].restarts {
			return rows[i].restarts > rows[j].restarts
		}
		return rows[i].ns+"/"+rows[i].name < rows[j].ns+"/"+rows[j].name
	})
	return rows
}

func listPods(d Deps, cmd *cobra.Command, all bool, selector string) ([]corev1.Pod, error) {
	cs, ns, err := d.client()
	if err != nil {
		return nil, err
	}
	if all {
		ns = ""
	}
	list, err := cs.CoreV1().Pods(ns).List(cmd.Context(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func printPods(out io.Writer, rows []podRow, filter string, onlyBad, showNS bool) error {
	var shown []podRow
	for _, r := range rows {
		if onlyBad && r.healthy {
			continue
		}
		if filter != "" && !strings.Contains(r.name, filter) {
			continue
		}
		shown = append(shown, r)
	}
	if len(shown) == 0 {
		if onlyBad {
			fmt.Fprintln(out, "No unhealthy pods.")
		} else {
			fmt.Fprintln(out, "No pods found.")
		}
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if showNS {
		fmt.Fprint(w, "NAMESPACE\t")
	}
	fmt.Fprintln(w, "NAME\tREADY\tSTATUS\tRESTARTS\tAGE\tNODE")
	for _, r := range shown {
		if showNS {
			fmt.Fprintf(w, "%s\t", r.ns)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", r.name, r.ready, r.status, r.restarts, age(r.created), r.node)
	}
	return w.Flush()
}

func podsCmd(d Deps) *cobra.Command {
	var all, bad bool
	var selector string
	cmd := &cobra.Command{
		Use:     "pods [NAME-FILTER]",
		Aliases: []string{"ps"},
		Short:   "Pods sorted problems first, with real status (CrashLoopBackOff, OOMKilled, ...)",
		Example: `  kp pods            # current namespace
  kp pods api -A     # pods whose name contains "api", all namespaces
  kp pods --bad -A   # only pods that are not Running and Ready`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pods, err := listPods(d, cmd, all, selector)
			if err != nil {
				return err
			}
			filter := ""
			if len(args) == 1 {
				filter = args[0]
			}
			return printPods(d.Out, podRows(pods), filter, bad, all)
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "all namespaces")
	cmd.Flags().BoolVar(&bad, "bad", false, "only unhealthy pods")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "label selector")
	return cmd
}

func badCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "bad",
		Short: "Unhealthy pods in all namespaces (same as: kp pods --bad -A)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			pods, err := listPods(d, cmd, true, "")
			if err != nil {
				return err
			}
			return printPods(d.Out, podRows(pods), "", true, true)
		},
	}
}

// --- sh -----------------------------------------------------------------

func shCmd(d Deps) *cobra.Command {
	var container string
	cmd := &cobra.Command{
		Use:     "sh POD|TYPE/NAME [-- COMMAND...]",
		Aliases: []string{"shell"},
		Short:   "Open a shell in a pod: bash if present, otherwise sh",
		Example: `  kp sh web-0
  kp sh deploy/api -c app
  kp sh web-0 -- ls /data`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			kargs := []string{"exec", "-it", target}
			if container != "" {
				kargs = append(kargs, "-c", container)
			}
			kargs = append(kargs, "--")
			if len(args) > 1 {
				kargs = append(kargs, args[1:]...)
			} else {
				kargs = append(kargs, "sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash -l || exec sh")
			}
			return d.Kubectl(kargs...)
		},
	}
	cmd.Flags().StringVarP(&container, "container", "c", "", "container name")
	return cmd
}

// --- tail ---------------------------------------------------------------

func tailCmd(d Deps) *cobra.Command {
	var lines int
	var since string
	var noFollow bool
	cmd := &cobra.Command{
		Use:     "tail SELECTOR|TYPE/NAME",
		Aliases: []string{"logs-all", "lg"},
		Short:   "Follow logs from every pod and container of an app, prefixed by pod",
		Example: `  kp tail app=web               # every pod matching the label
  kp tail deploy/api --since 10m
  kp tail sts/db --lines 200 --no-follow`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kargs := []string{"logs", "--all-containers", "--prefix", "--max-log-requests=50", fmt.Sprintf("--tail=%d", lines)}
			if !noFollow {
				kargs = append(kargs, "-f")
			}
			if since != "" {
				kargs = append(kargs, "--since="+since)
			}
			sel := args[0]
			if strings.Contains(sel, "/") && !strings.Contains(sel, "=") {
				// TYPE/NAME: resolve the workload's selector so every pod is followed,
				// not just one, which is what "kubectl logs deploy/x" does.
				s, err := workloadSelector(d, cmd, sel)
				if err != nil {
					return err
				}
				sel = s
			}
			kargs = append(kargs, "-l", sel)
			return d.Kubectl(kargs...)
		},
	}
	cmd.Flags().IntVar(&lines, "lines", 20, "lines of history per container")
	cmd.Flags().StringVar(&since, "since", "", "only logs newer than this (5s, 2m, 3h)")
	cmd.Flags().BoolVar(&noFollow, "no-follow", false, "print and exit")
	return cmd
}

func workloadSelector(d Deps, cmd *cobra.Command, ref string) (string, error) {
	cs, ns, err := d.client()
	if err != nil {
		return "", err
	}
	kind, name, _ := strings.Cut(ref, "/")
	ctx := cmd.Context()
	var sel *metav1.LabelSelector
	switch strings.ToLower(kind) {
	case "deploy", "deployment", "deployments":
		o, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		sel = o.Spec.Selector
	case "sts", "statefulset", "statefulsets":
		o, err := cs.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		sel = o.Spec.Selector
	case "ds", "daemonset", "daemonsets":
		o, err := cs.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		sel = o.Spec.Selector
	case "job", "jobs":
		o, err := cs.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		sel = o.Spec.Selector
	case "svc", "service", "services":
		o, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if len(o.Spec.Selector) == 0 {
			return "", fmt.Errorf("service %s has no selector", name)
		}
		return labels.SelectorFromSet(o.Spec.Selector).String(), nil
	default:
		return "", fmt.Errorf("unsupported kind %q (use deploy, sts, ds, job, svc or a label selector)", kind)
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return "", err
	}
	return s.String(), nil
}

// --- restart --------------------------------------------------------------

func restartCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "restart TYPE/NAME...",
		Short: "Rolling restart of deployments, statefulsets or daemonsets, then wait for it",
		Example: `  kp restart deploy/api
  kp restart deploy/api sts/worker`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, t := range args {
				if err := d.Kubectl("rollout", "restart", t); err != nil {
					return err
				}
			}
			for _, t := range args {
				if err := d.Kubectl("rollout", "status", t, "--timeout=10m"); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// --- images ---------------------------------------------------------------

func imagesCmd(d Deps) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "images",
		Short: "Every container image in use, with how many pods run it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			pods, err := listPods(d, cmd, all, "")
			if err != nil {
				return err
			}
			counts := map[string]int{}
			for _, p := range pods {
				seen := map[string]bool{}
				for _, c := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
					if !seen[c.Image] {
						counts[c.Image]++
						seen[c.Image] = true
					}
				}
			}
			images := make([]string, 0, len(counts))
			for i := range counts {
				images = append(images, i)
			}
			sort.Slice(images, func(i, j int) bool {
				if counts[images[i]] != counts[images[j]] {
					return counts[images[i]] > counts[images[j]]
				}
				return images[i] < images[j]
			})
			w := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PODS\tIMAGE")
			for _, i := range images {
				fmt.Fprintf(w, "%d\t%s\n", counts[i], i)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "all namespaces")
	return cmd
}

// --- clean ----------------------------------------------------------------

func cleanCmd(d Deps) *cobra.Command {
	var all, yes, completed bool
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Delete Failed and Evicted pods (dry run unless --yes)",
		Example: `  kp clean -A              # list what would be deleted
  kp clean -A --yes        # delete Failed/Evicted pods
  kp clean --completed --yes   # also delete Succeeded pods`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cs, ns, err := d.client()
			if err != nil {
				return err
			}
			if all {
				ns = ""
			}
			list, err := cs.CoreV1().Pods(ns).List(cmd.Context(), metav1.ListOptions{})
			if err != nil {
				return err
			}
			var targets []corev1.Pod
			for _, p := range list.Items {
				if p.Status.Phase == corev1.PodFailed || (completed && p.Status.Phase == corev1.PodSucceeded) {
					targets = append(targets, p)
				}
			}
			if len(targets) == 0 {
				fmt.Fprintln(d.Out, "Nothing to clean.")
				return nil
			}
			for _, p := range targets {
				reason := string(p.Status.Phase)
				if p.Status.Reason != "" {
					reason = p.Status.Reason
				}
				action := "would delete"
				if yes {
					if err := cs.CoreV1().Pods(p.Namespace).Delete(cmd.Context(), p.Name, metav1.DeleteOptions{}); err != nil {
						fmt.Fprintf(d.ErrOut, "failed to delete %s/%s: %v\n", p.Namespace, p.Name, err)
						continue
					}
					action = "deleted"
				}
				fmt.Fprintf(d.Out, "%s %s/%s (%s, %s old)\n", action, p.Namespace, p.Name, reason, age(p.CreationTimestamp.Time))
			}
			if !yes {
				fmt.Fprintf(d.Out, "\n%d pods. Re-run with --yes to delete them.\n", len(targets))
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "all namespaces")
	cmd.Flags().BoolVar(&yes, "yes", false, "actually delete")
	cmd.Flags().BoolVar(&completed, "completed", false, "also delete Succeeded pods")
	return cmd
}

// --- neat -----------------------------------------------------------------

func neatCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:     "neat TYPE NAME | TYPE/NAME",
		Aliases: []string{"yaml", "clean-yaml"},
		Short:   "YAML without status, managedFields and server-set metadata, ready to re-apply",
		Example: `  kp neat deploy/api > api.yaml
  kp neat svc web`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var buf bytes.Buffer
			if err := d.Run(&buf, append([]string{"get", "-o", "json"}, args...)...); err != nil {
				return err
			}
			out, err := Neat(buf.Bytes())
			if err != nil {
				return err
			}
			_, err = d.Out.Write(out)
			return err
		},
	}
}

// Neat strips server-populated fields from a JSON object or List.
func Neat(data []byte) ([]byte, error) {
	var obj map[string]interface{}
	if err := yaml.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	if items, ok := obj["items"].([]interface{}); ok && obj["kind"] == "List" {
		for _, it := range items {
			if m, ok := it.(map[string]interface{}); ok {
				neatObject(m)
			}
		}
	} else {
		neatObject(obj)
	}
	return yaml.Marshal(obj)
}

func neatObject(obj map[string]interface{}) {
	delete(obj, "status")
	md, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		return
	}
	for _, k := range []string{"managedFields", "resourceVersion", "uid", "creationTimestamp", "generation", "selfLink", "ownerReferences"} {
		delete(md, k)
	}
	if ann, ok := md["annotations"].(map[string]interface{}); ok {
		delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		delete(ann, "deployment.kubernetes.io/revision")
		if len(ann) == 0 {
			delete(md, "annotations")
		}
	}
	if spec, ok := obj["spec"].(map[string]interface{}); ok && obj["kind"] == "Service" {
		for _, k := range []string{"clusterIP", "clusterIPs"} {
			delete(spec, k)
		}
	}
}

// --- decode ---------------------------------------------------------------

func decodeCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:     "decode SECRET [KEY]",
		Aliases: []string{"secret"},
		Short:   "Print a Secret's values base64-decoded (careful: shows secret material)",
		Example: `  kp decode db-credentials
  kp decode db-credentials password`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, ns, err := d.client()
			if err != nil {
				return err
			}
			s, err := cs.CoreV1().Secrets(ns).Get(cmd.Context(), args[0], metav1.GetOptions{})
			if err != nil {
				return err
			}
			if len(args) == 2 {
				v, ok := s.Data[args[1]]
				if !ok {
					return fmt.Errorf("secret %s has no key %q", args[0], args[1])
				}
				_, err := d.Out.Write(v)
				return err
			}
			keys := make([]string, 0, len(s.Data))
			for k := range s.Data {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v := string(s.Data[k])
				if strings.Contains(v, "\n") {
					fmt.Fprintf(d.Out, "%s: |\n  %s\n", k, strings.ReplaceAll(strings.TrimRight(v, "\n"), "\n", "\n  "))
				} else {
					fmt.Fprintf(d.Out, "%s: %s\n", k, v)
				}
			}
			return nil
		},
	}
}

// EncodeB64 is a tiny helper used by docs and tests.
func EncodeB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// --- nodes ----------------------------------------------------------------

func nodesCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:     "nodes",
		Aliases: []string{"nd"},
		Short:   "Nodes with readiness, version, pod count and CPU/memory requested vs allocatable",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cs, _, err := d.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "status.phase!=Succeeded,status.phase!=Failed"})
			if err != nil {
				return err
			}
			type use struct {
				cpu, mem resource.Quantity
				pods     int
			}
			byNode := map[string]*use{}
			for _, p := range pods.Items {
				if p.Spec.NodeName == "" {
					continue
				}
				u := byNode[p.Spec.NodeName]
				if u == nil {
					u = &use{}
					byNode[p.Spec.NodeName] = u
				}
				u.pods++
				for _, c := range p.Spec.Containers {
					if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
						u.cpu.Add(q)
					}
					if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
						u.mem.Add(q)
					}
				}
			}
			w := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATUS\tVERSION\tPODS\tCPU REQ\tMEM REQ\tAGE")
			for _, n := range nodes.Items {
				status := "NotReady"
				for _, c := range n.Status.Conditions {
					if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
						status = "Ready"
					}
				}
				if n.Spec.Unschedulable {
					status += ",Cordoned"
				}
				u := byNode[n.Name]
				if u == nil {
					u = &use{}
				}
				acpu := n.Status.Allocatable[corev1.ResourceCPU]
				amem := n.Status.Allocatable[corev1.ResourceMemory]
				apods := n.Status.Allocatable[corev1.ResourcePods]
				fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\t%s\t%s\t%s\n", n.Name, status, n.Status.NodeInfo.KubeletVersion,
					u.pods, apods.Value(), pct(u.cpu.MilliValue(), acpu.MilliValue()), pct(u.mem.Value(), amem.Value()), age(n.CreationTimestamp.Time))
			}
			return w.Flush()
		},
	}
}

func pct(used, total int64) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", used*100/total)
}

// --- netshoot ---------------------------------------------------------------

func netshootCmd(d Deps) *cobra.Command {
	var image string
	cmd := &cobra.Command{
		Use:     "netshoot",
		Aliases: []string{"debug-pod", "toolbox"},
		Short:   "Start a throwaway troubleshooting pod (dig, curl, tcpdump, ...) and delete it on exit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			name := fmt.Sprintf("kp-netshoot-%d", time.Now().Unix()%100000)
			return d.Kubectl("run", name, "--rm", "-it", "--restart=Never", "--image="+image, "--labels=app.kubernetes.io/managed-by=kp", "--", "bash")
		},
	}
	cmd.Flags().StringVar(&image, "image", "nicolaka/netshoot:v0.13", "troubleshooting image")
	return cmd
}

// --- usage -----------------------------------------------------------------

func usageCmd(d Deps) *cobra.Command {
	var all bool
	var by string
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Top pods by memory or CPU (needs metrics-server)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			args := []string{"top", "pods", "--sort-by=" + by}
			if all {
				args = append(args, "-A")
			}
			return d.Kubectl(args...)
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "all namespaces")
	cmd.Flags().StringVar(&by, "by", "memory", "memory or cpu")
	return cmd
}

// --- each -------------------------------------------------------------------

func eachCmd(d Deps) *cobra.Command {
	var selector, container string
	cmd := &cobra.Command{
		Use:   "each -l SELECTOR -- COMMAND...",
		Short: "Run a command in every running pod matching a selector, one after another",
		Example: `  kp each -l app=web -- cat /etc/hostname
  kp each -l app=api -c app -- env`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if selector == "" {
				return fmt.Errorf("--selector (-l) is required")
			}
			pods, err := listPods(d, cmd, false, selector)
			if err != nil {
				return err
			}
			for _, p := range pods {
				if p.Status.Phase != corev1.PodRunning {
					continue
				}
				fmt.Fprintf(d.Out, "==> %s <==\n", p.Name)
				kargs := []string{"exec", p.Name}
				if container != "" {
					kargs = append(kargs, "-c", container)
				}
				kargs = append(append(kargs, "--"), args...)
				if err := d.Kubectl(kargs...); err != nil {
					fmt.Fprintf(d.ErrOut, "%s: %v\n", p.Name, err)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "label selector (required)")
	cmd.Flags().StringVarP(&container, "container", "c", "", "container name")
	return cmd
}

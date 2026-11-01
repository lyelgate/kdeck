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
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lyelgate/kdeck/internal/match"
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
		whoCmd(d), podsCmd(d), badCmd(d), podCmd(d), logCmd(d), descCmd(d), imgCmd(d), poCmd(d), findCmd(d), shCmd(d), tailCmd(d), pfCmd(d), restartCmd(d),
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

func printPods(out io.Writer, rows []podRow, filter *match.Matcher, onlyBad, showNS bool) error {
	var shown []podRow
	for _, r := range rows {
		if onlyBad && r.healthy {
			continue
		}
		if !filter.Match(r.name) {
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
		Use:     "pods [NAME-REGEX]",
		Aliases: []string{"ps"},
		Short:   "Pods sorted problems first, with real status (CrashLoopBackOff, OOMKilled, ...); filter by regex",
		Long:    "List pods, unhealthy first, optionally filtered by a name regex.\n" + regexHelp,
		Example: `  kdeck pods                 # current namespace
  kdeck pods api -A          # names matching "api", all namespaces
  kdeck pods '^(web|api)-'   # regex
  kdeck pods --bad -A        # only pods that are not Running and Ready`,
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
			m, err := match.Compile(filter)
			if err != nil {
				return err
			}
			return printPods(d.Out, podRows(pods), m, bad, all)
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "all namespaces")
	cmd.Flags().BoolVar(&bad, "bad", false, "only unhealthy pods")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "label selector")
	return cmd
}

func badCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:     "bad",
		Aliases: []string{"dkbad"},
		Short:   "Unhealthy pods in all namespaces (same as: kdeck pods --bad -A)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			pods, err := listPods(d, cmd, true, "")
			if err != nil {
				return err
			}
			return printPods(d.Out, podRows(pods), nil, true, true)
		},
	}
}

// --- sh -----------------------------------------------------------------

func shCmd(d Deps) *cobra.Command {
	var container string
	cmd := &cobra.Command{
		Use:     "sh POD-REGEX|TYPE/NAME [-- COMMAND...]",
		Aliases: []string{"shell", "dksh"},
		Short:   "Open a shell in the best pod matching a regex: bash if present, otherwise sh",
		Long:    "Open a shell in a pod. A regex picks the best match: running and ready first.\n" + regexHelp,
		Example: `  kdeck sh api               # first ready pod matching "api"
  kdeck sh '^web-0$'
  kdeck sh deploy/api -c app
  kdeck sh api -- ls /data`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			if !strings.Contains(target, "/") {
				pods, err := matchingPods(d, cmd, target, false, "")
				if err != nil {
					return err
				}
				var running []string
				for _, p := range pods {
					if p.Status.Phase == corev1.PodRunning {
						running = append(running, p.Name)
					}
				}
				if len(running) == 0 {
					return fmt.Errorf("no running pod matches %q", target)
				}
				if len(running) > 1 {
					fmt.Fprintf(d.ErrOut, "%d pods match; using %s\n", len(running), running[0])
				}
				target = running[0]
			}
			// -t only with a terminal, so scripts and pipes do not get a TTY warning.
			mode := "-it"
			if !isTTY(os.Stdin) {
				mode = "-i"
			}
			kargs := []string{"exec", mode, target}
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
	cmd := &cobra.Command{
		Use:   "restart TYPE/NAME... | WORKLOAD-REGEX",
		Short: "Rolling restart of deployments, statefulsets or daemonsets (by name or regex), then wait",
		Long:  "Rolling restart, then wait for each rollout. A regex matches Deployment, StatefulSet and DaemonSet names; more than one match needs --yes.\n" + regexHelp,
		Example: `  kdeck restart deploy/api
  kdeck restart deploy/api sts/worker
  kdeck restart '^api-'            # list what would restart
  kdeck restart '^api-' --yes`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && !strings.Contains(args[0], "/") {
				targets, err := matchingWorkloads(d, cmd, args[0])
				if err != nil {
					return err
				}
				if len(targets) == 0 {
					return fmt.Errorf("no deployment, statefulset or daemonset matches %q", args[0])
				}
				if yes, _ := cmd.Flags().GetBool("yes"); len(targets) > 1 && !yes {
					fmt.Fprintf(d.Out, "%d workloads match:\n  %s\nRe-run with --yes to restart them all.\n", len(targets), strings.Join(targets, "\n  "))
					return nil
				}
				args = targets
			}
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
	cmd.Flags().Bool("yes", false, "confirm when a regex matches several workloads")
	return cmd
}

// --- images ---------------------------------------------------------------

func imagesCmd(d Deps) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "images [IMAGE-REGEX]",
		Short: "Every container image in use, with how many pods run it; filter by regex",
		Example: `  kdeck images -A
  kdeck images 'nginx|envoy' -A
  kdeck images ':latest$' -A      # images pinned to latest`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := ""
			if len(args) == 1 {
				pattern = args[0]
			}
			m, err := match.Compile(pattern)
			if err != nil {
				return err
			}
			pods, err := listPods(d, cmd, all, "")
			if err != nil {
				return err
			}
			counts := map[string]int{}
			for _, p := range pods {
				seen := map[string]bool{}
				for _, c := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
					if !seen[c.Image] && m.Match(c.Image) {
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
	var matchRe string
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Delete Failed and Evicted pods (dry run unless --yes)",
		Example: `  kdeck clean -A              # list what would be deleted
  kdeck clean -A --yes        # delete Failed/Evicted pods
  kdeck clean --completed --yes   # also delete Succeeded pods`,
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
			mm, err := match.Compile(matchRe)
			if err != nil {
				return err
			}
			for _, p := range list.Items {
				if !mm.Match(p.Name) {
					continue
				}
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
	cmd.Flags().StringVar(&matchRe, "match", "", "only pods whose name matches this regex")
	return cmd
}

// --- neat -----------------------------------------------------------------

func neatCmd(d Deps) *cobra.Command {
	return &cobra.Command{
		Use:     "neat TYPE NAME | TYPE/NAME",
		Aliases: []string{"yaml", "clean-yaml"},
		Short:   "YAML without status, managedFields and server-set metadata, ready to re-apply",
		Example: `  kdeck neat deploy/api > api.yaml
  kdeck neat svc web`,
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
		Example: `  kdeck decode db-credentials
  kdeck decode db-credentials password`,
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
		Use:     "nodes [NODE-REGEX]",
		Aliases: []string{"nd"},
		Short:   "Nodes with readiness, version, pod count and CPU/memory requested vs allocatable; filter by regex",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := ""
			if len(args) == 1 {
				pattern = args[0]
			}
			nm, err := match.Compile(pattern)
			if err != nil {
				return err
			}
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
				if !nm.Match(n.Name) {
					continue
				}
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
			name := fmt.Sprintf("kdeck-netshoot-%d", time.Now().Unix()%100000)
			return d.Kubectl("run", name, "--rm", "-it", "--restart=Never", "--image="+image, "--labels=app.kubernetes.io/managed-by=kdeck", "--", "bash")
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
		Use:   "each [POD-REGEX] [-l SELECTOR] -- COMMAND...",
		Short: "Run a command in every running pod matching a regex and/or selector",
		Example: `  kdeck each '^web-' -- cat /etc/hostname
  kdeck each -l app=api -c app -- env
  kdeck each api -l tier=backend -- date`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := ""
			if dash := cmd.ArgsLenAtDash(); dash == 1 {
				pattern, args = args[0], args[1:]
			} else if dash < 0 {
				return fmt.Errorf("put the command after --, e.g. kdeck each api -- date")
			}
			if pattern == "" && selector == "" {
				return fmt.Errorf("give a pod regex or --selector (-l)")
			}
			pods, err := matchingPods(d, cmd, pattern, false, selector)
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

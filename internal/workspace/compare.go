package workspace

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/lyelgate/kdeck/internal/match"
)

// Side is "context:namespace"; either part may be empty (current).
type Side struct{ Context, Namespace string }

// ParseSide reads "ctx:ns", "ctx:", ":ns" or a bare namespace.
func ParseSide(s string) Side {
	if c, n, ok := strings.Cut(s, ":"); ok {
		return Side{c, n}
	}
	return Side{Namespace: s}
}

func (s Side) String() string {
	c, n := s.Context, s.Namespace
	if c == "" {
		c = "(current)"
	}
	if n == "" {
		n = "(default ns)"
	}
	return c + ":" + n
}

// Workload is the comparable shape of a Deployment, StatefulSet or DaemonSet.
type Workload struct {
	Kind, Name string
	Fields     map[string]string
}

func podFields(replicas string, spec corev1.PodSpec) map[string]string {
	f := map[string]string{}
	if replicas != "" {
		f["replicas"] = replicas
	}
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		p := "container " + c.Name + " "
		f[p+"image"] = c.Image
		f[p+"requests"] = res(c.Resources.Requests)
		f[p+"limits"] = res(c.Resources.Limits)
		var env, refs []string
		for _, e := range c.Env {
			env = append(env, e.Name)
			if e.ValueFrom != nil {
				if r := e.ValueFrom.ConfigMapKeyRef; r != nil {
					refs = append(refs, "cm/"+r.Name)
				}
				if r := e.ValueFrom.SecretKeyRef; r != nil {
					refs = append(refs, "secret/"+r.Name)
				}
			}
		}
		for _, ef := range c.EnvFrom {
			if ef.ConfigMapRef != nil {
				refs = append(refs, "cm/"+ef.ConfigMapRef.Name)
			}
			if ef.SecretRef != nil {
				refs = append(refs, "secret/"+ef.SecretRef.Name)
			}
		}
		sort.Strings(env)
		sort.Strings(refs)
		f[p+"env names"] = strings.Join(env, ",")
		f[p+"config refs"] = strings.Join(uniq(refs), ",")
	}
	for _, v := range spec.Volumes {
		switch {
		case v.ConfigMap != nil:
			f["volume "+v.Name] = "cm/" + v.ConfigMap.Name
		case v.Secret != nil:
			f["volume "+v.Name] = "secret/" + v.Secret.SecretName
		case v.PersistentVolumeClaim != nil:
			f["volume "+v.Name] = "pvc/" + v.PersistentVolumeClaim.ClaimName
		}
	}
	return f
}

func uniq(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func res(r corev1.ResourceList) string {
	var parts []string
	if q, ok := r[corev1.ResourceCPU]; ok {
		parts = append(parts, "cpu="+q.String())
	}
	if q, ok := r[corev1.ResourceMemory]; ok {
		parts = append(parts, "mem="+q.String())
	}
	return strings.Join(parts, ",")
}

func replicas(p *int32) string {
	if p == nil {
		return "1"
	}
	return fmt.Sprint(*p)
}

func load(ctx context.Context, flags *genericclioptions.ConfigFlags, s Side) ([]Workload, string, error) {
	loader := flags.ToRawKubeConfigLoader()
	raw, err := loader.RawConfig()
	if err != nil {
		return nil, "", err
	}
	override := &clientcmd.ConfigOverrides{CurrentContext: s.Context}
	cc := clientcmd.NewNonInteractiveClientConfig(raw, raw.CurrentContext, override, nil)
	if s.Context != "" {
		cc = clientcmd.NewNonInteractiveClientConfig(raw, s.Context, override, nil)
	}
	rc, err := cc.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	ns := s.Namespace
	if ns == "" {
		ns, _, _ = cc.Namespace()
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, "", err
	}
	var out []Workload
	opts := metav1.ListOptions{}
	deps, err := cs.AppsV1().Deployments(ns).List(ctx, opts)
	if err != nil {
		return nil, "", err
	}
	for _, d := range deps.Items {
		out = append(out, Workload{"Deployment", d.Name, podFields(replicas(d.Spec.Replicas), d.Spec.Template.Spec)})
	}
	if l, err := cs.AppsV1().StatefulSets(ns).List(ctx, opts); err == nil {
		for _, d := range l.Items {
			out = append(out, Workload{"StatefulSet", d.Name, podFields(replicas(d.Spec.Replicas), d.Spec.Template.Spec)})
		}
	}
	if l, err := cs.AppsV1().DaemonSets(ns).List(ctx, opts); err == nil {
		for _, d := range l.Items {
			out = append(out, Workload{"DaemonSet", d.Name, podFields("", d.Spec.Template.Spec)})
		}
	}
	_ = appsv1.SchemeGroupVersion
	return out, ns, nil
}

// Diff is one difference between the two sides.
type Diff struct{ Object, Field, A, B string }

// CompareWorkloads returns differences, including objects on one side only.
func CompareWorkloads(a, b []Workload, filter *match.Matcher) []Diff {
	key := func(w Workload) string { return w.Kind + "/" + w.Name }
	am, bm := map[string]Workload{}, map[string]Workload{}
	keys := map[string]bool{}
	for _, w := range a {
		if filter.Match(w.Name) {
			am[key(w)] = w
			keys[key(w)] = true
		}
	}
	for _, w := range b {
		if filter.Match(w.Name) {
			bm[key(w)] = w
			keys[key(w)] = true
		}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []Diff
	for _, k := range sorted {
		wa, okA := am[k]
		wb, okB := bm[k]
		switch {
		case !okA:
			out = append(out, Diff{k, "(exists)", "missing", "present"})
			continue
		case !okB:
			out = append(out, Diff{k, "(exists)", "present", "missing"})
			continue
		}
		fields := map[string]bool{}
		for f := range wa.Fields {
			fields[f] = true
		}
		for f := range wb.Fields {
			fields[f] = true
		}
		fl := make([]string, 0, len(fields))
		for f := range fields {
			fl = append(fl, f)
		}
		sort.Strings(fl)
		for _, f := range fl {
			if wa.Fields[f] != wb.Fields[f] {
				out = append(out, Diff{k, f, orDash(wa.Fields[f]), orDash(wb.Fields[f])})
			}
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// CompareCommand returns "kdeck compare".
func CompareCommand(flags *genericclioptions.ConfigFlags, out io.Writer) *cobra.Command {
	var filter string
	cmd := &cobra.Command{
		Use:     "compare A B",
		Aliases: []string{"drift", "cmp"},
		Short:   "Drift between two namespaces or clusters: images, replicas, resources, env, config refs",
		Long: `Compare Deployments, StatefulSets and DaemonSets on two sides. Each side is
CONTEXT:NAMESPACE; leave either part empty for the current one.`,
		Example: `  kdeck compare staging prod                    # two namespaces, current context
  kdeck compare stg-cluster:shop prod-cluster:shop
  kdeck compare :shop prod-eu:                  # current context's shop vs prod-eu's default ns
  kdeck compare staging prod -m '^api'          # only workloads matching a regex`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, b := ParseSide(args[0]), ParseSide(args[1])
			m, err := match.Compile(filter)
			if err != nil {
				return err
			}
			wa, nsA, err := load(cmd.Context(), flags, a)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			wb, nsB, err := load(cmd.Context(), flags, b)
			if err != nil {
				return fmt.Errorf("%s: %w", args[1], err)
			}
			if a.Namespace == "" {
				a.Namespace = nsA
			}
			if b.Namespace == "" {
				b.Namespace = nsB
			}
			diffs := CompareWorkloads(wa, wb, m)
			if len(diffs) == 0 {
				fmt.Fprintf(out, "No differences between %s and %s (%d workloads).\n", a, b, len(wa))
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "OBJECT\tFIELD\t%s\t%s\n", strings.ToUpper(a.String()), strings.ToUpper(b.String()))
			for _, d := range diffs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Object, d.Field, d.A, d.B)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out, "\n%d differences.\n", len(diffs))
			return nil
		},
	}
	cmd.Flags().StringVarP(&filter, "match", "m", "", "only workloads whose name matches this regex")
	return cmd
}

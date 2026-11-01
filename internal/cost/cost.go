// Package cost estimates monthly cost per namespace on any cluster.
//
// Each node's monthly price is shared among the pods on it by their
// dominant request (the larger of CPU share and memory share of the node's
// allocatable). What no pod requested is reported as idle. PVCs add their
// capacity times a per-GB price. Prices come from config (cost.nodePrices,
// cost.defaultNodePrice, cost.storagePerGB) or, on Linode LKE, from the
// Linode API.
package cost

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"

	"github.com/lyelgate/kubepilot/internal/config"
)

// NodePricer returns a node's monthly price and where it came from.
type NodePricer func(n corev1.Node) (float64, string)

// Line is one namespace's estimate.
type Line struct {
	Namespace        string
	Pods             int
	CPU, Mem         float64 // requested cores and GiB
	Compute, Storage float64
}

// Report is the whole estimate.
type Report struct {
	Lines     []Line
	NodesCost float64
	Idle      float64
	Unpriced  []string
	Currency  string
}

func dominantShare(p corev1.Pod, n corev1.Node) (float64, float64, float64) {
	var cpu, mem float64
	for _, c := range p.Spec.Containers {
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpu += q.AsApproximateFloat64()
		}
		if q, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			mem += q.AsApproximateFloat64()
		}
	}
	acpu := n.Status.Allocatable[corev1.ResourceCPU]
	amem := n.Status.Allocatable[corev1.ResourceMemory]
	share := 0.0
	if a := acpu.AsApproximateFloat64(); a > 0 {
		share = cpu / a
	}
	if a := amem.AsApproximateFloat64(); a > 0 && mem/a > share {
		share = mem / a
	}
	return share, cpu, mem / (1 << 30)
}

// Estimate builds the report from cluster objects.
func Estimate(nodes []corev1.Node, pods []corev1.Pod, pvcs []corev1.PersistentVolumeClaim, price NodePricer, storagePerGB float64) Report {
	r := Report{}
	byNode := map[string]corev1.Node{}
	nodePrice := map[string]float64{}
	for _, n := range nodes {
		byNode[n.Name] = n
		p, _ := price(n)
		if p == 0 {
			r.Unpriced = append(r.Unpriced, n.Name)
		}
		nodePrice[n.Name] = p
		r.NodesCost += p
	}
	lines := map[string]*Line{}
	line := func(ns string) *Line {
		if lines[ns] == nil {
			lines[ns] = &Line{Namespace: ns}
		}
		return lines[ns]
	}
	used := map[string]float64{}
	for _, p := range pods {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		n, ok := byNode[p.Spec.NodeName]
		if !ok {
			continue
		}
		share, cpu, mem := dominantShare(p, n)
		if used[n.Name]+share > 1 {
			share = 1 - used[n.Name] // overcommitted node: cap at its price
			if share < 0 {
				share = 0
			}
		}
		used[n.Name] += share
		l := line(p.Namespace)
		l.Pods++
		l.CPU += cpu
		l.Mem += mem
		l.Compute += share * nodePrice[n.Name]
	}
	for name, p := range nodePrice {
		r.Idle += (1 - used[name]) * p
	}
	for _, c := range pvcs {
		if c.Status.Phase != corev1.ClaimBound {
			continue
		}
		q := c.Status.Capacity[corev1.ResourceStorage]
		line(c.Namespace).Storage += q.AsApproximateFloat64() / (1 << 30) * storagePerGB
	}
	for _, l := range lines {
		r.Lines = append(r.Lines, *l)
	}
	sort.Slice(r.Lines, func(i, j int) bool {
		return r.Lines[i].Compute+r.Lines[i].Storage > r.Lines[j].Compute+r.Lines[j].Storage
	})
	sort.Strings(r.Unpriced)
	return r
}

// ConfigPricer prices nodes from config, by instance-type label.
func ConfigPricer(c config.CostConfig) NodePricer {
	return func(n corev1.Node) (float64, string) {
		t := n.Labels["node.kubernetes.io/instance-type"]
		if p, ok := c.NodePrices[t]; ok {
			return p, "config"
		}
		if c.DefaultNodePrice > 0 {
			return c.DefaultNodePrice, "default"
		}
		return 0, ""
	}
}

// NewCommand returns "kp cost". extra may add a pricer (Linode) tried
// before config; it returns nil when not applicable.
func NewCommand(flags *genericclioptions.ConfigFlags, out io.Writer, extra func(ctx context.Context, nodes []corev1.Node) (NodePricer, float64)) *cobra.Command {
	return &cobra.Command{
		Use:   "cost",
		Short: "Estimated monthly cost per namespace: node price shared by CPU/memory requests, plus PVC storage and idle capacity",
		Long: `Estimate monthly cost per namespace. Each node's price is shared among its
pods by their dominant request (the larger of CPU and memory share of the
node). Capacity nobody requested is shown as idle.

Prices: on Linode LKE they come from the Linode API automatically; elsewhere set
them in ~/.kp/config.yaml:

  cost:
    nodePrices: {m6i.large: 70.08, e2-standard-4: 97.09}   # by node.kubernetes.io/instance-type
    defaultNodePrice: 50
    storagePerGB: 0.10`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			rc, err := flags.ToRESTConfig()
			if err != nil {
				return err
			}
			cs, err := kubernetes.NewForConfig(rc)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
			if err != nil {
				return err
			}
			pvcs, _ := cs.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
			var pvcItems []corev1.PersistentVolumeClaim
			if pvcs != nil {
				pvcItems = pvcs.Items
			}
			pricer := ConfigPricer(cfg.Cost)
			storage := cfg.Cost.StoragePerGB
			source := "config"
			if extra != nil {
				if lp, gb := extra(ctx, nodes.Items); lp != nil {
					conf := pricer
					pricer = func(n corev1.Node) (float64, string) {
						if p, s := conf(n); p > 0 && s == "config" {
							return p, s // explicit config wins
						}
						if p, s := lp(n); p > 0 {
							return p, s
						}
						return conf(n)
					}
					if storage == 0 {
						storage = gb
					}
					source = "Linode API list prices"
				}
			}
			r := Estimate(nodes.Items, pods.Items, pvcItems, pricer, storage)
			cur := cfg.Cost.Currency
			if cur == "" {
				cur = "USD"
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "NAMESPACE\tPODS\tCPU REQ\tMEM REQ\tCOMPUTE/MO\tSTORAGE/MO\tTOTAL/MO\n")
			var sum float64
			for _, l := range r.Lines {
				t := l.Compute + l.Storage
				sum += t
				fmt.Fprintf(tw, "%s\t%d\t%.2f\t%.1f GiB\t%.2f\t%.2f\t%.2f\n", l.Namespace, l.Pods, l.CPU, l.Mem, l.Compute, l.Storage, t)
			}
			_ = tw.Flush()
			fmt.Fprintf(out, "\nNodes: %.2f %s/mo (%s); requested: %.2f; idle (nobody requested it): %.2f\n", r.NodesCost, cur, source, r.NodesCost-r.Idle, r.Idle)
			if len(r.Unpriced) > 0 {
				fmt.Fprintf(out, "Unpriced nodes (counted as 0): %v. Set cost.nodePrices or cost.defaultNodePrice.\n", r.Unpriced)
			}
			if storage == 0 {
				fmt.Fprintln(out, "Storage unpriced: set cost.storagePerGB.")
			}
			return nil
		},
	}
}

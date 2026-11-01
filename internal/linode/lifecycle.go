package linode

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/linode/linodego"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/lyelgate/kdeck/internal/match"
)

// Prices holds Linode list prices, region-specific where Linode publishes them.
type Prices struct {
	types       map[string]linodego.LinodeType
	volumeGB    float64
	volumeByReg map[string]float64
	nb          float64
	nbByReg     map[string]float64
	lkeHA       float64
	lkeHAByReg  map[string]float64
}

// LoadPrices fetches node, volume, NodeBalancer and LKE HA prices.
func LoadPrices(ctx context.Context, c *linodego.Client) (*Prices, error) {
	p := &Prices{types: map[string]linodego.LinodeType{}, volumeByReg: map[string]float64{}, nbByReg: map[string]float64{}, lkeHAByReg: map[string]float64{}}
	ts, err := c.ListTypes(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, t := range ts {
		p.types[t.ID] = t
	}
	if vts, err := c.ListVolumeTypes(ctx, nil); err == nil && len(vts) > 0 {
		p.volumeGB = vts[0].Price.Monthly
		for _, rp := range vts[0].RegionPrices {
			p.volumeByReg[rp.ID] = rp.Monthly
		}
	}
	if nts, err := c.ListNodeBalancerTypes(ctx, nil); err == nil && len(nts) > 0 {
		p.nb = nts[0].Price.Monthly
		for _, rp := range nts[0].RegionPrices {
			p.nbByReg[rp.ID] = rp.Monthly
		}
	}
	if lts, err := c.ListLKETypes(ctx, nil); err == nil {
		for _, t := range lts {
			if t.ID == "lke-ha" {
				p.lkeHA = t.Price.Monthly
				for _, rp := range t.RegionPrices {
					p.lkeHAByReg[rp.ID] = rp.Monthly
				}
			}
		}
	}
	return p, nil
}

// Node returns the monthly price of a plan in a region (0 when unknown).
func (p *Prices) Node(typeID, region string) float64 {
	t, ok := p.types[typeID]
	if !ok {
		return 0
	}
	for _, rp := range t.RegionPrices {
		if rp.ID == region {
			return float64(rp.Monthly)
		}
	}
	if t.Price != nil {
		return float64(t.Price.Monthly)
	}
	return 0
}

func regional(base float64, by map[string]float64, region string) float64 {
	if v, ok := by[region]; ok && v > 0 {
		return v
	}
	return base
}

// VolumeGB is the monthly price per GB of block storage in a region.
func (p *Prices) VolumeGB(region string) float64 { return regional(p.volumeGB, p.volumeByReg, region) }

// NodeBalancer is the monthly price of one NodeBalancer in a region.
func (p *Prices) NodeBalancer(region string) float64 { return regional(p.nb, p.nbByReg, region) }

// HA is the monthly price of an HA control plane in a region.
func (p *Prices) HA(region string) float64 { return regional(p.lkeHA, p.lkeHAByReg, region) }

func money(v float64) string { return fmt.Sprintf("$%.2f", v) }

func lifecycleCmds(o *opts) []*cobra.Command {
	var version string
	upgrade := &cobra.Command{
		Use:   "upgrade CLUSTER-ID [--version X] --yes",
		Short: "Upgrade an LKE cluster's Kubernetes version (then recycle nodes to move them over)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := atoi(args[0], "cluster ID")
			if err != nil {
				return err
			}
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			k, err := c.GetLKECluster(cx, id)
			if err != nil {
				return err
			}
			if version == "" {
				vs, err := c.ListLKEVersions(cx, nil)
				if err != nil {
					return err
				}
				version = nextVersion(k.K8sVersion, vs)
				if version == "" {
					fmt.Fprintf(o.out, "Cluster %s is on %s, the newest version LKE offers.\n", k.Label, k.K8sVersion)
					return nil
				}
			}
			fmt.Fprintf(o.out, "Cluster %s: %s -> %s\n", k.Label, k.K8sVersion, version)
			if err := needYes(cmd, "Upgrading a cluster"); err != nil {
				return err
			}
			if _, err := c.UpdateLKECluster(cx, id, linodego.LKEClusterUpdateOptions{K8sVersion: version}); err != nil {
				return err
			}
			fmt.Fprintf(o.out, "Control plane upgrade to %s started. Nodes keep the old version until recycled:\n  kdeck silent --check workload   # make sure no PDB blocks draining (SF-PDB-001)\n  kdeck linode lke recycle %d --yes\n", version, id)
			return nil
		},
	}
	upgrade.Flags().StringVar(&version, "version", "", "target version (default: the next minor LKE offers)")
	upgrade.Flags().Bool("yes", false, "confirm the change")

	var pool int
	recycle := &cobra.Command{
		Use:   "recycle CLUSTER-ID [--pool POOL-ID] --yes",
		Short: "Replace nodes with fresh ones (all pools, or one), e.g. after an upgrade",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := atoi(args[0], "cluster ID")
			if err != nil {
				return err
			}
			if err := needYes(cmd, "Recycling nodes"); err != nil {
				return err
			}
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			if pool > 0 {
				err = c.RecycleLKENodePool(cx, id, pool)
			} else {
				err = c.RecycleLKEClusterNodes(cx, id)
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(o.out, "Recycle started; nodes are drained and replaced one at a time. Watch: kdeck nodes")
			return nil
		},
	}
	recycle.Flags().IntVar(&pool, "pool", 0, "only this node pool")
	recycle.Flags().Bool("yes", false, "confirm the change")

	var minN, maxN int
	var off bool
	autoscale := &cobra.Command{
		Use:   "autoscale CLUSTER-ID POOL-ID --min N --max M --yes | --off --yes",
		Short: "Turn the node pool autoscaler on (with limits) or off",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := atoi(args[0], "cluster ID")
			if err != nil {
				return err
			}
			p, err := atoi(args[1], "pool ID")
			if err != nil {
				return err
			}
			as := linodego.LKENodePoolAutoscaler{Enabled: !off, Min: minN, Max: maxN}
			if !off && (minN < 1 || maxN < minN) {
				return fmt.Errorf("need 1 <= --min <= --max (or --off)")
			}
			if err := needYes(cmd, "Changing the autoscaler"); err != nil {
				return err
			}
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			np, err := c.UpdateLKENodePool(cx, id, p, linodego.LKENodePoolUpdateOptions{Autoscaler: &as})
			if err != nil {
				return err
			}
			if np.Autoscaler.Enabled {
				fmt.Fprintf(o.out, "Pool %d autoscaler: %d-%d nodes.\n", p, np.Autoscaler.Min, np.Autoscaler.Max)
			} else {
				fmt.Fprintf(o.out, "Pool %d autoscaler off (fixed at %d nodes).\n", p, np.Count)
			}
			return nil
		},
	}
	autoscale.Flags().IntVar(&minN, "min", 0, "minimum nodes")
	autoscale.Flags().IntVar(&maxN, "max", 0, "maximum nodes")
	autoscale.Flags().BoolVar(&off, "off", false, "disable the autoscaler")
	autoscale.Flags().Bool("yes", false, "confirm the change")
	return []*cobra.Command{upgrade, recycle, autoscale}
}

// nextVersion returns the smallest offered version newer than current.
func nextVersion(current string, offered []linodego.LKEVersion) string {
	cur := minorOf(current)
	best, bestMinor := "", 1<<30
	for _, v := range offered {
		m := minorOf(v.ID)
		if m > cur && m < bestMinor {
			best, bestMinor = v.ID, m
		}
	}
	return best
}

func minorOf(v string) int {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) < 2 {
		return -1
	}
	n, _ := strconv.Atoi(parts[1])
	return n
}

func firewallsCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use: "firewalls", Aliases: []string{"fw"}, Short: "Cloud firewalls: policy, rule counts and what they protect",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			fws, err := c.ListFirewalls(cx, nil)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, f := range fws {
				var attached []string
				for _, e := range f.Entities {
					attached = append(attached, string(e.Type)+":"+e.Label)
				}
				rows = append(rows, []string{strconv.Itoa(f.ID), f.Label, string(f.Status),
					fmt.Sprintf("%s (%d rules)", f.Rules.InboundPolicy, len(f.Rules.Inbound)),
					fmt.Sprintf("%s (%d rules)", f.Rules.OutboundPolicy, len(f.Rules.Outbound)),
					strings.Join(attached, ", ")})
			}
			return table(o.out, "ID\tLABEL\tSTATUS\tINBOUND\tOUTBOUND\tATTACHED TO", rows)
		},
	}
}

func costCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "cost [CLUSTER-REGEX]",
		Short: "Monthly list-price cost per LKE cluster (nodes, HA, attached volumes) plus unattached volumes and NodeBalancers",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := ""
			if len(args) == 1 {
				pattern = args[0]
			}
			m, err := match.Compile(pattern)
			if err != nil {
				return err
			}
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			prices, err := LoadPrices(cx, c)
			if err != nil {
				return err
			}
			clusters, err := c.ListLKEClusters(cx, nil)
			if err != nil {
				return err
			}
			vols, err := c.ListVolumes(cx, nil)
			if err != nil {
				return err
			}
			nbs, _ := c.ListNodeBalancers(cx, nil)
			instanceCluster := map[int]string{}
			var rows [][]string
			var total float64
			for _, k := range clusters {
				if !m.Match(k.Label) {
					continue
				}
				pools, err := c.ListLKENodePools(cx, k.ID, nil)
				if err != nil {
					return err
				}
				var nodes int
				var nodeCost, haCost, volCost float64
				for _, p := range pools {
					nodes += p.Count
					nodeCost += float64(p.Count) * prices.Node(p.Type, k.Region)
					for _, n := range p.Linodes {
						instanceCluster[n.InstanceID] = k.Label
					}
				}
				if k.ControlPlane.HighAvailability {
					haCost = prices.HA(k.Region)
				}
				for _, v := range vols {
					if v.LinodeID != nil && instanceCluster[*v.LinodeID] == k.Label {
						volCost += float64(v.Size) * prices.VolumeGB(v.Region)
					}
				}
				sum := nodeCost + haCost + volCost
				total += sum
				rows = append(rows, []string{k.Label, k.Region, strconv.Itoa(nodes), money(nodeCost), money(haCost), money(volCost), money(sum)})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i][6] > rows[j][6] })
			if err := table(o.out, "CLUSTER\tREGION\tNODES\tNODES/MO\tHA/MO\tVOLUMES/MO\tTOTAL/MO", rows); err != nil {
				return err
			}
			var unattachedGB int
			var unattachedCost, nbCost float64
			for _, v := range vols {
				if v.LinodeID == nil {
					unattachedGB += v.Size
					unattachedCost += float64(v.Size) * prices.VolumeGB(v.Region)
				}
			}
			for _, nb := range nbs {
				nbCost += prices.NodeBalancer(nb.Region)
			}
			fmt.Fprintf(o.out, "\nClusters total:        %s/mo\n", money(total))
			fmt.Fprintf(o.out, "Unattached volumes:    %s/mo (%d GB; see kdeck linode leftovers)\n", money(unattachedCost), unattachedGB)
			fmt.Fprintf(o.out, "NodeBalancers:         %s/mo (%d)\n", money(nbCost), len(nbs))
			fmt.Fprintf(o.out, "Account total:         %s/mo  (list prices, before credits, transfer and backups)\n", money(total+unattachedCost+nbCost))
			return nil
		},
	}
}

func leftoversCmd(o *opts) *cobra.Command {
	var k8sContexts string
	cmd := &cobra.Command{
		Use:   "leftovers",
		Short: "Resources billing with nothing using them: unattached volumes, NodeBalancers no LoadBalancer Service points at",
		Example: `  kdeck linode leftovers                          # checks NodeBalancers against the current context
  kdeck linode leftovers --k8s-contexts '^lke-'   # against every matching kubeconfig context`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			prices, err := LoadPrices(cx, c)
			if err != nil {
				return err
			}
			vols, err := c.ListVolumes(cx, nil)
			if err != nil {
				return err
			}
			var rows [][]string
			var monthly float64
			for _, v := range vols {
				if v.LinodeID != nil {
					continue
				}
				cost := float64(v.Size) * prices.VolumeGB(v.Region)
				monthly += cost
				why := "not attached"
				if v.Created != nil {
					why += " (created " + v.Created.Format("2006-01-02") + ")"
				}
				rows = append(rows, []string{"volume", strconv.Itoa(v.ID), v.Label, v.Region, fmt.Sprintf("%d GB", v.Size), money(cost), why})
			}
			lbIPs, checked, err := loadBalancerIPs(o, k8sContexts)
			if err != nil {
				fmt.Fprintf(o.out, "note: could not read LoadBalancer Services (%v); NodeBalancers not checked\n", err)
			} else {
				nbs, err := c.ListNodeBalancers(cx, nil)
				if err != nil {
					return err
				}
				for _, nb := range nbs {
					if nb.IPv4 == nil || lbIPs[*nb.IPv4] {
						continue
					}
					cost := prices.NodeBalancer(nb.Region)
					monthly += cost
					label := "-"
					if nb.Label != nil {
						label = *nb.Label
					}
					rows = append(rows, []string{"nodebalancer", strconv.Itoa(nb.ID), label, nb.Region, *nb.IPv4, money(cost), "no LoadBalancer Service in " + strings.Join(checked, ", ")})
				}
			}
			if len(rows) == 0 {
				fmt.Fprintln(o.out, "No leftovers found.")
				return nil
			}
			if err := table(o.out, "KIND\tID\tLABEL\tREGION\tSIZE/IP\tMONTHLY\tWHY", rows); err != nil {
				return err
			}
			fmt.Fprintf(o.out, "\n%d leftovers, %s/mo. Check before deleting: a volume may hold the only copy of data, and a NodeBalancer may serve a cluster that is not in your kubeconfig.\n", len(rows), money(monthly))
			return nil
		},
	}
	cmd.Flags().StringVar(&k8sContexts, "k8s-contexts", "", "regex of kubeconfig contexts whose LoadBalancer Services count as users (default: current context)")
	return cmd
}

func loadBalancerIPs(o *opts, pattern string) (map[string]bool, []string, error) {
	raw, err := o.flags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return nil, nil, err
	}
	var ctxs []string
	if pattern == "" {
		ctxs = []string{raw.CurrentContext}
	} else {
		m, err := match.Compile(pattern)
		if err != nil {
			return nil, nil, err
		}
		for n := range raw.Contexts {
			if m.Match(n) {
				ctxs = append(ctxs, n)
			}
		}
		sort.Strings(ctxs)
	}
	ips := map[string]bool{}
	for _, name := range ctxs {
		cc := clientcmd.NewNonInteractiveClientConfig(raw, name, &clientcmd.ConfigOverrides{}, nil)
		rc, err := cc.ClientConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		cs, err := kubernetes.NewForConfig(rc)
		if err != nil {
			return nil, nil, err
		}
		svcs, err := cs.CoreV1().Services("").List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, s := range svcs.Items {
			if s.Spec.Type != corev1.ServiceTypeLoadBalancer {
				continue
			}
			for _, ing := range s.Status.LoadBalancer.Ingress {
				ips[ing.IP] = true
			}
		}
	}
	return ips, ctxs, nil
}

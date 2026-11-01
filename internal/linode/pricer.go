package linode

import (
	"context"
	"io"

	corev1 "k8s.io/api/core/v1"

	"github.com/lyelgate/kdeck/internal/cost"
)

// CostPricer returns a node pricer for LKE nodes (they carry the
// lke.linode.com/pool-id label and their plan as the instance type), or nil
// when the cluster is not LKE or no Linode token is available.
func CostPricer(ctx context.Context, nodes []corev1.Node) (cost.NodePricer, float64) {
	lke := false
	for _, n := range nodes {
		if _, ok := n.Labels["lke.linode.com/pool-id"]; ok {
			lke = true
		}
	}
	if !lke {
		return nil, 0
	}
	o := &opts{out: io.Discard}
	c, err := o.client()
	if err != nil {
		return nil, 0
	}
	prices, err := LoadPrices(ctx, c)
	if err != nil {
		return nil, 0
	}
	region := ""
	if len(nodes) > 0 {
		region = nodes[0].Labels["topology.kubernetes.io/region"]
	}
	return func(n corev1.Node) (float64, string) {
		p := prices.Node(n.Labels["node.kubernetes.io/instance-type"], n.Labels["topology.kubernetes.io/region"])
		return p, "linode"
	}, prices.VolumeGB(region)
}

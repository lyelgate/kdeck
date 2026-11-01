// Package linode manages Linode Kubernetes Engine (LKE) clusters and the
// Linode resources around them, using the official linodego SDK.
//
// The API token is taken from --token, then LINODE_TOKEN, then
// LINODE_CLI_TOKEN, then the linode-cli config file. It is never stored by kdeck.
// Commands that create, change or delete resources need --yes.
package linode

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/linode/linodego"
	"github.com/lyelgate/kdeck/internal/kubeconfig"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

type opts struct {
	token string
	out   io.Writer
	flags *genericclioptions.ConfigFlags
}

// TokenFromLinodeCLI reads the token of the default user from a linode-cli
// config file (INI format), returning "" when none is found.
func TokenFromLinodeCLI(paths ...string) string {
	if len(paths) == 0 {
		if dir, err := os.UserConfigDir(); err == nil {
			paths = append(paths, filepath.Join(dir, "linode-cli"))
		}
		if h, err := os.UserHomeDir(); err == nil {
			paths = append(paths, filepath.Join(h, ".config", "linode-cli"), filepath.Join(h, ".linode-cli"))
		}
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sections := map[string]map[string]string{}
		cur := "DEFAULT"
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				cur = strings.TrimSpace(line[1 : len(line)-1])
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			if sections[cur] == nil {
				sections[cur] = map[string]string{}
			}
			sections[cur][strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		f.Close()
		if user := sections["DEFAULT"]["default-user"]; user != "" {
			if t := sections[user]["token"]; t != "" {
				return t
			}
		}
		if t := sections["DEFAULT"]["token"]; t != "" {
			return t
		}
	}
	return ""
}

func (o *opts) client() (*linodego.Client, error) {
	tok := o.token
	for _, env := range []string{"LINODE_TOKEN", "LINODE_CLI_TOKEN"} {
		if tok == "" {
			tok = os.Getenv(env)
		}
	}
	if tok == "" {
		tok = TokenFromLinodeCLI()
	}
	if tok == "" {
		return nil, fmt.Errorf("no Linode API token: pass --token, set LINODE_TOKEN, or configure linode-cli")
	}
	c := linodego.NewClient(nil)
	c.SetToken(tok)
	c.SetUserAgent("kdeck")
	if base := os.Getenv("LINODE_URL"); base != "" {
		c.SetBaseURL(base)
	}
	return &c, nil
}

func ctx(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return context.WithTimeout(cmd.Context(), 2*time.Minute)
}

func atoi(s, what string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", what, s)
	}
	return n, nil
}

func needYes(cmd *cobra.Command, action string) error {
	if yes, _ := cmd.Flags().GetBool("yes"); !yes {
		return fmt.Errorf("%s changes your Linode account; re-run with --yes to confirm", action)
	}
	return nil
}

// NewCommand returns "kdeck linode".
func NewCommand(flags *genericclioptions.ConfigFlags, out io.Writer) *cobra.Command {
	o := &opts{out: out, flags: flags}
	cmd := &cobra.Command{
		Use:     "linode",
		Aliases: []string{"lin", "lnd"},
		Short:   "Manage Linode LKE clusters, node pools, volumes and more",
		Example: `  kdeck linode lke ls
  kdeck linode lke kubeconfig 12345 --merge --set-current   # cluster -> kubectl in one step
  kdeck linode lke pools 12345
  kdeck linode lke scale 12345 67890 5 --yes
  kdeck linode lke create --label dev --region us-east --version 1.33 --pool g6-standard-2:3 --yes
  kdeck linode volumes ls --unattached                       # volumes billing with no node
  kdeck linode cli linodes list                              # anything else, via linode-cli`,
	}
	cmd.PersistentFlags().StringVar(&o.token, "token", "", "Linode API token (default: LINODE_TOKEN, then linode-cli config)")
	cmd.AddCommand(lkeCmd(o), regionsCmd(o), typesCmd(o), volumesCmd(o), nodeBalancersCmd(o), instancesCmd(o),
		firewallsCmd(o), costCmd(o), leftoversCmd(o), cliCmd())
	return cmd
}

func table(out io.Writer, header string, rows [][]string) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

// --- lke -------------------------------------------------------------------------

func lkeCmd(o *opts) *cobra.Command {
	cmd := &cobra.Command{Use: "lke", Aliases: []string{"k8s", "clusters"}, Short: "LKE clusters"}
	cmd.AddCommand(lifecycleCmds(o)...)

	cmd.AddCommand(&cobra.Command{
		Use: "ls", Aliases: []string{"list"}, Short: "List LKE clusters",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			cl, err := c.ListLKEClusters(cx, nil)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, k := range cl {
				ha := "no"
				if k.ControlPlane.HighAvailability {
					ha = "yes"
				}
				rows = append(rows, []string{strconv.Itoa(k.ID), k.Label, k.Region, k.K8sVersion, string(k.Status), ha, strings.Join(k.Tags, ",")})
			}
			return table(o.out, "ID\tLABEL\tREGION\tVERSION\tSTATUS\tHA\tTAGS", rows)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use: "get ID", Short: "Show one cluster with its node pools", Args: cobra.ExactArgs(1),
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
			fmt.Fprintf(o.out, "ID:       %d\nLabel:    %s\nRegion:   %s\nVersion:  %s\nStatus:   %s\nHA:       %v\nTags:     %s\n\n",
				k.ID, k.Label, k.Region, k.K8sVersion, k.Status, k.ControlPlane.HighAvailability, strings.Join(k.Tags, ","))
			return printPools(cx, c, id, o.out)
		},
	})

	var merge, setCurrent bool
	var outFile, ctxName string
	kc := &cobra.Command{
		Use:     "kubeconfig ID",
		Aliases: []string{"kc", "creds"},
		Short:   "Fetch a cluster's kubeconfig: print it, write it, or merge it into ~/.kube/config",
		Example: `  kdeck linode lke kubeconfig 12345                          # print to stdout
  kdeck linode lke kubeconfig 12345 -f lke.yaml
  kdeck linode lke kubeconfig 12345 --merge --set-current     # backup + merge + switch`,
		Args: cobra.ExactArgs(1),
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
			res, err := c.GetLKEClusterKubeconfig(cx, id)
			if err != nil {
				return err
			}
			raw, err := base64.StdEncoding.DecodeString(res.KubeConfig)
			if err != nil {
				return fmt.Errorf("decoding kubeconfig: %w", err)
			}
			switch {
			case merge:
				name := ctxName
				if name == "" {
					name = "lke-" + k.Label
				}
				path, backup, err := kubeconfig.Merge(o.flags, raw, name, setCurrent)
				if err != nil {
					return err
				}
				fmt.Fprintf(o.out, "Merged cluster %s as context %q into %s (backup: %s).\n", k.Label, name, path, backup)
				if setCurrent {
					fmt.Fprintf(o.out, "Current context is now %q. Try: kdeck get nodes\n", name)
				}
				return nil
			case outFile != "":
				if err := os.WriteFile(outFile, raw, 0o600); err != nil {
					return err
				}
				fmt.Fprintf(o.out, "Wrote %s. Use it with: kdeck --kubeconfig %s get nodes\n", outFile, outFile)
				return nil
			default:
				_, err := o.out.Write(raw)
				return err
			}
		},
	}
	kc.Flags().BoolVar(&merge, "merge", false, "merge into your kubeconfig (a backup is written first)")
	kc.Flags().BoolVar(&setCurrent, "set-current", false, "with --merge, switch to the new context")
	kc.Flags().StringVarP(&outFile, "file", "f", "", "write to this file instead of stdout")
	kc.Flags().StringVar(&ctxName, "context-name", "", "context name for --merge (default lke-<label>)")
	cmd.AddCommand(kc)

	cmd.AddCommand(&cobra.Command{
		Use: "pools ID", Aliases: []string{"pool", "np"}, Short: "List a cluster's node pools and nodes", Args: cobra.ExactArgs(1),
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
			return printPools(cx, c, id, o.out)
		},
	})

	scale := &cobra.Command{
		Use: "scale CLUSTER_ID POOL_ID COUNT", Short: "Resize a node pool (needs --yes)", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := atoi(args[0], "cluster ID")
			if err != nil {
				return err
			}
			pool, err := atoi(args[1], "pool ID")
			if err != nil {
				return err
			}
			count, err := atoi(args[2], "count")
			if err != nil {
				return err
			}
			if count < 1 {
				return fmt.Errorf("count must be at least 1")
			}
			if err := needYes(cmd, "Scaling a node pool"); err != nil {
				return err
			}
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			p, err := c.UpdateLKENodePool(cx, id, pool, linodego.LKENodePoolUpdateOptions{Count: count})
			if err != nil {
				return err
			}
			fmt.Fprintf(o.out, "Pool %d (%s) now targets %d nodes.\n", p.ID, p.Type, p.Count)
			return nil
		},
	}
	scale.Flags().Bool("yes", false, "confirm the change")
	cmd.AddCommand(scale)

	var label, region, version string
	var pools, tags []string
	var ha bool
	create := &cobra.Command{
		Use:   "create",
		Short: "Create an LKE cluster (needs --yes)",
		Example: `  kdeck linode lke create --label dev --region us-east --version 1.33 \
      --pool g6-standard-2:3 --pool g6-standard-4:1 --yes`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if label == "" || region == "" || version == "" || len(pools) == 0 {
				return fmt.Errorf("--label, --region, --version and at least one --pool TYPE:COUNT are required")
			}
			var np []linodego.LKENodePoolCreateOptions
			for _, p := range pools {
				t, n, ok := strings.Cut(p, ":")
				count, err := strconv.Atoi(n)
				if !ok || err != nil || count < 1 {
					return fmt.Errorf("--pool must look like g6-standard-2:3, got %q", p)
				}
				np = append(np, linodego.LKENodePoolCreateOptions{Type: t, Count: count})
			}
			if err := needYes(cmd, "Creating a cluster"); err != nil {
				return err
			}
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			opts := linodego.LKEClusterCreateOptions{Label: label, Region: region, K8sVersion: version, NodePools: np, Tags: tags}
			if ha {
				opts.ControlPlane = &linodego.LKEClusterControlPlaneOptions{HighAvailability: &ha}
			}
			k, err := c.CreateLKECluster(cx, opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(o.out, "Created cluster %s (ID %d) in %s. When it is ready:\n  kdeck linode lke kubeconfig %d --merge --set-current\n", k.Label, k.ID, k.Region, k.ID)
			return nil
		},
	}
	create.Flags().StringVar(&label, "label", "", "cluster label")
	create.Flags().StringVar(&region, "region", "", "region, e.g. us-east (see: kdeck linode regions)")
	create.Flags().StringVar(&version, "version", "", "Kubernetes version (see: kdeck linode lke versions)")
	create.Flags().StringArrayVar(&pools, "pool", nil, "node pool TYPE:COUNT, repeatable (see: kdeck linode types)")
	create.Flags().StringSliceVar(&tags, "tag", nil, "tags")
	create.Flags().BoolVar(&ha, "ha", false, "high-availability control plane (extra cost)")
	create.Flags().Bool("yes", false, "confirm the change")
	cmd.AddCommand(create)

	del := &cobra.Command{
		Use: "delete ID", Aliases: []string{"rm"}, Short: "Delete an LKE cluster and its nodes (needs --yes)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := atoi(args[0], "cluster ID")
			if err != nil {
				return err
			}
			if err := needYes(cmd, "Deleting a cluster"); err != nil {
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
			if err := c.DeleteLKECluster(cx, id); err != nil {
				return err
			}
			fmt.Fprintf(o.out, "Deleted cluster %s (ID %d). Persistent volumes are NOT deleted; check: kdeck linode volumes ls --unattached\n", k.Label, id)
			return nil
		},
	}
	del.Flags().Bool("yes", false, "confirm the change")
	cmd.AddCommand(del)

	cmd.AddCommand(&cobra.Command{
		Use: "versions", Short: "Kubernetes versions LKE offers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			vs, err := c.ListLKEVersions(cx, nil)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, v := range vs {
				rows = append(rows, []string{v.ID})
			}
			return table(o.out, "VERSION", rows)
		},
	})
	return cmd
}

func printPools(cx context.Context, c *linodego.Client, id int, out io.Writer) error {
	pools, err := c.ListLKENodePools(cx, id, nil)
	if err != nil {
		return err
	}
	var rows [][]string
	for _, p := range pools {
		auto := "off"
		if p.Autoscaler.Enabled {
			auto = fmt.Sprintf("%d-%d", p.Autoscaler.Min, p.Autoscaler.Max)
		}
		ready := 0
		for _, n := range p.Linodes {
			if n.Status == "ready" {
				ready++
			}
		}
		rows = append(rows, []string{strconv.Itoa(p.ID), p.Type, fmt.Sprintf("%d/%d", ready, p.Count), auto, strings.Join(p.Tags, ",")})
	}
	return table(out, "POOL\tTYPE\tREADY/COUNT\tAUTOSCALE\tTAGS", rows)
}

// --- other resources ----------------------------------------------------------------

func regionsCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use: "regions", Short: "Regions and whether they offer LKE",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			rs, err := c.ListRegions(cx, nil)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, r := range rs {
				lke := "no"
				for _, cap := range r.Capabilities {
					if cap == "Kubernetes" {
						lke = "yes"
					}
				}
				rows = append(rows, []string{r.ID, r.Label, r.Country, r.Status, lke})
			}
			return table(o.out, "ID\tLABEL\tCOUNTRY\tSTATUS\tLKE", rows)
		},
	}
}

func typesCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use: "types", Aliases: []string{"plans"}, Short: "Node plans with CPU, memory and monthly price",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			ts, err := c.ListTypes(cx, nil)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, t := range ts {
				price := "-"
				if t.Price != nil {
					price = fmt.Sprintf("$%.0f", t.Price.Monthly)
				}
				rows = append(rows, []string{t.ID, string(t.Class), strconv.Itoa(t.VCPUs), fmt.Sprintf("%d GB", t.Memory/1024), fmt.Sprintf("%d GB", t.Disk/1024), price})
			}
			return table(o.out, "TYPE\tCLASS\tVCPU\tMEMORY\tDISK\tMONTHLY", rows)
		},
	}
}

func volumesCmd(o *opts) *cobra.Command {
	cmd := &cobra.Command{Use: "volumes", Aliases: []string{"vol", "volume"}, Short: "Block storage volumes"}
	var unattached bool
	ls := &cobra.Command{
		Use: "ls", Aliases: []string{"list"}, Short: "List volumes; --unattached shows ones billing with no node",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			vs, err := c.ListVolumes(cx, nil)
			if err != nil {
				return err
			}
			var rows [][]string
			total := 0
			for _, v := range vs {
				attached := "-"
				if v.LinodeID != nil {
					attached = v.LinodeLabel
					if attached == "" {
						attached = strconv.Itoa(*v.LinodeID)
					}
				}
				if unattached && v.LinodeID != nil {
					continue
				}
				total += v.Size
				created := ""
				if v.Created != nil {
					created = v.Created.Format("2006-01-02")
				}
				rows = append(rows, []string{strconv.Itoa(v.ID), v.Label, v.Region, fmt.Sprintf("%d GB", v.Size), string(v.Status), attached, created})
			}
			if err := table(o.out, "ID\tLABEL\tREGION\tSIZE\tSTATUS\tATTACHED TO\tCREATED", rows); err != nil {
				return err
			}
			fmt.Fprintf(o.out, "\n%d volumes, %d GB total.\n", len(rows), total)
			return nil
		},
	}
	ls.Flags().BoolVar(&unattached, "unattached", false, "only volumes not attached to any node")
	cmd.AddCommand(ls)
	return cmd
}

func nodeBalancersCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use: "nodebalancers", Aliases: []string{"nb", "lb"}, Short: "NodeBalancers (created by LoadBalancer Services)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			nbs, err := c.ListNodeBalancers(cx, nil)
			if err != nil {
				return err
			}
			str := func(p *string) string {
				if p == nil {
					return "-"
				}
				return *p
			}
			var rows [][]string
			for _, nb := range nbs {
				rows = append(rows, []string{strconv.Itoa(nb.ID), str(nb.Label), nb.Region, str(nb.IPv4), str(nb.Hostname)})
			}
			return table(o.out, "ID\tLABEL\tREGION\tIPV4\tHOSTNAME", rows)
		},
	}
}

func instancesCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use: "instances", Aliases: []string{"linodes", "vms"}, Short: "Linode instances, including LKE nodes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.client()
			if err != nil {
				return err
			}
			cx, cancel := ctx(cmd)
			defer cancel()
			is, err := c.ListInstances(cx, nil)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, i := range is {
				ip := "-"
				if len(i.IPv4) > 0 && i.IPv4[0] != nil {
					ip = i.IPv4[0].String()
				}
				rows = append(rows, []string{strconv.Itoa(i.ID), i.Label, i.Region, i.Type, string(i.Status), ip})
			}
			return table(o.out, "ID\tLABEL\tREGION\tTYPE\tSTATUS\tIPV4", rows)
		},
	}
}

func cliCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "cli [ARGS...]",
		Short:              "Run linode-cli with the given arguments (must be installed: pip install linode-cli)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := exec.LookPath("linode-cli")
			if err != nil {
				return fmt.Errorf("linode-cli is not installed (pip install linode-cli); kdeck's own commands cover LKE, volumes, NodeBalancers and instances")
			}
			c := exec.Command(bin, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
}

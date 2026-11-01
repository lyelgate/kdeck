// Package cloud lists and connects to managed Kubernetes on AWS (EKS),
// Google Cloud (GKE) and Azure (AKS) through each provider's official CLI,
// so existing cloud logins, SSO and profiles work unchanged. Connecting
// backs up the kubeconfig first and can rename the new context to something
// short with --name.
package cloud

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/lyelgate/kdeck/internal/kubeconfig"
	"github.com/lyelgate/kdeck/internal/match"
)

// Cluster is one managed cluster as listed by a provider.
type Cluster struct {
	Name, Location, Group, Version, Status string
}

type provider struct {
	cli, install string
	list         func(flags map[string]string) ([]Cluster, error)
	connect      func(c Cluster, flags map[string]string, kubeconfigPath string) error
	needs        []string // credential plugins the kubeconfig will call
}

func lookCLI(name, install string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s is not installed: %s", name, install)
	}
	return p, nil
}

func output(cli, install string, args ...string) ([]byte, error) {
	bin, err := lookCLI(cli, install)
	if err != nil {
		return nil, err
	}
	c := exec.Command(bin, args...)
	c.Stderr = os.Stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", cli, strings.Join(args, " "), err)
	}
	return out, nil
}

func runCLI(env []string, cli, install string, args ...string) error {
	bin, err := lookCLI(cli, install)
	if err != nil {
		return err
	}
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = append(os.Environ(), env...)
	return c.Run()
}

func opt(flags map[string]string, key, flag string) []string {
	if v := flags[key]; v != "" {
		return []string{flag, v}
	}
	return nil
}

var providers = map[string]provider{
	"eks": {
		cli: "aws", install: "https://aws.amazon.com/cli/ (brew install awscli / winget install Amazon.AWSCLI)",
		list: func(f map[string]string) ([]Cluster, error) {
			args := append([]string{"eks", "list-clusters", "--output", "json"}, append(opt(f, "region", "--region"), opt(f, "profile", "--profile")...)...)
			data, err := output("aws", "", args...)
			if err != nil {
				return nil, err
			}
			var res struct {
				Clusters []string `json:"clusters"`
			}
			if err := json.Unmarshal(data, &res); err != nil {
				return nil, err
			}
			var out []Cluster
			for _, n := range res.Clusters {
				out = append(out, Cluster{Name: n, Location: f["region"]})
			}
			return out, nil
		},
		connect: func(c Cluster, f map[string]string, path string) error {
			args := []string{"eks", "update-kubeconfig", "--name", c.Name, "--kubeconfig", path}
			args = append(args, opt(f, "region", "--region")...)
			args = append(args, opt(f, "profile", "--profile")...)
			args = append(args, opt(f, "name", "--alias")...)
			return runCLI(nil, "aws", "", args...)
		},
		needs: []string{"aws"},
	},
	"gke": {
		cli: "gcloud", install: "https://cloud.google.com/sdk/docs/install, then: gcloud components install gke-gcloud-auth-plugin",
		list: func(f map[string]string) ([]Cluster, error) {
			args := append([]string{"container", "clusters", "list", "--format=json"}, opt(f, "project", "--project")...)
			data, err := output("gcloud", "", args...)
			if err != nil {
				return nil, err
			}
			var res []struct {
				Name, Location, Status string
				CurrentMasterVersion   string `json:"currentMasterVersion"`
			}
			if err := json.Unmarshal(data, &res); err != nil {
				return nil, err
			}
			var out []Cluster
			for _, r := range res {
				out = append(out, Cluster{Name: r.Name, Location: r.Location, Version: r.CurrentMasterVersion, Status: r.Status})
			}
			return out, nil
		},
		connect: func(c Cluster, f map[string]string, path string) error {
			args := []string{"container", "clusters", "get-credentials", c.Name, "--location", c.Location}
			args = append(args, opt(f, "project", "--project")...)
			return runCLI([]string{"KUBECONFIG=" + path}, "gcloud", "", args...)
		},
		needs: []string{"gke-gcloud-auth-plugin"},
	},
	"aks": {
		cli: "az", install: "https://learn.microsoft.com/cli/azure/install-azure-cli (brew install azure-cli / winget install Microsoft.AzureCLI)",
		list: func(f map[string]string) ([]Cluster, error) {
			args := append([]string{"aks", "list", "-o", "json"}, opt(f, "subscription", "--subscription")...)
			data, err := output("az", "", args...)
			if err != nil {
				return nil, err
			}
			var res []struct {
				Name, Location    string
				ResourceGroup     string                `json:"resourceGroup"`
				KubernetesVersion string                `json:"kubernetesVersion"`
				PowerState        struct{ Code string } `json:"powerState"`
			}
			if err := json.Unmarshal(data, &res); err != nil {
				return nil, err
			}
			var out []Cluster
			for _, r := range res {
				out = append(out, Cluster{Name: r.Name, Location: r.Location, Group: r.ResourceGroup, Version: r.KubernetesVersion, Status: r.PowerState.Code})
			}
			return out, nil
		},
		connect: func(c Cluster, f map[string]string, path string) error {
			args := []string{"aks", "get-credentials", "--name", c.Name, "--resource-group", c.Group, "--file", path}
			args = append(args, opt(f, "subscription", "--subscription")...)
			if err := runCLI(nil, "az", "", args...); err != nil {
				return err
			}
			if f["kubelogin"] == "true" {
				// Entra ID (AAD) clusters: let kubelogin reuse the az login.
				return runCLI(nil, "kubelogin", "https://azure.github.io/kubelogin/ (brew install Azure/kubelogin/kubelogin / winget install Microsoft.Azure.Kubelogin)",
					"convert-kubeconfig", "-l", "azurecli", "--kubeconfig", path)
			}
			return nil
		},
		needs: nil, // kubelogin only with --kubelogin, which runs it and fails clearly if missing
	},
}

// NewCommands returns "kdeck eks", "kdeck gke" and "kdeck aks".
func NewCommands(cfgFlags *genericclioptions.ConfigFlags, out io.Writer) []*cobra.Command {
	var cmds []*cobra.Command
	titles := map[string]string{"eks": "AWS EKS", "gke": "Google GKE", "aks": "Azure AKS"}
	scopeFlags := map[string][][2]string{
		"eks": {{"region", "AWS region (default: your aws config)"}, {"profile", "AWS profile"}},
		"gke": {{"project", "GCP project (default: gcloud config)"}},
		"aks": {{"subscription", "Azure subscription (default: az account)"}},
	}
	for _, key := range []string{"eks", "gke", "aks"} {
		key, p := key, providers[key]
		ptrs := map[string]*string{}
		vals := func() map[string]string {
			m := map[string]string{}
			for k, v := range ptrs {
				m[k] = *v
			}
			return m
		}
		root := &cobra.Command{
			Use:   key,
			Short: titles[key] + ": list clusters and connect kubectl (uses the " + p.cli + " CLI and your existing login)",
			Example: fmt.Sprintf(`  kdeck %[1]s ls
  kdeck %[1]s ls prod                      # regex filter
  kdeck %[1]s use prod-api --name prod     # connect, rename the context to "prod", switch to it`, key),
		}
		for _, sf := range scopeFlags[key] {
			ptrs[sf[0]] = root.PersistentFlags().String(sf[0], "", sf[1])
		}
		root.AddCommand(&cobra.Command{
			Use: "ls [REGEX]", Aliases: []string{"list"}, Short: "List clusters",
			Args: cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cs, err := filtered(p, vals(), args)
				if err != nil {
					return err
				}
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tLOCATION\tRESOURCE GROUP\tVERSION\tSTATUS")
				for _, c := range cs {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Name, dash(c.Location), dash(c.Group), dash(c.Version), dash(c.Status))
				}
				return tw.Flush()
			},
		})
		var name string
		var kubelogin bool
		use := &cobra.Command{
			Use: "use CLUSTER-REGEX", Aliases: []string{"connect", "kubeconfig", "login"},
			Short: "Add the cluster to your kubeconfig (backup first) and switch to it",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cs, err := filtered(p, vals(), nil)
				if err != nil {
					return err
				}
				c, err := pick(cs, args[0])
				if err != nil {
					return err
				}
				path := kubeconfig.Path(cfgFlags)
				backup, err := kubeconfig.Backup(path)
				if err != nil {
					return err
				}
				before := contexts(path)
				f := map[string]string{"name": name}
				for k, v := range vals() {
					f[k] = v
				}
				if kubelogin {
					f["kubelogin"] = "true"
				}
				if err := p.connect(c, f, path); err != nil {
					return err
				}
				added := newContext(before, contexts(path))
				if name != "" && added != "" && added != name {
					if err := rename(path, added, name); err != nil {
						return err
					}
					added = name
				}
				fmt.Fprintf(out, "Connected %s %q", titles[key], c.Name)
				if added != "" {
					fmt.Fprintf(out, " as context %q", added)
				}
				if backup != "" {
					fmt.Fprintf(out, " (backup: %s)", backup)
				}
				fmt.Fprintln(out, ".")
				for _, need := range p.needs {
					bin := strings.Fields(need)[0]
					if _, err := exec.LookPath(bin); err != nil {
						fmt.Fprintf(out, "Note: the kubeconfig calls %s for credentials, and it is not on PATH.\n", need)
					}
				}
				return nil
			},
		}
		use.Flags().StringVar(&name, "name", "", "short context name to use instead of the provider's long one")
		if key == "aks" {
			use.Flags().BoolVar(&kubelogin, "kubelogin", false, "convert to kubelogin (Entra ID / AAD clusters)")
		}
		root.AddCommand(use)
		root.GroupID = "kdeck"
		cmds = append(cmds, root)
	}
	return cmds
}

func filtered(p provider, flags map[string]string, args []string) ([]Cluster, error) {
	cs, err := p.list(flags)
	if err != nil {
		return nil, err
	}
	pattern := ""
	if len(args) == 1 {
		pattern = args[0]
	}
	m, err := match.Compile(pattern)
	if err != nil {
		return nil, err
	}
	var out []Cluster
	for _, c := range cs {
		if m.Any(c.Name, c.Location, c.Group) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func pick(cs []Cluster, pattern string) (Cluster, error) {
	for _, c := range cs {
		if c.Name == pattern {
			return c, nil
		}
	}
	m, err := match.Compile(pattern)
	if err != nil {
		return Cluster{}, err
	}
	var hits []Cluster
	for _, c := range cs {
		if m.Match(c.Name) {
			hits = append(hits, c)
		}
	}
	switch len(hits) {
	case 0:
		return Cluster{}, fmt.Errorf("no cluster matches %q", pattern)
	case 1:
		return hits[0], nil
	}
	var names []string
	for _, h := range hits {
		names = append(names, h.Name)
	}
	return Cluster{}, fmt.Errorf("%d clusters match %q: %s", len(hits), pattern, strings.Join(names, ", "))
}

func contexts(path string) map[string]bool {
	set := map[string]bool{}
	if c, err := clientcmd.LoadFromFile(path); err == nil {
		for n := range c.Contexts {
			set[n] = true
		}
	}
	return set
}

// newContext returns the single context that appeared, or the current one.
func newContext(before, after map[string]bool) string {
	var added []string
	for n := range after {
		if !before[n] {
			added = append(added, n)
		}
	}
	if len(added) == 1 {
		return added[0]
	}
	return ""
}

func rename(path, from, to string) error {
	c, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return err
	}
	ctx, ok := c.Contexts[from]
	if !ok {
		return nil
	}
	c.Contexts[to] = ctx
	delete(c.Contexts, from)
	if c.CurrentContext == from {
		c.CurrentContext = to
	}
	return clientcmd.WriteToFile(*c, path)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

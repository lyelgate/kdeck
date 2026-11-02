package workspace

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/lyelgate/kdeck/internal/config"
	"github.com/lyelgate/kdeck/internal/kubeconfig"
	"github.com/lyelgate/kdeck/internal/match"
)

// KubeconfigCommand returns "kdeck kubeconfig": one kubeconfig file per
// cluster in ~/.kdeck/kubeconfigs.
func KubeconfigCommand(flags *genericclioptions.ConfigFlags, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "kubeconfig",
		Aliases: []string{"kc", "kubeconfigs", "kcfg"},
		Short:   "One kubeconfig file per cluster in ~/.kdeck/kubeconfigs: list, add, split, remove",
		Long: `kdeck keeps one kubeconfig file per cluster in ~/.kdeck/kubeconfigs
(KDECK_KUBECONFIG_DIR to move it), so an expired, broken or removed cluster
never touches the others. New clusters land there: kdeck tu (Teleport),
kdeck linode lke kubeconfig --merge, kdeck eks/gke/aks use, kdeck ctx import.

kdeck adds every file to KUBECONFIG after your main kubeconfig (~/.kube/config),
which keeps the current context, so kdeck ctx, kh and every kubectl command see
all clusters. The shell-init hook does the same for kubectl, helm and k9s in
your shell. "kdeck kubeconfig mode merge" goes back to one merged file.`,
		Example: `  kdeck kc                          # list the files and their contexts
  kdeck kc add ~/Downloads/staging.yaml --name staging --set-current
  kdeck kc split                    # move every context of ~/.kube/config into its own file
  kdeck kc rm staging
  kdeck kc env                      # KUBECONFIG line for tools started outside kdeck`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return listKubeconfigs(out, args) },
	}
	cmd.AddCommand(&cobra.Command{
		Use: "ls [REGEX]", Aliases: []string{"list"}, Short: "List the per-cluster kubeconfig files and their contexts",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return listKubeconfigs(out, args) },
	})

	var name string
	var setCurrent bool
	add := &cobra.Command{
		Use: "add FILE", Short: "Copy a kubeconfig file into the store as NAME (contexts renamed NAME, NAME-2, ...)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			n := name
			if n == "" {
				if c, err := clientcmd.Load(raw); err == nil && c.CurrentContext != "" {
					n = c.CurrentContext
				} else {
					n = strings.TrimSuffix(filepath.Base(args[0]), filepath.Ext(args[0]))
				}
			}
			verb := "Added"
			if _, err := os.Stat(kubeconfig.FileFor(n)); err == nil {
				verb = "Replaced the existing file with"
			}
			path, err := kubeconfig.Store(raw, n)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s %s as context %q in %s.\n", verb, args[0], n, path)
			warnShadowed(out, path)
			if setCurrent {
				if err := kubeconfig.SetCurrent(n); err != nil {
					return err
				}
				fmt.Fprintf(out, "Switched to context %q.\n", n)
			}
			return nil
		},
	}
	add.Flags().StringVar(&name, "name", "", "context and file name (default: the file's current context)")
	add.Flags().BoolVar(&setCurrent, "set-current", false, "switch to the added context")
	cmd.AddCommand(add)

	var keep bool
	split := &cobra.Command{
		Use:   "split [CONTEXT-REGEX]",
		Short: "Move contexts from your main kubeconfig into one file each (backup first; --keep to copy)",
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
			kubeconfig.Activate()
			main := kubeconfig.Path(flags)
			cfg, err := clientcmd.LoadFromFile(main)
			if err != nil {
				return err
			}
			var moved []string
			for _, n := range sortedKeys(cfg.Contexts) {
				if !m.Match(n) {
					continue
				}
				if _, err := os.Stat(kubeconfig.FileFor(n)); err == nil {
					fmt.Fprintf(out, "skip %s: %s already exists\n", n, kubeconfig.FileFor(n))
					continue
				}
				one := Extract(cfg, n)
				kubeconfig.Normalize(one)
				if err := os.MkdirAll(kubeconfig.Dir(), 0o700); err != nil {
					return err
				}
				if err := clientcmd.WriteToFile(*one, kubeconfig.FileFor(n)); err != nil {
					return err
				}
				_ = os.Chmod(kubeconfig.FileFor(n), 0o600)
				fmt.Fprintf(out, "%s -> %s\n", n, kubeconfig.FileFor(n))
				moved = append(moved, n)
			}
			if len(moved) == 0 {
				fmt.Fprintln(out, "Nothing to split.")
				return nil
			}
			if keep {
				fmt.Fprintf(out, "Copied %d contexts; %s still has them, and its copies win (first file in KUBECONFIG).\n", len(moved), main)
				return nil
			}
			backup, err := kubeconfig.Backup(main)
			if err != nil {
				return err
			}
			Prune(cfg, moved)
			if err := clientcmd.WriteToFile(*cfg, main); err != nil {
				return err
			}
			fmt.Fprintf(out, "Moved %d contexts out of %s (backup: %s). The current context is unchanged.\n", len(moved), main, backup)
			fmt.Fprintln(out, `kubectl outside kdeck needs the files in KUBECONFIG: eval "$(kdeck shell-init zsh)" (or bash, fish) keeps it up to date.`)
			return nil
		},
	}
	split.Flags().BoolVar(&keep, "keep", false, "copy instead of move")
	cmd.AddCommand(split)

	cmd.AddCommand(&cobra.Command{
		Use: "rm NAME", Aliases: []string{"remove", "delete"}, Short: "Remove a cluster's file (kept aside as NAME.yaml.removed-<time>)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kubeconfig.Activate()
			current := ""
			if merged, err := clientcmd.NewDefaultClientConfigLoadingRules().Load(); err == nil {
				current = merged.CurrentContext
			}
			var held bool
			if cfg, err := clientcmd.LoadFromFile(kubeconfig.FileFor(args[0])); err == nil {
				_, held = cfg.Contexts[current]
			}
			aside, err := kubeconfig.Remove(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Removed %s (moved to %s).\n", args[0], aside)
			if held {
				fmt.Fprintf(out, "The current context %q was in that file; pick another: kdeck ctx\n", current)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "path [NAME]", Short: "Print the store directory, or the file for NAME",
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) == 1 {
				fmt.Fprintln(out, kubeconfig.FileFor(args[0]))
				return
			}
			fmt.Fprintln(out, kubeconfig.Dir())
		},
	})
	var shell string
	var value bool
	env := &cobra.Command{
		Use:   "env",
		Short: "Print the KUBECONFIG line that adds every file (for tools started outside kdeck)",
		Example: `  eval "$(kdeck kubeconfig env)"
  kdeck kubeconfig env --shell powershell | Out-String | Invoke-Expression`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			list := strings.Join(kubeconfig.Precedence(os.Getenv("KUBECONFIG"), kubeconfig.Files()), listSep())
			if value {
				fmt.Fprintln(out, list)
				return
			}
			fmt.Fprintln(out, EnvLine(shell, list))
		},
	}
	env.Flags().StringVar(&shell, "shell", defaultShell(), "bash, zsh, fish, powershell or cmd")
	env.Flags().BoolVar(&value, "value", false, "print only the value")
	cmd.AddCommand(env)
	cmd.AddCommand(&cobra.Command{
		Use: "mode [separate|merge]", Short: "Show or set where new clusters go: own files (separate, default) or merged into ~/.kube/config",
		Args: cobra.MaximumNArgs(1), ValidArgs: []string{"separate", "merge"},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				mode := cfg.Kubeconfig.Mode
				if mode == "" {
					mode = "separate"
				}
				fmt.Fprintln(out, mode)
				return nil
			}
			switch args[0] {
			case "separate":
				cfg.Kubeconfig.Mode = ""
			case "merge":
				cfg.Kubeconfig.Mode = "merge"
			default:
				return fmt.Errorf("mode is separate or merge, not %q", args[0])
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(out, "New clusters now go %s. Existing files are unchanged.\n", map[string]string{"separate": "to their own file in " + kubeconfig.Dir(), "merge": "into your main kubeconfig"}[args[0]])
			return nil
		},
	})
	return cmd
}

func listKubeconfigs(out io.Writer, args []string) error {
	pattern := ""
	if len(args) == 1 {
		pattern = args[0]
	}
	m, err := match.Compile(pattern)
	if err != nil {
		return err
	}
	kubeconfig.Activate()
	current := ""
	if merged, err := clientcmd.NewDefaultClientConfigLoadingRules().Load(); err == nil {
		current = merged.CurrentContext
	}
	files := kubeconfig.Files()
	mode := "separate"
	if !kubeconfig.Separate() {
		mode = "merge"
	}
	fmt.Fprintf(out, "%s (mode: %s, %d files)\n", kubeconfig.Dir(), mode, len(files))
	if len(files) == 0 {
		fmt.Fprintln(out, "No per-cluster files yet. Add one: kdeck kc add FILE, kdeck tu CLUSTER, or move yours: kdeck kc split")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CURRENT\tFILE\tCONTEXTS\tSERVER\tNOTE")
	for _, f := range files {
		cfg, err := clientcmd.LoadFromFile(f)
		base := strings.TrimSuffix(filepath.Base(f), ".yaml")
		if err != nil {
			fmt.Fprintf(tw, "\t%s\t-\t-\tunreadable: %v\n", base, err)
			continue
		}
		ctxs := sortedKeys(cfg.Contexts)
		var servers []string
		for _, c := range ctxs {
			if cl, ok := cfg.Clusters[cfg.Contexts[c].Cluster]; ok {
				servers = append(servers, cl.Server)
			}
		}
		if !m.Any(append(append([]string{base}, ctxs...), servers...)...) {
			continue
		}
		mark := ""
		for _, c := range ctxs {
			if c == current {
				mark = "*"
			}
		}
		var notes []string
		for c, by := range kubeconfig.Shadowed(f, ctxs) {
			notes = append(notes, fmt.Sprintf("%s hidden by %s", c, by))
		}
		sort.Strings(notes)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", mark, base, strings.Join(ctxs, ","), strings.Join(dedupe(servers), ","), strings.Join(notes, "; "))
	}
	return tw.Flush()
}

func warnShadowed(out io.Writer, path string) {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return
	}
	for c, by := range kubeconfig.Shadowed(path, sortedKeys(cfg.Contexts)) {
		fmt.Fprintf(out, "Note: %s also defines context %q and comes first in KUBECONFIG, so that one is used. Rename one: kdeck ctx rename\n", by, c)
	}
}

// Extract returns a config holding only context name, with its cluster and
// user.
func Extract(cfg *api.Config, name string) *api.Config {
	out := api.NewConfig()
	c := cfg.Contexts[name]
	out.Contexts[name] = c.DeepCopy()
	if v, ok := cfg.Clusters[c.Cluster]; ok {
		out.Clusters[c.Cluster] = v.DeepCopy()
	}
	if v, ok := cfg.AuthInfos[c.AuthInfo]; ok {
		out.AuthInfos[c.AuthInfo] = v.DeepCopy()
	}
	return out
}

// Prune deletes the named contexts from cfg, and their clusters and users
// when no remaining context uses them. Other entries, used or not, and the
// current context are left alone.
func Prune(cfg *api.Config, names []string) {
	clusters, users := map[string]bool{}, map[string]bool{}
	for _, n := range names {
		if c, ok := cfg.Contexts[n]; ok {
			clusters[c.Cluster], users[c.AuthInfo] = true, true
		}
		delete(cfg.Contexts, n)
	}
	for _, c := range cfg.Contexts {
		delete(clusters, c.Cluster)
		delete(users, c.AuthInfo)
	}
	for k := range clusters {
		delete(cfg.Clusters, k)
	}
	for k := range users {
		delete(cfg.AuthInfos, k)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Package kctx switches kubeconfig contexts and namespaces, in the spirit of
// kubectx and kubens, with "-" for the previous one and an fzf picker.
package kctx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/lyelgate/kdeck/internal/config"
	"github.com/lyelgate/kdeck/internal/kubeconfig"
	"github.com/lyelgate/kdeck/internal/match"
)

// NewCtxCommand returns "kdeck ctx".
func NewCtxCommand(flags *genericclioptions.ConfigFlags, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ctx [NAME | -]",
		Aliases: []string{"context", "cx", "dkctx"},
		Short:   "List or switch kubeconfig contexts (\"-\" = previous, no argument = picker)",
		Example: `  kdeck ctx            # pick with fzf, or list when fzf is missing
  kdeck ctx prod       # switch to "prod" (exact name, or a regex with one match)
  kdeck ctx 'lke.*dev' # regex; several matches open the picker
  kdeck ctx -          # back to the previous context
  kdeck ctx -c         # print the current context
  kdeck ctx rename old new
  kdeck ctx rm old`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, access, err := load(flags)
			if err != nil {
				return err
			}
			if cur, _ := cmd.Flags().GetBool("current"); cur {
				fmt.Fprintln(out, cfg.CurrentContext)
				return nil
			}
			names := sortedContexts(cfg)
			target := ""
			switch {
			case len(args) == 1 && args[0] == "-":
				target = config.LoadState().PreviousContext
				if target == "" {
					return fmt.Errorf("no previous context recorded yet")
				}
			case len(args) == 1:
				target = args[0]
			default:
				if picked, ok := pick(names, cfg.CurrentContext); ok {
					target = picked
				} else {
					for _, n := range names {
						marker := "  "
						if n == cfg.CurrentContext {
							marker = "* "
						}
						fmt.Fprintf(out, "%s%s\n", marker, n)
					}
					return nil
				}
			}
			if _, ok := cfg.Contexts[target]; !ok {
				resolved, err := resolve("context", target, names)
				if err != nil {
					return err
				}
				target = resolved
			}
			prev := cfg.CurrentContext
			cfg.CurrentContext = target
			if err := clientcmd.ModifyConfig(access, *cfg, true); err != nil {
				return err
			}
			if prev != target {
				st := config.LoadState()
				st.PreviousContext = prev
				_ = st.Save()
			}
			fmt.Fprintf(out, "Switched to context %q (namespace %q).\n", target, nsOf(cfg, target))
			return nil
		},
	}
	cmd.Flags().BoolP("current", "c", false, "print the current context")
	var importName string
	var importCurrent bool
	imp := &cobra.Command{
		Use:   "import FILE",
		Short: "Merge a kubeconfig file into yours (backup first; entries renamed so nothing is overwritten)",
		Example: `  kdeck ctx import ~/Downloads/staging.yaml --name staging --set-current
  kdeck ctx import vendor-kubeconfig.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			name := importName
			if name == "" {
				if c, err := clientcmd.Load(raw); err == nil && c.CurrentContext != "" {
					name = c.CurrentContext
				} else {
					name = strings.TrimSuffix(filepath.Base(args[0]), filepath.Ext(args[0]))
				}
			}
			path, backup, err := kubeconfig.Merge(flags, raw, name, importCurrent)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Imported %s as context %q into %s", args[0], name, path)
			if backup != "" {
				fmt.Fprintf(out, " (backup: %s)", backup)
			}
			fmt.Fprintln(out, ".")
			return nil
		},
	}
	imp.Flags().StringVar(&importName, "name", "", "context name (default: the file's current context)")
	imp.Flags().BoolVar(&importCurrent, "set-current", false, "switch to the imported context")
	cmd.AddCommand(imp)
	cmd.AddCommand(&cobra.Command{
		Use:   "rename OLD NEW",
		Short: "Rename a context",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, access, err := load(flags)
			if err != nil {
				return err
			}
			c, ok := cfg.Contexts[args[0]]
			if !ok {
				return fmt.Errorf("context %q not found", args[0])
			}
			if _, exists := cfg.Contexts[args[1]]; exists {
				return fmt.Errorf("context %q already exists", args[1])
			}
			cfg.Contexts[args[1]] = c
			delete(cfg.Contexts, args[0])
			if cfg.CurrentContext == args[0] {
				cfg.CurrentContext = args[1]
			}
			if err := clientcmd.ModifyConfig(access, *cfg, true); err != nil {
				return err
			}
			fmt.Fprintf(out, "Renamed %q to %q.\n", args[0], args[1])
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:     "rm NAME",
		Aliases: []string{"delete"},
		Short:   "Delete a context (its cluster and user entries are kept)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, access, err := load(flags)
			if err != nil {
				return err
			}
			if _, ok := cfg.Contexts[args[0]]; !ok {
				return fmt.Errorf("context %q not found", args[0])
			}
			delete(cfg.Contexts, args[0])
			if cfg.CurrentContext == args[0] {
				cfg.CurrentContext = ""
			}
			if err := clientcmd.ModifyConfig(access, *cfg, true); err != nil {
				return err
			}
			fmt.Fprintf(out, "Deleted context %q.\n", args[0])
			return nil
		},
	})
	return cmd
}

// NewNsCommand returns "kdeck ns".
func NewNsCommand(flags *genericclioptions.ConfigFlags, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ns [NAME | -]",
		Aliases: []string{"namespace-switch", "dkns"},
		Short:   "List or switch the current context's namespace (\"-\" = previous)",
		Example: `  kdeck ns             # pick with fzf, or list when fzf is missing
  kdeck ns payments    # use "payments" (exact name, or a regex with one match)
  kdeck ns '^pay'      # regex
  kdeck ns -           # back to the previous namespace
  kdeck ns -c          # print the current namespace`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, access, err := load(flags)
			if err != nil {
				return err
			}
			ctxName := cfg.CurrentContext
			if flags.Context != nil && *flags.Context != "" {
				ctxName = *flags.Context
			}
			kc, ok := cfg.Contexts[ctxName]
			if !ok {
				return fmt.Errorf("no current context; run \"kdeck ctx\" first")
			}
			current := kc.Namespace
			if current == "" {
				current = "default"
			}
			if cur, _ := cmd.Flags().GetBool("current"); cur {
				fmt.Fprintln(out, current)
				return nil
			}
			target := ""
			switch {
			case len(args) == 1 && args[0] == "-":
				target = config.LoadState().PreviousNamespace[ctxName]
				if target == "" {
					return fmt.Errorf("no previous namespace recorded for context %q", ctxName)
				}
			case len(args) == 1:
				target = args[0]
			default:
				names, err := listNamespaces(flags)
				if err != nil {
					return err
				}
				if picked, ok := pick(names, current); ok {
					target = picked
				} else {
					for _, n := range names {
						marker := "  "
						if n == current {
							marker = "* "
						}
						fmt.Fprintf(out, "%s%s\n", marker, n)
					}
					return nil
				}
			}
			if skip, _ := cmd.Flags().GetBool("force"); !skip {
				if names, err := listNamespaces(flags); err == nil && !contains(names, target) {
					resolved, rerr := resolve("namespace", target, names)
					if rerr != nil {
						return fmt.Errorf("%v (use --force to set %q anyway)", rerr, target)
					}
					target = resolved
				}
			}
			kc.Namespace = target
			if err := clientcmd.ModifyConfig(access, *cfg, true); err != nil {
				return err
			}
			if current != target {
				st := config.LoadState()
				st.PreviousNamespace[ctxName] = current
				_ = st.Save()
			}
			fmt.Fprintf(out, "Context %q now uses namespace %q.\n", ctxName, target)
			return nil
		},
	}
	cmd.Flags().BoolP("current", "c", false, "print the current namespace")
	cmd.Flags().Bool("force", false, "set the namespace even if it does not exist")
	return cmd
}

func load(flags *genericclioptions.ConfigFlags) (*api.Config, clientcmd.ConfigAccess, error) {
	loader := flags.ToRawKubeConfigLoader()
	access := loader.ConfigAccess()
	cfg, err := access.GetStartingConfig()
	if err != nil {
		return nil, nil, err
	}
	return cfg, access, nil
}

func sortedContexts(cfg *api.Config) []string {
	names := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func nsOf(cfg *api.Config, ctx string) string {
	if c, ok := cfg.Contexts[ctx]; ok && c.Namespace != "" {
		return c.Namespace
	}
	return "default"
}

func listNamespaces(flags *genericclioptions.ConfigFlags) ([]string, error) {
	rc, err := flags.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	list, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	return names, nil
}

// resolve treats a name that is not an exact match as a regex: one match is
// used, several open the picker (or are listed), none is an error.
func resolve(what, pattern string, names []string) (string, error) {
	m, err := match.Compile(pattern)
	if err != nil {
		return "", err
	}
	var hits []string
	for _, n := range names {
		if m.Match(n) {
			hits = append(hits, n)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no %s matches %q", what, pattern)
	case 1:
		return hits[0], nil
	}
	if picked, ok := pick(hits, ""); ok {
		return picked, nil
	}
	return "", fmt.Errorf("%d %ss match %q: %s", len(hits), what, pattern, strings.Join(hits, ", "))
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// pick runs fzf when it is installed and both stdin and stdout are terminals.
func pick(items []string, current string) (string, bool) {
	if os.Getenv("KDECK_NO_FZF") != "" || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return "", false
	}
	fzf, err := exec.LookPath("fzf")
	if err != nil {
		return "", false
	}
	c := exec.Command(fzf, "--height=40%", "--reverse", "--prompt=> ", "--header=current: "+current)
	c.Stdin = strings.NewReader(strings.Join(items, "\n"))
	c.Stderr = os.Stderr
	var buf bytes.Buffer
	c.Stdout = &buf
	if err := c.Run(); err != nil {
		return "", false
	}
	choice := strings.TrimSpace(buf.String())
	return choice, choice != ""
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

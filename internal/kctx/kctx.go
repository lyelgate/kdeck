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
	"sort"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/lyelgate/kubepilot/internal/config"
)

// NewCtxCommand returns "kp ctx".
func NewCtxCommand(flags *genericclioptions.ConfigFlags, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ctx [NAME | -]",
		Aliases: []string{"context", "cx"},
		Short:   "List or switch kubeconfig contexts (\"-\" = previous, no argument = picker)",
		Example: `  kp ctx            # pick with fzf, or list when fzf is missing
  kp ctx prod       # switch to "prod"
  kp ctx -          # back to the previous context
  kp ctx -c         # print the current context
  kp ctx rename old new
  kp ctx rm old`,
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
				return fmt.Errorf("context %q not found (have: %s)", target, strings.Join(names, ", "))
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

// NewNsCommand returns "kp ns".
func NewNsCommand(flags *genericclioptions.ConfigFlags, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ns [NAME | -]",
		Aliases: []string{"namespace-switch"},
		Short:   "List or switch the current context's namespace (\"-\" = previous)",
		Example: `  kp ns             # pick with fzf, or list when fzf is missing
  kp ns payments    # use "payments" for the current context
  kp ns -           # back to the previous namespace
  kp ns -c          # print the current namespace`,
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
				return fmt.Errorf("no current context; run \"kp ctx\" first")
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
					return fmt.Errorf("namespace %q does not exist (use --force to set it anyway)", target)
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
	if os.Getenv("KP_NO_FZF") != "" || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
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

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

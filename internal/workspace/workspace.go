// Package workspace holds the commands for working safely across many
// clusters: safety rules, history, a context per terminal, prompt segment,
// profiles and drift comparison.
package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/lyelgate/kdeck/internal/config"
	"github.com/lyelgate/kdeck/internal/guard"
	"github.com/lyelgate/kdeck/internal/match"
	"github.com/lyelgate/kdeck/internal/teleport"
)

// --- safety ----------------------------------------------------------------------

// SafetyCommand returns "kdeck safety".
func SafetyCommand(flags *genericclioptions.ConfigFlags, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "safety",
		Short: "Production and read-only rules: which contexts need confirmation, which refuse changes",
		Example: `  kdeck safety                         # rules, and how they apply to the current context
  kdeck safety prod add 'prod|live'    # writes on matching contexts need the name typed back
  kdeck safety ro add '^audit-'        # matching contexts refuse writes and exec
  kdeck safety diff off                # skip the diff shown before apply on production
  kdeck --ro delete pod x              # one-off read-only (also KDECK_RO=1)`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			raw, _ := flags.ToRawKubeConfigLoader().RawConfig()
			ctx := raw.CurrentContext
			if flags.Context != nil && *flags.Context != "" {
				ctx = *flags.Context
			}
			p := guard.Resolve(cfg.Safety, ctx, false)
			fmt.Fprintf(out, "production patterns: %s\nread-only patterns:  %s\ndiff before apply:   %v\n\n", list(cfg.Safety.Production), list(cfg.Safety.ReadOnly), p.DiffApply)
			names := make([]string, 0, len(raw.Contexts))
			for n := range raw.Contexts {
				names = append(names, n)
			}
			sort.Strings(names)
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CONTEXT\tPROTECTION")
			for _, n := range names {
				q := guard.Resolve(cfg.Safety, n, false)
				state := "none"
				switch {
				case q.ReadOnly:
					state = "read-only"
				case q.Production:
					state = "production (confirm writes)"
				}
				mark := "  "
				if n == ctx {
					mark = "* "
				}
				fmt.Fprintf(tw, "%s%s\t%s\n", mark, n, state)
			}
			return tw.Flush()
		},
	}
	edit := func(use, field, short string) *cobra.Command {
		c := &cobra.Command{Use: use, Short: short}
		c.AddCommand(&cobra.Command{
			Use: "add REGEX", Short: "Add a context pattern", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if _, err := match.Compile(args[0]); err != nil {
					return err
				}
				return update(out, func(s *config.SafetyConfig) {
					l := pick(s, field)
					for _, x := range *l {
						if x == args[0] {
							return
						}
					}
					*l = append(*l, args[0])
				})
			},
		}, &cobra.Command{
			Use: "rm REGEX", Short: "Remove a context pattern", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return update(out, func(s *config.SafetyConfig) {
					l := pick(s, field)
					var keep []string
					for _, x := range *l {
						if x != args[0] {
							keep = append(keep, x)
						}
					}
					*l = keep
				})
			},
		})
		return c
	}
	cmd.AddCommand(edit("prod", "production", "Production context patterns (confirm writes, diff before apply)"))
	cmd.AddCommand(edit("ro", "readOnly", "Read-only context patterns (refuse writes and exec)"))
	cmd.AddCommand(&cobra.Command{
		Use: "diff on|off", Short: "Show kubectl diff before apply on production contexts", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			on := args[0] == "on"
			if !on && args[0] != "off" {
				return fmt.Errorf("use on or off")
			}
			return update(out, func(s *config.SafetyConfig) { s.DiffBeforeApply = &on })
		},
	})
	return cmd
}

func pick(s *config.SafetyConfig, field string) *[]string {
	if field == "production" {
		return &s.Production
	}
	return &s.ReadOnly
}

func update(out io.Writer, f func(*config.SafetyConfig)) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	f(&cfg.Safety)
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "production: %s\nread-only:  %s\n", list(cfg.Safety.Production), list(cfg.Safety.ReadOnly))
	return nil
}

func list(l []string) string {
	if len(l) == 0 {
		return "(none)"
	}
	return strings.Join(l, ", ")
}

// --- history ---------------------------------------------------------------------

// HistoryCommand returns "kdeck history".
func HistoryCommand(out io.Writer) *cobra.Command {
	var n int
	var undoOnly bool
	cmd := &cobra.Command{
		Use:     "history [REGEX]",
		Aliases: []string{"hist"},
		Short:   "Changes and execs you ran through kdeck: when, where, exit code, and how to undo",
		Example: `  kdeck history                 # last 30
  kdeck history prod --limit 100   # regex over context and command
  kdeck history --undo          # only entries with an undo command`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := guard.History()
			if err != nil {
				return err
			}
			pattern := ""
			if len(args) == 1 {
				pattern = args[0]
			}
			m, err := match.Compile(pattern)
			if err != nil {
				return err
			}
			var shown []guard.Entry
			for _, e := range entries {
				if !m.Any(e.Context, strings.Join(e.Command, " ")) || (undoOnly && e.Undo == "") {
					continue
				}
				shown = append(shown, e)
			}
			if len(shown) > n {
				shown = shown[len(shown)-n:]
			}
			if len(shown) == 0 {
				fmt.Fprintln(out, "No matching history.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TIME\tCONTEXT\tKIND\tEXIT\tCOMMAND\tUNDO")
			for _, e := range shown {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\tkp %s\t%s\n", e.Time.Local().Format("2006-01-02 15:04"), e.Context, e.Kind, e.Exit, strings.Join(e.Command, " "), dash(e.Undo))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&n, "limit", 30, "how many entries")
	cmd.Flags().BoolVar(&undoOnly, "undo", false, "only entries with an undo command")
	return cmd
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// --- here: a context per terminal -------------------------------------------------

func listSep() string {
	if runtime.GOOS == "windows" {
		return ";"
	}
	return ":"
}

func sessionsDir() string {
	d := config.Path("sessions")
	_ = os.MkdirAll(d, 0o700)
	return d
}

func gcSessions() {
	entries, _ := os.ReadDir(sessionsDir())
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
			_ = os.Remove(filepath.Join(sessionsDir(), e.Name()))
		}
	}
}

// baseKubeconfigs is the KUBECONFIG list without kdeck session files.
func baseKubeconfigs() []string {
	var out []string
	for _, p := range filepath.SplitList(os.Getenv("KUBECONFIG")) {
		if p != "" && !strings.HasPrefix(p, sessionsDir()) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = []string{clientcmd.RecommendedHomeFile}
	}
	return out
}

// EnvLine renders "set KUBECONFIG to value" for a shell; value "" unsets.
func EnvLine(shell, value string) string {
	q := strings.ReplaceAll(value, "'", "'\\''")
	switch shell {
	case "fish":
		if value == "" {
			return "set -e KUBECONFIG"
		}
		return "set -gx KUBECONFIG '" + q + "'"
	case "powershell", "pwsh":
		if value == "" {
			return "Remove-Item Env:KUBECONFIG -ErrorAction SilentlyContinue"
		}
		return "$env:KUBECONFIG = '" + strings.ReplaceAll(value, "'", "''") + "'"
	case "cmd":
		return "set KUBECONFIG=" + value
	}
	if value == "" {
		return "unset KUBECONFIG"
	}
	return "export KUBECONFIG='" + q + "'"
}

// HereCommand returns "kdeck here", used by the kh shell function.
func HereCommand(out io.Writer) *cobra.Command {
	var shell, namespace string
	var off bool
	cmd := &cobra.Command{
		Use:   "here [CONTEXT-REGEX]",
		Short: "Use a context in this terminal only (other terminals keep theirs); used via the kh shell function",
		Long: `Switch the context for the current terminal only. kdeck writes a small session
kubeconfig and prints the line that points KUBECONFIG at it; the kh function
from "kdeck shell-init" evaluates that line for you:

  kh prod            # this terminal on prod; every other terminal unchanged
  kh staging -n web  # with a namespace
  kh --off           # back to the shared kubeconfig

Without kh: eval "$(kdeck here prod --shell zsh)"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			gcSessions()
			base := baseKubeconfigs()
			if off {
				if os.Getenv("KUBECONFIG") == "" || strings.Join(base, listSep()) == clientcmd.RecommendedHomeFile {
					fmt.Fprintln(out, EnvLine(shell, ""))
				} else {
					fmt.Fprintln(out, EnvLine(shell, strings.Join(base, listSep())))
				}
				return nil
			}
			if len(args) == 0 {
				return fmt.Errorf("give a context (regex), or --off")
			}
			rules := &clientcmd.ClientConfigLoadingRules{Precedence: base}
			merged, err := rules.Load()
			if err != nil {
				return err
			}
			name, err := resolveContext(merged, args[0])
			if err != nil {
				return err
			}
			c := merged.Contexts[name].DeepCopy()
			if namespace != "" {
				c.Namespace = namespace
			}
			session := api.NewConfig()
			session.CurrentContext = name
			session.Contexts[name] = c
			b := make([]byte, 4)
			_, _ = rand.Read(b)
			path := filepath.Join(sessionsDir(), fmt.Sprintf("%s-%s.yaml", sanitize(name), hex.EncodeToString(b)))
			if err := clientcmd.WriteToFile(*session, path); err != nil {
				return err
			}
			fmt.Fprintln(out, EnvLine(shell, strings.Join(append([]string{path}, base...), listSep())))
			fmt.Fprintf(os.Stderr, "This terminal now uses context %q (namespace %q).\n", name, nsOr(c.Namespace))
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", defaultShell(), "bash, zsh, fish, powershell or cmd")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace for this terminal")
	cmd.Flags().BoolVar(&off, "off", false, "go back to the shared kubeconfig")
	return cmd
}

func defaultShell() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	switch filepath.Base(os.Getenv("SHELL")) {
	case "fish":
		return "fish"
	case "zsh":
		return "zsh"
	}
	return "bash"
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == ' ' || r == '@' {
			return '_'
		}
		return r
	}, s)
}

func nsOr(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}

func resolveContext(cfg *api.Config, pattern string) (string, error) {
	if _, ok := cfg.Contexts[pattern]; ok {
		return pattern, nil
	}
	m, err := match.Compile(pattern)
	if err != nil {
		return "", err
	}
	var hits []string
	for n := range cfg.Contexts {
		if m.Match(n) {
			hits = append(hits, n)
		}
	}
	sort.Strings(hits)
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no context matches %q", pattern)
	case 1:
		return hits[0], nil
	}
	return "", fmt.Errorf("%d contexts match %q: %s", len(hits), pattern, strings.Join(hits, ", "))
}

// --- prompt -----------------------------------------------------------------------

// Prompt renders the prompt segment without contacting the API (fast).
func Prompt(format string, color bool) string {
	raw, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil || raw.CurrentContext == "" {
		return ""
	}
	ctx := raw.CurrentContext
	ns := "default"
	if c := raw.Contexts[ctx]; c != nil && c.Namespace != "" {
		ns = c.Namespace
	}
	s := strings.NewReplacer("%c", ctx, "%n", ns).Replace(format)
	if !color {
		return s
	}
	cfg, _ := config.Load()
	if cfg != nil {
		p := guard.Resolve(cfg.Safety, ctx, false)
		switch {
		case p.ReadOnly:
			return "\x1b[34m" + s + "\x1b[0m"
		case p.Production:
			return "\x1b[1;31m" + s + "\x1b[0m"
		}
	}
	return "\x1b[32m" + s + "\x1b[0m"
}

// PromptCommand returns "kdeck prompt" (documentation; the fast path in the app
// answers before the command tree is built).
func PromptCommand(out io.Writer) *cobra.Command {
	var format string
	var noColor bool
	cmd := &cobra.Command{
		Use:   "prompt",
		Short: "Print context/namespace for your shell prompt (red on production, blue when read-only)",
		Long: `Print the current context and namespace for a shell prompt. It reads only the
kubeconfig, never the API, so it is fast.

  zsh:        setopt PROMPT_SUBST; RPROMPT='$(kdeck prompt)'
  bash:       PS1='$(kdeck prompt) \w \$ '
  PowerShell: function prompt { "$(kdeck prompt --no-color) PS $($PWD)> " }
  starship:   [custom.kdeck] command = "kdeck prompt --no-color", when = true`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(out, Prompt(format, !noColor))
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "%c/%n", "%c = context, %n = namespace")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "plain text")
	return cmd
}

// --- profiles -----------------------------------------------------------------------

// ProfileCommand returns "kdeck profile".
func ProfileCommand(flags *genericclioptions.ConfigFlags, out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "profile [NAME]",
		Aliases: []string{"prof"},
		Short:   "Switch Teleport cluster, context and namespace in one step",
		Example: `  kdeck profile save payments --teleport prod-us   # from the current context and namespace
  kdeck profile payments                           # tsh kube login prod-us, context, namespace
  kdeck profile ls
  kdeck profile rm payments`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				return listProfiles(out, cfg)
			}
			p, ok := cfg.Profiles[args[0]]
			if !ok {
				return fmt.Errorf("no profile %q (see: kdeck profile ls)", args[0])
			}
			if p.Teleport != "" {
				if err := teleport.Use(out, p.Teleport); err != nil {
					return err
				}
			}
			access := flags.ToRawKubeConfigLoader().ConfigAccess()
			raw, err := access.GetStartingConfig()
			if err != nil {
				return err
			}
			if p.Context != "" {
				if _, ok := raw.Contexts[p.Context]; !ok {
					return fmt.Errorf("profile %q: context %q not in kubeconfig", args[0], p.Context)
				}
				raw.CurrentContext = p.Context
			}
			if p.Namespace != "" && raw.Contexts[raw.CurrentContext] != nil {
				raw.Contexts[raw.CurrentContext].Namespace = p.Namespace
			}
			if err := clientcmd.ModifyConfig(access, *raw, true); err != nil {
				return err
			}
			fmt.Fprintf(out, "Profile %q: context %q, namespace %q.\n", args[0], raw.CurrentContext, nsOr(p.Namespace))
			return nil
		},
	}
	var tp string
	save := &cobra.Command{
		Use: "save NAME", Short: "Save the current context and namespace as a profile", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			raw, err := flags.ToRawKubeConfigLoader().RawConfig()
			if err != nil {
				return err
			}
			ns := ""
			if c := raw.Contexts[raw.CurrentContext]; c != nil {
				ns = c.Namespace
			}
			if cfg.Profiles == nil {
				cfg.Profiles = map[string]config.Profile{}
			}
			cfg.Profiles[args[0]] = config.Profile{Teleport: tp, Context: raw.CurrentContext, Namespace: ns}
			if err := cfg.Save(); err != nil {
				return err
			}
			return listProfiles(out, cfg)
		},
	}
	save.Flags().StringVar(&tp, "teleport", "", "Teleport kube cluster (regex) to log in to first")
	cmd.AddCommand(save, &cobra.Command{
		Use: "ls", Aliases: []string{"list"}, Short: "List profiles",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return listProfiles(out, cfg)
		},
	}, &cobra.Command{
		Use: "rm NAME", Short: "Delete a profile", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			delete(cfg.Profiles, args[0])
			return cfg.Save()
		},
	})
	return cmd
}

func listProfiles(out io.Writer, cfg *config.Config) error {
	if len(cfg.Profiles) == 0 {
		fmt.Fprintln(out, "No profiles. Create one: kdeck profile save NAME [--teleport CLUSTER]")
		return nil
	}
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROFILE\tTELEPORT\tCONTEXT\tNAMESPACE")
	for _, n := range names {
		p := cfg.Profiles[n]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, dash(p.Teleport), dash(p.Context), nsOr(p.Namespace))
	}
	return tw.Flush()
}

// Package shell provides "kp shell-init", "kp alias" and "kp shortcuts":
// everything that makes kp quicker to type.
package shell

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lyelgate/kubepilot/internal/alias"
	"github.com/lyelgate/kubepilot/internal/config"
	"github.com/lyelgate/kubepilot/internal/match"
	"github.com/lyelgate/kubepilot/internal/quick"
)

// InitCommand returns "kp shell-init".
func InitCommand(out io.Writer) *cobra.Command {
	var all bool
	var name string
	cmd := &cobra.Command{
		Use:   "shell-init bash|zsh|fish|powershell|cmd",
		Short: "Print shell aliases (k, kgpo, kdpo, ...) and completion setup to eval in your shell rc",
		Long: `Print shell setup: an alias for kp itself (k by default), shell aliases for
the common kubectl-aliases combinations (kgpo, kgdep, kdpo, krmpo, ...), short
helpers (kctx, kns, kbad), and completion for kp and its alias.

Add one line to your shell rc file:

  bash:  echo 'eval "$(kp shell-init bash)"' >> ~/.bashrc
  zsh:   echo 'eval "$(kp shell-init zsh)"'  >> ~/.zshrc
  fish:  echo 'kp shell-init fish | source'  >> ~/.config/fish/config.fish
  PowerShell (Windows, macOS, Linux):
         Add-Content $PROFILE 'kp shell-init powershell | Out-String | Invoke-Expression'
  cmd.exe:
         kp shell-init cmd > "%USERPROFILE%\kp-macros.cmd"   (instructions inside the file)

WSL uses the Linux binary with bash or zsh.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell", "cmd"},
		RunE: func(cmd *cobra.Command, args []string) error {
			sh := args[0]
			if sh == "pwsh" {
				sh = "powershell"
			}
			switch sh {
			case "bash", "zsh", "fish", "powershell", "cmd":
			default:
				return fmt.Errorf("unsupported shell %q (bash, zsh, fish, powershell, cmd)", sh)
			}
			words := alias.Curated
			if all {
				words = alias.All()
			}
			comment := "#"
			if sh == "cmd" {
				comment = "@rem"
				fmt.Fprintln(out, "@echo off")
			}
			fmt.Fprintf(out, "%s kubepilot shell setup (%s)\n", comment, sh)
			if sh == "cmd" {
				fmt.Fprintln(out, "@rem Load in every cmd.exe window by registering this file as AutoRun:")
				_, _ = io.WriteString(out, `@rem   reg add "HKCU\Software\Microsoft\Command Processor" /v AutoRun /t REG_EXPAND_SZ /d "%USERPROFILE%\kp-macros.cmd" /f`+"\n")
			}
			emit := func(n, expansion string) {
				switch sh {
				case "fish":
					fmt.Fprintf(out, "alias %s '%s'\n", n, expansion)
				case "powershell":
					// PowerShell aliases cannot carry arguments; functions can.
					fmt.Fprintf(out, "function %s { %s @args }\n", n, expansion)
				case "cmd":
					fmt.Fprintf(out, "doskey %s=%s $*\n", n, expansion)
				default:
					fmt.Fprintf(out, "alias %s='%s'\n", n, expansion)
				}
			}
			emit(name, "kp")
			for _, n := range quick.KPNames {
				emit(n, "kp "+n)
			}
			for _, h := range [][2]string{
				{"kctx", "kp ctx"}, {"kns", "kp ns"}, {"kwho", "kp who"}, {"kbad", "kp bad"},
				{"ksf", "kp silent"}, {"ksh", "kp sh"}, {"ktail", "kp tail"}, {"kneat", "kp neat"},
			} {
				emit(h[0], h[1])
			}
			user := map[string]bool{}
			if cfg, err := config.Load(); err == nil {
				for k := range cfg.Aliases {
					user[k] = true
				}
			}
			for _, w := range words {
				if user[w] {
					continue // user's own alias wins; it is emitted below
				}
				exp, ok := alias.ParseCombo(w)
				if !ok {
					continue
				}
				emit(w, "kp "+strings.Join(exp, " "))
			}
			if cfg, err := config.Load(); err == nil {
				keys := make([]string, 0, len(cfg.Aliases))
				for k := range cfg.Aliases {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					emit(k, "kp "+strings.ReplaceAll(cfg.Aliases[k], "'", ""))
				}
			}
			// kh: switch context for this terminal only (kp here).
			switch sh {
			case "bash", "zsh":
				fmt.Fprintf(out, "kh() { eval \"$(kp here --shell %s \"$@\")\"; }\n", sh)
			case "fish":
				fmt.Fprintln(out, "function kh; kp here --shell fish $argv | source; end")
			case "powershell":
				fmt.Fprintln(out, "function kh { kp here --shell powershell @args | Out-String | Invoke-Expression }")
			case "cmd":
				_, _ = io.WriteString(out, `doskey kh=kp here --shell cmd $* ^> "%TEMP%\kp-here.cmd" $T call "%TEMP%\kp-here.cmd"`+"\n")
			}
			switch sh {
			case "bash":
				fmt.Fprintf(out, "source <(kp completion bash)\ncomplete -o default -F __start_kp %s\n", name)
			case "zsh":
				fmt.Fprintf(out, "source <(kp completion zsh)\ncompdef _kp %s 2>/dev/null\n", name)
			case "fish":
				fmt.Fprintf(out, "kp completion fish | source\ncomplete -c %s -w kp\n", name)
			case "powershell":
				fmt.Fprintln(out, "kp completion powershell | Out-String | Invoke-Expression")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "emit every valid combination (about 800 aliases) instead of the curated set")
	cmd.Flags().StringVar(&name, "name", "k", "short name for kp itself")
	return cmd
}

// AliasCommand returns "kp alias".
func AliasCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "alias",
		Aliases: []string{"aliases", "al"},
		Short:   "List, explain, add and remove shortcuts",
		Example: `  kp alias list                 # verb shortcuts, combos and your aliases
  kp alias explain kgpooyaml    # what a word expands to
  kp alias set kgr 'get pods --field-selector=status.phase=Running'
  kp kgr -A                     # use it
  kp alias rm kgr`,
	}
	var all bool
	list := &cobra.Command{
		Use: "list [REGEX]", Aliases: []string{"ls"}, Short: "List shortcuts, optionally filtered by a regex over name and expansion",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := ""
			if len(args) == 1 {
				pattern = args[0]
			}
			re, err := match.Compile(pattern)
			if err != nil {
				return err
			}
			row := func(tw io.Writer, name, exp, kind string) {
				if re.Any(name, exp) {
					fmt.Fprintf(tw, "%s\tkubectl %s\t%s\n", name, exp, kind)
				}
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SHORTCUT\tEXPANDS TO\tKIND")
			cfg, _ := config.Load()
			if cfg != nil {
				keys := make([]string, 0, len(cfg.Aliases))
				for k := range cfg.Aliases {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					row(tw, k, cfg.Aliases[k], "yours")
				}
			}
			verbs := make([]string, 0, len(alias.Verbs))
			for k := range alias.Verbs {
				verbs = append(verbs, k)
			}
			sort.Strings(verbs)
			for _, k := range verbs {
				row(tw, k, alias.VerbHelp[k], "verb")
			}
			words := alias.Curated
			if all {
				words = alias.All()
			}
			for _, w := range words {
				exp, _ := alias.ParseCombo(w)
				row(tw, w, strings.Join(exp, " "), "combo")
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if !all {
				fmt.Fprintln(out, "\nCombos follow [k][sys]<cmd>[<resource>][<modifiers>]; --all lists every valid word.")
				fmt.Fprintln(out, "cmd: g=get d=describe rm=delete a=apply -f ex=exec -it lo=logs -f lop=logs -f -p p=proxy pf=port-forward")
				fmt.Fprintln(out, "resource: po dep sts ds rs svc ing cm sec no ns job cj pvc pv hpa sa ev ep pdb np crd sc lease")
				fmt.Fprintln(out, "modifiers: oyaml ojson owide a|all w sl, and one of l (-l) n (--namespace) f (-f) taking the next argument")
			}
			return nil
		},
	}
	list.Flags().BoolVar(&all, "all", false, "every valid combination")
	cmd.AddCommand(list)
	cmd.AddCommand(&cobra.Command{
		Use: "explain WORD", Aliases: []string{"x", "what"}, Short: "Show what a word expands to", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := config.Load()
			user := map[string]string{}
			if cfg != nil {
				user = cfg.Aliases
			}
			exp, used := alias.Expand(args, user, nil)
			if used == "" {
				return fmt.Errorf("%q is not a shortcut", args[0])
			}
			fmt.Fprintf(out, "%s -> kubectl %s\n", args[0], strings.Join(exp, " "))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "set NAME EXPANSION", Aliases: []string{"add"}, Short: "Add or replace your own alias", Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cfg.Aliases == nil {
				cfg.Aliases = map[string]string{}
			}
			cfg.Aliases[args[0]] = strings.Join(args[1:], " ")
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s -> kubectl %s\n", args[0], cfg.Aliases[args[0]])
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "rm NAME", Aliases: []string{"remove", "delete"}, Short: "Remove one of your aliases", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if _, ok := cfg.Aliases[args[0]]; !ok {
				return fmt.Errorf("no alias %q", args[0])
			}
			delete(cfg.Aliases, args[0])
			return cfg.Save()
		},
	})
	return cmd
}

// ShortcutsCommand returns "kp shortcuts", which links shortcut names to the
// kp binary so they work without shell aliases (busybox style).
func ShortcutsCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shortcuts",
		Short: "Link shortcut names (k, kgpo, ...) to kp so they work without shell aliases",
	}
	var dir string
	var all bool
	install := &cobra.Command{
		Use: "install", Short: "Create symlinks k, kgpo, kdpo, ... pointing at kp",
		RunE: func(cmd *cobra.Command, _ []string) error {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			if self, err = filepath.EvalSymlinks(self); err != nil {
				return err
			}
			if dir == "" {
				dir = config.Path("shortcuts")
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			words := append(append([]string{"k"}, quick.KPNames...), alias.Curated...)
			if all {
				words = append(append([]string{"k"}, quick.KPNames...), alias.All()...)
			}
			n := 0
			for _, w := range words {
				if runtime.GOOS == "windows" {
					// Symlinks need admin rights on Windows; a .cmd shim works
					// in cmd.exe and PowerShell alike.
					shim := filepath.Join(dir, w+".cmd")
					body := fmt.Sprintf("@echo off\r\n\"%s\" %s %%*\r\n", self, w)
					if w == "k" {
						body = fmt.Sprintf("@echo off\r\n\"%s\" %%*\r\n", self)
					}
					if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
						return err
					}
				} else {
					link := filepath.Join(dir, w)
					_ = os.Remove(link)
					if err := os.Symlink(self, link); err != nil {
						return err
					}
				}
				n++
			}
			fmt.Fprintf(out, "Created %d shortcuts in %s (kplog, kpns, kgpo, ...). Add it to PATH:\n", n, dir)
			if runtime.GOOS == "windows" {
				fmt.Fprintf(out, "  setx PATH \"%s;%%PATH%%\"\n", dir)
			} else {
				fmt.Fprintf(out, "  export PATH=\"%s:$PATH\"\n", dir)
			}
			return nil
		},
	}
	install.Flags().StringVar(&dir, "dir", "", "directory for the links (default ~/.kp/shortcuts)")
	install.Flags().BoolVar(&all, "all", false, "link every valid combination")
	cmd.AddCommand(install)
	return cmd
}

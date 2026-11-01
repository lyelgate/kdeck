// Package shell provides "kdeck shell-init", "kdeck alias" and "kdeck shortcuts":
// everything that makes kdeck quicker to type.
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

	"github.com/lyelgate/kdeck/internal/alias"
	"github.com/lyelgate/kdeck/internal/config"
	"github.com/lyelgate/kdeck/internal/match"
	"github.com/lyelgate/kdeck/internal/quick"
)

// InitCommand returns "kdeck shell-init".
func InitCommand(out io.Writer) *cobra.Command {
	var all bool
	var name string
	cmd := &cobra.Command{
		Use:   "shell-init bash|zsh|fish|powershell|cmd",
		Short: "Print shell aliases (k, kgpo, kdpo, ...) and completion setup to eval in your shell rc",
		Long: `Print shell setup: an alias for kdeck itself (k by default), shell aliases for
the common kubectl-aliases combinations (kgpo, kgdep, kdpo, krmpo, ...), short
helpers (kctx, kns, kbad), and completion for kdeck and its alias.

Add one line to your shell rc file:

  bash:  echo 'eval "$(kdeck shell-init bash)"' >> ~/.bashrc
  zsh:   echo 'eval "$(kdeck shell-init zsh)"'  >> ~/.zshrc
  fish:  echo 'kdeck shell-init fish | source'  >> ~/.config/fish/config.fish
  PowerShell (Windows, macOS, Linux):
         Add-Content $PROFILE 'kdeck shell-init powershell | Out-String | Invoke-Expression'
  cmd.exe:
         kdeck shell-init cmd > "%USERPROFILE%\kdeck-macros.cmd"   (instructions inside the file)

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
			fmt.Fprintf(out, "%s kdeck shell setup (%s)\n", comment, sh)
			if sh == "cmd" {
				fmt.Fprintln(out, "@rem Load in every cmd.exe window by registering this file as AutoRun:")
				_, _ = io.WriteString(out, `@rem   reg add "HKCU\Software\Microsoft\Command Processor" /v AutoRun /t REG_EXPAND_SZ /d "%USERPROFILE%\kdeck-macros.cmd" /f`+"\n")
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
			emit(name, "kdeck")
			for _, n := range quick.DKNames {
				emit(n, "kdeck "+n)
			}
			for _, h := range [][2]string{
				{"kctx", "kdeck ctx"}, {"kns", "kdeck ns"}, {"kwho", "kdeck who"}, {"kbad", "kdeck bad"},
				{"ksf", "kdeck silent"}, {"ksh", "kdeck sh"}, {"ktail", "kdeck tail"}, {"kneat", "kdeck neat"},
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
				emit(w, "kdeck "+strings.Join(exp, " "))
			}
			if cfg, err := config.Load(); err == nil {
				keys := make([]string, 0, len(cfg.Aliases))
				for k := range cfg.Aliases {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					emit(k, "kdeck "+strings.ReplaceAll(cfg.Aliases[k], "'", ""))
				}
			}
			// kh: switch context for this terminal only (kdeck here).
			switch sh {
			case "bash", "zsh":
				fmt.Fprintf(out, "kh() { eval \"$(kdeck here --shell %s \"$@\")\"; }\n", sh)
			case "fish":
				fmt.Fprintln(out, "function kh; kdeck here --shell fish $argv | source; end")
			case "powershell":
				fmt.Fprintln(out, "function kh { kdeck here --shell powershell @args | Out-String | Invoke-Expression }")
			case "cmd":
				_, _ = io.WriteString(out, `doskey kh=kdeck here --shell cmd $* ^> "%TEMP%\kdeck-here.cmd" $T call "%TEMP%\kdeck-here.cmd"`+"\n")
			}
			switch sh {
			case "bash":
				fmt.Fprintf(out, "source <(kdeck completion bash)\ncomplete -o default -F __start_kdeck %s\n", name)
			case "zsh":
				fmt.Fprintf(out, "source <(kdeck completion zsh)\ncompdef _kdeck %s 2>/dev/null\n", name)
			case "fish":
				fmt.Fprintf(out, "kdeck completion fish | source\ncomplete -c %s -w kdeck\n", name)
			case "powershell":
				fmt.Fprintln(out, "kdeck completion powershell | Out-String | Invoke-Expression")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "emit every valid combination (about 800 aliases) instead of the curated set")
	cmd.Flags().StringVar(&name, "name", "k", "short name for kdeck itself")
	return cmd
}

// AliasCommand returns "kdeck alias".
func AliasCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "alias",
		Aliases: []string{"aliases", "al"},
		Short:   "List, explain, add and remove shortcuts",
		Example: `  kdeck alias list                 # verb shortcuts, combos and your aliases
  kdeck alias explain kgpooyaml    # what a word expands to
  kdeck alias set kgr 'get pods --field-selector=status.phase=Running'
  kdeck kgr -A                     # use it
  kdeck alias rm kgr`,
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

// ShortcutsCommand returns "kdeck shortcuts", which links shortcut names to the
// kdeck binary so they work without shell aliases (busybox style).
func ShortcutsCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shortcuts",
		Short: "Link shortcut names (k, kgpo, ...) to kdeck so they work without shell aliases",
	}
	var dir string
	var all bool
	install := &cobra.Command{
		Use: "install", Short: "Create symlinks k, kgpo, kdpo, ... pointing at kdeck",
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
			words := append(append([]string{"k"}, quick.DKNames...), alias.Curated...)
			if all {
				words = append(append([]string{"k"}, quick.DKNames...), alias.All()...)
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
			fmt.Fprintf(out, "Created %d shortcuts in %s (dklog, dkns, kgpo, ...). Add it to PATH:\n", n, dir)
			if runtime.GOOS == "windows" {
				fmt.Fprintf(out, "  setx PATH \"%s;%%PATH%%\"\n", dir)
			} else {
				fmt.Fprintf(out, "  export PATH=\"%s:$PATH\"\n", dir)
			}
			return nil
		},
	}
	install.Flags().StringVar(&dir, "dir", "", "directory for the links (default ~/.kdeck/shortcuts)")
	install.Flags().BoolVar(&all, "all", false, "link every valid combination")
	cmd.AddCommand(install)
	return cmd
}

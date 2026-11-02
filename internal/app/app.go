// Package app assembles kdeck: the full kubectl command tree, embedded as a
// library, plus kdeck's own commands, shortcut expansion and plugin dispatch.
package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/component-base/cli"
	kubectlcmd "k8s.io/kubectl/pkg/cmd"
	kubectlplugin "k8s.io/kubectl/pkg/cmd/plugin"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/lyelgate/kdeck/internal/alias"
	"github.com/lyelgate/kdeck/internal/cloud"
	"github.com/lyelgate/kdeck/internal/config"
	"github.com/lyelgate/kdeck/internal/cost"
	"github.com/lyelgate/kdeck/internal/guard"
	"github.com/lyelgate/kdeck/internal/kctx"
	"github.com/lyelgate/kdeck/internal/kubeconfig"
	"github.com/lyelgate/kdeck/internal/linode"
	"github.com/lyelgate/kdeck/internal/plugin"
	"github.com/lyelgate/kdeck/internal/quick"
	"github.com/lyelgate/kdeck/internal/selfupdate"
	"github.com/lyelgate/kdeck/internal/setup"
	"github.com/lyelgate/kdeck/internal/shell"
	"github.com/lyelgate/kdeck/internal/silent"
	"github.com/lyelgate/kdeck/internal/teleport"
	"github.com/lyelgate/kdeck/internal/workspace"
)

// Version is set at build time with -ldflags "-X .../internal/app.Version=v0.1.0".
var Version = "dev"

// names under which the binary behaves as plain kdeck (anything else is
// treated as a shortcut name, busybox style: a symlink named kgpo runs "kdeck kgpo").
var selfNames = map[string]bool{"kdeck": true, "k": true, "kubectl": true, "kubectl-kdeck": true}

// Run executes kdeck with the process arguments and returns the exit code.
func Run(argv []string) int {
	// Per-cluster kubeconfigs in ~/.kdeck/kubeconfigs join KUBECONFIG for
	// this process and everything it starts.
	kubeconfig.Activate()
	base := strings.TrimSuffix(filepath.Base(argv[0]), ".exe")
	args := append([]string{}, argv[1:]...)
	if !selfNames[base] && !strings.HasPrefix(base, "kdeck-") && !strings.HasPrefix(base, "kdeck.") {
		args = append([]string{base}, args...)
	}

	if len(args) > 0 && args[0] == "prompt" && !hasArg(args, "--help", "-h") {
		format, color := "%c/%n", true
		for i := 1; i < len(args); i++ {
			switch {
			case args[i] == "--no-color":
				color = false
			case args[i] == "--format" && i+1 < len(args):
				format = args[i+1]
				i++
			case strings.HasPrefix(args[i], "--format="):
				format = strings.TrimPrefix(args[i], "--format=")
			}
		}
		fmt.Println(workspace.Prompt(format, color))
		return 0
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: ignoring %s: %v\n", config.Path("config.yaml"), err)
		cfg = &config.Config{}
	}
	addToPath(plugin.BinDirs()...)

	kf, args := extractKPFlags(args)

	// Expand shortcuts against the real command names, then hand the
	// expanded arguments to kubectl's constructor (kubectl's own kuberc
	// aliases are applied there, after ours).
	names := commandNames()
	expanded, used := alias.Expand(args, cfg.Aliases, func(w string) bool { return names[w] })
	if used != "" && os.Getenv("KDECK_DEBUG_ALIAS") != "" {
		fmt.Fprintf(os.Stderr, "kdeck: %s -> %s\n", used, strings.Join(expanded, " "))
	}
	if kf.contexts != "" && os.Getenv("KDECK_MULTI_CHILD") == "" {
		return runMulti(cfg, kf, expanded)
	}
	entry, gerr := guardCommand(cfg, kf, expanded)
	if gerr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", gerr)
		return 1
	}
	record := func(code int) {
		if entry != nil {
			entry.Exit = code
			guard.Record(*entry)
		}
	}
	// kubectl commands exit through this hook on fatal errors; record first.
	cmdutil.BehaviorOnFatal(func(msg string, code int) {
		record(code)
		if len(msg) > 0 {
			if !strings.HasSuffix(msg, "\n") {
				msg += "\n"
			}
			fmt.Fprint(os.Stderr, msg)
		}
		os.Exit(code)
	})
	os.Args = append([]string{argv[0]}, expanded...)

	root, streams, flags := build(os.Args)
	handler := kubectlcmd.NewDefaultPluginHandler(kubectlplugin.ValidPluginFilenamePrefixes)

	// Plugins run only when no built-in or kdeck command matches, so a plugin
	// can never shadow a kdeck command.
	if len(expanded) > 0 {
		if _, _, err := root.Find(expanded); err != nil {
			switch firstWord(expanded) {
			case "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			default:
				if err := kubectlcmd.HandlePluginCommand(handler, expanded, 1); err != nil {
					fmt.Fprintf(streams.ErrOut, "Error: %v\n", err)
					return 1
				}
			}
		}
	}
	_ = flags

	if err := cli.RunNoErrOutput(root); err != nil {
		var exit silent.ExitError
		if errors.As(err, &exit) {
			record(exit.Code)
			return exit.Code
		}
		cmdutil.CheckErr(err) // prints kubectl-style and exits via the hook above
		return 1
	}
	record(0)
	return 0
}

func hasArg(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

func firstWord(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// commandNames builds a throwaway tree to learn every command name and alias.
func commandNames() map[string]bool {
	root, _, _ := build([]string{"kdeck"})
	names := map[string]bool{"help": true, "completion": true, cobra.ShellCompRequestCmd: true, cobra.ShellCompNoDescRequestCmd: true}
	for _, c := range root.Commands() {
		names[c.Name()] = true
		for _, a := range c.Aliases {
			names[a] = true
		}
	}
	return names
}

func build(arguments []string) (*cobra.Command, genericiooptions.IOStreams, *genericclioptions.ConfigFlags) {
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
	flags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag().WithDiscoveryBurst(300).WithDiscoveryQPS(50.0).WithWarningPrinter(streams)
	root := kubectlcmd.NewKubectlCommand(kubectlcmd.KubectlOptions{
		PluginHandler: kubectlcmd.NewDefaultPluginHandler(kubectlplugin.ValidPluginFilenamePrefixes),
		Arguments:     arguments,
		ConfigFlags:   flags,
		IOStreams:     streams,
	})
	root.Use = "kdeck"
	root.Short = "kdeck: kubectl plus shortcuts, silent-failure checks, plugins and Linode LKE in one binary"
	root.Long = `kdeck is a single binary for running Kubernetes end to end.

  * Every kubectl command, built in and identical: kdeck get pods, kdeck apply -f x.yaml
  * Shortcuts: kdeck kgpo, kdeck kdpol app=web, kdeck l pod, kdeck rr deploy/api
    (kdeck alias reference lists every one with its full command; kdeck help kgpo explains one)
  * Context and namespace switching: kdeck ctx, kdeck ns, with "-" for previous
  * Everyday helpers: kdeck pods, kdeck bad, kdeck sh, kdeck tail, kdeck neat, kdeck nodes, kdeck clean
  * Silent-failure detection: kdeck silent (backups, certs, leases, webhooks, PDBs, ...)
  * Plugins from the krew index without krew: kdeck plugin install view-secret
  * Linode LKE: kdeck linode lke ls, kdeck linode lke kubeconfig ID --merge

Run "kdeck shell-init zsh" to get short aliases (k, kgpo, ...) and completion.`

	// Replace kubectl's "plugin" command (list only) with kdeck's manager.
	for _, c := range root.Commands() {
		if c.Name() == "plugin" {
			root.RemoveCommand(c)
		}
	}
	root.AddGroup(
		&cobra.Group{ID: "kdeck", Title: "kdeck commands:"},
		&cobra.Group{ID: "shortcuts", Title: "kdeck shortcuts:"},
	)
	out := streams.Out
	run := func(w io.Writer, args ...string) error { return runKubectl(w, args...) }
	kpCmds := []*cobra.Command{
		kctx.NewCtxCommand(flags, out),
		kctx.NewNsCommand(flags, out),
		silent.NewCommand(flags, out, Version),
		plugin.NewCommand(out),
		linode.NewCommand(flags, out),
		teleport.NewCommand(out),
		workspace.SafetyCommand(flags, out),
		workspace.HistoryCommand(out),
		workspace.HereCommand(out),
		workspace.KubeconfigCommand(flags, out),
		workspace.PromptCommand(out),
		workspace.ProfileCommand(flags, out),
		workspace.CompareCommand(flags, out),
		workspace.SettingsCommand(out),
		selfupdate.NewCommand(out, Version),
		setup.NewSetupCommand(out),
		setup.NewDoctorCommand(flags, out, Version, kubectlVersion()),
		shell.AliasCommand(out),
		shell.InitCommand(out),
		shell.ShortcutsCommand(out),
		aboutCommand(out),
	}
	kpCmds = append(kpCmds, cloud.NewCommands(flags, out)...)
	kpCmds = append(kpCmds, cost.NewCommand(flags, out, linode.CostPricer))
	for _, c := range kpCmds {
		c.GroupID = "kdeck"
		root.AddCommand(c)
	}
	deps := quick.Deps{Flags: flags, Out: out, ErrOut: streams.ErrOut, Run: run}
	root.AddCommand(quick.Commands(deps)...)
	root.AddCommand(quick.TroubleCommands(deps)...)
	installHelp(root, kpCmds)
	return root, streams, flags
}

// runKubectl runs a fresh embedded kubectl command tree with args, writing
// standard output to out. Used by shortcuts that delegate to kubectl.
func runKubectl(out io.Writer, args ...string) error {
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: out, ErrOut: os.Stderr}
	flags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag().WithDiscoveryBurst(300).WithDiscoveryQPS(50.0).WithWarningPrinter(streams)
	// Carry over global flags the user gave kdeck (--context, -n, --kubeconfig).
	full := append(globalFlags(os.Args[1:]), args...)
	cmd := kubectlcmd.NewKubectlCommand(kubectlcmd.KubectlOptions{
		PluginHandler: kubectlcmd.NewDefaultPluginHandler(kubectlplugin.ValidPluginFilenamePrefixes),
		Arguments:     append([]string{"kdeck"}, full...),
		ConfigFlags:   flags,
		IOStreams:     streams,
	})
	cmd.SetArgs(full)
	cmd.SetOut(out)
	return cmd.Execute()
}

// globalFlags extracts connection flags from the original command line.
func globalFlags(args []string) []string {
	var out []string
	valued := map[string]bool{"--context": true, "--kubeconfig": true, "-n": true, "--namespace": true, "--cluster": true, "--user": true, "-s": true, "--server": true, "--token": true, "--as": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, hasEq := strings.Cut(a, "=")
		if !valued[name] {
			continue
		}
		if hasEq {
			out = append(out, a)
		} else if i+1 < len(args) {
			out = append(out, a, args[i+1])
			i++
		}
	}
	return out
}

func addToPath(dirs ...string) {
	parts := filepath.SplitList(os.Getenv("PATH"))
	have := map[string]bool{}
	for _, p := range parts {
		have[p] = true
	}
	var add []string
	for _, d := range dirs {
		if !have[d] {
			add = append(add, d)
		}
	}
	if len(add) > 0 {
		_ = os.Setenv("PATH", strings.Join(append(add, parts...), string(os.PathListSeparator)))
	}
}

func aboutCommand(out io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "about",
		Short: "kdeck version, embedded kubectl version, and where kdeck keeps its files",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(out, "kdeck %s (%s/%s, %s)\n", Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
			fmt.Fprintf(out, "embedded kubectl: %s\n", kubectlVersion())
			fmt.Fprintf(out, "home: %s\nplugins on PATH: %s\n", config.Home(), strings.Join(plugin.BinDirs(), ", "))
		},
	}
}

// installHelp appends kdeck's commands to kubectl's help output, which uses its
// own templates and would otherwise list only kubectl's command groups.
func installHelp(root *cobra.Command, kdeck []*cobra.Command) {
	orig := root.HelpFunc()
	root.SetHelpFunc(func(c *cobra.Command, args []string) {
		orig(c, args)
		if c != root {
			return
		}
		w := c.OutOrStdout()
		fmt.Fprintln(w, "\nkdeck commands:")
		for _, k := range kdeck {
			fmt.Fprintf(w, "  %-14s %s\n", k.Name(), k.Short)
		}
		fmt.Fprintln(w, "\nkdeck shortcuts:")
		for _, k := range root.Commands() {
			if k.GroupID == "shortcuts" {
				fmt.Fprintf(w, "  %-14s %s\n", k.Name(), k.Short)
			}
		}
		fmt.Fprintln(w, "\nShortcuts: kdeck alias reference (every shortcut and its full command), kdeck help WORD")
		fmt.Fprintln(w, "Shell setup: kdeck shell-init zsh")
	})
	installHelpCommand(root)
}

// installHelpCommand replaces cobra's help command so that "kdeck help kgpo"
// explains a shortcut (what it expands to) and then shows the help of the
// command it runs. Real commands keep their normal help.
func installHelpCommand(root *cobra.Command) {
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command | shortcut]",
		Short: "Help about any command, or what a shortcut expands to",
		Long:  "Help about any command. For a shortcut (kgpo, rr, dklog, your own aliases) it prints the full command first, then that command's help.",
		ValidArgsFunction: func(c *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			var names []string
			for _, sub := range root.Commands() {
				if sub.IsAvailableCommand() && strings.HasPrefix(sub.Name(), toComplete) {
					names = append(names, sub.Name())
				}
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		},
		Run: func(c *cobra.Command, args []string) {
			w := c.OutOrStdout()
			target, rest, err := root.Find(args)
			if len(args) > 0 && (err != nil || target == root) {
				cfg, _ := config.Load()
				user := map[string]string{}
				if cfg != nil {
					user = cfg.Aliases
				}
				exp, used := alias.Expand(args, user, nil)
				if used == "" {
					fmt.Fprintf(w, "Unknown help topic %q. Run \"kdeck alias reference\" for every shortcut.\n", strings.Join(args, " "))
					return
				}
				prog := "kubectl"
				if t, _, ferr := root.Find(exp); ferr == nil && t != root && isKdeckCommand(t) {
					prog = "kdeck"
				}
				fmt.Fprintf(w, "%s = %s %s\n\n", used, prog, strings.Join(exp, " "))
				target, rest, err = root.Find(exp)
			}
			_ = rest
			if err == nil && target != nil && target != root && len(args) > 0 && args[len(args)-1] != target.Name() && target.HasAlias(args[len(args)-1]) {
				prog := "kubectl"
				if isKdeckCommand(target) {
					prog = "kdeck"
				}
				fmt.Fprintf(w, "%s = %s %s\n\n", strings.Join(args, " "), prog, strings.TrimPrefix(target.CommandPath(), root.Name()+" "))
			}
			if target == nil || err != nil {
				_ = root.Usage()
				return
			}
			target.InitDefaultHelpFlag()
			target.InitDefaultVersionFlag()
			_ = target.Help()
		},
	})
}

// isKdeckCommand reports whether c belongs to kdeck rather than kubectl.
func isKdeckCommand(c *cobra.Command) bool {
	for ; c != nil && c.HasParent(); c = c.Parent() {
		if c.GroupID == "kdeck" || c.GroupID == "shortcuts" {
			return true
		}
	}
	return false
}

// Package app assembles kp: the full kubectl command tree, embedded as a
// library, plus kp's own commands, shortcut expansion and plugin dispatch.
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

	"github.com/lyelgate/kubepilot/internal/alias"
	"github.com/lyelgate/kubepilot/internal/cloud"
	"github.com/lyelgate/kubepilot/internal/config"
	"github.com/lyelgate/kubepilot/internal/kctx"
	"github.com/lyelgate/kubepilot/internal/linode"
	"github.com/lyelgate/kubepilot/internal/plugin"
	"github.com/lyelgate/kubepilot/internal/quick"
	"github.com/lyelgate/kubepilot/internal/setup"
	"github.com/lyelgate/kubepilot/internal/shell"
	"github.com/lyelgate/kubepilot/internal/silent"
	"github.com/lyelgate/kubepilot/internal/teleport"
)

// Version is set at build time with -ldflags "-X .../internal/app.Version=v0.1.0".
var Version = "dev"

// names under which the binary behaves as plain kp (anything else is
// treated as a shortcut name, busybox style: a symlink named kgpo runs "kp kgpo").
var selfNames = map[string]bool{"kp": true, "kubepilot": true, "k": true, "kubectl": true}

// Run executes kp with the process arguments and returns the exit code.
func Run(argv []string) int {
	base := strings.TrimSuffix(filepath.Base(argv[0]), ".exe")
	args := append([]string{}, argv[1:]...)
	if !selfNames[base] && !strings.HasPrefix(base, "kp-") && !strings.HasPrefix(base, "kp.") {
		args = append([]string{base}, args...)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: ignoring %s: %v\n", config.Path("config.yaml"), err)
		cfg = &config.Config{}
	}
	addToPath(plugin.BinDirs()...)

	// Expand shortcuts against the real command names, then hand the
	// expanded arguments to kubectl's constructor (kubectl's own kuberc
	// aliases are applied there, after ours).
	names := commandNames()
	expanded, used := alias.Expand(args, cfg.Aliases, func(w string) bool { return names[w] })
	if used != "" && os.Getenv("KP_DEBUG_ALIAS") != "" {
		fmt.Fprintf(os.Stderr, "kp: %s -> %s\n", used, strings.Join(expanded, " "))
	}
	os.Args = append([]string{argv[0]}, expanded...)

	root, streams, flags := build(os.Args)
	handler := kubectlcmd.NewDefaultPluginHandler(kubectlplugin.ValidPluginFilenamePrefixes)

	// Plugins run only when no built-in or kp command matches, so a plugin
	// can never shadow a kp command.
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
			return exit.Code
		}
		cmdutil.CheckErr(err) // prints kubectl-style and exits
		return 1
	}
	return 0
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
	root, _, _ := build([]string{"kp"})
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
	root.Use = "kp"
	root.Short = "kubepilot: kubectl plus shortcuts, silent-failure checks, plugins and Linode LKE in one binary"
	root.Long = `kubepilot (kp) is a single binary for running Kubernetes end to end.

  * Every kubectl command, built in and identical: kp get pods, kp apply -f x.yaml
  * Shortcuts: kp kgpo, kp kdpol app=web, kp l pod, kp rr deploy/api (kp alias list)
  * Context and namespace switching: kp ctx, kp ns, with "-" for previous
  * Everyday helpers: kp pods, kp bad, kp sh, kp tail, kp neat, kp nodes, kp clean
  * Silent-failure detection: kp silent (backups, certs, leases, webhooks, PDBs, ...)
  * Plugins from the krew index without krew: kp plugin install view-secret
  * Linode LKE: kp linode lke ls, kp linode lke kubeconfig ID --merge

Run "kp shell-init zsh" to get short aliases (k, kgpo, ...) and completion.`

	// Replace kubectl's "plugin" command (list only) with kp's manager.
	for _, c := range root.Commands() {
		if c.Name() == "plugin" {
			root.RemoveCommand(c)
		}
	}
	root.AddGroup(
		&cobra.Group{ID: "kp", Title: "kubepilot commands:"},
		&cobra.Group{ID: "shortcuts", Title: "kubepilot shortcuts:"},
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
		setup.NewSetupCommand(out),
		setup.NewDoctorCommand(flags, out, Version, kubectlVersion()),
		shell.AliasCommand(out),
		shell.InitCommand(out),
		shell.ShortcutsCommand(out),
		aboutCommand(out),
	}
	kpCmds = append(kpCmds, cloud.NewCommands(flags, out)...)
	for _, c := range kpCmds {
		c.GroupID = "kp"
		root.AddCommand(c)
	}
	root.AddCommand(quick.Commands(quick.Deps{Flags: flags, Out: out, ErrOut: streams.ErrOut, Run: run})...)
	installHelp(root, kpCmds)
	return root, streams, flags
}

// runKubectl runs a fresh embedded kubectl command tree with args, writing
// standard output to out. Used by shortcuts that delegate to kubectl.
func runKubectl(out io.Writer, args ...string) error {
	streams := genericiooptions.IOStreams{In: os.Stdin, Out: out, ErrOut: os.Stderr}
	flags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag().WithDiscoveryBurst(300).WithDiscoveryQPS(50.0).WithWarningPrinter(streams)
	// Carry over global flags the user gave kp (--context, -n, --kubeconfig).
	full := append(globalFlags(os.Args[1:]), args...)
	cmd := kubectlcmd.NewKubectlCommand(kubectlcmd.KubectlOptions{
		PluginHandler: kubectlcmd.NewDefaultPluginHandler(kubectlplugin.ValidPluginFilenamePrefixes),
		Arguments:     append([]string{"kp"}, full...),
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
		Short: "kp version, embedded kubectl version, and where kp keeps its files",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(out, "kubepilot %s (%s/%s, %s)\n", Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
			fmt.Fprintf(out, "embedded kubectl: %s\n", kubectlVersion())
			fmt.Fprintf(out, "home: %s\nplugins on PATH: %s\n", config.Home(), strings.Join(plugin.BinDirs(), ", "))
		},
	}
}

// installHelp appends kp's commands to kubectl's help output, which uses its
// own templates and would otherwise list only kubectl's command groups.
func installHelp(root *cobra.Command, kp []*cobra.Command) {
	orig := root.HelpFunc()
	root.SetHelpFunc(func(c *cobra.Command, args []string) {
		orig(c, args)
		if c != root {
			return
		}
		w := c.OutOrStdout()
		fmt.Fprintln(w, "\nkubepilot commands:")
		for _, k := range kp {
			fmt.Fprintf(w, "  %-14s %s\n", k.Name(), k.Short)
		}
		fmt.Fprintln(w, "\nkubepilot shortcuts:")
		for _, k := range root.Commands() {
			if k.GroupID == "shortcuts" {
				fmt.Fprintf(w, "  %-14s %s\n", k.Name(), k.Short)
			}
		}
		fmt.Fprintln(w, "\nShort aliases: kp alias list   ·   Shell setup: kp shell-init zsh")
	})
}

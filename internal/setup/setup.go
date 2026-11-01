// Package setup provides "kp setup" (one-question first run) and
// "kp doctor" (environment and compatibility checks).
package setup

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"

	"github.com/lyelgate/kubepilot/internal/config"
	"github.com/lyelgate/kubepilot/internal/teleport"
)

// Tools kp can use, and what they are for.
var Tools = [][2]string{
	{"fzf", "interactive pickers for ctx, ns, pods"},
	{"tsh", "Teleport plugin (kp tl / tls / tu)"},
	{"aws", "kp eks, and EKS credentials"},
	{"gcloud", "kp gke"},
	{"gke-gcloud-auth-plugin", "GKE credentials"},
	{"az", "kp aks"},
	{"kubelogin", "AKS Entra ID credentials"},
	{"linode-cli", "kp linode cli pass-through (optional)"},
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func found(name string) (string, bool) {
	p, err := exec.LookPath(exe(name))
	if err != nil {
		p, err = exec.LookPath(name) // .cmd / .bat shims on Windows
	}
	return p, err == nil
}

// DetectShell guesses the user's shell: bash, zsh, fish, powershell or cmd.
func DetectShell() string {
	if runtime.GOOS == "windows" {
		if os.Getenv("PSModulePath") != "" && os.Getenv("PROMPT") == "" {
			return "powershell"
		}
		return "cmd"
	}
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh":
		return "zsh"
	case "fish":
		return "fish"
	}
	return "bash"
}

// RCLine is the line that loads kp's shell setup, and the file it goes in.
func RCLine(shell string) (line, file string) {
	home, _ := os.UserHomeDir()
	switch shell {
	case "zsh":
		return `eval "$(kp shell-init zsh)"`, filepath.Join(home, ".zshrc")
	case "fish":
		return `kp shell-init fish | source`, filepath.Join(home, ".config", "fish", "config.fish")
	case "powershell":
		return `kp shell-init powershell | Out-String | Invoke-Expression`, "$PROFILE"
	case "cmd":
		return `kp shell-init cmd > "%USERPROFILE%\kp-macros.cmd"  (then see the comments in that file)`, ""
	}
	return `eval "$(kp shell-init bash)"`, filepath.Join(home, ".bashrc")
}

// NewSetupCommand returns "kp setup".
func NewSetupCommand(out io.Writer) *cobra.Command {
	var proxy string
	var skipTeleport, writeRC bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "First run: asks only for your Teleport proxy, then shows tools found and your shell setup line",
		Example: `  kp setup                                  # interactive: one question
  kp setup --teleport-proxy tp.example.com:443
  kp setup --skip-teleport --write-rc       # also append the shell line to your rc file`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(out, "kubepilot setup (%s/%s)\n\n", runtime.GOOS, runtime.GOARCH)
			if !skipTeleport {
				cfg, _ := config.Load()
				if proxy == "" && cfg != nil && cfg.Teleport.Proxy != "" {
					fmt.Fprintf(out, "Teleport proxy already set: %s (change with: kp tsh setup)\n", cfg.Teleport.Proxy)
				} else if proxy != "" {
					if err := teleport.Setup(os.Stdin, out, proxy, "", ""); err != nil {
						return err
					}
				} else {
					fmt.Fprint(out, "Teleport proxy address, or Enter to skip: ")
					var line string
					_, _ = fmt.Fscanln(os.Stdin, &line)
					if line = strings.TrimSpace(line); line != "" {
						if err := teleport.Setup(os.Stdin, out, line, "", ""); err != nil {
							return err
						}
					} else {
						fmt.Fprintln(out, "Skipped Teleport.")
					}
				}
			}
			fmt.Fprintln(out, "\nTools:")
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, t := range Tools {
				state := "not found"
				if p, ok := found(t[0]); ok {
					state = p
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", t[0], state, t[1])
			}
			_ = tw.Flush()

			shell := DetectShell()
			line, file := RCLine(shell)
			fmt.Fprintf(out, "\nShell (%s): add this line to load shortcuts and completion:\n  %s\n", shell, line)
			if file != "" && file != "$PROFILE" {
				fmt.Fprintf(out, "  file: %s\n", file)
			}
			if writeRC && file != "" && file != "$PROFILE" {
				data, _ := os.ReadFile(file)
				if strings.Contains(string(data), "kp shell-init") {
					fmt.Fprintln(out, "  already present; not changed")
				} else {
					if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
						return err
					}
					f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(f, "\n# kubepilot\n%s\n", line)
					f.Close()
					if err != nil {
						return err
					}
					fmt.Fprintln(out, "  added; open a new terminal")
				}
			}
			fmt.Fprintln(out, "\nShortcut commands without shell setup (kplog, kpns, kgpo, ...): kp shortcuts install")
			fmt.Fprintln(out, "Check everything: kp doctor")
			return nil
		},
	}
	cmd.Flags().StringVar(&proxy, "teleport-proxy", "", "Teleport proxy address (skips the question)")
	cmd.Flags().BoolVar(&skipTeleport, "skip-teleport", false, "do not ask about Teleport")
	cmd.Flags().BoolVar(&writeRC, "write-rc", false, "append the shell line to your rc file (bash, zsh, fish)")
	return cmd
}

// minor extracts the minor version from "v1.37.0", "1.37+" or "v0.37.2".
func minor(v string) int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	if err != nil {
		return -1
	}
	return n
}

// Skew describes the client/server minor difference; ok is false outside ±1.
func Skew(client, server string) (int, bool) {
	c, s := minor(client), minor(server)
	if c < 0 || s < 0 {
		return 0, true
	}
	d := s - c
	return d, d >= -1 && d <= 1
}

// NewDoctorCommand returns "kp doctor".
func NewDoctorCommand(flags *genericclioptions.ConfigFlags, out io.Writer, version, kubectlVersion string) *cobra.Command {
	var allContexts bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check kubeconfig, connectivity, kubectl/cluster version skew and the credential plugins each context needs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			problems := 0
			fmt.Fprintf(out, "kp %s, embedded kubectl %s, %s/%s, shell %s\n", version, kubectlVersion, runtime.GOOS, runtime.GOARCH, DetectShell())
			loader := flags.ToRawKubeConfigLoader()
			raw, err := loader.RawConfig()
			if err != nil {
				return fmt.Errorf("kubeconfig: %w", err)
			}
			fmt.Fprintf(out, "kubeconfig: %s\ncurrent context: %s\n\n", strings.Join(loader.ConfigAccess().GetLoadingPrecedence(), string(os.PathListSeparator)), dash(raw.CurrentContext))

			names := []string{raw.CurrentContext}
			if allContexts {
				names = names[:0]
				for n := range raw.Contexts {
					names = append(names, n)
				}
				sort.Strings(names)
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CONTEXT\tAUTH\tCREDENTIAL PLUGIN\tSTATUS")
			for _, n := range names {
				c := raw.Contexts[n]
				if c == nil {
					continue
				}
				auth, plugin, status := "none", "-", "ok"
				if u := raw.AuthInfos[c.AuthInfo]; u != nil {
					switch {
					case u.Exec != nil:
						auth = "exec"
						plugin = filepath.Base(u.Exec.Command)
						if _, ok := found(u.Exec.Command); !ok {
							status = "MISSING " + plugin
							problems++
						}
					case u.AuthProvider != nil:
						auth = "auth-provider " + u.AuthProvider.Name
						status = "REMOVED in kubectl 1.26+: re-fetch credentials (kp gke use / kp aks use --kubelogin)"
						problems++
					case u.Token != "" || u.TokenFile != "":
						auth = "token"
					case len(u.ClientCertificateData) > 0 || u.ClientCertificate != "":
						auth = "client certificate"
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, auth, plugin, status)
			}
			_ = tw.Flush()

			if raw.CurrentContext != "" {
				rc, err := flags.ToRESTConfig()
				if err == nil {
					rc.Timeout = 8 * time.Second
					if cs, err := kubernetes.NewForConfig(rc); err == nil {
						ctx, cancel := context.WithTimeout(cmd.Context(), 8*time.Second)
						defer cancel()
						_ = ctx
						if v, err := cs.Discovery().ServerVersion(); err == nil {
							d, ok := Skew(kubectlVersion, v.GitVersion)
							fmt.Fprintf(out, "\ncluster %s reachable, server %s", raw.CurrentContext, v.GitVersion)
							if ok {
								fmt.Fprintln(out, " (within the supported ±1 minor of the embedded kubectl)")
							} else {
								fmt.Fprintf(out, "\n  WARNING: server is %+d minor versions from the embedded kubectl %s; outside the supported ±1 skew. Most commands work, edge cases may not.\n", d, kubectlVersion)
								problems++
							}
						} else {
							fmt.Fprintf(out, "\ncluster %s NOT reachable: %v\n", raw.CurrentContext, err)
							problems++
						}
					}
				}
			}

			fmt.Fprintln(out, "\ntools:")
			tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, t := range Tools {
				state := "-"
				if p, ok := found(t[0]); ok {
					state = p
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", t[0], state, t[1])
			}
			_ = tw.Flush()
			if cfg, err := config.Load(); err == nil && cfg.Teleport.Proxy != "" {
				state := "not logged in (kp tl)"
				if teleport.LoggedIn() {
					state = "logged in"
				}
				fmt.Fprintf(out, "\nteleport: proxy %s, %s\n", cfg.Teleport.Proxy, state)
			}
			if problems > 0 {
				fmt.Fprintf(out, "\n%d problem(s) found.\n", problems)
			} else {
				fmt.Fprintln(out, "\nNo problems found.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&allContexts, "all-contexts", false, "check every context, not just the current one")
	return cmd
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

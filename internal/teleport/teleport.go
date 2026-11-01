// Package teleport is kp's built-in Teleport plugin. It wraps the official
// tsh binary (Windows, macOS, Linux), so kp never handles Teleport
// credentials itself. Setup asks one question, the proxy address; after that
// the short commands need nothing else:
//
//	kp tl            tsh login --proxy=<saved proxy>
//	kp tls [REGEX]   Kubernetes clusters you can reach
//	kp tu REGEX      log in to the matching cluster and switch kubectl to it
//	kp ts            tsh status
//	kp tlo           tsh logout
package teleport

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lyelgate/kubepilot/internal/config"
	"github.com/lyelgate/kubepilot/internal/match"
)

// Binary returns the path of tsh, or an error with install guidance.
func Binary() (string, error) {
	if p := os.Getenv("KP_TSH"); p != "" {
		return p, nil
	}
	name := "tsh"
	if runtime.GOOS == "windows" {
		name = "tsh.exe"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("tsh is not installed. Install the Teleport client for %s from https://goteleport.com/download/ (or: brew install teleport / winget install Teleport.Teleport), then run: kp tsh setup", runtime.GOOS)
	}
	return p, nil
}

func run(out io.Writer, args ...string) error {
	bin, err := Binary()
	if err != nil {
		return err
	}
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, os.Stderr
	return c.Run()
}

func capture(args ...string) ([]byte, error) {
	bin, err := Binary()
	if err != nil {
		return nil, err
	}
	c := exec.Command(bin, args...)
	c.Stderr = os.Stderr
	return c.Output()
}

func loadCfg() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// Proxy returns the saved proxy or an error telling the user to run setup.
func Proxy() (config.TeleportConfig, error) {
	cfg, err := loadCfg()
	if err != nil {
		return config.TeleportConfig{}, err
	}
	if cfg.Teleport.Proxy == "" {
		return cfg.Teleport, errors.New("no Teleport proxy configured; run: kp tsh setup")
	}
	return cfg.Teleport, nil
}

// Setup asks for the proxy (unless given) and saves it.
func Setup(in io.Reader, out io.Writer, proxy, user, auth string) error {
	cfg, err := loadCfg()
	if err != nil {
		return err
	}
	if proxy == "" {
		fmt.Fprint(out, "Teleport proxy address (e.g. teleport.example.com:443): ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		proxy = strings.TrimSpace(line)
	}
	if proxy == "" {
		return errors.New("no proxy given; nothing saved")
	}
	proxy = strings.TrimPrefix(strings.TrimPrefix(proxy, "https://"), "http://")
	proxy = strings.TrimRight(proxy, "/")
	cfg.Teleport.Proxy = proxy
	if user != "" {
		cfg.Teleport.User = user
	}
	if auth != "" {
		cfg.Teleport.Auth = auth
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved Teleport proxy %s. Next: kp tl (login), kp tls (clusters), kp tu <cluster> (switch).\n", proxy)
	return nil
}

// Login runs tsh login against the saved proxy.
func Login(out io.Writer, extra ...string) error {
	t, err := Proxy()
	if err != nil {
		return err
	}
	args := []string{"login", "--proxy=" + t.Proxy}
	if t.User != "" {
		args = append(args, "--user="+t.User)
	}
	if t.Auth != "" {
		args = append(args, "--auth="+t.Auth)
	}
	return run(out, append(args, extra...)...)
}

// LoggedIn reports whether tsh has a valid session.
func LoggedIn() bool {
	bin, err := Binary()
	if err != nil {
		return false
	}
	c := exec.Command(bin, "status")
	c.Stdout, c.Stderr = io.Discard, io.Discard
	return c.Run() == nil
}

// Cluster is one Kubernetes cluster reachable through Teleport.
type Cluster struct {
	Name     string            `json:"name"`
	Labels   map[string]string `json:"labels"`
	Selected bool              `json:"selected"`
}

// ParseKubeLS reads "tsh kube ls --format=json", tolerating field names used
// by different Teleport versions.
func ParseKubeLS(data []byte) ([]Cluster, error) {
	var raw []map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing tsh kube ls: %w", err)
	}
	var out []Cluster
	for _, r := range raw {
		c := Cluster{Labels: map[string]string{}}
		for _, k := range []string{"kube_cluster_name", "name", "cluster_name"} {
			if v, ok := r[k].(string); ok && v != "" {
				c.Name = v
				break
			}
		}
		if c.Name == "" {
			continue
		}
		if sel, ok := r["selected"].(bool); ok {
			c.Selected = sel
		}
		if labels, ok := r["labels"].(map[string]interface{}); ok {
			for k, v := range labels {
				c.Labels[k] = fmt.Sprint(v)
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func clusters() ([]Cluster, error) {
	data, err := capture("kube", "ls", "--format=json")
	if err != nil {
		return nil, fmt.Errorf("tsh kube ls failed (logged in? try: kp tl): %w", err)
	}
	return ParseKubeLS(data)
}

func ensureLogin(out io.Writer) error {
	if LoggedIn() {
		return nil
	}
	fmt.Fprintln(out, "Teleport session missing or expired; logging in ...")
	return Login(out)
}

// NewCommand returns "kp tsh".
func NewCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tsh",
		Aliases: []string{"teleport", "tp-plugin"},
		Short:   "Teleport plugin: one-time proxy setup, then kp tl / tls / tu for login, clusters and switching",
		Long: `Teleport plugin, wrapping the official tsh client.

Run "kp tsh setup" once (or "kp setup"); it asks only for your proxy address.
Afterwards:
  kp tl            log in (browser or SSO prompt as configured in Teleport)
  kp tls [REGEX]   list Kubernetes clusters, filtered by regex
  kp tu REGEX      log in to the matching cluster and switch kubectl to it
  kp ts            session status
  kp tlo           log out
Anything else: kp tsh -- <tsh arguments>`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return run(out, args...)
		},
	}
	var user, auth string
	setup := &cobra.Command{
		Use:   "setup [PROXY]",
		Short: "Save your Teleport proxy (asked interactively when not given)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			proxy := ""
			if len(args) == 1 {
				proxy = args[0]
			}
			return Setup(os.Stdin, out, proxy, user, auth)
		},
	}
	setup.Flags().StringVar(&user, "user", "", "Teleport user (optional)")
	setup.Flags().StringVar(&auth, "auth", "", "auth connector, e.g. github or okta (optional)")
	cmd.AddCommand(setup)
	cmd.AddCommand(&cobra.Command{
		Use: "login", Aliases: []string{"l"}, Short: "tsh login with the saved proxy (extra args passed through)",
		DisableFlagParsing: true,
		RunE:               func(cmd *cobra.Command, args []string) error { return Login(out, args...) },
	})
	cmd.AddCommand(&cobra.Command{
		Use: "ls [REGEX]", Aliases: []string{"clusters", "list"}, Short: "Kubernetes clusters reachable through Teleport",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureLogin(out); err != nil {
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
			cs, err := clusters()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CLUSTER\tSELECTED\tLABELS")
			for _, c := range cs {
				var labels []string
				for k, v := range c.Labels {
					labels = append(labels, k+"="+v)
				}
				sort.Strings(labels)
				if !m.Any(append([]string{c.Name}, labels...)...) {
					continue
				}
				sel := ""
				if c.Selected {
					sel = "*"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, sel, strings.Join(labels, ","))
			}
			return tw.Flush()
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "use CLUSTER-REGEX", Aliases: []string{"switch", "kube"}, Short: "Log in to the matching Kubernetes cluster and switch kubectl to it",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureLogin(out); err != nil {
				return err
			}
			cs, err := clusters()
			if err != nil {
				return err
			}
			name, err := Resolve(cs, args[0])
			if err != nil {
				return err
			}
			if err := run(out, "kube", "login", name); err != nil {
				return err
			}
			fmt.Fprintf(out, "Switched kubectl to Teleport cluster %q. Try: kp get nodes\n", name)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "status", Short: "tsh status",
		RunE: func(cmd *cobra.Command, _ []string) error { return run(out, "status") },
	})
	cmd.AddCommand(&cobra.Command{
		Use: "logout", Short: "tsh logout",
		RunE: func(cmd *cobra.Command, _ []string) error { return run(out, "logout") },
	})
	return cmd
}

// Resolve picks one cluster: an exact name, or a single regex match.
func Resolve(cs []Cluster, pattern string) (string, error) {
	for _, c := range cs {
		if c.Name == pattern {
			return c.Name, nil
		}
	}
	m, err := match.Compile(pattern)
	if err != nil {
		return "", err
	}
	var hits []string
	for _, c := range cs {
		if m.Match(c.Name) {
			hits = append(hits, c.Name)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no Teleport cluster matches %q (see: kp tls)", pattern)
	case 1:
		return hits[0], nil
	}
	return "", fmt.Errorf("%d clusters match %q: %s", len(hits), pattern, strings.Join(hits, ", "))
}

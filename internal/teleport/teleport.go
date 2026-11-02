// Package teleport is kdeck's built-in Teleport plugin. It wraps the official
// tsh binary (Windows, macOS, Linux), so kdeck never handles Teleport
// credentials itself. Setup asks one question, the proxy address; after that
// the short commands need nothing else:
//
//	kdeck tl            tsh login --proxy=<saved proxy>
//	kdeck tls [REGEX]   Kubernetes clusters you can reach
//	kdeck tu REGEX      log in to the matching cluster and switch kubectl to it
//	kdeck ts            tsh status
//	kdeck tlo           tsh logout
//	kdeck treq REGEX    request access to a cluster, wait for approval, switch to it
//	kdeck treqs         your access requests
//	kdeck tdrop [ID]    drop assumed access requests
//	kdeck tcl [NAME]    Teleport clusters (root and leaf); NAME logs in to that one
//
// With the default "separate" kubeconfig mode, each Teleport Kubernetes
// cluster gets its own file in ~/.kdeck/kubeconfigs.
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

	"github.com/lyelgate/kdeck/internal/config"
	"github.com/lyelgate/kdeck/internal/kubeconfig"
	"github.com/lyelgate/kdeck/internal/match"
)

// Binary returns the path of tsh, or an error with install guidance.
func Binary() (string, error) {
	if p := os.Getenv("KDECK_TSH"); p != "" {
		return p, nil
	}
	name := "tsh"
	if runtime.GOOS == "windows" {
		name = "tsh.exe"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("tsh is not installed. Install the Teleport client for %s from https://goteleport.com/download/ (or: brew install teleport / winget install Teleport.Teleport), then run: kdeck tsh setup", runtime.GOOS)
	}
	return p, nil
}

func run(out io.Writer, args ...string) error {
	return runEnv(out, nil, args...)
}

// runEnv runs tsh with extra environment entries (KEY=value).
func runEnv(out io.Writer, env []string, args ...string) error {
	bin, err := Binary()
	if err != nil {
		return err
	}
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, os.Stderr
	if env != nil {
		c.Env = append(os.Environ(), env...)
	}
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
		return cfg.Teleport, errors.New("no Teleport proxy configured; run: kdeck tsh setup")
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
	fmt.Fprintf(out, "Saved Teleport proxy %s. Next: kdeck tl (login), kdeck tls (clusters), kdeck tu <cluster> (switch).\n", proxy)
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
		return nil, fmt.Errorf("tsh kube ls failed (logged in? try: kdeck tl): %w", err)
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

// NewCommand returns "kdeck tsh".
func NewCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tsh",
		Aliases: []string{"teleport", "tp-plugin"},
		Short:   "Teleport plugin: one-time proxy setup, then kdeck tl / tls / tu for login, clusters and switching",
		Long: `Teleport plugin, wrapping the official tsh client.

Run "kdeck tsh setup" once (or "kdeck setup"); it asks only for your proxy address.
Afterwards:
  kdeck tl            log in (browser or SSO prompt as configured in Teleport)
  kdeck tls [REGEX]   list Kubernetes clusters, filtered by regex
  kdeck tu REGEX      log in to the matching cluster and switch kubectl to it
  kdeck ts            session status
  kdeck tlo           log out

Access requests and switching:
  kdeck treq REGEX --reason "INC-42"   request a cluster, wait for approval, switch to it
  kdeck treq --roles dba --reason ...  request roles
  kdeck treqs                          your requests (kdeck tsh request show ID)
  kdeck tsh request assume ID          use an approved request (--cluster to switch)
  kdeck tdrop [ID]                     drop assumed requests (all when no ID)
  kdeck tsh request approve|deny ID    review someone else's request
  kdeck tcl [NAME]                     Teleport clusters; NAME logs in to that root or leaf

Each Kubernetes cluster gets its own kubeconfig in ~/.kdeck/kubeconfigs
(kdeck kubeconfig). Anything else: kdeck tsh -- <tsh arguments>`,
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
		Use: "ls [REGEX]", Aliases: []string{"list"}, Short: "Kubernetes clusters reachable through Teleport",
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
		RunE: func(cmd *cobra.Command, args []string) error { return Use(out, args[0]) },
	})
	cmd.AddCommand(requestCommand(out))
	cmd.AddCommand(&cobra.Command{
		Use: "drop [REQUEST-ID...]", Short: "Drop assumed access requests from your certificates (all when no ID is given)",
		RunE: func(cmd *cobra.Command, args []string) error { return Drop(out, args) },
	})
	cmd.AddCommand(&cobra.Command{
		Use: "clusters [NAME-REGEX]", Aliases: []string{"cluster", "leaf"}, Short: "Teleport clusters (root and leaf); with NAME, log in to that cluster",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := ""
			if len(args) == 1 {
				pattern = args[0]
			}
			return SwitchCluster(out, pattern)
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
		return "", fmt.Errorf("no Teleport cluster matches %q (see: kdeck tls)", pattern)
	case 1:
		return hits[0], nil
	}
	return "", fmt.Errorf("%d clusters match %q: %s", len(hits), pattern, strings.Join(hits, ", "))
}

// Use logs in to the Teleport Kubernetes cluster matching pattern (logging
// in to Teleport first when needed) and switches kubectl to it.
func Use(out io.Writer, pattern string) error {
	if err := ensureLogin(out); err != nil {
		return err
	}
	cs, err := clusters()
	if err != nil {
		return err
	}
	name, err := Resolve(cs, pattern)
	if err != nil {
		return err
	}
	return login(out, name)
}

// login runs tsh kube login for an exact cluster name and switches to it.
func login(out io.Writer, name string) error {
	if !kubeconfig.Separate() {
		if err := run(out, "kube", "login", name); err != nil {
			return err
		}
		fmt.Fprintf(out, "Switched kubectl to Teleport cluster %q. Try: kdeck get nodes\n", name)
		return nil
	}
	// tsh writes the cluster's own file; kdeck then switches the main
	// kubeconfig's current context to it.
	path := kubeconfig.FileFor(name)
	if err := os.MkdirAll(kubeconfig.Dir(), 0o700); err != nil {
		return err
	}
	if err := runEnv(out, []string{"KUBECONFIG=" + path}, "kube", "login", name); err != nil {
		return err
	}
	ctx, names, err := kubeconfig.NormalizeFile(path)
	if err != nil {
		return err
	}
	if ctx == "" && len(names) > 0 {
		ctx = names[0]
	}
	if ctx == "" {
		return fmt.Errorf("tsh kube login wrote no context to %s", path)
	}
	if err := kubeconfig.SetCurrent(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "Switched kubectl to Teleport cluster %q (context %q, kubeconfig %s). Try: kdeck get nodes\n", name, ctx, path)
	return nil
}

// activeCluster is the Teleport cluster of the current tsh session, used in
// resource IDs (/<cluster>/kube_cluster/<name>). It falls back to the proxy
// host.
func activeCluster() string {
	bin, err := Binary()
	if err == nil {
		if data, err := exec.Command(bin, "status", "--format=json").Output(); err == nil {
			var st struct {
				Active struct {
					Cluster string `json:"cluster"`
				} `json:"active"`
			}
			if json.Unmarshal(data, &st) == nil && st.Active.Cluster != "" {
				return st.Active.Cluster
			}
		}
	}
	t, _ := Proxy()
	host, _, _ := strings.Cut(t.Proxy, ":")
	return host
}

// ParseSearch reads "tsh request search --format=json": the Kubernetes
// clusters you may request, whether or not you can reach them now.
func ParseSearch(data []byte) ([]string, error) {
	var raw []map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing tsh request search: %w", err)
	}
	var out []string
	for _, r := range raw {
		name := ""
		if md, ok := r["metadata"].(map[string]interface{}); ok {
			name, _ = md["name"].(string)
		}
		for _, k := range []string{"kube_cluster_name", "name"} {
			if v, ok := r[k].(string); ok && name == "" {
				name = v
			}
		}
		if name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// requestable resolves pattern against the clusters you may request; when
// tsh cannot search (older Teleport), against the clusters you can reach, and
// finally a plain name is taken as given.
func requestable(pattern string) (string, error) {
	var cs []Cluster
	if data, err := exec.Command(mustBinary(), "request", "search", "--kind=kube_cluster", "--format=json").Output(); err == nil {
		if names, err := ParseSearch(data); err == nil {
			for _, n := range names {
				cs = append(cs, Cluster{Name: n})
			}
		}
	}
	if len(cs) == 0 {
		cs, _ = clusters()
	}
	name, err := Resolve(cs, pattern)
	if err != nil && len(cs) == 0 && !strings.ContainsAny(pattern, `.*+?()[]{}|^$\`) {
		return pattern, nil
	}
	return name, err
}

func mustBinary() string {
	bin, _ := Binary()
	return bin
}

// RequestOptions are the flags of "kdeck tsh request".
type RequestOptions struct {
	Reason    string
	Roles     []string
	Reviewers []string
	NoWait    bool
}

// Request asks for access to the Kubernetes cluster matching pattern (and/or
// roles). tsh waits for approval and then reissues your certificates with the
// request; kdeck then logs in to the cluster and switches to it.
func Request(out io.Writer, pattern string, o RequestOptions) error {
	if pattern == "" && len(o.Roles) == 0 {
		return errors.New("give a cluster (regex) to request, or --roles")
	}
	if err := ensureLogin(out); err != nil {
		return err
	}
	args := []string{"request", "create"}
	name := ""
	if pattern != "" {
		var err error
		if name, err = requestable(pattern); err != nil {
			return err
		}
		args = append(args, "--resource=/"+activeCluster()+"/kube_cluster/"+name)
	}
	if len(o.Roles) > 0 {
		args = append(args, "--roles="+strings.Join(o.Roles, ","))
	}
	if o.Reason != "" {
		args = append(args, "--reason="+o.Reason)
	}
	if len(o.Reviewers) > 0 {
		args = append(args, "--reviewers="+strings.Join(o.Reviewers, ","))
	}
	if o.NoWait {
		args = append(args, "--nowait")
	}
	if err := run(out, args...); err != nil {
		return err
	}
	if o.NoWait {
		fmt.Fprintln(out, "Request sent. When it is approved: kdeck tsh request assume ID"+map[bool]string{true: " --cluster " + name, false: ""}[name != ""])
		return nil
	}
	if name == "" {
		fmt.Fprintln(out, "Access granted. See your clusters: kdeck tls")
		return nil
	}
	// The cluster was not reachable before approval, so switch by its
	// exact name rather than matching the old cluster list again.
	return login(out, name)
}

// Assume logs in with an approved request and, when cluster is given,
// switches to it.
func Assume(out io.Writer, id, cluster string) error {
	if err := Login(out, "--request-id="+id); err != nil {
		return err
	}
	if cluster == "" {
		fmt.Fprintln(out, "Request assumed. See your clusters: kdeck tls; switch: kdeck tu CLUSTER")
		return nil
	}
	return Use(out, cluster)
}

// Drop removes assumed requests (all when ids is empty) from your
// certificates.
func Drop(out io.Writer, ids []string) error {
	if err := run(out, append([]string{"request", "drop"}, ids...)...); err != nil {
		return err
	}
	fmt.Fprintln(out, "Dropped. Clusters you can still reach: kdeck tls")
	return nil
}

// SwitchCluster logs in to a Teleport cluster (root or leaf) matching
// pattern; with no pattern it lists them.
func SwitchCluster(out io.Writer, pattern string) error {
	if err := ensureLogin(out); err != nil {
		return err
	}
	if pattern == "" {
		return run(out, "clusters")
	}
	data, err := capture("clusters", "--format=json")
	if err != nil {
		return err
	}
	cs, err := ParseKubeLS(data)
	if err != nil {
		return err
	}
	name, err := Resolve(cs, pattern)
	if err != nil {
		return err
	}
	if err := Login(out, name); err != nil {
		return err
	}
	fmt.Fprintf(out, "Logged in to Teleport cluster %q. Its Kubernetes clusters: kdeck tls; switch: kdeck tu CLUSTER\n", name)
	return nil
}

func requestCommand(out io.Writer) *cobra.Command {
	var o RequestOptions
	cmd := &cobra.Command{
		Use:     "request [CLUSTER-REGEX]",
		Aliases: []string{"req", "requests"},
		Short:   "Request access to a Kubernetes cluster (or roles), wait for approval, then switch to it",
		Example: `  kdeck tsh request prod-eu --reason "INC-42 investigate 5xx"
  kdeck tsh request --roles dba --reason "schema migration"
  kdeck tsh request prod-eu --reason "..." --nowait     # then: kdeck tsh request assume ID
  kdeck tsh request ls
  kdeck tsh request drop`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := ""
			if len(args) == 1 {
				pattern = args[0]
			}
			return Request(out, pattern, o)
		},
	}
	cmd.Flags().StringVar(&o.Reason, "reason", "", "why you need access (shown to reviewers)")
	cmd.Flags().StringSliceVar(&o.Roles, "roles", nil, "roles to request, comma-separated")
	cmd.Flags().StringSliceVar(&o.Reviewers, "reviewers", nil, "suggested reviewers")
	cmd.Flags().BoolVar(&o.NoWait, "nowait", false, "send the request and return without waiting for approval")
	cmd.AddCommand(&cobra.Command{
		Use: "ls", Aliases: []string{"list"}, Short: "Your access requests (tsh request ls)",
		RunE: func(cmd *cobra.Command, _ []string) error { return run(out, "request", "ls") },
	})
	cmd.AddCommand(&cobra.Command{
		Use: "show REQUEST-ID", Short: "One access request in detail",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return run(out, "request", "show", args[0]) },
	})
	var cluster string
	assume := &cobra.Command{
		Use: "assume REQUEST-ID", Short: "Log in with an approved request (--cluster to switch to a cluster it grants)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return Assume(out, args[0], cluster) },
	}
	assume.Flags().StringVar(&cluster, "cluster", "", "Kubernetes cluster (regex) to switch to afterwards")
	cmd.AddCommand(assume)
	cmd.AddCommand(&cobra.Command{
		Use: "drop [REQUEST-ID...]", Short: "Drop assumed requests from your certificates (all when no ID is given)",
		RunE: func(cmd *cobra.Command, args []string) error { return Drop(out, args) },
	})
	for _, verb := range []string{"approve", "deny"} {
		var reason string
		review := &cobra.Command{
			Use: verb + " REQUEST-ID", Short: strings.ToUpper(verb[:1]) + verb[1:] + " someone else's access request",
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				a := []string{"request", "review", "--" + verb}
				if reason != "" {
					a = append(a, "--reason="+reason)
				}
				return run(out, append(a, args[0])...)
			},
		}
		review.Flags().StringVar(&reason, "reason", "", "note for the requester")
		cmd.AddCommand(review)
	}
	return cmd
}

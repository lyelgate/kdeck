package kubeconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/lyelgate/kdeck/internal/config"
)

// The store keeps one kubeconfig file per cluster in ~/.kdeck/kubeconfigs, so
// a broken, expired or deleted cluster never touches the others. kdeck adds
// every file in the store to KUBECONFIG, after the main kubeconfig, which
// keeps the current context; the shell-init hook does the same for kubectl,
// helm and other tools.

// Dir is the store directory ($KDECK_KUBECONFIG_DIR, or kubeconfigs in
// kdeck's home).
func Dir() string {
	if d := os.Getenv("KDECK_KUBECONFIG_DIR"); d != "" {
		return d
	}
	return config.Path("kubeconfigs")
}

// Files lists the store's kubeconfig files, sorted.
func Files() []string {
	files, _ := filepath.Glob(filepath.Join(Dir(), "*.yaml"))
	sort.Strings(files)
	return files
}

// Separate reports whether new clusters go to the store (the default) rather
// than being merged into the main kubeconfig.
func Separate() bool {
	cfg, err := config.Load()
	return err != nil || cfg.Kubeconfig.Mode != "merge"
}

// FileFor is the store file for a cluster name.
func FileFor(name string) string {
	return filepath.Join(Dir(), fileName(name)+".yaml")
}

func fileName(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ', '@', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, name)
}

func listSep() string {
	if runtime.GOOS == "windows" {
		return ";"
	}
	return ":"
}

func inStore(path string) bool {
	rel, err := filepath.Rel(Dir(), path)
	return err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// Precedence is the KUBECONFIG list kdeck uses: the entries of env (or the
// default ~/.kube/config) without store files, then every store file.
func Precedence(env string, files []string) []string {
	var out []string
	for _, p := range filepath.SplitList(env) {
		if p != "" && !inStore(p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = []string{clientcmd.RecommendedHomeFile}
	}
	return append(out, files...)
}

// Activate points this process's KUBECONFIG (and so every child process:
// plugins, tsh, helm) at the main kubeconfig plus the store. With an empty
// store it changes nothing. The first file holds the current context, so it
// is created when missing; otherwise kubectl would write current-context into
// a store file.
func Activate() {
	files := Files()
	if len(files) == 0 {
		return
	}
	list := Precedence(os.Getenv("KUBECONFIG"), files)
	if _, err := os.Stat(list[0]); errors.Is(err, os.ErrNotExist) {
		_ = os.MkdirAll(filepath.Dir(list[0]), 0o700)
		_ = os.WriteFile(list[0], []byte("apiVersion: v1\nkind: Config\n"), 0o600)
	}
	_ = os.Setenv("KUBECONFIG", strings.Join(list, listSep()))
}

// Normalize gives each context's cluster and user the context's own name, so
// files in the store never share entry names, and clears current-context
// (the main kubeconfig holds it). It returns the context names, sorted.
func Normalize(cfg *api.Config) []string {
	out := api.NewConfig()
	var names []string
	for name, c := range cfg.Contexts {
		names = append(names, name)
		cl, au := name, name
		if v, ok := cfg.Clusters[c.Cluster]; ok {
			out.Clusters[cl] = v.DeepCopy()
		}
		if v, ok := cfg.AuthInfos[c.AuthInfo]; ok {
			out.AuthInfos[au] = v.DeepCopy()
		}
		nc := c.DeepCopy()
		nc.Cluster, nc.AuthInfo = cl, au
		out.Contexts[name] = nc
	}
	sort.Strings(names)
	cfg.Clusters, cfg.AuthInfos, cfg.Contexts = out.Clusters, out.AuthInfos, out.Contexts
	cfg.CurrentContext = ""
	return names
}

// NormalizeFile applies Normalize to a store file in place and returns the
// file's current context before it was cleared, and the context names.
func NormalizeFile(path string) (string, []string, error) {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return "", nil, err
	}
	current := cfg.CurrentContext
	names := Normalize(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", nil, err
	}
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		return "", nil, err
	}
	return current, names, os.Chmod(path, 0o600)
}

// Store writes raw as the store file for name, its contexts renamed name,
// name-2, ... as Merge does. An existing file for name is replaced (a
// refreshed kubeconfig for the same cluster).
func Store(raw []byte, name string) (string, error) {
	incoming, err := clientcmd.Load(raw)
	if err != nil {
		return "", fmt.Errorf("parsing kubeconfig: %w", err)
	}
	cfg := MergeConfigs(api.NewConfig(), incoming, name, false)
	Normalize(cfg)
	path := FileFor(name)
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return "", err
	}
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		return "", err
	}
	return path, os.Chmod(path, 0o600)
}

// SetCurrent makes name the current context. It writes only current-context,
// to the main kubeconfig (the first file of the list Activate builds).
func SetCurrent(name string) error {
	Activate()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := rules.Load()
	if err != nil {
		return err
	}
	if _, ok := cfg.Contexts[name]; !ok {
		return fmt.Errorf("context %q not found", name)
	}
	cfg.CurrentContext = name
	return clientcmd.ModifyConfig(rules, *cfg, true)
}

// Shadowed returns the files, other than path, that also define one of the
// contexts in path. The first file in KUBECONFIG wins, so those contexts in
// path are hidden.
func Shadowed(path string, contexts []string) map[string]string {
	out := map[string]string{}
	for _, f := range Precedence(os.Getenv("KUBECONFIG"), Files()) {
		if f == path {
			break
		}
		cfg, err := clientcmd.LoadFromFile(f)
		if err != nil {
			continue
		}
		for _, c := range contexts {
			if _, ok := cfg.Contexts[c]; ok {
				out[c] = f
			}
		}
	}
	return out
}

// Remove moves a store file aside (<file>.removed-<timestamp>, outside the
// *.yaml pattern) instead of deleting it.
func Remove(name string) (string, error) {
	path := FileFor(name)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no kubeconfig %q in %s (see: kdeck kubeconfig ls)", name, Dir())
	}
	aside := fmt.Sprintf("%s.removed-%s", path, time.Now().Format("20060102-150405"))
	return aside, os.Rename(path, aside)
}

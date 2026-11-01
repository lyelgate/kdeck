// Package plugin is a krew-compatible plugin manager built into kp.
//
// It reads the same index as krew (github.com/kubernetes-sigs/krew-index),
// installs plugins into ~/.kp, and links them as kubectl-<name> in ~/.kp/bin,
// which kp puts on PATH at start-up. Existing krew installs (~/.krew/bin)
// keep working too. Every download is verified against the index's sha256.
package plugin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/lyelgate/kubepilot/internal/config"
	"github.com/lyelgate/kubepilot/internal/match"
)

// IndexURL is the krew index archive.
var IndexURL = "https://github.com/kubernetes-sigs/krew-index/archive/refs/heads/master.tar.gz"

// Manifest is a krew plugin manifest (krew.googlecontainertools.github.com/v1alpha2).
type Manifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Version          string     `json:"version"`
		Homepage         string     `json:"homepage"`
		ShortDescription string     `json:"shortDescription"`
		Description      string     `json:"description"`
		Caveats          string     `json:"caveats"`
		Platforms        []Platform `json:"platforms"`
	} `json:"spec"`
}

// Platform is one downloadable build of a plugin.
type Platform struct {
	Selector struct {
		MatchLabels      map[string]string `json:"matchLabels"`
		MatchExpressions []struct {
			Key      string   `json:"key"`
			Operator string   `json:"operator"`
			Values   []string `json:"values"`
		} `json:"matchExpressions"`
	} `json:"selector"`
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
	Bin    string `json:"bin"`
	Files  []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"files"`
}

// Receipt records an installed plugin.
type Receipt struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Bin         string    `json:"bin"`
	InstalledAt time.Time `json:"installedAt"`
}

// Matches reports whether the platform selector matches os/arch.
func (p Platform) Matches(goos, goarch string) bool {
	labels := map[string]string{"os": goos, "arch": goarch}
	for k, v := range p.Selector.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	for _, e := range p.Selector.MatchExpressions {
		in := false
		for _, v := range e.Values {
			if labels[e.Key] == v {
				in = true
			}
		}
		switch e.Operator {
		case "In":
			if !in {
				return false
			}
		case "NotIn":
			if in {
				return false
			}
		case "Exists":
			if _, ok := labels[e.Key]; !ok {
				return false
			}
		case "DoesNotExist":
			if _, ok := labels[e.Key]; ok {
				return false
			}
		}
	}
	return true
}

// BinName is the executable name kubectl looks up: dashes in the plugin
// name become underscores, as krew does ("view-secret" -> kubectl-view_secret).
func BinName(plugin string) string {
	name := "kubectl-" + strings.ReplaceAll(plugin, "-", "_")
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func indexDir() string   { return config.Path("index", "plugins") }
func binDir() string     { return config.Path("bin") }
func storeDir() string   { return config.Path("store") }
func receiptDir() string { return config.Path("receipts") }

// BinDirs returns plugin directories to put on PATH: kp's own, then krew's.
func BinDirs() []string {
	dirs := []string{binDir()}
	if h, err := os.UserHomeDir(); err == nil {
		krew := filepath.Join(h, ".krew", "bin")
		if root := os.Getenv("KREW_ROOT"); root != "" {
			krew = filepath.Join(root, "bin")
		}
		dirs = append(dirs, krew)
	}
	return dirs
}

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// UpdateIndex downloads the krew index and replaces the local copy.
func UpdateIndex() (int, error) {
	resp, err := httpClient.Get(IndexURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("downloading index: %s", resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return 0, err
	}
	tmp := indexDir() + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		// krew-index-master/plugins/<name>.yaml
		parts := strings.Split(h.Name, "/")
		if len(parts) != 3 || parts[1] != "plugins" || !strings.HasSuffix(parts[2], ".yaml") || h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(tmp, filepath.Base(parts[2])), data, 0o644); err != nil {
			return 0, err
		}
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("index archive contained no plugins")
	}
	_ = os.RemoveAll(indexDir())
	if err := os.Rename(tmp, indexDir()); err != nil {
		return 0, err
	}
	return n, nil
}

func ensureIndex() error {
	if _, err := os.Stat(indexDir()); err == nil {
		return nil
	}
	_, err := UpdateIndex()
	return err
}

// Load reads one manifest from the local index.
func Load(name string) (*Manifest, error) {
	if err := ensureIndex(); err != nil {
		return nil, err
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return nil, fmt.Errorf("invalid plugin name %q", name)
	}
	data, err := os.ReadFile(filepath.Join(indexDir(), name+".yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("plugin %q not found in the index (try: kp plugin search %s)", name, name)
	}
	if err != nil {
		return nil, err
	}
	m := &Manifest{}
	if err := yaml.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	return m, nil
}

// All reads every manifest in the local index.
func All() ([]*Manifest, error) {
	if err := ensureIndex(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(indexDir())
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		if m, err := Load(strings.TrimSuffix(e.Name(), ".yaml")); err == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// Search returns plugins whose name or description matches the regex.
func Search(q string) ([]*Manifest, error) {
	re, err := match.Compile(q)
	if err != nil {
		return nil, err
	}
	all, err := All()
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, m := range all {
		if re.Any(m.Metadata.Name, m.Spec.ShortDescription) {
			out = append(out, m)
		}
	}
	return out, nil
}

// Installed lists receipts of plugins installed by kp.
func Installed() ([]Receipt, error) {
	entries, err := os.ReadDir(receiptDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Receipt
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(receiptDir(), e.Name()))
		if err != nil {
			continue
		}
		var r Receipt
		if yaml.Unmarshal(data, &r) == nil && r.Name != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Install downloads, verifies and links a plugin. log receives progress lines.
func Install(name string, log io.Writer) (*Manifest, error) {
	m, err := Load(name)
	if err != nil {
		return nil, err
	}
	var plat *Platform
	for i := range m.Spec.Platforms {
		if m.Spec.Platforms[i].Matches(runtime.GOOS, runtime.GOARCH) {
			plat = &m.Spec.Platforms[i]
			break
		}
	}
	if plat == nil {
		return nil, fmt.Errorf("plugin %q has no build for %s/%s", name, runtime.GOOS, runtime.GOARCH)
	}
	fmt.Fprintf(log, "Downloading %s %s ...\n", name, m.Spec.Version)
	data, err := download(plat.URI)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, plat.SHA256) {
		return nil, fmt.Errorf("sha256 mismatch for %s: got %s, index says %s (refusing to install)", plat.URI, got, plat.SHA256)
	}
	unpack, err := os.MkdirTemp("", "kp-plugin-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(unpack)
	if err := extract(data, plat.URI, unpack); err != nil {
		return nil, err
	}
	dest := filepath.Join(storeDir(), name, m.Spec.Version)
	_ = os.RemoveAll(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	if len(plat.Files) == 0 {
		if err := copyTree(unpack, dest); err != nil {
			return nil, err
		}
	}
	for _, f := range plat.Files {
		matches, _ := filepath.Glob(filepath.Join(unpack, filepath.FromSlash(f.From)))
		if len(matches) == 0 {
			return nil, fmt.Errorf("manifest file %q not found in the archive", f.From)
		}
		to := filepath.Join(dest, filepath.FromSlash(f.To))
		if !within(dest, to) {
			return nil, fmt.Errorf("manifest destination %q escapes the install directory", f.To)
		}
		for _, src := range matches {
			target := to
			if len(matches) > 1 || strings.HasSuffix(f.To, "/") || f.To == "." {
				target = filepath.Join(to, filepath.Base(src))
			}
			if err := copyPath(src, target); err != nil {
				return nil, err
			}
		}
	}
	binPath := filepath.Join(dest, filepath.FromSlash(plat.Bin))
	if !within(dest, binPath) {
		return nil, fmt.Errorf("manifest bin %q escapes the install directory", plat.Bin)
	}
	if err := os.Chmod(binPath, 0o755); err != nil {
		return nil, fmt.Errorf("plugin binary %s: %w", plat.Bin, err)
	}
	if err := os.MkdirAll(binDir(), 0o755); err != nil {
		return nil, err
	}
	link := filepath.Join(binDir(), BinName(name))
	_ = os.Remove(link)
	if err := os.Symlink(binPath, link); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(receiptDir(), 0o755); err != nil {
		return nil, err
	}
	rec, _ := yaml.Marshal(Receipt{Name: name, Version: m.Spec.Version, Bin: binPath, InstalledAt: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(receiptDir(), name+".yaml"), rec, 0o644); err != nil {
		return nil, err
	}
	// Keep only the version just installed.
	if olds, _ := os.ReadDir(filepath.Join(storeDir(), name)); olds != nil {
		for _, o := range olds {
			if o.Name() != m.Spec.Version {
				_ = os.RemoveAll(filepath.Join(storeDir(), name, o.Name()))
			}
		}
	}
	return m, nil
}

// Uninstall removes a plugin installed by kp.
func Uninstall(name string) error {
	rec := filepath.Join(receiptDir(), name+".yaml")
	if _, err := os.Stat(rec); err != nil {
		return fmt.Errorf("plugin %q is not installed by kp", name)
	}
	_ = os.Remove(filepath.Join(binDir(), BinName(name)))
	_ = os.RemoveAll(filepath.Join(storeDir(), name))
	return os.Remove(rec)
}

func download(uri string) ([]byte, error) {
	resp, err := httpClient.Get(uri)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", uri, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 500<<20))
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// extract unpacks a .tar.gz, .tgz or .zip archive; anything else is treated
// as a single binary.
func extract(data []byte, uri, dest string) error {
	lower := strings.ToLower(uri)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return err
		}
		for _, f := range zr.File {
			target := filepath.Join(dest, filepath.FromSlash(f.Name))
			if !within(dest, target) {
				return fmt.Errorf("archive entry %q escapes the destination", f.Name)
			}
			if f.FileInfo().IsDir() {
				if err := os.MkdirAll(target, 0o755); err != nil {
					return err
				}
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = writeFile(target, rc, f.Mode())
			rc.Close()
			if err != nil {
				return err
			}
		}
		return nil
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			target := filepath.Join(dest, filepath.FromSlash(h.Name))
			if !within(dest, target) {
				return fmt.Errorf("archive entry %q escapes the destination", h.Name)
			}
			switch h.Typeflag {
			case tar.TypeDir:
				if err := os.MkdirAll(target, 0o755); err != nil {
					return err
				}
			case tar.TypeReg:
				if err := writeFile(target, tr, os.FileMode(h.Mode)); err != nil {
					return err
				}
			}
		}
	default:
		return writeFile(filepath.Join(dest, filepath.Base(uri)), bytes.NewReader(data), 0o755)
	}
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func copyPath(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return copyTree(src, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFile(dst, in, st.Mode())
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyPath(p, target)
	})
}

// Package selfupdate implements "kp update": find the latest GitHub release,
// download the archive for this OS and CPU, verify it against the release's
// checksums.txt, and replace the running binary.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lyelgate/kubepilot/internal/config"
)

// DefaultRepo publishes kp releases unless update.repo is set.
const DefaultRepo = "lyelgate/kubepilot"

type release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

var client = &http.Client{Timeout: 5 * time.Minute}

func apiBase() string {
	if b := os.Getenv("KP_UPDATE_URL"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://api.github.com"
}

func latest(repo string) (*release, error) {
	req, _ := http.NewRequest("GET", apiBase()+"/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("checking %s releases: %s", repo, resp.Status)
	}
	var r release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Newer reports whether tag is newer than current ("dev" is always older).
func Newer(tag, current string) bool {
	if current == "" || current == "dev" || strings.Contains(current, "-dirty") {
		return true
	}
	a, b := parse(tag), parse(current)
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func parse(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

func download(url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 300<<20))
}

// ArchiveName is the release asset for an OS and CPU.
func ArchiveName(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("kp_%s_%s_%s%s", strings.TrimPrefix(version, "v"), goos, goarch, ext)
}

// ChecksumFor finds an asset's sha256 in a checksums.txt body.
func ChecksumFor(checksums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("%s is not listed in checksums.txt", name)
}

func extractBinary(archive []byte, name string) ([]byte, error) {
	bin := "kp"
	if strings.HasSuffix(name, ".zip") {
		bin = "kp.exe"
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == bin {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("%s not found in %s", bin, name)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s not found in %s", bin, name)
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(h.Name) == bin && h.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

// replace swaps the running executable for data.
func replace(self string, data []byte) error {
	dir := filepath.Dir(self)
	tmp, err := os.CreateTemp(dir, ".kp-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s (%w); reinstall with your package manager or run with permission to that directory", self, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// A running .exe cannot be overwritten, but it can be renamed.
		old := self + ".old"
		_ = os.Remove(old)
		if err := os.Rename(self, old); err != nil {
			os.Remove(tmp.Name())
			return err
		}
	}
	return os.Rename(tmp.Name(), self)
}

// NewCommand returns "kp update".
func NewCommand(out io.Writer, current string) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update kp to the latest release (checksum-verified); --check only reports",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repo := DefaultRepo
			if cfg, err := config.Load(); err == nil && cfg.Update.Repo != "" {
				repo = cfg.Update.Repo
			}
			r, err := latest(repo)
			if err != nil {
				return err
			}
			if !Newer(r.Tag, current) {
				fmt.Fprintf(out, "kp %s is up to date (latest %s).\n", current, r.Tag)
				return nil
			}
			fmt.Fprintf(out, "kp %s -> %s available.\n", current, r.Tag)
			if check {
				return nil
			}
			name := ArchiveName(r.Tag, runtime.GOOS, runtime.GOARCH)
			var archiveURL, sumsURL string
			for _, a := range r.Assets {
				switch a.Name {
				case name:
					archiveURL = a.URL
				case "checksums.txt":
					sumsURL = a.URL
				}
			}
			if archiveURL == "" || sumsURL == "" {
				return fmt.Errorf("release %s has no %s or checksums.txt", r.Tag, name)
			}
			sums, err := download(sumsURL)
			if err != nil {
				return err
			}
			want, err := ChecksumFor(sums, name)
			if err != nil {
				return err
			}
			archive, err := download(archiveURL)
			if err != nil {
				return err
			}
			got := sha256.Sum256(archive)
			if hex.EncodeToString(got[:]) != want {
				return fmt.Errorf("checksum mismatch for %s: refusing to install", name)
			}
			bin, err := extractBinary(archive, name)
			if err != nil {
				return err
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			if self, err = filepath.EvalSymlinks(self); err != nil {
				return err
			}
			if strings.Contains(self, string(filepath.Separator)+"Cellar"+string(filepath.Separator)) {
				return fmt.Errorf("kp was installed with Homebrew; run: brew upgrade kp")
			}
			if err := replace(self, bin); err != nil {
				return err
			}
			fmt.Fprintf(out, "Updated %s to %s (sha256 verified).\n", self, r.Tag)
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only report whether an update exists")
	return cmd
}

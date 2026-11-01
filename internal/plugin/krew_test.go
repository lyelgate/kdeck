package plugin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const manifest = `apiVersion: krew.googlecontainertools.github.com/v1alpha2
kind: Plugin
metadata:
  name: view-secret
spec:
  version: v0.13.0
  shortDescription: Decode Kubernetes secrets
  platforms:
  - selector:
      matchLabels: {os: darwin, arch: arm64}
    uri: https://example.com/a.tar.gz
    sha256: abc
    bin: kubectl-view_secret
  - selector:
      matchExpressions:
      - {key: os, operator: In, values: [linux, freebsd]}
      - {key: arch, operator: NotIn, values: ["386"]}
    uri: https://example.com/b.tar.gz
    sha256: def
    bin: kubectl-view_secret
`

func TestPlatformMatching(t *testing.T) {
	var m Manifest
	if err := yaml.Unmarshal([]byte(manifest), &m); err != nil {
		t.Fatal(err)
	}
	p := m.Spec.Platforms
	for _, tc := range []struct {
		os, arch string
		want     []bool
	}{
		{"darwin", "arm64", []bool{true, false}},
		{"darwin", "amd64", []bool{false, false}},
		{"linux", "amd64", []bool{false, true}},
		{"linux", "386", []bool{false, false}},
	} {
		for i, want := range tc.want {
			if got := p[i].Matches(tc.os, tc.arch); got != want {
				t.Errorf("platform %d on %s/%s: got %v, want %v", i, tc.os, tc.arch, got, want)
			}
		}
	}
}

func TestBinName(t *testing.T) {
	if got := BinName("view-secret"); !strings.HasPrefix(got, "kubectl-view_secret") {
		t.Fatalf("got %q", got)
	}
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("evil")
	_ = tw.WriteHeader(&tar.Header{Name: "../../escape.sh", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	tw.Close()
	gz.Close()
	dest := t.TempDir()
	if err := extract(buf.Bytes(), "x.tar.gz", dest); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dest)), "escape.sh")); err == nil {
		t.Fatal("file escaped the destination")
	}
}

func TestExtractTarGz(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho hi\n")
	_ = tw.WriteHeader(&tar.Header{Name: "pkg/kubectl-hello", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	tw.Close()
	gz.Close()
	dest := t.TempDir()
	if err := extract(buf.Bytes(), "x.tgz", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "pkg", "kubectl-hello"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("extracted content: %q %v", got, err)
	}
}

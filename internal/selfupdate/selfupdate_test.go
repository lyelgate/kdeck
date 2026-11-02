package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		tag, cur string
		want     bool
	}{
		{"v0.3.0", "v0.2.9", true},
		{"v0.2.9", "v0.3.0", false},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.1", "1.0.0", true},
		{"v0.1.0", "dev", true},
		{"v0.4.0", "v0.4.0-3-gabc-dirty", true},
	} {
		if got := Newer(tc.tag, tc.cur); got != tc.want {
			t.Errorf("Newer(%s, %s) = %v", tc.tag, tc.cur, got)
		}
	}
}

func TestChecksumAndExtract(t *testing.T) {
	name := ArchiveName("v0.2.0", "linux", "arm64")
	if name != "kdeck_0.2.0_linux_arm64.tar.gz" || ArchiveName("v1.0.0", "windows", "amd64") != "kdeck_1.0.0_windows_amd64.zip" {
		t.Fatalf("archive name %q", name)
	}
	sums := []byte("abc123  kdeck_0.2.0_darwin_arm64.tar.gz\ndef456  " + name + "\n")
	if got, err := ChecksumFor(sums, name); err != nil || got != "def456" {
		t.Fatalf("checksum %q %v", got, err)
	}
	if _, err := ChecksumFor(sums, "missing.tar.gz"); err == nil {
		t.Error("missing asset must fail")
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("binary")
	_ = tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.WriteHeader(&tar.Header{Name: "kdeck", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	tw.Close()
	gz.Close()
	got, err := extractBinary(buf.Bytes(), name)
	if err != nil || string(got) != "binary" {
		t.Fatalf("extract %q %v", got, err)
	}
}

func TestLatestByRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/o/r/releases/latest" {
			http.Redirect(w, r, "/o/r/releases/tag/v1.2.3", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	r, err := latestByRedirect(srv.URL, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if r.Tag != "v1.2.3" || len(r.Assets) != 2 || r.Assets[1].URL != srv.URL+"/o/r/releases/download/v1.2.3/checksums.txt" {
		t.Fatalf("%+v", r)
	}
	if _, err := latestByRedirect(srv.URL, "x/y"); err == nil {
		t.Error("no redirect should fail")
	}
}

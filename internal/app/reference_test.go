package app

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// docs/SHORTCUTS.md must match "kdeck alias reference --markdown"; run
// "make docs" after changing shortcuts or command aliases.
func TestShortcutsDocUpToDate(t *testing.T) {
	t.Setenv("KDECK_HOME", t.TempDir())
	// kdeck commands write to the stdout they were built with, so swap it
	// before building the tree.
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r)
		done <- b.Bytes()
	}()
	root, _, _ := build([]string{"kdeck", "alias", "reference", "--markdown"})
	root.SetArgs([]string{"alias", "reference", "--markdown"})
	execErr := root.Execute()
	_ = w.Close()
	os.Stdout = stdout
	got := <-done
	if execErr != nil {
		t.Fatal(execErr)
	}
	want, err := os.ReadFile("../../docs/SHORTCUTS.md")
	if err != nil {
		t.Fatal(err)
	}
	// Git on Windows may check the file out with CRLF line endings.
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Fatalf("docs/SHORTCUTS.md is out of date; run make docs (got %d bytes, file has %d)", len(got), len(want))
	}
	for _, s := range []string{"| `kgpo` | `kubectl get pods` |", "| `dklog` | `kdeck log` |", "| `tl` | `kdeck tsh login` |", "| `kgpol` | `kubectl get pods -l <value>` |"} {
		if !strings.Contains(string(got), s) {
			t.Errorf("reference lacks %s", s)
		}
	}
}

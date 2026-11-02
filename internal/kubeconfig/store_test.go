package kubeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

const lke = `apiVersion: v1
kind: Config
clusters:
- name: lke1
  cluster: {server: https://lke1.example.com:443}
users:
- name: lke1-admin
  user: {token: t}
contexts:
- name: lke1-ctx
  context: {cluster: lke1, user: lke1-admin, namespace: web}
current-context: lke1-ctx
`

func TestPrecedence(t *testing.T) {
	t.Setenv("KDECK_KUBECONFIG_DIR", "/s")
	sep := string(os.PathListSeparator)
	got := Precedence(strings.Join([]string{"/sess/x", "/s/old.yaml", "/home/.kube/config"}, sep), []string{"/s/a.yaml", "/s/b.yaml"})
	want := []string{"/sess/x", "/home/.kube/config", "/s/a.yaml", "/s/b.yaml"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v want %v", got, want)
	}
	if got := Precedence("", nil); len(got) != 1 || got[0] != clientcmd.RecommendedHomeFile {
		t.Errorf("empty env: %v", got)
	}
}

func TestStoreActivateSetCurrent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KDECK_HOME", home)
	t.Setenv("KDECK_KUBECONFIG_DIR", "")
	main := filepath.Join(home, "main-config")
	t.Setenv("KUBECONFIG", main)

	path, err := Store([]byte(lke), "staging")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, "kubeconfigs", "staging.yaml") {
		t.Fatalf("path %s", path)
	}
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := cfg.Contexts["staging"]
	if !ok || c.Cluster != "staging" || c.AuthInfo != "staging" || c.Namespace != "web" || cfg.CurrentContext != "" {
		t.Fatalf("stored config: %+v current=%q", cfg.Contexts, cfg.CurrentContext)
	}
	if cfg.Clusters["staging"].Server != "https://lke1.example.com:443" || cfg.AuthInfos["staging"].Token != "t" {
		t.Fatal("cluster or user not carried over")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}

	if err := SetCurrent("staging"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(os.Getenv("KUBECONFIG"), path) {
		t.Errorf("KUBECONFIG %q lacks the store file", os.Getenv("KUBECONFIG"))
	}
	m, err := clientcmd.LoadFromFile(main)
	if err != nil {
		t.Fatal(err)
	}
	if m.CurrentContext != "staging" || len(m.Contexts) != 0 {
		t.Errorf("main kubeconfig should hold only current-context: %+v", m)
	}
	if s, _ := clientcmd.LoadFromFile(path); s.CurrentContext != "" {
		t.Error("store file got a current-context")
	}
	if err := SetCurrent("nope"); err == nil {
		t.Error("unknown context should fail")
	}

	aside, err := Remove("staging")
	if err != nil || len(Files()) != 0 {
		t.Fatalf("remove: %v, files %v", err, Files())
	}
	if _, err := os.Stat(aside); err != nil {
		t.Errorf("removed file not kept aside: %v", err)
	}
}

func TestNormalizeFileKeepsCurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.yaml")
	if err := os.WriteFile(p, []byte(lke), 0o644); err != nil {
		t.Fatal(err)
	}
	cur, names, err := NormalizeFile(p)
	if err != nil || cur != "lke1-ctx" || len(names) != 1 {
		t.Fatalf("cur=%q names=%v err=%v", cur, names, err)
	}
	cfg, _ := clientcmd.LoadFromFile(p)
	if cfg.Contexts["lke1-ctx"].Cluster != "lke1-ctx" || cfg.CurrentContext != "" {
		t.Errorf("not normalized: %+v", cfg.Contexts["lke1-ctx"])
	}
}

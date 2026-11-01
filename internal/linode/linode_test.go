package linode

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd/api"
)

func TestTokenFromLinodeCLI(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "linode-cli")
	ini := "[DEFAULT]\ndefault-user = alice\n\n[alice]\ntoken = tok-alice\nregion = us-east\n\n[bob]\ntoken = tok-bob\n"
	if err := os.WriteFile(p, []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := TokenFromLinodeCLI(p); got != "tok-alice" {
		t.Fatalf("got %q, want tok-alice", got)
	}
	if got := TokenFromLinodeCLI(filepath.Join(dir, "missing")); got != "" {
		t.Fatalf("missing file: got %q", got)
	}
}

func TestMergeConfigs(t *testing.T) {
	existing := api.NewConfig()
	existing.Clusters["prod"] = &api.Cluster{Server: "https://prod"}
	existing.AuthInfos["prod-admin"] = &api.AuthInfo{Token: "p"}
	existing.Contexts["prod"] = &api.Context{Cluster: "prod", AuthInfo: "prod-admin"}
	existing.CurrentContext = "prod"

	// LKE kubeconfigs use generic names like lke123 / lke123-admin / lke123-ctx.
	incoming := api.NewConfig()
	incoming.Clusters["lke123"] = &api.Cluster{Server: "https://lke123.example"}
	incoming.AuthInfos["lke123-admin"] = &api.AuthInfo{Token: "t"}
	incoming.Contexts["lke123-ctx"] = &api.Context{Cluster: "lke123", AuthInfo: "lke123-admin", Namespace: "default"}

	out := MergeConfigs(existing, incoming, "lke-dev", true)
	if out.CurrentContext != "lke-dev" {
		t.Errorf("current context: %q", out.CurrentContext)
	}
	c := out.Contexts["lke-dev"]
	if c == nil || c.Cluster != "lke-dev" || c.AuthInfo != "lke-dev-admin" {
		t.Fatalf("context: %+v", c)
	}
	if out.Clusters["lke-dev"].Server != "https://lke123.example" || out.AuthInfos["lke-dev-admin"].Token != "t" {
		t.Error("cluster or user not copied")
	}
	if out.Contexts["prod"] == nil || existing.CurrentContext != "prod" {
		t.Error("existing entries must be kept and the input must not be mutated")
	}
	if _, leaked := out.Contexts["lke123-ctx"]; leaked {
		t.Error("original LKE context name should not be added")
	}
}

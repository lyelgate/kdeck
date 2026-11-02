package workspace

import (
	"testing"

	"k8s.io/client-go/tools/clientcmd/api"
)

func TestExtractAndPrune(t *testing.T) {
	cfg := api.NewConfig()
	cfg.Clusters["shared"], cfg.Clusters["own"], cfg.Clusters["orphan"] = &api.Cluster{Server: "s"}, &api.Cluster{Server: "o"}, &api.Cluster{}
	cfg.AuthInfos["u1"], cfg.AuthInfos["u2"] = &api.AuthInfo{}, &api.AuthInfo{}
	cfg.Contexts["a"] = &api.Context{Cluster: "own", AuthInfo: "u1"}
	cfg.Contexts["b"] = &api.Context{Cluster: "shared", AuthInfo: "u2"}
	cfg.Contexts["c"] = &api.Context{Cluster: "shared", AuthInfo: "u2"}
	cfg.CurrentContext = "a"

	one := Extract(cfg, "a")
	if len(one.Contexts) != 1 || one.Clusters["own"].Server != "o" || one.AuthInfos["u1"] == nil {
		t.Fatalf("extract: %+v", one)
	}
	Prune(cfg, []string{"a", "b"})
	if _, ok := cfg.Clusters["own"]; ok {
		t.Error("cluster used only by a should go")
	}
	if _, ok := cfg.Clusters["shared"]; !ok {
		t.Error("cluster still used by c must stay")
	}
	if _, ok := cfg.Clusters["orphan"]; !ok {
		t.Error("unrelated orphan cluster must stay")
	}
	if _, ok := cfg.AuthInfos["u1"]; ok || cfg.AuthInfos["u2"] == nil {
		t.Error("users pruned wrongly")
	}
	if cfg.CurrentContext != "a" || len(cfg.Contexts) != 1 {
		t.Errorf("contexts %v current %q", cfg.Contexts, cfg.CurrentContext)
	}
}

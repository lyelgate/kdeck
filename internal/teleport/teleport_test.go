package teleport

import "testing"

func TestParseKubeLSAndResolve(t *testing.T) {
	data := []byte(`[
	  {"kube_cluster_name": "prod-us", "labels": {"env": "prod"}, "selected": true},
	  {"kube_cluster_name": "prod-eu", "labels": {"env": "prod"}},
	  {"name": "staging"}
	]`)
	cs, err := ParseKubeLS(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 || cs[0].Name != "prod-eu" || !cs[1].Selected || cs[2].Name != "staging" {
		t.Fatalf("parsed %+v", cs)
	}
	if n, err := Resolve(cs, "stag"); err != nil || n != "staging" {
		t.Errorf("stag -> %q, %v", n, err)
	}
	if n, err := Resolve(cs, "prod-eu"); err != nil || n != "prod-eu" {
		t.Errorf("exact -> %q, %v", n, err)
	}
	if _, err := Resolve(cs, "prod"); err == nil {
		t.Error("ambiguous regex should fail")
	}
	if _, err := Resolve(cs, "dev"); err == nil {
		t.Error("no match should fail")
	}
}

package workspace

import (
	"testing"

	"github.com/lyelgate/kubepilot/internal/match"
)

func TestParseSide(t *testing.T) {
	for in, want := range map[string]Side{
		"prod:shop": {"prod", "shop"},
		"prod:":     {"prod", ""},
		":shop":     {"", "shop"},
		"shop":      {"", "shop"},
	} {
		if got := ParseSide(in); got != want {
			t.Errorf("%q: got %+v, want %+v", in, got, want)
		}
	}
}

func TestCompareWorkloads(t *testing.T) {
	a := []Workload{
		{"Deployment", "api", map[string]string{"replicas": "2", "container app image": "api:1.4"}},
		{"Deployment", "worker", map[string]string{"replicas": "1"}},
	}
	b := []Workload{
		{"Deployment", "api", map[string]string{"replicas": "6", "container app image": "api:1.4"}},
		{"Deployment", "cron", map[string]string{"replicas": "1"}},
	}
	got := CompareWorkloads(a, b, nil)
	want := []Diff{
		{"Deployment/api", "replicas", "2", "6"},
		{"Deployment/cron", "(exists)", "missing", "present"},
		{"Deployment/worker", "(exists)", "present", "missing"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("diff %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	if d := CompareWorkloads(a, b, match.MustCompile("^api$")); len(d) != 1 {
		t.Errorf("filtered: %+v", d)
	}
}

func TestEnvLine(t *testing.T) {
	for shell, want := range map[string]string{
		"bash":       "export KUBECONFIG='/a:/b'",
		"fish":       "set -gx KUBECONFIG '/a:/b'",
		"powershell": "$env:KUBECONFIG = '/a:/b'",
		"cmd":        "set KUBECONFIG=/a:/b",
	} {
		if got := EnvLine(shell, "/a:/b"); got != want {
			t.Errorf("%s: %q", shell, got)
		}
	}
	if EnvLine("zsh", "") != "unset KUBECONFIG" {
		t.Error("unset")
	}
}

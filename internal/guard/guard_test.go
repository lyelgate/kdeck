package guard

import (
	"strings"
	"testing"

	"github.com/lyelgate/kubepilot/internal/config"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		in   string
		kind Kind
	}{
		{"get pods", Read},
		{"-n prod delete pod web-0", Write},
		{"--context prod apply -f x.yaml", Write},
		{"apply -f x.yaml --dry-run=server", Read},
		{"apply -f x.yaml --dry-run client", Read},
		{"apply -f x.yaml --dry-run=none", Write},
		{"apply view-last-applied deploy/x", Read},
		{"diff -f x.yaml", Read},
		{"rollout restart deploy/api", Write},
		{"rollout status deploy/api", Read},
		{"rollout history deploy/api", Read},
		{"scale deploy/api --replicas=0", Write},
		{"exec -it web-0 -- sh", Exec},
		{"sh api", Exec},
		{"each api -- rm -rf /tmp/x", Exec},
		{"clean -A", Read},
		{"clean -A --yes", Write},
		{"restart deploy/api", Write},
		{"logs web-0", Read},
		{"config use-context prod", Read},
		{"silent", Read},
		{"get pods -- delete", Read},
		{"delete --help", Read},
		{"restart -h", Read},
	} {
		got, _ := Classify(strings.Fields(tc.in))
		if got != tc.kind {
			t.Errorf("%q: got %s, want %s", tc.in, got, tc.kind)
		}
	}
}

func TestCheck(t *testing.T) {
	cfg := config.SafetyConfig{Production: []string{"prod|live"}, ReadOnly: []string{"^audit-"}}
	yes := func(Policy, string) bool { return true }
	no := func(Policy, string) bool { return false }

	p := Resolve(cfg, "lke-prod-eu", false)
	if !p.Production || p.ReadOnly || !p.DiffApply {
		t.Fatalf("policy: %+v", p)
	}
	if err := Check(p, Write, "delete", no); err == nil {
		t.Error("unconfirmed production write must be refused")
	}
	if err := Check(p, Write, "delete", yes); err != nil {
		t.Errorf("confirmed write: %v", err)
	}
	if err := Check(p, Exec, "exec", no); err != nil {
		t.Error("exec on production needs no confirmation")
	}
	t.Setenv("KP_CONFIRM_CONTEXT", "lke-prod-eu")
	if err := Check(p, Write, "delete", no); err != nil {
		t.Error("KP_CONFIRM_CONTEXT should confirm in scripts")
	}

	ro := Resolve(cfg, "audit-cluster", false)
	if err := Check(ro, Exec, "exec", yes); err == nil {
		t.Error("read-only must refuse exec")
	}
	if err := Check(ro, Read, "get", yes); err != nil {
		t.Error("read-only allows reads")
	}
	if !Resolve(cfg, "staging", true).ReadOnly {
		t.Error("--ro flag must force read-only")
	}
}

func TestUndoAndDiff(t *testing.T) {
	args := strings.Fields("-n shop scale deploy/api --replicas=0")
	if got := ScaleTarget(args); got != "deploy/api" {
		t.Errorf("scale target %q", got)
	}
	if got := UndoHint(args, "prod", "3"); got != "kp --context prod scale deploy/api --replicas=3 -n shop" {
		t.Errorf("undo %q", got)
	}
	if got := UndoHint(strings.Fields("rollout restart deploy/api"), "prod", ""); !strings.Contains(got, "rollout undo deploy/api") {
		t.Errorf("rollout undo %q", got)
	}
	if got := strings.Join(ApplyDiffArgs(strings.Fields("apply -f x.yaml --server-side --prune -l a=b")), " "); got != "diff -f x.yaml -l a=b" {
		t.Errorf("diff args %q", got)
	}
}

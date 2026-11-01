package quick

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDiagnose(t *testing.T) {
	now := time.Now()
	oom := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "app", RestartCount: 7,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.NewTime(now.Add(-time.Minute))}},
	}}}}
	d := Diagnose(oom, nil, now)
	if !strings.Contains(d.Verdict, "keeps crashing") || !strings.Contains(strings.Join(d.Next, " "), "memory limit") {
		t.Errorf("oom: %+v", d)
	}
	pull := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "app", Image: "reg/x:9",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "manifest unknown"}},
	}}}}
	if d := Diagnose(pull, nil, now); !strings.Contains(strings.Join(d.Next, " "), "does not exist") {
		t.Errorf("pull: %+v", d)
	}
	pending := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/3 nodes are available: 3 Insufficient memory."}}}}
	if d := Diagnose(pending, nil, now); !strings.Contains(d.Verdict, "not scheduled") || !strings.Contains(strings.Join(d.Next, " "), "requests") {
		t.Errorf("pending: %+v", d)
	}
	between := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: 3,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(now.Add(-20 * time.Second))}}}}}}
	if d := Diagnose(between, nil, now); !strings.Contains(d.Verdict, "keeps crashing") || !strings.Contains(strings.Join(d.Next, " "), "-p") {
		t.Errorf("between restarts: %+v", d)
	}
	notReady := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}}}
	evs := []corev1.Event{{Type: corev1.EventTypeWarning, Reason: "Unhealthy", Message: "Readiness probe failed: HTTP probe failed with statuscode: 503"}}
	if d := Diagnose(notReady, evs, now); !strings.Contains(strings.Join(d.Next, " "), "readiness probe") {
		t.Errorf("not ready: %+v", d)
	}
}

func TestFitVerdict(t *testing.T) {
	q := func(s string) *resource.Quantity { v := resource.MustParse(s); return &v }
	for _, tc := range []struct {
		res           string
		use, req, lim *resource.Quantity
		want          string
	}{
		{"memory", q("950Mi"), q("512Mi"), q("1Gi"), "OOM risk"},
		{"cpu", q("50m"), q("1"), nil, "over-requested"},
		{"cpu", q("900m"), q("200m"), nil, "under-requested"},
		{"memory", q("100Mi"), nil, nil, "no memory request"},
		{"memory", q("300Mi"), q("256Mi"), q("1Gi"), ""},
	} {
		if got := FitVerdict(tc.res, tc.use, tc.req, tc.lim); !strings.Contains(got, tc.want) || (tc.want == "" && got != "") {
			t.Errorf("%s %v/%v/%v: got %q, want %q", tc.res, tc.use, tc.req, tc.lim, got, tc.want)
		}
	}
}

func TestTraceMatching(t *testing.T) {
	if !hostMatches("*.example.com", "shop.example.com") || hostMatches("*.example.com", "a.b.example.com") {
		t.Error("wildcard host")
	}
	if ok, _ := pathMatches("Prefix", "/api", "/api/orders"); !ok {
		t.Error("prefix")
	}
	if ok, _ := pathMatches("Prefix", "/api", "/apix"); ok {
		t.Error("prefix must match whole path elements")
	}
	if ok, _ := pathMatches("Exact", "/api", "/api/x"); ok {
		t.Error("exact")
	}
}

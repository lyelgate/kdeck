package cost

import (
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func node(name string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi")}}}
}

func pod(ns, nodeName, cpu, mem string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns}, Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

func TestEstimate(t *testing.T) {
	price := func(corev1.Node) (float64, string) { return 100, "test" }
	pvc := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "db"}, Status: corev1.PersistentVolumeClaimStatus{
		Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}}}
	r := Estimate([]corev1.Node{node("n1")},
		[]corev1.Pod{pod("web", "n1", "1", "1Gi"), pod("db", "n1", "500m", "8Gi")}, // web: cpu 25%; db: mem 50%
		[]corev1.PersistentVolumeClaim{pvc}, price, 0.10)
	got := map[string]Line{}
	for _, l := range r.Lines {
		got[l.Namespace] = l
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.01 }
	if !near(got["web"].Compute, 25) || !near(got["db"].Compute, 50) || !near(got["db"].Storage, 5) {
		t.Fatalf("lines: %+v", got)
	}
	if !near(r.Idle, 25) || !near(r.NodesCost, 100) {
		t.Errorf("idle %.2f nodes %.2f", r.Idle, r.NodesCost)
	}
	if r.Lines[0].Namespace != "db" {
		t.Errorf("most expensive first: %+v", r.Lines)
	}
}

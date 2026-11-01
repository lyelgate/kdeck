package silent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/lyelgate/kubepilot/internal/config"
)

var now = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func om(ns, name string, age time.Duration) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(now.Add(-age))}
}

func run(t *testing.T, objs []runtime.Object, opts Options) Result {
	t.Helper()
	opts.Now = now
	return Run(context.Background(), NewSnapshot(fake.NewSimpleClientset(objs...), nil, ""), opts)
}

func only(r Result, check string) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func TestMissedRuns(t *testing.T) {
	from := time.Date(2026, 10, 31, 0, 30, 0, 0, time.UTC)
	n, err := missedRuns("0 * * * *", "", from, now) // hourly, 35.5h later
	if err != nil || n != 35 {
		t.Fatalf("got %d, %v; want 35", n, err)
	}
	if _, err := missedRuns("not a schedule", "", from, now); err == nil {
		t.Fatal("expected an error for an invalid schedule")
	}
	// 02:00 UTC runs twice in the window (Oct 31 and Nov 1); 02:00 in
	// Asia/Kolkata is 20:30 UTC the day before, so only once.
	if n, _ = missedRuns("0 2 * * *", "", from, now); n != 2 {
		t.Fatalf("daily in UTC: got %d, want 2", n)
	}
	if n, _ = missedRuns("0 2 * * *", "Asia/Kolkata", from, now); n != 1 {
		t.Fatalf("daily in Asia/Kolkata: got %d, want 1", n)
	}
}

func TestCronCheck(t *testing.T) {
	lastOK := metav1.NewTime(now.Add(-5 * 24 * time.Hour))
	failing := &batchv1.CronJob{ObjectMeta: om("ops", "db-backup", 30*24*time.Hour),
		Spec:   batchv1.CronJobSpec{Schedule: "0 2 * * *"},
		Status: batchv1.CronJobStatus{LastSuccessfulTime: &lastOK}}
	healthy := &batchv1.CronJob{ObjectMeta: om("ops", "report", 30*24*time.Hour),
		Spec:   batchv1.CronJobSpec{Schedule: "0 2 * * *"},
		Status: batchv1.CronJobStatus{LastSuccessfulTime: &metav1.Time{Time: now.Add(-10 * time.Hour)}}}
	suspended := &batchv1.CronJob{ObjectMeta: om("ops", "old", 300*24*time.Hour),
		Spec: batchv1.CronJobSpec{Schedule: "* * * * *", Suspend: boolp(true)}}
	r := run(t, []runtime.Object{failing, healthy, suspended}, Options{Checks: []string{"SF-CRON-001"}})
	fs := only(r, "SF-CRON-001")
	if len(fs) != 1 || fs[0].Name != "db-backup" {
		t.Fatalf("want one finding for db-backup, got %+v", fs)
	}
	if fs[0].Severity != Critical {
		t.Errorf("a backup CronJob should be critical, got %s", fs[0].Severity)
	}
	if fs[0].Since == nil || !fs[0].Since.Equal(lastOK.Time) || fs[0].SinceSource != "status.lastSuccessfulTime" {
		t.Errorf("since: got %v from %q", fs[0].Since, fs[0].SinceSource)
	}
}

func TestLeaseCheck(t *testing.T) {
	holder, dur := "autoscaler-abc", int32(15)
	lease := func(name string, renewed time.Duration) *coordinationv1.Lease {
		rt := metav1.NewMicroTime(now.Add(-renewed))
		return &coordinationv1.Lease{ObjectMeta: om("kube-system", name, 100*24*time.Hour),
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &dur, RenewTime: &rt}}
	}
	nodeLease := lease("node-1", time.Hour)
	nodeLease.Namespace = "kube-node-lease"
	r := run(t, []runtime.Object{lease("fresh", 5*time.Second), lease("stopped", 6*time.Hour), lease("ancient", 90*24*time.Hour), nodeLease},
		Options{Checks: []string{"SF-LEASE-001"}})
	got := map[string]Severity{}
	for _, f := range only(r, "SF-LEASE-001") {
		got[f.Name] = f.Severity
	}
	if len(got) != 2 || got["stopped"] != High || got["ancient"] != Low {
		t.Fatalf("got %v; want stopped=high, ancient=low", got)
	}
}

func TestWebhookCheck(t *testing.T) {
	fail, ignore := admissionv1.Fail, admissionv1.Ignore
	cfg := &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: om("", "policy", 24*time.Hour),
		Webhooks: []admissionv1.ValidatingWebhook{
			{Name: "strict.example.io", FailurePolicy: &fail, ClientConfig: admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{Namespace: "policy", Name: "gone"}}},
			{Name: "lenient.example.io", FailurePolicy: &ignore, ClientConfig: admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{Namespace: "policy", Name: "empty"}}},
		}}
	svc := &corev1.Service{ObjectMeta: om("policy", "empty", time.Hour), Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "x"}}}
	r := run(t, []runtime.Object{cfg, svc}, Options{Checks: []string{"SF-WEBHOOK-001"}})
	fs := only(r, "SF-WEBHOOK-001")
	if len(fs) != 2 {
		t.Fatalf("want 2 findings, got %+v", fs)
	}
	if fs[0].Severity != Critical || !strings.Contains(fs[0].Summary, "does not exist") {
		t.Errorf("first finding: %s %q", fs[0].Severity, fs[0].Summary)
	}
	if fs[1].Severity != High || !strings.Contains(fs[1].Summary, "silently skipped") {
		t.Errorf("second finding: %s %q", fs[1].Severity, fs[1].Summary)
	}
}

func TestServiceEndpointsCheck(t *testing.T) {
	notReady := &corev1.Pod{ObjectMeta: om("shop", "api-1", 3*24*time.Hour), Status: corev1.PodStatus{
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-30 * time.Hour))}}}}
	notReady.Labels = map[string]string{"app": "api"}
	broken := &corev1.Service{ObjectMeta: om("shop", "api", 10*24*time.Hour), Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "api"}}}
	zero := int32(0)
	parked := &corev1.Service{ObjectMeta: om("shop", "parked", 10*24*time.Hour), Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "parked"}}}
	parkedDep := &appsv1.Deployment{ObjectMeta: om("shop", "parked", 10*24*time.Hour), Spec: appsv1.DeploymentSpec{Replicas: &zero,
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "parked"}}}}}
	headless := &corev1.Service{ObjectMeta: om("shop", "manual", 10*24*time.Hour)}
	r := run(t, []runtime.Object{notReady, broken, parked, parkedDep, headless}, Options{Checks: []string{"SF-NET-001"}})
	fs := only(r, "SF-NET-001")
	if len(fs) != 1 || fs[0].Name != "api" {
		t.Fatalf("want one finding for api, got %+v", fs)
	}
	if fs[0].Severity != High || fs[0].Since == nil {
		t.Errorf("broken for 30h should be high with a since; got %s %v", fs[0].Severity, fs[0].Since)
	}
}

func TestPDBCheckAndSuppression(t *testing.T) {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: om("pay", "api", 20*24*time.Hour), Status: policyv1.PodDisruptionBudgetStatus{
		ExpectedPods: 1, CurrentHealthy: 1, DesiredHealthy: 1, DisruptionsAllowed: 0,
		Conditions: []metav1.Condition{{Type: "DisruptionAllowed", Status: metav1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-12 * 24 * time.Hour))}}}}
	r := run(t, []runtime.Object{pdb}, Options{Checks: []string{"workload"}})
	if fs := only(r, "SF-PDB-001"); len(fs) != 1 || fs[0].Suppressed != "" {
		t.Fatalf("want one unsuppressed PDB finding, got %+v", fs)
	}
	cfg := config.SilentConfig{Suppressions: []config.Suppression{{Check: "SF-PDB-001", Object: "pay/PodDisruptionBudget/*", Reason: "single replica by design", Until: "2027-01-31"}}}
	r = run(t, []runtime.Object{pdb}, Options{Checks: []string{"SF-PDB-001"}, Config: cfg})
	if fs := only(r, "SF-PDB-001"); len(fs) != 1 || fs[0].Suppressed != "single replica by design" {
		t.Fatalf("expected suppression, got %+v", fs)
	}
	expired := config.SilentConfig{Suppressions: []config.Suppression{{Check: "SF-PDB-001", Object: "*", Reason: "old", Until: "2026-01-01"}}}
	r = run(t, []runtime.Object{pdb}, Options{Checks: []string{"SF-PDB-001"}, Config: expired})
	if fs := only(r, "SF-PDB-001"); len(fs) != 1 || fs[0].Suppressed != "" {
		t.Fatalf("an expired suppression must not apply, got %+v", fs)
	}
	pdb.Annotations = map[string]string{AnnotationIgnore: "SF-PDB-001; reason=known"}
	r = run(t, []runtime.Object{pdb}, Options{Checks: []string{"SF-PDB-001"}})
	if fs := only(r, "SF-PDB-001"); len(fs) != 1 || fs[0].Suppressed != "known" {
		t.Fatalf("annotation suppression: got %+v", fs)
	}
}

func TestRBACCheck(t *testing.T) {
	sa := &corev1.ServiceAccount{ObjectMeta: om("csi", "controller", 30*24*time.Hour)}
	pod := &corev1.Pod{ObjectMeta: om("csi", "controller-0", time.Hour), Spec: corev1.PodSpec{ServiceAccountName: "controller"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	role := &rbacv1.ClusterRole{ObjectMeta: om("", "csi-attacher", 30*24*time.Hour)}
	danglingSubject := &rbacv1.ClusterRoleBinding{ObjectMeta: om("", "csi-attacher", 30*24*time.Hour),
		RoleRef:  rbacv1.RoleRef{Kind: "ClusterRole", Name: "csi-attacher"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "csi", Name: "csi-user"}}}
	missingRole := &rbacv1.ClusterRoleBinding{ObjectMeta: om("", "csi-provisioner", 30*24*time.Hour),
		RoleRef:  rbacv1.RoleRef{Kind: "ClusterRole", Name: "csi-provisioner"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "csi", Name: "controller"}}}
	fresh := &rbacv1.ClusterRoleBinding{ObjectMeta: om("", "just-applied", time.Minute),
		RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "nope"}}
	r := run(t, []runtime.Object{sa, pod, role, danglingSubject, missingRole, fresh}, Options{Checks: []string{"SF-RBAC-001"}})
	fs := only(r, "SF-RBAC-001")
	if len(fs) != 2 {
		t.Fatalf("want 2 findings, got %+v", fs)
	}
	if fs[0].Name != "csi-provisioner" || fs[0].Severity != High {
		t.Errorf("missing role used by a running pod should be high first; got %s %s", fs[0].Name, fs[0].Severity)
	}
	if fs[1].Name != "csi-attacher" || !strings.Contains(fs[1].Summary, "csi-user") {
		t.Errorf("dangling subject: got %s %q", fs[1].Name, fs[1].Summary)
	}
}

func TestSecretCertCheckIsSensitive(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: om("ingress", "envoy-xds", 400*24*time.Hour), Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": selfSigned(t, "envoy", now.Add(-2*time.Hour))}}
	r := run(t, []runtime.Object{sec}, Options{})
	if len(only(r, "SF-CERT-001")) != 0 {
		t.Fatal("SF-CERT-001 must not run without --sensitive")
	}
	skippedNote := false
	for _, s := range r.Skipped {
		if s.Check == "SF-CERT-001" {
			skippedNote = true
		}
	}
	if !skippedNote {
		t.Error("expected SF-CERT-001 in the skipped list")
	}
	r = run(t, []runtime.Object{sec}, Options{Sensitive: true, Checks: []string{"certificate"}})
	fs := only(r, "SF-CERT-001")
	if len(fs) != 1 || fs[0].Severity != Critical || !strings.Contains(fs[0].Summary, "expired") || fs[0].Since == nil {
		t.Fatalf("want one critical expired finding with a since, got %+v", fs)
	}
}

func TestSinceFilterAndSort(t *testing.T) {
	a := Finding{Check: "A", Severity: High, Kind: "X", Name: "old", Since: tp(now.Add(-48 * time.Hour))}
	b := Finding{Check: "B", Severity: High, Kind: "X", Name: "new", Since: tp(now.Add(-time.Hour))}
	c := Finding{Check: "C", Severity: Critical, Kind: "X", Name: "unknown"}
	fs := []Finding{b, c, a}
	Sort(fs)
	if fs[0].Name != "unknown" || fs[1].Name != "old" || fs[2].Name != "new" {
		t.Fatalf("sort order: %s %s %s", fs[0].Name, fs[1].Name, fs[2].Name)
	}
	d, err := ParseDuration("1d12h")
	if err != nil || d != 36*time.Hour {
		t.Fatalf("ParseDuration: %v %v", d, err)
	}
}

func boolp(b bool) *bool { return &b }

func selfSigned(t *testing.T, cn string, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: cn},
		NotBefore: notAfter.Add(-365 * 24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestNewChecks(t *testing.T) {
	staleSince := metav1.NewTime(now.Add(-3 * time.Hour))
	notReady := &corev1.Node{ObjectMeta: om("", "node-3", 30*24*time.Hour), Spec: corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown, Reason: "NodeStatusUnknown", LastTransitionTime: staleSince}}}}
	released := &corev1.PersistentVolume{ObjectMeta: om("", "pv-old", 90*24*time.Hour), Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased}}
	quota := &corev1.ResourceQuota{ObjectMeta: om("team", "compute", 10*24*time.Hour), Status: corev1.ResourceQuotaStatus{
		Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("10")}, Used: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("10")}}}
	resize := &corev1.PersistentVolumeClaim{ObjectMeta: om("db", "data", 10*24*time.Hour), Status: corev1.PersistentVolumeClaimStatus{
		Conditions: []corev1.PersistentVolumeClaimCondition{{Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionTrue, LastTransitionTime: staleSince}}}}
	r := run(t, []runtime.Object{notReady, released, quota, resize}, Options{Checks: []string{"node", "storage", "SF-QUOTA-001"}})
	got := map[string]bool{}
	for _, f := range r.Findings {
		got[f.Check] = true
	}
	for _, id := range []string{"SF-NODE-001", "SF-NODE-002", "SF-VOL-004", "SF-QUOTA-001", "SF-VOL-002"} {
		if !got[id] {
			t.Errorf("expected a %s finding; got %v", id, got)
		}
	}
}

func TestDeprecatedCalls(t *testing.T) {
	m := `# HELP apiserver_requested_deprecated_apis x
apiserver_requested_deprecated_apis{group="policy",removed_release="1.25",resource="podsecuritypolicies",subresource="",version="v1beta1"} 1
apiserver_requested_deprecated_apis{group="",removed_release="",resource="componentstatuses",subresource="",version="v1"} 1
apiserver_requested_deprecated_apis{group="x",removed_release="1.40",resource="y",subresource="",version="v1"} 0
`
	calls := DeprecatedCalls(m)
	if len(calls) != 2 || calls[0]["resource"] != "podsecuritypolicies" || calls[0]["removed_release"] != "1.25" {
		t.Fatalf("got %+v", calls)
	}
}

func TestWriteMetrics(t *testing.T) {
	var b strings.Builder
	f := Finding{Check: "SF-PDB-001", Severity: Medium, Kind: "PodDisruptionBudget", Namespace: "pay", Name: `we"ird`, Since: tp(now)}
	WriteMetrics(&b, Result{Findings: []Finding{f}, Skipped: []Skipped{{"SF-BACKUP-001", "API not installed"}}, Duration: "12ms"}, now, false)
	out := b.String()
	for _, want := range []string{
		`silent_finding{check="SF-PDB-001",severity="medium",category="workload",namespace="pay",kind="PodDisruptionBudget",name="we\"ird"} 1`,
		`silent_findings_total{severity="medium"} 1`,
		`silent_check_skipped{check="SF-BACKUP-001",reason="API not installed"} 1`,
		"silent_run_duration_seconds 0.012",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

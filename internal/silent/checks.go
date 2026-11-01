package silent

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/duration"
)

var errNotInstalled = errors.New("not installed")

// Registry lists every check, in catalogue order.
var Registry = []Check{
	{"SF-CRON-001", "CronJob keeps running but never succeeds", "backup", false, checkCron},
	{"SF-BACKUP-001", "Velero backups failing or not running", "backup", false, checkVelero},
	{"SF-CERT-001", "TLS certificate in a Secret expiring or expired", "certificate", true, checkSecretCerts},
	{"SF-CERT-002", "Webhook, APIService or CRD CA bundle expiring", "certificate", false, checkCABundles},
	{"SF-RBAC-001", "Binding points at a role or ServiceAccount that does not exist", "rbac", false, checkRBAC},
	{"SF-LEASE-001", "A controller stopped renewing its leader lease", "control", false, checkLeases},
	{"SF-API-001", "Aggregated API unavailable", "control", false, checkAPIServices},
	{"SF-WEBHOOK-001", "Admission webhook has no ready backend", "webhook", false, checkWebhooks},
	{"SF-NET-001", "Service has no ready endpoints", "network", false, checkServiceEndpoints},
	{"SF-NET-002", "Ingress or HTTPRoute sends traffic nowhere", "network", false, checkRoutes},
	{"SF-HPA-001", "HPA cannot scale", "workload", false, checkHPAs},
	{"SF-PDB-001", "PodDisruptionBudget allows zero disruptions", "workload", false, checkPDBs},
	{"SF-ROLLOUT-001", "Deployment stuck or ReplicaSet cannot create pods", "workload", false, checkRollouts},
	{"SF-FINALIZER-001", "Object stuck in deletion", "workload", false, checkFinalizers},
	{"SF-VOL-001", "Volume cannot attach or detach", "storage", false, checkVolumeAttachments},
}

func ago(t time.Time, now time.Time) string { return duration.HumanDuration(now.Sub(t)) }

func cond[T any](conds []T, match func(T) bool) (T, bool) {
	for _, c := range conds {
		if match(c) {
			return c, true
		}
	}
	var zero T
	return zero, false
}

func finding(c, title string, sev Severity, kind string, om metav1.ObjectMeta, summary, remediation string) Finding {
	return Finding{Check: c, Title: title, Severity: sev, Kind: kind, Namespace: om.Namespace, Name: om.Name,
		Summary: summary, Remediation: remediation, annotations: om.Annotations}
}

// --- SF-CRON-001 -------------------------------------------------------------

// missedRuns counts scheduled times strictly after from and before now.
func missedRuns(schedule, tz string, from, now time.Time) (int, error) {
	spec := schedule
	if tz != "" && !strings.HasPrefix(spec, "CRON_TZ=") && !strings.HasPrefix(spec, "TZ=") {
		spec = "CRON_TZ=" + tz + " " + spec
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return 0, err
	}
	n := 0
	for t := sched.Next(from); t.Before(now) && n < 10000; t = sched.Next(t) {
		n++
	}
	return n, nil
}

func checkCron(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	cjs, err := s.CronJobs(ctx)
	if err != nil {
		return nil, err
	}
	threshold := int(p.T("SF-CRON-001", "missedRuns", 3))
	var out []Finding
	for _, cj := range cjs {
		if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
			continue
		}
		base, source := cj.CreationTimestamp.Time, "metadata.creationTimestamp"
		if cj.Status.LastSuccessfulTime != nil {
			base, source = cj.Status.LastSuccessfulTime.Time, "status.lastSuccessfulTime"
		}
		tz := ""
		if cj.Spec.TimeZone != nil {
			tz = *cj.Spec.TimeZone
		}
		missed, err := missedRuns(cj.Spec.Schedule, tz, base, p.Now)
		if err != nil || missed < threshold {
			continue
		}
		// The newest run may still be in progress; do not count it.
		if len(cj.Status.Active) > 0 {
			missed--
			if missed < threshold {
				continue
			}
		}
		sev := High
		if isBackup(cj.ObjectMeta, p.backupSel) {
			sev = Critical
		}
		summary := fmt.Sprintf("no successful run in %d scheduled runs (%s)", missed, ago(base, p.Now))
		if cj.Status.LastSuccessfulTime == nil {
			summary = fmt.Sprintf("never succeeded; %d scheduled runs since creation %s ago", missed, ago(base, p.Now))
		}
		f := finding("SF-CRON-001", "CronJob keeps running but never succeeds", sev, "CronJob", cj.ObjectMeta, summary,
			"Inspect the latest Job's pod logs: kubectl logs job/<latest-job> -n "+cj.Namespace)
		f.Since, f.SinceSource = tp(base), source
		f.Evidence = []string{"spec.schedule=" + cj.Spec.Schedule}
		if cj.Status.LastScheduleTime != nil {
			f.Evidence = append(f.Evidence, "status.lastScheduleTime="+cj.Status.LastScheduleTime.UTC().Format(time.RFC3339))
		}
		out = append(out, f)
	}
	return out, nil
}

func isBackup(om metav1.ObjectMeta, sel []string) bool {
	if strings.Contains(strings.ToLower(om.Name), "backup") {
		return true
	}
	for _, s := range sel {
		k, v, _ := strings.Cut(s, "=")
		if om.Labels[k] == v {
			return true
		}
	}
	return false
}

// --- SF-BACKUP-001 -------------------------------------------------------------

func checkVelero(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	schedules, err := s.Dynamic(ctx, gvrVeleroSched, true)
	if err != nil {
		return nil, err
	}
	backups, err := s.Dynamic(ctx, gvrVeleroBackup, true)
	if err != nil {
		return nil, err
	}
	var out []Finding
	if bsls, err := s.Dynamic(ctx, gvrVeleroBSL, true); err == nil {
		for _, b := range bsls {
			phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
			if phase != "Unavailable" {
				continue
			}
			f := finding("SF-BACKUP-001", "Velero backups failing or not running", High, "BackupStorageLocation", objMeta(b),
				"backup storage location is Unavailable; every backup to it fails",
				"Check the bucket, credentials Secret and endpoint in the BackupStorageLocation spec")
			if t, ok := nestedTime(b.Object, "status", "lastValidationTime"); ok {
				f.Evidence = []string{"status.lastValidationTime=" + t.Format(time.RFC3339)}
			}
			f.Evidence = append(f.Evidence, "status.phase=Unavailable")
			out = append(out, f)
		}
	}
	for _, sc := range schedules {
		name, ns := sc.GetName(), sc.GetNamespace()
		var lastOK time.Time
		var recent []string
		type b struct {
			t     time.Time
			phase string
		}
		var mine []b
		for _, bk := range backups {
			if bk.GetNamespace() != ns || bk.GetLabels()["velero.io/schedule-name"] != name {
				continue
			}
			phase, _, _ := unstructured.NestedString(bk.Object, "status", "phase")
			t, ok := nestedTime(bk.Object, "status", "completionTimestamp")
			if !ok {
				t = bk.GetCreationTimestamp().Time
			}
			mine = append(mine, b{t, phase})
			if phase == "Completed" && t.After(lastOK) {
				lastOK = t
			}
		}
		sort.Slice(mine, func(i, j int) bool { return mine[i].t.After(mine[j].t) })
		failedInRow := 0
		for _, x := range mine {
			if x.phase == "Failed" || x.phase == "PartiallyFailed" || x.phase == "FailedValidation" {
				failedInRow++
				if len(recent) < 3 {
					recent = append(recent, x.phase)
				}
				continue
			}
			if x.phase == "Completed" {
				break
			}
		}
		schedule, _, _ := unstructured.NestedString(sc.Object, "spec", "schedule")
		base, source := lastOK, "Backup.status.completionTimestamp"
		if base.IsZero() {
			base, source = sc.GetCreationTimestamp().Time, "Schedule.metadata.creationTimestamp"
		}
		missed, _ := missedRuns(schedule, "", base, p.Now)
		if failedInRow < 3 && missed < 2 {
			continue
		}
		sev := High
		if p.Now.Sub(base) > Days(7) {
			sev = Critical
		}
		summary := fmt.Sprintf("no successful backup in %s (%d failed in a row)", ago(base, p.Now), failedInRow)
		if lastOK.IsZero() {
			summary = fmt.Sprintf("no successful backup since the schedule was created %s ago", ago(base, p.Now))
		}
		f := finding("SF-BACKUP-001", "Velero backups failing or not running", sev, "Schedule", objMeta(sc), summary,
			"velero backup describe --details on the newest backup; check the BackupStorageLocation and node-agent pods")
		f.Since, f.SinceSource = tp(base), source
		f.Evidence = []string{"spec.schedule=" + schedule}
		if len(recent) > 0 {
			f.Evidence = append(f.Evidence, "latest phases: "+strings.Join(recent, ", "))
		}
		out = append(out, f)
	}
	return out, nil
}

func objMeta(u unstructured.Unstructured) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: u.GetName(), Namespace: u.GetNamespace(), Annotations: u.GetAnnotations(), CreationTimestamp: u.GetCreationTimestamp()}
}

func nestedTime(obj map[string]interface{}, fields ...string) (time.Time, bool) {
	s, ok, _ := unstructured.NestedString(obj, fields...)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// --- certificates ---------------------------------------------------------------

// parseCerts decodes every certificate in a PEM bundle.
func parseCerts(data []byte) []*x509.Certificate {
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return certs
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if c, err := x509.ParseCertificate(block.Bytes); err == nil {
			certs = append(certs, c)
		}
	}
}

// certSeverity grades days left; ok=false when not worth reporting.
func certSeverity(p *Params, id string, notAfter, now time.Time) (Severity, string, bool) {
	left := notAfter.Sub(now)
	days := int(math.Floor(left.Hours() / 24))
	switch {
	case left <= 0:
		return Critical, fmt.Sprintf("expired %s ago", duration.HumanDuration(-left)), true
	case left < Days(p.T(id, "criticalDays", 7)):
		return Critical, fmt.Sprintf("expires in %d days", days), true
	case left < Days(p.T(id, "highDays", 21)):
		return High, fmt.Sprintf("expires in %d days", days), true
	case left < Days(p.T(id, "mediumDays", 30)):
		return Medium, fmt.Sprintf("expires in %d days", days), true
	}
	return Low, "", false
}

func certName(c *x509.Certificate) string {
	if c.Subject.CommonName != "" {
		return c.Subject.CommonName
	}
	if len(c.DNSNames) > 0 {
		return c.DNSNames[0]
	}
	return c.Subject.String()
}

func checkSecretCerts(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	secrets, err := s.Secrets(ctx)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, sec := range secrets {
		for _, key := range []string{"tls.crt", "ca.crt"} {
			data, ok := sec.Data[key]
			if !ok {
				continue
			}
			for _, c := range parseCerts(data) {
				sev, when, ok := certSeverity(p, "SF-CERT-001", c.NotAfter, p.Now)
				if !ok {
					continue
				}
				f := finding("SF-CERT-001", "TLS certificate in a Secret expiring or expired", sev, "Secret", sec.ObjectMeta,
					fmt.Sprintf("%s certificate %q %s", key, certName(c), when),
					"Renew the certificate; if cert-manager owns it, check: kubectl describe certificate -n "+sec.Namespace)
				if c.NotAfter.Before(p.Now) {
					f.Since, f.SinceSource = tp(c.NotAfter), "certificate NotAfter"
				}
				f.Evidence = []string{"key=" + key, "notAfter=" + c.NotAfter.UTC().Format(time.RFC3339), "issuer=" + c.Issuer.CommonName}
				f.detailKey = key + "/" + c.SerialNumber.String()
				out = append(out, f)
			}
		}
	}
	return out, nil
}

func checkCABundles(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	var out []Finding
	report := func(kind string, om metav1.ObjectMeta, where string, bundle []byte) {
		for _, c := range parseCerts(bundle) {
			sev, when, ok := certSeverity(p, "SF-CERT-002", c.NotAfter, p.Now)
			if !ok {
				continue
			}
			f := finding("SF-CERT-002", "Webhook, APIService or CRD CA bundle expiring", sev, kind, om,
				fmt.Sprintf("caBundle for %s (%q) %s", where, certName(c), when),
				"Rotate the serving certificate and its CA, then update caBundle (cert-manager's CA injector can do this)")
			if c.NotAfter.Before(p.Now) {
				f.Since, f.SinceSource = tp(c.NotAfter), "certificate NotAfter"
			}
			f.Evidence = []string{"notAfter=" + c.NotAfter.UTC().Format(time.RFC3339)}
			f.detailKey = where + "/" + c.SerialNumber.String()
			out = append(out, f)
		}
	}
	var firstErr error
	if v, err := s.ValidatingWebhooks(ctx); err == nil {
		for _, cfg := range v {
			for _, w := range cfg.Webhooks {
				report("ValidatingWebhookConfiguration", cfg.ObjectMeta, "webhook "+w.Name, w.ClientConfig.CABundle)
			}
		}
	} else {
		firstErr = err
	}
	if m, err := s.MutatingWebhooks(ctx); err == nil {
		for _, cfg := range m {
			for _, w := range cfg.Webhooks {
				report("MutatingWebhookConfiguration", cfg.ObjectMeta, "webhook "+w.Name, w.ClientConfig.CABundle)
			}
		}
	}
	if apis, err := s.Dynamic(ctx, gvrAPIService, false); err == nil {
		for _, a := range apis {
			if b64, ok, _ := unstructured.NestedString(a.Object, "spec", "caBundle"); ok && b64 != "" {
				if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
					report("APIService", objMeta(a), "APIService", raw)
				}
			}
		}
	}
	if crds, err := s.Dynamic(ctx, gvrCRD, false); err == nil {
		for _, c := range crds {
			if b64, ok, _ := unstructured.NestedString(c.Object, "spec", "conversion", "webhook", "clientConfig", "caBundle"); ok && b64 != "" {
				if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
					report("CustomResourceDefinition", objMeta(c), "conversion webhook", raw)
				}
			}
		}
	}
	if out == nil && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// --- SF-RBAC-001 -------------------------------------------------------------------

func checkRBAC(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	roles, err := s.Roles(ctx)
	if err != nil {
		return nil, err
	}
	croles, err := s.ClusterRoles(ctx)
	if err != nil {
		return nil, err
	}
	sas, err := s.ServiceAccountsAll(ctx)
	if err != nil {
		return nil, err
	}
	pods, _ := s.PodsAll(ctx)
	roleSet, croleSet, saSet, saUsed := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range roles {
		roleSet[r.Namespace+"/"+r.Name] = true
	}
	for _, r := range croles {
		croleSet[r.Name] = true
	}
	for _, sa := range sas {
		saSet[sa.Namespace+"/"+sa.Name] = true
	}
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodRunning {
			sa := pod.Spec.ServiceAccountName
			if sa == "" {
				sa = "default"
			}
			saUsed[pod.Namespace+"/"+sa] = true
		}
	}
	grace := time.Duration(p.T("SF-RBAC-001", "graceMinutes", 60)) * time.Minute
	var out []Finding
	examine := func(kind string, om metav1.ObjectMeta, refKind, refName string, subjects []struct{ kind, ns, name string }) {
		if p.Now.Sub(om.CreationTimestamp.Time) < grace || strings.HasPrefix(om.Name, "system:") {
			return
		}
		usedByRunningPod := false
		for _, sub := range subjects {
			if sub.kind == "ServiceAccount" && saUsed[sub.ns+"/"+sub.name] {
				usedByRunningPod = true
			}
		}
		missingRole := (refKind == "Role" && !roleSet[om.Namespace+"/"+refName]) || (refKind == "ClusterRole" && !croleSet[refName])
		if missingRole {
			sev := Medium
			if usedByRunningPod {
				sev = High
			}
			f := finding("SF-RBAC-001", "Binding points at a role or ServiceAccount that does not exist", sev, kind, om,
				fmt.Sprintf("roleRef %s %q does not exist; its subjects get no permissions from it", refKind, refName),
				"Recreate the missing role or point roleRef at the right one (roleRef cannot be edited: delete and recreate the binding)")
			f.Evidence = []string{"roleRef=" + refKind + "/" + refName}
			f.detailKey = "role/" + refName
			out = append(out, f)
		}
		for _, sub := range subjects {
			if sub.kind != "ServiceAccount" || saSet[sub.ns+"/"+sub.name] {
				continue
			}
			f := finding("SF-RBAC-001", "Binding points at a role or ServiceAccount that does not exist", Medium, kind, om,
				fmt.Sprintf("subject ServiceAccount %s/%s does not exist; the workload meant to get %s %q probably runs as a different account",
					sub.ns, sub.name, refKind, refName),
				"Fix the subject to the ServiceAccount the workload actually uses (see its pods' spec.serviceAccountName)")
			f.Evidence = []string{"subject=ServiceAccount/" + sub.ns + "/" + sub.name}
			f.detailKey = "sa/" + sub.ns + "/" + sub.name
			out = append(out, f)
		}
	}
	type sub = struct{ kind, ns, name string }
	if rbs, err := s.RoleBindings(ctx); err == nil {
		for _, rb := range rbs {
			var subs []sub
			for _, sj := range rb.Subjects {
				ns := sj.Namespace
				if ns == "" {
					ns = rb.Namespace
				}
				subs = append(subs, sub{sj.Kind, ns, sj.Name})
			}
			examine("RoleBinding", rb.ObjectMeta, rb.RoleRef.Kind, rb.RoleRef.Name, subs)
		}
	}
	if crbs, err := s.ClusterRoleBindings(ctx); err == nil {
		for _, rb := range crbs {
			var subs []sub
			for _, sj := range rb.Subjects {
				subs = append(subs, sub{sj.Kind, sj.Namespace, sj.Name})
			}
			examine("ClusterRoleBinding", rb.ObjectMeta, rb.RoleRef.Kind, rb.RoleRef.Name, subs)
		}
	}
	return out, nil
}

// --- SF-LEASE-001 -------------------------------------------------------------------

func checkLeases(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	leases, err := s.Leases(ctx)
	if err != nil {
		return nil, err
	}
	factor := p.T("SF-LEASE-001", "durationFactor", 5)
	var out []Finding
	for _, l := range leases {
		if l.Namespace == "kube-node-lease" || l.Spec.RenewTime == nil || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" {
			continue
		}
		dur := 15 * time.Second
		if l.Spec.LeaseDurationSeconds != nil {
			dur = time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
		}
		renewed := l.Spec.RenewTime.Time
		staleFor := p.Now.Sub(renewed)
		limit := time.Duration(factor * float64(dur))
		if limit < 2*time.Minute {
			limit = 2 * time.Minute
		}
		if staleFor < limit {
			continue
		}
		sev, hint := High, "the holder stopped renewing recently: is the controller running?"
		if staleFor > Days(7) {
			sev, hint = Low, "stale for a long time: probably left behind by an uninstalled component"
		}
		f := finding("SF-LEASE-001", "A controller stopped renewing its leader lease", sev, "Lease", l.ObjectMeta,
			fmt.Sprintf("lease not renewed for %s (duration %s); %s", duration.HumanDuration(staleFor), dur, hint),
			"Find the holder's pods ("+*l.Spec.HolderIdentity+") and check they are running and not crash-looping; delete the Lease if the component is gone")
		f.Since, f.SinceSource = tp(renewed), "spec.renewTime"
		f.Evidence = []string{"holderIdentity=" + *l.Spec.HolderIdentity}
		out = append(out, f)
	}
	return out, nil
}

// --- SF-API-001 ------------------------------------------------------------------------

func checkAPIServices(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	apis, err := s.Dynamic(ctx, gvrAPIService, false)
	if err != nil {
		return nil, err
	}
	hpas, _ := s.HPAs(ctx)
	var out []Finding
	for _, a := range apis {
		conds, _, _ := unstructured.NestedSlice(a.Object, "status", "conditions")
		for _, c := range conds {
			m, ok := c.(map[string]interface{})
			if !ok || m["type"] != "Available" || m["status"] != "False" {
				continue
			}
			since, _ := nestedTime(m, "lastTransitionTime")
			if !since.IsZero() && p.Now.Sub(since) < 10*time.Minute {
				continue
			}
			sev := High
			extra := ""
			if strings.HasSuffix(a.GetName(), ".metrics.k8s.io") && len(hpas) > 0 {
				sev = Critical
				extra = fmt.Sprintf("; %d HPAs cannot read metrics and are frozen", len(hpas))
			}
			reason, _ := m["reason"].(string)
			msg, _ := m["message"].(string)
			f := finding("SF-API-001", "Aggregated API unavailable", sev, "APIService", objMeta(a),
				fmt.Sprintf("Available=False (%s)%s", reason, extra),
				"Check the backing Service and its pods (spec.service); for metrics.k8s.io check metrics-server")
			f.Since, f.SinceSource = tp(since), "status.conditions[Available].lastTransitionTime"
			if msg != "" {
				f.Evidence = []string{"message=" + msg}
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// --- SF-WEBHOOK-001 -----------------------------------------------------------------------

type readyIndex map[string]int // "namespace/service" -> ready endpoints

func buildReady(ctx context.Context, s *Snapshot) (readyIndex, map[string]corev1.Service, error) {
	slices, err := s.EndpointSlicesAll(ctx)
	if err != nil {
		return nil, nil, err
	}
	svcs, err := s.ServicesAll(ctx)
	if err != nil {
		return nil, nil, err
	}
	ready := readyIndex{}
	for _, sl := range slices {
		svc := sl.Labels["kubernetes.io/service-name"]
		if svc == "" {
			continue
		}
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				ready[sl.Namespace+"/"+svc]++
			}
		}
	}
	byName := map[string]corev1.Service{}
	for _, sv := range svcs {
		byName[sv.Namespace+"/"+sv.Name] = sv
	}
	return ready, byName, nil
}

func checkWebhooks(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	ready, svcs, err := buildReady(ctx, s)
	if err != nil {
		return nil, err
	}
	var out []Finding
	examine := func(kind string, om metav1.ObjectMeta, name string, ref *admissionv1.ServiceReference, policy *admissionv1.FailurePolicyType) {
		if ref == nil {
			return
		}
		key := ref.Namespace + "/" + ref.Name
		fail := policy == nil || *policy == admissionv1.Fail
		var problem string
		if _, ok := svcs[key]; !ok {
			problem = "Service " + key + " does not exist"
		} else if ready[key] == 0 {
			problem = "Service " + key + " has no ready endpoints"
		} else {
			return
		}
		sev, effect := High, "failurePolicy=Ignore: the webhook is silently skipped"
		if fail {
			sev, effect = Critical, "failurePolicy=Fail: matching API requests are rejected"
		}
		f := finding("SF-WEBHOOK-001", "Admission webhook has no ready backend", sev, kind, om,
			fmt.Sprintf("webhook %s: %s; %s", name, problem, effect),
			"Restore the webhook's pods, or delete the webhook configuration if its controller was uninstalled")
		f.detailKey = name
		f.Evidence = []string{"clientConfig.service=" + key}
		out = append(out, f)
	}
	if v, err := s.ValidatingWebhooks(ctx); err == nil {
		for _, c := range v {
			for _, w := range c.Webhooks {
				examine("ValidatingWebhookConfiguration", c.ObjectMeta, w.Name, w.ClientConfig.Service, w.FailurePolicy)
			}
		}
	} else {
		return nil, err
	}
	if m, err := s.MutatingWebhooks(ctx); err == nil {
		for _, c := range m {
			for _, w := range c.Webhooks {
				examine("MutatingWebhookConfiguration", c.ObjectMeta, w.Name, w.ClientConfig.Service, w.FailurePolicy)
			}
		}
	}
	return out, nil
}

// --- SF-NET-001 ------------------------------------------------------------------------------

func checkServiceEndpoints(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	ready, _, err := buildReady(ctx, s)
	if err != nil {
		return nil, err
	}
	svcs, err := s.Services(ctx)
	if err != nil {
		return nil, err
	}
	pods, err := s.Pods(ctx)
	if err != nil {
		return nil, err
	}
	deps, _ := s.Deployments(ctx)
	stss, _ := s.StatefulSets(ctx)
	ingressRefs := map[string]bool{}
	if ings, err := s.Ingresses(ctx); err == nil {
		for _, ing := range ings {
			for _, b := range ingressBackends(ing.Spec.DefaultBackend, ing.Spec.Rules) {
				ingressRefs[ing.Namespace+"/"+b.name] = true
			}
		}
	}
	grace := time.Duration(p.T("SF-NET-001", "graceMinutes", 15)) * time.Minute
	var out []Finding
	for _, svc := range svcs {
		if len(svc.Spec.Selector) == 0 || svc.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}
		key := svc.Namespace + "/" + svc.Name
		if ready[key] > 0 || p.Now.Sub(svc.CreationTimestamp.Time) < grace {
			continue
		}
		sel := labels.SelectorFromSet(svc.Spec.Selector)
		var matching []corev1.Pod
		for _, pod := range pods {
			if pod.Namespace == svc.Namespace && sel.Matches(labels.Set(pod.Labels)) {
				matching = append(matching, pod)
			}
		}
		if len(matching) == 0 && scaledToZero(svc.Namespace, sel, deps, stss) {
			continue
		}
		var since time.Time
		for _, pod := range matching {
			if c, ok := cond(pod.Status.Conditions, func(c corev1.PodCondition) bool { return c.Type == corev1.PodReady && c.Status != corev1.ConditionTrue }); ok {
				if c.LastTransitionTime.After(since) {
					since = c.LastTransitionTime.Time
				}
			}
		}
		if !since.IsZero() && p.Now.Sub(since) < grace {
			continue
		}
		sev := Medium
		if (!since.IsZero() && p.Now.Sub(since) > 24*time.Hour) || ingressRefs[key] {
			sev = High
		}
		summary := fmt.Sprintf("no ready endpoints; %d pods match the selector, none Ready", len(matching))
		if len(matching) == 0 {
			summary = "no ready endpoints; no pods match the selector " + sel.String()
		}
		if ingressRefs[key] {
			summary += "; an Ingress routes traffic here"
		}
		f := finding("SF-NET-001", "Service has no ready endpoints", sev, "Service", svc.ObjectMeta, summary,
			"Compare the Service selector with the pods' labels, and check why matching pods are not Ready: kp pods -n "+svc.Namespace)
		if !since.IsZero() {
			f.Since, f.SinceSource = tp(since), "pods' Ready condition lastTransitionTime"
		}
		f.Evidence = []string{"selector=" + sel.String()}
		out = append(out, f)
	}
	return out, nil
}

// --- SF-NET-002 -------------------------------------------------------------------------------

// --- SF-HPA-001 ----------------------------------------------------------------------------------

func checkHPAs(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	hpas, err := s.HPAs(ctx)
	if err != nil {
		return nil, err
	}
	grace := time.Duration(p.T("SF-HPA-001", "graceMinutes", 60)) * time.Minute
	var out []Finding
	for _, h := range hpas {
		c, ok := cond(h.Status.Conditions, func(c autoscalingv2.HorizontalPodAutoscalerCondition) bool {
			return (c.Type == autoscalingv2.ScalingActive && c.Status == corev1.ConditionFalse && c.Reason != "ScalingDisabled") ||
				(c.Type == autoscalingv2.AbleToScale && c.Status == corev1.ConditionFalse)
		})
		if !ok || p.Now.Sub(c.LastTransitionTime.Time) < grace {
			continue
		}
		sev := Medium
		if p.Now.Sub(c.LastTransitionTime.Time) > 24*time.Hour {
			sev = High
		}
		f := finding("SF-HPA-001", "HPA cannot scale", sev, "HorizontalPodAutoscaler", h.ObjectMeta,
			fmt.Sprintf("%s=False (%s); replicas frozen at %d", c.Type, c.Reason, h.Status.CurrentReplicas),
			"kubectl describe hpa "+h.Name+" -n "+h.Namespace+"; usually metrics-server or a custom metrics adapter is down, or the target lacks resource requests")
		f.Since, f.SinceSource = tp(c.LastTransitionTime.Time), "status.conditions["+string(c.Type)+"].lastTransitionTime"
		if c.Message != "" {
			f.Evidence = []string{"message=" + c.Message}
		}
		out = append(out, f)
	}
	return out, nil
}

// --- SF-PDB-001 ------------------------------------------------------------------------------------

func checkPDBs(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	pdbs, err := s.PDBs(ctx)
	if err != nil {
		return nil, err
	}
	grace := time.Duration(p.T("SF-PDB-001", "graceMinutes", 30)) * time.Minute
	var out []Finding
	for _, pdb := range pdbs {
		if pdb.Status.ExpectedPods == 0 || pdb.Status.DisruptionsAllowed > 0 {
			continue
		}
		var since time.Time
		if c, ok := cond(pdb.Status.Conditions, func(c metav1.Condition) bool {
			return c.Type == "DisruptionAllowed" && c.Status == metav1.ConditionFalse
		}); ok {
			since = c.LastTransitionTime.Time
		}
		if !since.IsZero() && p.Now.Sub(since) < grace {
			continue
		}
		f := finding("SF-PDB-001", "PodDisruptionBudget allows zero disruptions", Medium, "PodDisruptionBudget", pdb.ObjectMeta,
			fmt.Sprintf("allows 0 disruptions (%d of %d pods healthy, %d required); node drains and upgrades will block",
				pdb.Status.CurrentHealthy, pdb.Status.ExpectedPods, pdb.Status.DesiredHealthy),
			"Add replicas, or relax minAvailable/maxUnavailable so at least one pod may be evicted")
		if !since.IsZero() {
			f.Since, f.SinceSource = tp(since), "status.conditions[DisruptionAllowed].lastTransitionTime"
		}
		out = append(out, f)
	}
	return out, nil
}

// --- SF-ROLLOUT-001 ------------------------------------------------------------------------------------

// --- SF-FINALIZER-001 ----------------------------------------------------------------------------------

// --- SF-VOL-001 ------------------------------------------------------------------------------------------

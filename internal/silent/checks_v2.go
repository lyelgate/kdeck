package silent

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/duration"
)

func init() {
	Registry = append(Registry,
		Check{"SF-CERT-003", "API server serving certificate expiring", "certificate", false, checkAPIServerCert},
		Check{"SF-CERT-004", "Certificate signing requests left pending", "certificate", false, checkCSRs},
		Check{"SF-ROLLOUT-002", "StatefulSet or DaemonSet rollout never finishes", "workload", false, checkSTSDS},
		Check{"SF-VOL-002", "Filesystem resize never finished", "storage", false, checkResizePending},
		Check{"SF-VOL-003", "Volume nearly full, or smaller than its PVC", "storage", true, checkVolumeUsage},
		Check{"SF-VOL-004", "Released or Failed PersistentVolumes still billing", "storage", false, checkReleasedPVs},
		Check{"SF-NODE-001", "Node cordoned and forgotten", "node", false, checkCordoned},
		Check{"SF-NODE-002", "Node NotReady for a long time", "node", false, checkNotReady},
		Check{"SF-QUOTA-001", "ResourceQuota exhausted", "workload", false, checkQuotas},
		Check{"SF-POD-001", "Failed and evicted pods piling up", "workload", false, checkFailedPods},
		Check{"SF-HELM-001", "Helm release stuck pending or failed", "gitops", false, checkHelm},
		Check{"SF-GITOPS-001", "Argo CD application or Flux object unhealthy", "gitops", false, checkGitOps},
		Check{"SF-API-002", "Deprecated APIs still being called", "control", false, checkDeprecatedAPIs},
	)
}

// --- SF-CERT-003 -------------------------------------------------------------------

func checkAPIServerCert(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	if s.Host == "" {
		return nil, errNotInstalled
	}
	u, err := url.Parse(s.Host)
	if err != nil || u.Scheme != "https" {
		return nil, nil
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	// Only the expiry date is read; nothing is sent over this connection.
	conn, err := tls.DialWithDialer(d, "tcp", host, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, nil
	}
	c := certs[0]
	sev, when, ok := certSeverity(p, "SF-CERT-003", c.NotAfter, p.Now)
	if !ok {
		return nil, nil
	}
	f := Finding{Check: "SF-CERT-003", Title: "API server serving certificate expiring", Severity: sev, Kind: "APIServer", Name: u.Host,
		Summary:     fmt.Sprintf("serving certificate %q %s", certName(c), when),
		Remediation: "Rotate the API server certificate (kubeadm: kubeadm certs renew apiserver; managed clusters rotate it themselves, so contact the provider if it is close)"}
	if c.NotAfter.Before(p.Now) {
		f.Since, f.SinceSource = tp(c.NotAfter), "certificate NotAfter"
	}
	f.Evidence = []string{"notAfter=" + c.NotAfter.UTC().Format(time.RFC3339), "issuer=" + c.Issuer.CommonName}
	return []Finding{f}, nil
}

// --- SF-CERT-004 -------------------------------------------------------------------

func checkCSRs(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	csrs, err := s.CSRs(ctx)
	if err != nil {
		return nil, err
	}
	grace := time.Duration(p.T("SF-CERT-004", "graceMinutes", 60)) * time.Minute
	var out []Finding
	for _, c := range csrs {
		if len(c.Status.Conditions) > 0 || p.Now.Sub(c.CreationTimestamp.Time) < grace {
			continue
		}
		sev := Medium
		if c.Spec.SignerName == certificatesv1.KubeletServingSignerName || c.Spec.SignerName == certificatesv1.KubeAPIServerClientKubeletSignerName {
			sev = High
		}
		f := finding("SF-CERT-004", "Certificate signing requests left pending", sev, "CertificateSigningRequest", c.ObjectMeta,
			fmt.Sprintf("pending for %s, signer %s, requested by %s", duration.HumanDuration(p.Now.Sub(c.CreationTimestamp.Time)), c.Spec.SignerName, c.Spec.Username),
			"Approve if expected (kubectl certificate approve "+c.Name+"); kubelet serving certificates usually need an approver such as kubelet-csr-approver")
		f.Since, f.SinceSource = tp(c.CreationTimestamp.Time), "metadata.creationTimestamp"
		out = append(out, f)
	}
	return out, nil
}

// --- SF-ROLLOUT-002 -------------------------------------------------------------------

func checkSTSDS(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	stss, err := s.StatefulSets(ctx)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, st := range stss {
		want := deref(st.Spec.Replicas, 1)
		if st.Status.UpdateRevision == "" || st.Status.UpdateRevision == st.Status.CurrentRevision || st.Status.UpdatedReplicas >= want {
			continue
		}
		sev := Medium
		hint := "check the newest pod (kubectl rollout status sts/" + st.Name + "); a pod that never becomes Ready stops the rollout"
		if st.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
			sev, hint = Low, "updateStrategy is OnDelete: pods only update when you delete them"
		}
		f := finding("SF-ROLLOUT-002", "StatefulSet or DaemonSet rollout never finishes", sev, "StatefulSet", st.ObjectMeta,
			fmt.Sprintf("rollout stuck: %d of %d replicas on the new revision, %d ready", st.Status.UpdatedReplicas, want, st.Status.ReadyReplicas), hint)
		f.Evidence = []string{"currentRevision=" + st.Status.CurrentRevision, "updateRevision=" + st.Status.UpdateRevision}
		out = append(out, f)
	}
	dss, err := s.DaemonSets(ctx)
	if err != nil {
		return out, nil
	}
	for _, ds := range dss {
		st := ds.Status
		if st.DesiredNumberScheduled == 0 || (st.NumberUnavailable == 0 && st.UpdatedNumberScheduled >= st.DesiredNumberScheduled) {
			continue
		}
		sev := Medium
		if ds.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType && st.NumberUnavailable == 0 {
			sev = Low
		}
		f := finding("SF-ROLLOUT-002", "StatefulSet or DaemonSet rollout never finishes", sev, "DaemonSet", ds.ObjectMeta,
			fmt.Sprintf("%d of %d nodes updated, %d unavailable", st.UpdatedNumberScheduled, st.DesiredNumberScheduled, st.NumberUnavailable),
			"kdeck pods "+ds.Name+" -n "+ds.Namespace+" --bad; a pod failing on some nodes (taints, resources, image) blocks the rollout there")
		out = append(out, f)
	}
	return out, nil
}

// --- SF-VOL-002 ----------------------------------------------------------------------

func checkResizePending(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	pvcs, err := s.PVCs(ctx)
	if err != nil {
		return nil, err
	}
	grace := time.Duration(p.T("SF-VOL-002", "graceMinutes", 30)) * time.Minute
	var out []Finding
	for _, c := range pvcs {
		cd, ok := cond(c.Status.Conditions, func(x corev1.PersistentVolumeClaimCondition) bool {
			return (x.Type == corev1.PersistentVolumeClaimFileSystemResizePending || x.Type == corev1.PersistentVolumeClaimResizing) && x.Status == corev1.ConditionTrue
		})
		if !ok || p.Now.Sub(cd.LastTransitionTime.Time) < grace {
			continue
		}
		req := c.Spec.Resources.Requests[corev1.ResourceStorage]
		have := c.Status.Capacity[corev1.ResourceStorage]
		f := finding("SF-VOL-002", "Filesystem resize never finished", High, "PersistentVolumeClaim", c.ObjectMeta,
			fmt.Sprintf("%s for %s: requested %s, filesystem still %s", cd.Type, duration.HumanDuration(p.Now.Sub(cd.LastTransitionTime.Time)), req.String(), have.String()),
			"FileSystemResizePending finishes when a pod mounts the volume; restart the pod using it. If it persists, the CSI node plugin did not grow the filesystem")
		f.Since, f.SinceSource = tp(cd.LastTransitionTime.Time), "status.conditions["+string(cd.Type)+"].lastTransitionTime"
		out = append(out, f)
	}
	return out, nil
}

// --- SF-VOL-003 (sensitive: kubelet API) -------------------------------------------------

type statsSummary struct {
	Pods []struct {
		Volume []struct {
			Name          string  `json:"name"`
			CapacityBytes *uint64 `json:"capacityBytes"`
			UsedBytes     *uint64 `json:"usedBytes"`
			PVCRef        *struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"pvcRef"`
		} `json:"volume"`
	} `json:"pods"`
}

func checkVolumeUsage(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	pvcs, _ := s.PVCs(ctx)
	byKey := map[string]corev1.PersistentVolumeClaim{}
	for _, c := range pvcs {
		byKey[c.Namespace+"/"+c.Name] = c
	}
	warn, crit := p.T("SF-VOL-003", "warnPercent", 85), p.T("SF-VOL-003", "highPercent", 95)
	seen := map[string]bool{}
	var out []Finding
	var lastErr error
	for _, n := range nodes {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		raw, err := s.NodeStats(cctx, n.Name)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		var sum statsSummary
		if json.Unmarshal(raw, &sum) != nil {
			continue
		}
		for _, pod := range sum.Pods {
			for _, v := range pod.Volume {
				if v.PVCRef == nil || v.CapacityBytes == nil || v.UsedBytes == nil || *v.CapacityBytes == 0 {
					continue
				}
				key := v.PVCRef.Namespace + "/" + v.PVCRef.Name
				if seen[key] || (s.Namespace != "" && v.PVCRef.Namespace != s.Namespace) {
					continue
				}
				seen[key] = true
				c := byKey[key]
				om := c.ObjectMeta
				if om.Name == "" {
					om = metav1.ObjectMeta{Name: v.PVCRef.Name, Namespace: v.PVCRef.Namespace}
				}
				capB, used := float64(*v.CapacityBytes), float64(*v.UsedBytes)
				pct := used / capB * 100
				if pct >= warn {
					sev := Medium
					if pct >= crit {
						sev = High
					}
					f := finding("SF-VOL-003", "Volume nearly full, or smaller than its PVC", sev, "PersistentVolumeClaim", om,
						fmt.Sprintf("%.0f%% full (%s of %s)", pct, bytes(used), bytes(capB)),
						"Grow the PVC (allowVolumeExpansion) or clean up data before writes start failing")
					f.detailKey = "usage"
					out = append(out, f)
				}
				if want, ok := c.Status.Capacity[corev1.ResourceStorage]; ok && capB < 0.9*float64(want.Value()) {
					f := finding("SF-VOL-003", "Volume nearly full, or smaller than its PVC", High, "PersistentVolumeClaim", om,
						fmt.Sprintf("filesystem is %s but the PVC says %s: the resize never reached the filesystem", bytes(capB), want.String()),
						"Restart the pod using it; if that does not help, grow the filesystem from the node (resize2fs/xfs_growfs) and check the CSI node plugin")
					f.detailKey = "size"
					out = append(out, f)
				}
			}
		}
	}
	if out == nil && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

func bytes(b float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

// --- SF-VOL-004 ------------------------------------------------------------------------

func checkReleasedPVs(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	pvs, err := s.PVs(ctx)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, v := range pvs {
		if v.Status.Phase != corev1.VolumeReleased && v.Status.Phase != corev1.VolumeFailed {
			continue
		}
		if v.Labels["silent.lyelgate.github.io/keep"] == "true" {
			continue
		}
		size := v.Spec.Capacity[corev1.ResourceStorage]
		claim := "-"
		if v.Spec.ClaimRef != nil {
			claim = v.Spec.ClaimRef.Namespace + "/" + v.Spec.ClaimRef.Name
		}
		f := finding("SF-VOL-004", "Released or Failed PersistentVolumes still billing", Low, "PersistentVolume", v.ObjectMeta,
			fmt.Sprintf("%s, %s, storage class %q, former claim %s, reclaim policy %s", v.Status.Phase, size.String(), v.Spec.StorageClassName, claim, v.Spec.PersistentVolumeReclaimPolicy),
			"If the data is not needed, delete the PV and the backing disk (Retain policy leaves the disk billing); label it silent.lyelgate.github.io/keep=true to keep it on purpose")
		out = append(out, f)
	}
	return out, nil
}

// --- SF-NODE-001 / 002 -------------------------------------------------------------------

func checkCordoned(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, n := range nodes {
		if !n.Spec.Unschedulable {
			continue
		}
		f := finding("SF-NODE-001", "Node cordoned and forgotten", Low, "Node", n.ObjectMeta,
			"cordoned: no new pods are scheduled here (Kubernetes records no time for this)",
			"kubectl uncordon "+n.Name+" when the maintenance is over, or drain and remove the node")
		for _, t := range n.Spec.Taints {
			if t.Key == corev1.TaintNodeUnschedulable && t.TimeAdded != nil {
				f.Since, f.SinceSource = tp(t.TimeAdded.Time), "unschedulable taint timeAdded"
				if p.Now.Sub(t.TimeAdded.Time) > Days(14) {
					f.Severity = Medium
				}
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func checkNotReady(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	grace := time.Duration(p.T("SF-NODE-002", "graceMinutes", 60)) * time.Minute
	var out []Finding
	for _, n := range nodes {
		c, ok := cond(n.Status.Conditions, func(c corev1.NodeCondition) bool {
			return c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue
		})
		if !ok || p.Now.Sub(c.LastTransitionTime.Time) < grace {
			continue
		}
		f := finding("SF-NODE-002", "Node NotReady for a long time", High, "Node", n.ObjectMeta,
			fmt.Sprintf("Ready=%s for %s (%s); its pods were evicted or are stuck, capacity is lower than it looks", c.Status, duration.HumanDuration(p.Now.Sub(c.LastTransitionTime.Time)), c.Reason),
			"Check the kubelet on the node, or remove the node so the autoscaler or provider replaces it")
		f.Since, f.SinceSource = tp(c.LastTransitionTime.Time), "status.conditions[Ready].lastTransitionTime"
		out = append(out, f)
	}
	return out, nil
}

// --- SF-QUOTA-001 ----------------------------------------------------------------------------

func checkQuotas(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	qs, err := s.ResourceQuotas(ctx)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, q := range qs {
		var full []string
		for res, hard := range q.Status.Hard {
			used, ok := q.Status.Used[res]
			if ok && hard.Cmp(used) <= 0 && !hard.IsZero() {
				full = append(full, fmt.Sprintf("%s %s/%s", res, used.String(), hard.String()))
			}
		}
		if len(full) == 0 {
			continue
		}
		sort.Strings(full)
		f := finding("SF-QUOTA-001", "ResourceQuota exhausted", Medium, "ResourceQuota", q.ObjectMeta,
			"at its limit: "+strings.Join(full, ", ")+"; new pods or objects in this namespace are rejected",
			"Raise the quota or free resources; ReplicaSets hitting it show ReplicaFailure (see SF-ROLLOUT-001)")
		out = append(out, f)
	}
	return out, nil
}

// --- SF-POD-001 ----------------------------------------------------------------------------------

func checkFailedPods(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	pods, err := s.Pods(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(p.T("SF-POD-001", "maxFailed", 100))
	count := map[string]int{}
	oldest := map[string]time.Time{}
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodFailed {
			continue
		}
		count[pod.Namespace]++
		if t := pod.CreationTimestamp.Time; oldest[pod.Namespace].IsZero() || t.Before(oldest[pod.Namespace]) {
			oldest[pod.Namespace] = t
		}
	}
	var out []Finding
	for ns, n := range count {
		if n < limit {
			continue
		}
		f := Finding{Check: "SF-POD-001", Title: "Failed and evicted pods piling up", Severity: Low, Kind: "Namespace", Name: ns,
			Summary:     fmt.Sprintf("%d pods in phase Failed (evicted or errored) are left behind", n),
			Remediation: "kdeck clean -n " + ns + " --yes, and look at why pods are being evicted (node pressure)"}
		f.Since, f.SinceSource = tp(oldest[ns]), "oldest failed pod"
		out = append(out, f)
	}
	return out, nil
}

// --- SF-HELM-001 ---------------------------------------------------------------------------------

func checkHelm(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	secrets, err := s.HelmReleaseSecrets(ctx)
	if err != nil {
		return nil, err
	}
	type rel struct {
		version int
		status  string
		meta    metav1.ObjectMeta
	}
	latest := map[string]rel{}
	for _, sec := range secrets {
		name := sec.Labels["name"]
		v, _ := strconv.Atoi(sec.Labels["version"])
		key := sec.Namespace + "/" + name
		if name == "" {
			continue
		}
		if r, ok := latest[key]; !ok || v > r.version {
			latest[key] = rel{v, sec.Labels["status"], sec.ObjectMeta}
		}
	}
	grace := time.Duration(p.T("SF-HELM-001", "graceMinutes", 15)) * time.Minute
	var out []Finding
	for key, r := range latest {
		ns, name, _ := strings.Cut(key, "/")
		age := p.Now.Sub(r.meta.CreationTimestamp.Time)
		var sev Severity
		var summary, fix string
		switch {
		case strings.HasPrefix(r.status, "pending-") && age >= grace:
			sev = High
			summary = fmt.Sprintf("revision %d stuck in %s for %s; every later helm upgrade fails with \"another operation is in progress\"", r.version, r.status, duration.HumanDuration(age))
			fix = fmt.Sprintf("helm history %s -n %s, then helm rollback %s <last-deployed-revision> -n %s", name, ns, name, ns)
		case r.status == "failed":
			sev = Medium
			summary = fmt.Sprintf("latest revision %d failed; the cluster runs an older revision than the chart says", r.version)
			fix = fmt.Sprintf("helm history %s -n %s to see the error, fix the values and upgrade again", name, ns)
		default:
			continue
		}
		f := Finding{Check: "SF-HELM-001", Title: "Helm release stuck pending or failed", Severity: sev, Kind: "HelmRelease", Namespace: ns, Name: name,
			Summary: summary, Remediation: fix}
		f.Since, f.SinceSource = tp(r.meta.CreationTimestamp.Time), "release Secret creationTimestamp"
		f.Evidence = []string{fmt.Sprintf("status=%s version=%d", r.status, r.version)}
		out = append(out, f)
	}
	return out, nil
}

// --- SF-GITOPS-001 ---------------------------------------------------------------------------------

func checkGitOps(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	grace := time.Duration(p.T("SF-GITOPS-001", "graceMinutes", 30)) * time.Minute
	var out []Finding
	found := false
	if apps, err := s.Dynamic(ctx, gvrArgoApp, true); err == nil {
		found = true
		for _, a := range apps {
			health, _, _ := unstructured.NestedString(a.Object, "status", "health", "status")
			sync, _, _ := unstructured.NestedString(a.Object, "status", "sync", "status")
			var since time.Time
			if t, ok := nestedTime(a.Object, "status", "operationState", "finishedAt"); ok {
				since = t
			}
			var sev Severity
			var what string
			switch {
			case health == "Degraded" || health == "Missing":
				sev, what = High, "health "+health
			case sync == "OutOfSync":
				sev, what = Medium, "OutOfSync with Git"
			default:
				continue
			}
			if !since.IsZero() && p.Now.Sub(since) < grace {
				continue
			}
			f := finding("SF-GITOPS-001", "Argo CD application or Flux object unhealthy", sev, "Application", objMeta(a),
				fmt.Sprintf("Argo CD application %s (sync %s, health %s)", what, sync, health),
				"argocd app get "+a.GetName()+"; an OutOfSync app with auto-sync off means Git and the cluster have quietly drifted")
			if !since.IsZero() {
				f.Since, f.SinceSource = tp(since), "status.operationState.finishedAt"
			}
			out = append(out, f)
		}
	}
	for _, flux := range []struct {
		kind string
		list func() ([]unstructured.Unstructured, error)
	}{
		{"Kustomization", func() ([]unstructured.Unstructured, error) { return s.Dynamic(ctx, gvrFluxKust, true) }},
		{"HelmRelease", func() ([]unstructured.Unstructured, error) { return s.Dynamic(ctx, gvrFluxHelm, true) }},
		{"GitRepository", func() ([]unstructured.Unstructured, error) { return s.Dynamic(ctx, gvrFluxGit, true) }},
	} {
		items, err := flux.list()
		if err != nil {
			continue
		}
		found = true
		for _, o := range items {
			if susp, _, _ := unstructured.NestedBool(o.Object, "spec", "suspend"); susp {
				f := finding("SF-GITOPS-001", "Argo CD application or Flux object unhealthy", Low, flux.kind, objMeta(o),
					"Flux "+flux.kind+" is suspended: changes in Git are not applied", "flux resume "+strings.ToLower(flux.kind)+" "+o.GetName()+" -n "+o.GetNamespace()+" when ready")
				f.detailKey = "suspended"
				out = append(out, f)
				continue
			}
			conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
			for _, c := range conds {
				m, ok := c.(map[string]interface{})
				if !ok || m["type"] != "Ready" || m["status"] != "False" {
					continue
				}
				since, _ := nestedTime(m, "lastTransitionTime")
				if !since.IsZero() && p.Now.Sub(since) < grace {
					continue
				}
				reason, _ := m["reason"].(string)
				msg, _ := m["message"].(string)
				f := finding("SF-GITOPS-001", "Argo CD application or Flux object unhealthy", High, flux.kind, objMeta(o),
					fmt.Sprintf("Flux %s not Ready (%s)", flux.kind, reason),
					"flux get "+strings.ToLower(flux.kind)+"s -n "+o.GetNamespace()+"; Git changes are not reaching the cluster")
				f.Since, f.SinceSource = tp(since), "status.conditions[Ready].lastTransitionTime"
				if msg != "" {
					f.Evidence = []string{"message=" + firstLine(msg)}
				}
				out = append(out, f)
			}
		}
	}
	if !found {
		return nil, errNotInstalled
	}
	return out, nil
}

// --- SF-API-002 ------------------------------------------------------------------------------------

// DeprecatedCalls parses apiserver_requested_deprecated_apis from /metrics.
func DeprecatedCalls(metrics string) []map[string]string {
	var out []map[string]string
	sc := bufio.NewScanner(strings.NewReader(metrics))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "apiserver_requested_deprecated_apis{") {
			continue
		}
		end := strings.LastIndex(line, "}")
		if end < 0 || strings.TrimSpace(line[end+1:]) != "1" {
			continue
		}
		labels := map[string]string{}
		for _, kv := range splitLabels(line[len("apiserver_requested_deprecated_apis{"):end]) {
			k, v, _ := strings.Cut(kv, "=")
			labels[k] = strings.Trim(v, `"`)
		}
		out = append(out, labels)
	}
	return out
}

func splitLabels(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
		case r == ',' && !inQ:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func checkDeprecatedAPIs(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	metrics, err := s.APIMetrics(ctx)
	if err != nil {
		return nil, err
	}
	serverMinor := -1
	if v, err := s.CS.Discovery().ServerVersion(); err == nil {
		serverMinor, _ = strconv.Atoi(strings.TrimRight(v.Minor, "+"))
	}
	var out []Finding
	for _, l := range DeprecatedCalls(metrics) {
		gv := l["version"]
		if l["group"] != "" {
			gv = l["group"] + "/" + l["version"]
		}
		res := l["resource"]
		if l["subresource"] != "" {
			res += "/" + l["subresource"]
		}
		sev := Medium
		removed := l["removed_release"]
		if parts := strings.Split(removed, "."); len(parts) == 2 && serverMinor >= 0 {
			if m, err := strconv.Atoi(parts[1]); err == nil && m <= serverMinor+1 {
				sev = High
			}
		}
		summary := fmt.Sprintf("something still calls %s %s", gv, res)
		if removed != "" {
			summary += ", removed in " + removed
		}
		f := Finding{Check: "SF-API-002", Title: "Deprecated APIs still being called", Severity: sev, Kind: "API", Name: gv + "/" + res,
			Summary:     summary + "; the next upgrade breaks that client",
			Remediation: "Find the caller in the API server audit log (userAgent) and move it to the newer API version; manifests: kubectl convert or pluto"}
		f.Evidence = []string{"apiserver_requested_deprecated_apis (since the API server last restarted)"}
		out = append(out, f)
	}
	return out, nil
}

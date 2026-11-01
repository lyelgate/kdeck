package silent

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/duration"
)

// scaledToZero reports whether every Deployment or StatefulSet whose pod
// template matches sel in ns is scaled to zero on purpose. With no matching
// workload at all it returns false, so an orphaned Service is still reported.
func scaledToZero(ns string, sel labels.Selector, deps []appsv1.Deployment, stss []appsv1.StatefulSet) bool {
	found := false
	for _, d := range deps {
		if d.Namespace == ns && sel.Matches(labels.Set(d.Spec.Template.Labels)) {
			found = true
			if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 {
				return false
			}
		}
	}
	for _, st := range stss {
		if st.Namespace == ns && sel.Matches(labels.Set(st.Spec.Template.Labels)) {
			found = true
			if st.Spec.Replicas == nil || *st.Spec.Replicas != 0 {
				return false
			}
		}
	}
	return found
}

// --- SF-NET-002 -----------------------------------------------------------------

type backendRef struct {
	name string
	port string // number or name; "" when unset
}

func ingressBackends(def *networkingv1.IngressBackend, rules []networkingv1.IngressRule) []backendRef {
	var out []backendRef
	add := func(b *networkingv1.IngressBackend) {
		if b == nil || b.Service == nil {
			return
		}
		port := b.Service.Port.Name
		if b.Service.Port.Number != 0 {
			port = strconv.Itoa(int(b.Service.Port.Number))
		}
		out = append(out, backendRef{b.Service.Name, port})
	}
	add(def)
	for _, r := range rules {
		if r.HTTP == nil {
			continue
		}
		for i := range r.HTTP.Paths {
			add(&r.HTTP.Paths[i].Backend)
		}
	}
	return out
}

func servicePortExists(svc corev1.Service, port string) bool {
	if port == "" {
		return true
	}
	for _, sp := range svc.Spec.Ports {
		if sp.Name == port || strconv.Itoa(int(sp.Port)) == port {
			return true
		}
	}
	return false
}

func checkRoutes(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	var out []Finding
	ings, err := s.Ingresses(ctx)
	if err != nil {
		return nil, err
	}
	svcs, err := s.Services(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]corev1.Service{}
	for _, sv := range svcs {
		byName[sv.Namespace+"/"+sv.Name] = sv
	}
	for _, ing := range ings {
		for _, b := range ingressBackends(ing.Spec.DefaultBackend, ing.Spec.Rules) {
			svc, ok := byName[ing.Namespace+"/"+b.name]
			var problem string
			switch {
			case !ok:
				problem = fmt.Sprintf("backend Service %q does not exist", b.name)
			case !servicePortExists(svc, b.port):
				problem = fmt.Sprintf("backend Service %q has no port %q", b.name, b.port)
			default:
				continue
			}
			f := finding("SF-NET-002", "Ingress or HTTPRoute sends traffic nowhere", High, "Ingress", ing.ObjectMeta, problem+"; requests get 503 or 404",
				"Fix the backend name/port in the Ingress, or restore the Service")
			f.detailKey = b.name + ":" + b.port
			out = append(out, f)
		}
	}
	// Gateway API routes report their own health in status.parents.
	if routes, err := s.Dynamic(ctx, gvrHTTPRoute, true); err == nil {
		for _, r := range routes {
			parents, _, _ := unstructured.NestedSlice(r.Object, "status", "parents")
			for _, pr := range parents {
				pm, ok := pr.(map[string]interface{})
				if !ok {
					continue
				}
				conds, _, _ := unstructured.NestedSlice(pm, "conditions")
				for _, c := range conds {
					cm, ok := c.(map[string]interface{})
					if !ok || cm["status"] != "False" || (cm["type"] != "Accepted" && cm["type"] != "ResolvedRefs") {
						continue
					}
					since, _ := nestedTime(cm, "lastTransitionTime")
					reason, _ := cm["reason"].(string)
					msg, _ := cm["message"].(string)
					f := finding("SF-NET-002", "Ingress or HTTPRoute sends traffic nowhere", High, "HTTPRoute", objMeta(r),
						fmt.Sprintf("%v=False (%s)", cm["type"], reason),
						"kubectl describe httproute "+r.GetName()+" -n "+r.GetNamespace()+"; fix the parentRef or backendRefs it names")
					f.Since, f.SinceSource = tp(since), "status.parents[].conditions.lastTransitionTime"
					if msg != "" {
						f.Evidence = []string{"message=" + msg}
					}
					f.detailKey = fmt.Sprint(cm["type"])
					out = append(out, f)
				}
			}
		}
	}
	return out, nil
}

// --- SF-ROLLOUT-001 ---------------------------------------------------------------

func checkRollouts(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	deps, err := s.Deployments(ctx)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, d := range deps {
		if d.Spec.Paused {
			continue
		}
		c, ok := cond(d.Status.Conditions, func(c appsv1.DeploymentCondition) bool {
			return c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded"
		})
		if !ok {
			continue
		}
		f := finding("SF-ROLLOUT-001", "Deployment stuck or ReplicaSet cannot create pods", High, "Deployment", d.ObjectMeta,
			fmt.Sprintf("rollout exceeded its progress deadline; %d of %d replicas updated", d.Status.UpdatedReplicas, deref(d.Spec.Replicas, 1)),
			"kubectl rollout status deploy/"+d.Name+" -n "+d.Namespace+"; check the new pods, then roll back with: kubectl rollout undo deploy/"+d.Name)
		f.Since, f.SinceSource = tp(c.LastTransitionTime.Time), "status.conditions[Progressing].lastTransitionTime"
		if c.Message != "" {
			f.Evidence = []string{"message=" + c.Message}
		}
		out = append(out, f)
	}
	rss, err := s.ReplicaSets(ctx)
	if err != nil {
		return out, nil
	}
	for _, rs := range rss {
		if deref(rs.Spec.Replicas, 1) == 0 {
			continue
		}
		c, ok := cond(rs.Status.Conditions, func(c appsv1.ReplicaSetCondition) bool {
			return c.Type == appsv1.ReplicaSetReplicaFailure && c.Status == corev1.ConditionTrue
		})
		if !ok {
			continue
		}
		f := finding("SF-ROLLOUT-001", "Deployment stuck or ReplicaSet cannot create pods", High, "ReplicaSet", rs.ObjectMeta,
			fmt.Sprintf("cannot create pods (%s); %d of %d exist", c.Reason, rs.Status.Replicas, deref(rs.Spec.Replicas, 1)),
			"Read the message: common causes are an exhausted ResourceQuota, a missing ServiceAccount or an admission webhook denial")
		f.Since, f.SinceSource = tp(c.LastTransitionTime.Time), "status.conditions[ReplicaFailure].lastTransitionTime"
		if c.Message != "" {
			f.Evidence = []string{"message=" + c.Message}
		}
		out = append(out, f)
	}
	return out, nil
}

func deref(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

// --- SF-FINALIZER-001 --------------------------------------------------------------

func checkFinalizers(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	grace := time.Duration(p.T("SF-FINALIZER-001", "graceMinutes", 60)) * time.Minute
	var out []Finding
	examine := func(kind string, om metav1.ObjectMeta, sev Severity) {
		if om.DeletionTimestamp == nil || len(om.Finalizers) == 0 {
			return
		}
		since := om.DeletionTimestamp.Time
		if p.Now.Sub(since) < grace {
			return
		}
		f := finding("SF-FINALIZER-001", "Object stuck in deletion", sev, kind, om,
			fmt.Sprintf("deletion requested %s ago, still waiting on finalizers %v", duration.HumanDuration(p.Now.Sub(since)), om.Finalizers),
			"Find the controller that owns the finalizer and fix it; remove the finalizer by hand only once you know what cleanup it skips")
		f.Since, f.SinceSource = tp(since), "metadata.deletionTimestamp"
		out = append(out, f)
	}
	var firstErr error
	if nss, err := s.Namespaces(ctx); err == nil {
		for _, n := range nss {
			if s.Namespace != "" && n.Name != s.Namespace {
				continue
			}
			om := n.ObjectMeta
			// Namespaces carry spec.finalizers rather than metadata.finalizers.
			if om.DeletionTimestamp != nil && len(om.Finalizers) == 0 && len(n.Spec.Finalizers) > 0 {
				for _, f := range n.Spec.Finalizers {
					om.Finalizers = append(om.Finalizers, string(f))
				}
			}
			examine("Namespace", om, High)
		}
	} else {
		firstErr = err
	}
	if pvcs, err := s.PVCs(ctx); err == nil {
		for _, c := range pvcs {
			examine("PersistentVolumeClaim", c.ObjectMeta, High)
		}
	}
	if pvs, err := s.PVs(ctx); err == nil {
		for _, v := range pvs {
			examine("PersistentVolume", v.ObjectMeta, Medium)
		}
	}
	if pods, err := s.Pods(ctx); err == nil {
		for _, pod := range pods {
			examine("Pod", pod.ObjectMeta, Medium)
		}
	}
	if out == nil && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// --- SF-VOL-001 -----------------------------------------------------------------------

func checkVolumeAttachments(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error) {
	vas, err := s.VolumeAttachments(ctx)
	if err != nil {
		return nil, err
	}
	grace := time.Duration(p.T("SF-VOL-001", "graceMinutes", 10)) * time.Minute
	var out []Finding
	for _, va := range vas {
		pv := ""
		if va.Spec.Source.PersistentVolumeName != nil {
			pv = *va.Spec.Source.PersistentVolumeName
		}
		var summary, source string
		var since time.Time
		switch {
		case va.Status.AttachError != nil:
			since, source = va.Status.AttachError.Time.Time, "status.attachError.time"
			summary = "attach failed: " + va.Status.AttachError.Message
		case va.Status.DetachError != nil:
			since, source = va.Status.DetachError.Time.Time, "status.detachError.time"
			summary = "detach failed: " + va.Status.DetachError.Message
		case !va.Status.Attached && va.DeletionTimestamp == nil && p.Now.Sub(va.CreationTimestamp.Time) > grace:
			since, source = va.CreationTimestamp.Time, "metadata.creationTimestamp"
			summary = "not attached " + duration.HumanDuration(p.Now.Sub(since)) + " after creation"
		default:
			continue
		}
		if !since.IsZero() && p.Now.Sub(since) < grace {
			continue
		}
		f := finding("SF-VOL-001", "Volume cannot attach or detach", High, "VolumeAttachment", va.ObjectMeta,
			fmt.Sprintf("PV %s on node %s: %s", pv, va.Spec.NodeName, summary),
			"Check the CSI driver's controller pods and RBAC, and that the backing disk still exists in the cloud provider")
		f.Since, f.SinceSource = tp(since), source
		f.Evidence = []string{"attacher=" + va.Spec.Attacher}
		out = append(out, f)
	}
	return out, nil
}

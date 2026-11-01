package silent

import (
	"context"
	"sync"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
)

// GVRs for objects read through the dynamic client, so kdeck does not need
// typed clients for every CRD it understands.
var (
	gvrAPIService   = schema.GroupVersionResource{Group: "apiregistration.k8s.io", Version: "v1", Resource: "apiservices"}
	gvrCRD          = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	gvrHTTPRoute    = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	gvrVeleroSched  = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "schedules"}
	gvrVeleroBackup = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backups"}
	gvrVeleroBSL    = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backupstoragelocations"}
	gvrArgoApp      = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	gvrFluxKust     = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	gvrFluxHelm     = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	gvrFluxGit      = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
	gvrSecrets      = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

// Snapshot lazily lists cluster objects, caching each list for the run.
// Namespace "" means all namespaces.
type Snapshot struct {
	CS        kubernetes.Interface
	Dyn       dynamic.Interface
	Meta      metadata.Interface // metadata-only reads (Helm release Secrets)
	Host      string             // API server URL, for its serving certificate
	Namespace string

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	val interface{}
	err error
}

// NewSnapshot creates an empty snapshot over the given clients.
func NewSnapshot(cs kubernetes.Interface, dyn dynamic.Interface, namespace string) *Snapshot {
	return &Snapshot{CS: cs, Dyn: dyn, Namespace: namespace, cache: map[string]entry{}}
}

func get[T any](s *Snapshot, key string, list func() (T, error)) (T, error) {
	s.mu.Lock()
	if e, ok := s.cache[key]; ok {
		s.mu.Unlock()
		if e.err != nil {
			var zero T
			return zero, e.err
		}
		return e.val.(T), nil
	}
	s.mu.Unlock()
	v, err := list()
	s.mu.Lock()
	s.cache[key] = entry{v, err}
	s.mu.Unlock()
	return v, err
}

var all = metav1.ListOptions{ResourceVersion: "0"} // served from the API server's watch cache

func (s *Snapshot) Pods(ctx context.Context) ([]corev1.Pod, error) {
	return get(s, "pods", func() ([]corev1.Pod, error) {
		l, err := s.CS.CoreV1().Pods(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) Services(ctx context.Context) ([]corev1.Service, error) {
	return get(s, "services", func() ([]corev1.Service, error) {
		l, err := s.CS.CoreV1().Services(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

// ServicesAll ignores the namespace scope; webhooks may point anywhere.
func (s *Snapshot) ServicesAll(ctx context.Context) ([]corev1.Service, error) {
	if s.Namespace == "" {
		return s.Services(ctx)
	}
	return get(s, "services-all", func() ([]corev1.Service, error) {
		l, err := s.CS.CoreV1().Services("").List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) EndpointSlicesAll(ctx context.Context) ([]discoveryv1.EndpointSlice, error) {
	return get(s, "endpointslices", func() ([]discoveryv1.EndpointSlice, error) {
		l, err := s.CS.DiscoveryV1().EndpointSlices("").List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) Deployments(ctx context.Context) ([]appsv1.Deployment, error) {
	return get(s, "deployments", func() ([]appsv1.Deployment, error) {
		l, err := s.CS.AppsV1().Deployments(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) StatefulSets(ctx context.Context) ([]appsv1.StatefulSet, error) {
	return get(s, "statefulsets", func() ([]appsv1.StatefulSet, error) {
		l, err := s.CS.AppsV1().StatefulSets(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) ReplicaSets(ctx context.Context) ([]appsv1.ReplicaSet, error) {
	return get(s, "replicasets", func() ([]appsv1.ReplicaSet, error) {
		l, err := s.CS.AppsV1().ReplicaSets(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) CronJobs(ctx context.Context) ([]batchv1.CronJob, error) {
	return get(s, "cronjobs", func() ([]batchv1.CronJob, error) {
		l, err := s.CS.BatchV1().CronJobs(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) HPAs(ctx context.Context) ([]autoscalingv2.HorizontalPodAutoscaler, error) {
	return get(s, "hpas", func() ([]autoscalingv2.HorizontalPodAutoscaler, error) {
		l, err := s.CS.AutoscalingV2().HorizontalPodAutoscalers(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) PDBs(ctx context.Context) ([]policyv1.PodDisruptionBudget, error) {
	return get(s, "pdbs", func() ([]policyv1.PodDisruptionBudget, error) {
		l, err := s.CS.PolicyV1().PodDisruptionBudgets(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) Ingresses(ctx context.Context) ([]networkingv1.Ingress, error) {
	return get(s, "ingresses", func() ([]networkingv1.Ingress, error) {
		l, err := s.CS.NetworkingV1().Ingresses(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) ValidatingWebhooks(ctx context.Context) ([]admissionv1.ValidatingWebhookConfiguration, error) {
	return get(s, "vwh", func() ([]admissionv1.ValidatingWebhookConfiguration, error) {
		l, err := s.CS.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) MutatingWebhooks(ctx context.Context) ([]admissionv1.MutatingWebhookConfiguration, error) {
	return get(s, "mwh", func() ([]admissionv1.MutatingWebhookConfiguration, error) {
		l, err := s.CS.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) Roles(ctx context.Context) ([]rbacv1.Role, error) {
	return get(s, "roles", func() ([]rbacv1.Role, error) {
		l, err := s.CS.RbacV1().Roles("").List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) ClusterRoles(ctx context.Context) ([]rbacv1.ClusterRole, error) {
	return get(s, "clusterroles", func() ([]rbacv1.ClusterRole, error) {
		l, err := s.CS.RbacV1().ClusterRoles().List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) RoleBindings(ctx context.Context) ([]rbacv1.RoleBinding, error) {
	return get(s, "rolebindings", func() ([]rbacv1.RoleBinding, error) {
		l, err := s.CS.RbacV1().RoleBindings(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) ClusterRoleBindings(ctx context.Context) ([]rbacv1.ClusterRoleBinding, error) {
	return get(s, "clusterrolebindings", func() ([]rbacv1.ClusterRoleBinding, error) {
		l, err := s.CS.RbacV1().ClusterRoleBindings().List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) ServiceAccountsAll(ctx context.Context) ([]corev1.ServiceAccount, error) {
	return get(s, "serviceaccounts", func() ([]corev1.ServiceAccount, error) {
		l, err := s.CS.CoreV1().ServiceAccounts("").List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) PodsAll(ctx context.Context) ([]corev1.Pod, error) {
	if s.Namespace == "" {
		return s.Pods(ctx)
	}
	return get(s, "pods-all", func() ([]corev1.Pod, error) {
		l, err := s.CS.CoreV1().Pods("").List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) Leases(ctx context.Context) ([]coordinationv1.Lease, error) {
	return get(s, "leases", func() ([]coordinationv1.Lease, error) {
		l, err := s.CS.CoordinationV1().Leases(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) VolumeAttachments(ctx context.Context) ([]storagev1.VolumeAttachment, error) {
	return get(s, "volumeattachments", func() ([]storagev1.VolumeAttachment, error) {
		l, err := s.CS.StorageV1().VolumeAttachments().List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) Namespaces(ctx context.Context) ([]corev1.Namespace, error) {
	return get(s, "namespaces", func() ([]corev1.Namespace, error) {
		l, err := s.CS.CoreV1().Namespaces().List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) PVCs(ctx context.Context) ([]corev1.PersistentVolumeClaim, error) {
	return get(s, "pvcs", func() ([]corev1.PersistentVolumeClaim, error) {
		l, err := s.CS.CoreV1().PersistentVolumeClaims(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) PVs(ctx context.Context) ([]corev1.PersistentVolume, error) {
	return get(s, "pvs", func() ([]corev1.PersistentVolume, error) {
		l, err := s.CS.CoreV1().PersistentVolumes().List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) Secrets(ctx context.Context) ([]corev1.Secret, error) {
	return get(s, "secrets", func() ([]corev1.Secret, error) {
		l, err := s.CS.CoreV1().Secrets(s.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

// Dynamic lists a resource through the dynamic client; namespaced resources
// honour the snapshot's namespace scope when namespaced is true.
func (s *Snapshot) Dynamic(ctx context.Context, gvr schema.GroupVersionResource, namespaced bool) ([]unstructured.Unstructured, error) {
	key := gvr.String()
	return get(s, key, func() ([]unstructured.Unstructured, error) {
		if s.Dyn == nil {
			return nil, errNotInstalled
		}
		var ri dynamic.ResourceInterface = s.Dyn.Resource(gvr)
		if namespaced {
			ri = s.Dyn.Resource(gvr).Namespace(s.Namespace)
		}
		l, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) Nodes(ctx context.Context) ([]corev1.Node, error) {
	return get(s, "nodes", func() ([]corev1.Node, error) {
		l, err := s.CS.CoreV1().Nodes().List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) DaemonSets(ctx context.Context) ([]appsv1.DaemonSet, error) {
	return get(s, "daemonsets", func() ([]appsv1.DaemonSet, error) {
		l, err := s.CS.AppsV1().DaemonSets(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) ResourceQuotas(ctx context.Context) ([]corev1.ResourceQuota, error) {
	return get(s, "resourcequotas", func() ([]corev1.ResourceQuota, error) {
		l, err := s.CS.CoreV1().ResourceQuotas(s.Namespace).List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (s *Snapshot) CSRs(ctx context.Context) ([]certificatesv1.CertificateSigningRequest, error) {
	return get(s, "csrs", func() ([]certificatesv1.CertificateSigningRequest, error) {
		l, err := s.CS.CertificatesV1().CertificateSigningRequests().List(ctx, all)
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

// HelmReleaseSecrets lists Helm release Secrets as metadata only: release
// name, revision and status live in labels, so Secret data is never read.
func (s *Snapshot) HelmReleaseSecrets(ctx context.Context) ([]metav1.PartialObjectMetadata, error) {
	return get(s, "helm-secrets", func() ([]metav1.PartialObjectMetadata, error) {
		if s.Meta == nil {
			return nil, errNotInstalled
		}
		l, err := s.Meta.Resource(gvrSecrets).Namespace(s.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm"})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

// APIMetrics fetches the API server's /metrics text.
func (s *Snapshot) APIMetrics(ctx context.Context) (string, error) {
	return get(s, "apimetrics", func() (string, error) {
		b, err := s.CS.Discovery().RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
		return string(b), err
	})
}

// NodeStats fetches the kubelet stats summary for a node via the API proxy.
func (s *Snapshot) NodeStats(ctx context.Context, node string) ([]byte, error) {
	return s.CS.CoreV1().RESTClient().Get().Resource("nodes").Name(node).SubResource("proxy").Suffix("stats/summary").DoRaw(ctx)
}

package silent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"

	"github.com/lyelgate/kubepilot/internal/config"
)

// maxSeries caps finding series; low findings are dropped first.
const maxSeries = 2000

func buildSnapshot(flags *genericclioptions.ConfigFlags, ns string) (*Snapshot, error) {
	rc, err := flags.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	rc.QPS, rc.Burst = 20, 40
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	snap := NewSnapshot(cs, dyn, ns)
	snap.Host = rc.Host
	if mc, err := metadata.NewForConfig(rc); err == nil {
		snap.Meta = mc
	}
	return snap, nil
}

type server struct {
	mu        sync.RWMutex
	last      Result
	lastRun   time.Time
	firstSeen map[string]time.Time
	lastSeen  map[string]time.Time
	truncated bool
}

// track fills Since for undated findings from first-seen times, keeping the
// original time when a finding disappears for less than 10 minutes.
func (s *server) track(r *Result, now time.Time) {
	for i := range r.Findings {
		f := &r.Findings[i]
		if last, ok := s.lastSeen[f.Fingerprint]; !ok || now.Sub(last) > 10*time.Minute {
			s.firstSeen[f.Fingerprint] = now
		}
		s.lastSeen[f.Fingerprint] = now
		if f.Since == nil {
			t := s.firstSeen[f.Fingerprint]
			f.Since, f.SinceSource = &t, "first seen by kp silent serve"
		}
	}
	for fp, t := range s.lastSeen {
		if now.Sub(t) > time.Hour {
			delete(s.lastSeen, fp)
			delete(s.firstSeen, fp)
		}
	}
}

func esc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// WriteMetrics renders a result in the Prometheus text format.
func WriteMetrics(w io.Writer, r Result, lastRun time.Time, truncated bool) {
	category := map[string]string{}
	for _, c := range Registry {
		category[c.ID] = c.Category
	}
	fmt.Fprintln(w, "# HELP silent_finding One series per active silent failure.")
	fmt.Fprintln(w, "# TYPE silent_finding gauge")
	labels := func(f Finding) string {
		return fmt.Sprintf(`check="%s",severity="%s",category="%s",namespace="%s",kind="%s",name="%s"`,
			esc(f.Check), f.Severity, esc(category[f.Check]), esc(f.Namespace), esc(f.Kind), esc(f.Name))
	}
	counts := map[Severity]int{}
	for _, f := range r.Findings {
		if f.Suppressed != "" {
			continue
		}
		counts[f.Severity]++
		fmt.Fprintf(w, "silent_finding{%s} 1\n", labels(f))
	}
	fmt.Fprintln(w, "# HELP silent_finding_since_timestamp_seconds When the failure began (or was first seen).")
	fmt.Fprintln(w, "# TYPE silent_finding_since_timestamp_seconds gauge")
	for _, f := range r.Findings {
		if f.Suppressed == "" && f.Since != nil {
			fmt.Fprintf(w, "silent_finding_since_timestamp_seconds{%s} %d\n", labels(f), f.Since.Unix())
		}
	}
	fmt.Fprintln(w, "# HELP silent_findings_total Active findings by severity.")
	fmt.Fprintln(w, "# TYPE silent_findings_total gauge")
	for _, s := range []Severity{Critical, High, Medium, Low} {
		fmt.Fprintf(w, "silent_findings_total{severity=\"%s\"} %d\n", s, counts[s])
	}
	fmt.Fprintln(w, "# HELP silent_check_skipped 1 when a check cannot run.")
	fmt.Fprintln(w, "# TYPE silent_check_skipped gauge")
	for _, s := range r.Skipped {
		fmt.Fprintf(w, "silent_check_skipped{check=\"%s\",reason=\"%s\"} 1\n", esc(s.Check), esc(s.Reason))
	}
	trunc := 0
	if truncated {
		trunc = 1
	}
	fmt.Fprintf(w, "# TYPE silent_findings_truncated gauge\nsilent_findings_truncated %d\n", trunc)
	fmt.Fprintf(w, "# TYPE silent_last_run_timestamp_seconds gauge\nsilent_last_run_timestamp_seconds %d\n", lastRun.Unix())
	if d, err := time.ParseDuration(r.Duration); err == nil {
		fmt.Fprintf(w, "# TYPE silent_run_duration_seconds gauge\nsilent_run_duration_seconds %.3f\n", d.Seconds())
	}
}

func serveCmd(flags *genericclioptions.ConfigFlags, version string) *cobra.Command {
	var listen, intervalStr string
	var sensitive bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the checks continuously and serve /metrics (Prometheus), /findings (JSON) and /healthz",
		Example: `  kp silent serve                         # :9090, every 60s, using your kubeconfig
  kp silent serve --listen :8080 --interval 2m
  kp silent manifest | kubectl apply -f -  # run it inside the cluster`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			interval, err := time.ParseDuration(intervalStr)
			if err != nil {
				return err
			}
			ns := ""
			if flags.Namespace != nil {
				ns = *flags.Namespace
			}
			s := &server{firstSeen: map[string]time.Time{}, lastSeen: map[string]time.Time{}}
			runOnce := func() {
				cfg, _ := config.Load()
				if cfg == nil {
					cfg = &config.Config{}
				}
				snap, err := buildSnapshot(flags, ns)
				if err != nil {
					fmt.Fprintf(os.Stderr, "kp silent serve: %v\n", err)
					return
				}
				now := time.Now()
				r := Run(cmd.Context(), snap, Options{Sensitive: sensitive, Now: now, Config: cfg.Silent})
				r.Version = version
				s.mu.Lock()
				s.track(&r, now)
				truncated := false
				if len(r.Findings) > maxSeries {
					Sort(r.Findings) // highest severity first
					r.Findings, truncated = r.Findings[:maxSeries], true
				}
				s.last, s.lastRun, s.truncated = r, now, truncated
				s.mu.Unlock()
				fmt.Fprintf(os.Stderr, "kp silent serve: %d findings, %d skipped, %s\n", len(r.Findings), len(r.Skipped), r.Duration)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
				s.mu.RLock()
				defer s.mu.RUnlock()
				w.Header().Set("Content-Type", "text/plain; version=0.0.4")
				WriteMetrics(w, s.last, s.lastRun, s.truncated)
			})
			mux.HandleFunc("/findings", func(w http.ResponseWriter, _ *http.Request) {
				s.mu.RLock()
				defer s.mu.RUnlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(s.last)
			})
			mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
				s.mu.RLock()
				defer s.mu.RUnlock()
				if s.lastRun.IsZero() || time.Since(s.lastRun) > 3*interval+time.Minute {
					http.Error(w, "no recent run", http.StatusServiceUnavailable)
					return
				}
				fmt.Fprintln(w, "ok")
			})
			srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				runOnce()
				t := time.NewTicker(interval)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						runOnce()
					}
				}
			}()
			go func() {
				<-ctx.Done()
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(sctx)
			}()
			fmt.Fprintf(os.Stderr, "kp silent serve: listening on %s, checking every %s\n", listen, interval)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&listen, "listen", ":9090", "address to serve on")
	cmd.Flags().StringVar(&intervalStr, "interval", "60s", "how often to run the checks")
	cmd.Flags().BoolVar(&sensitive, "sensitive", false, "enable checks that read Secrets and kubelet stats")
	return cmd
}

func manifestCmd(version string) *cobra.Command {
	var ns, image string
	var sensitive, rule bool
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Print Kubernetes YAML to run kp silent serve in the cluster (read-only RBAC, Service, alert rules)",
		Example: `  kp silent manifest | kubectl apply -f -
  kp silent manifest --sensitive --prometheus-rule > kp-silent.yaml`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			tag := version
			if tag == "" || tag == "dev" {
				tag = "latest"
			}
			if image == "" {
				image = "ghcr.io/lyelgate/kubepilot:" + tag
			}
			extraArgs := ""
			extraRules := ""
			if sensitive {
				extraArgs = `, "--sensitive"`
				extraRules = `
- apiGroups: [""]
  resources: [secrets]
  verbs: [list]
- apiGroups: [""]
  resources: [nodes/proxy]
  verbs: [get]`
			}
			fmt.Fprintf(out, manifestTemplate, ns, image, extraArgs, extraRules)
			if rule {
				fmt.Fprintf(out, ruleTemplate, ns)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&ns, "namespace-name", "kubepilot", "namespace to install into")
	cmd.Flags().StringVar(&image, "image", "", "container image (default ghcr.io/lyelgate/kubepilot:<version>)")
	cmd.Flags().BoolVar(&sensitive, "sensitive", false, "also grant Secret list and nodes/proxy (certificate and volume-usage checks)")
	cmd.Flags().BoolVar(&rule, "prometheus-rule", false, "also print a PrometheusRule (needs the Prometheus Operator)")
	return cmd
}

// SortedCategories lists categories for help text.
func SortedCategories() []string {
	set := map[string]bool{}
	for _, c := range Registry {
		set[c.Category] = true
	}
	var out []string
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

const manifestTemplate = `apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kp-silent
  namespace: %[1]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kp-silent-reader
rules:
- apiGroups: ["", "apps", "batch", "autoscaling", "policy", "discovery.k8s.io", "networking.k8s.io",
              "gateway.networking.k8s.io", "admissionregistration.k8s.io", "apiregistration.k8s.io",
              "apiextensions.k8s.io", "rbac.authorization.k8s.io", "coordination.k8s.io", "storage.k8s.io",
              "certificates.k8s.io", "velero.io", "argoproj.io", "kustomize.toolkit.fluxcd.io",
              "helm.toolkit.fluxcd.io", "source.toolkit.fluxcd.io"]
  resources: [pods, services, nodes, namespaces, persistentvolumes, persistentvolumeclaims, events,
              serviceaccounts, resourcequotas, deployments, replicasets, statefulsets, daemonsets, cronjobs,
              jobs, horizontalpodautoscalers, poddisruptionbudgets, endpointslices, ingresses, ingressclasses,
              gateways, httproutes, validatingwebhookconfigurations, mutatingwebhookconfigurations, apiservices,
              customresourcedefinitions, roles, rolebindings, clusterroles, clusterrolebindings, leases,
              volumeattachments, certificatesigningrequests, backups, schedules, backupstoragelocations,
              applications, kustomizations, helmreleases, gitrepositories]
  verbs: [get, list, watch]
- apiGroups: [""]
  resources: [secrets]
  verbs: [list]
  # metadata only: used for Helm release status labels; Secret data is never read by kp
- nonResourceURLs: ["/metrics"]
  verbs: [get]%[4]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kp-silent-reader
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: kp-silent-reader}
subjects: [{kind: ServiceAccount, name: kp-silent, namespace: %[1]s}]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kp-silent
  namespace: %[1]s
  labels: {app.kubernetes.io/name: kp-silent}
spec:
  replicas: 1
  selector: {matchLabels: {app.kubernetes.io/name: kp-silent}}
  template:
    metadata:
      labels: {app.kubernetes.io/name: kp-silent}
    spec:
      serviceAccountName: kp-silent
      securityContext: {runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
      containers:
      - name: kp
        image: %[2]s
        args: ["silent", "serve", "--listen", ":9090"%[3]s]
        env: [{name: KP_HOME, value: /tmp/kp}]
        ports: [{name: metrics, containerPort: 9090}]
        readinessProbe: {httpGet: {path: /healthz, port: metrics}, periodSeconds: 30}
        resources:
          requests: {cpu: 50m, memory: 64Mi}
          limits: {memory: 512Mi}
        securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
        volumeMounts: [{name: tmp, mountPath: /tmp}]
      volumes: [{name: tmp, emptyDir: {}}]
---
apiVersion: v1
kind: Service
metadata:
  name: kp-silent
  namespace: %[1]s
  labels: {app.kubernetes.io/name: kp-silent}
  annotations: {prometheus.io/scrape: "true", prometheus.io/port: "9090"}
spec:
  selector: {app.kubernetes.io/name: kp-silent}
  ports: [{name: metrics, port: 9090, targetPort: metrics}]
`

const ruleTemplate = `---
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: kp-silent
  namespace: %s
spec:
  groups:
  - name: kp-silent
    rules:
    - alert: SilentFailureCritical
      expr: max by (check, namespace, kind, name) (silent_finding{severity="critical"}) == 1
      for: 5m
      labels: {severity: critical}
      annotations: {summary: "{{ $labels.check }} on {{ $labels.kind }} {{ $labels.namespace }}/{{ $labels.name }}"}
    - alert: SilentFailureHigh
      expr: max by (check, namespace, kind, name) (silent_finding{severity="high"}) == 1
      for: 30m
      labels: {severity: warning}
      annotations: {summary: "{{ $labels.check }} on {{ $labels.kind }} {{ $labels.namespace }}/{{ $labels.name }}"}
    - alert: SilentCheckBlind
      expr: max by (check) (silent_check_skipped) == 1
      for: 1h
      labels: {severity: info}
    - alert: SilentServeStale
      expr: time() - silent_last_run_timestamp_seconds > 600
      for: 10m
      labels: {severity: warning}
`

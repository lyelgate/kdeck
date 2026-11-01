#!/usr/bin/env bash
# End-to-end check against a disposable cluster (kind in CI).
# Plants silent failures and asserts kdeck finds them; checks the production
# guard and a few shortcuts. Usage: hack/e2e.sh ./bin/kdeck
# Uses the current KUBECONFIG context; never run it against a real cluster.
set -euo pipefail
KP=${1:-./bin/kdeck}
KP=$(cd "$(dirname "$KP")" && pwd)/$(basename "$KP")
export KDECK_HOME=$(mktemp -d) KDECK_NO_FZF=1
NS=kdeck-e2e
cleanup() {
  echo "--- cleanup"
  # The guard test marks this context as production; confirm our own deletes.
  export KDECK_CONFIRM_CONTEXT=$($KP ctx -c)
  $KP -n $NS patch pvc stuck --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  $KP delete ns $NS --wait=false >/dev/null 2>&1 || true
  $KP delete validatingwebhookconfiguration kdeck-e2e-policy --ignore-not-found >/dev/null
  $KP delete clusterrolebinding kdeck-e2e-dangling --ignore-not-found >/dev/null
  rm -rf "$KDECK_HOME"
}
trap cleanup EXIT

cat > "$KDECK_HOME/config.yaml" <<'EOF'
silent:
  checks:
    SF-CRON-001: {thresholds: {missedRuns: 1}}
    SF-RBAC-001: {thresholds: {graceMinutes: 0}}
    SF-NET-001: {thresholds: {graceMinutes: 0}}
    SF-PDB-001: {thresholds: {graceMinutes: 0}}
    SF-FINALIZER-001: {thresholds: {graceMinutes: 0}}
    SF-HELM-001: {thresholds: {graceMinutes: 0}}
EOF

RENEW=$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%S.000000Z 2>/dev/null || date -u -v-1H +%Y-%m-%dT%H:%M:%S.000000Z)
$KP apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata: {name: $NS}
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: nightly-backup, namespace: $NS}
spec:
  schedule: "* * * * *"
  jobTemplate:
    spec:
      backoffLimit: 0
      template:
        spec:
          restartPolicy: Never
          containers: [{name: b, image: busybox:1.36, command: ["sh","-c","exit 1"]}]
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata: {name: kdeck-e2e-policy}
webhooks:
- name: strict.example.com
  failurePolicy: Fail
  sideEffects: None
  admissionReviewVersions: ["v1"]
  clientConfig: {service: {namespace: $NS, name: deleted-webhook}}
  rules: [{apiGroups: ["example.com"], apiVersions: ["v1"], operations: ["CREATE"], resources: ["widgets"]}]
---
apiVersion: v1
kind: Service
metadata: {name: api, namespace: $NS}
spec: {selector: {app: api}, ports: [{port: 80}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec: {containers: [{name: web, image: registry.k8s.io/pause:3.10}]}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: web, namespace: $NS}
spec: {minAvailable: 1, selector: {matchLabels: {app: web}}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: bad-image, namespace: $NS}
spec:
  progressDeadlineSeconds: 10
  selector: {matchLabels: {app: bad-image}}
  template:
    metadata: {labels: {app: bad-image}}
    spec: {containers: [{name: x, image: "registry.invalid/does-not-exist:1"}]}
---
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata: {name: ghost-controller, namespace: $NS}
spec: {holderIdentity: ghost, leaseDurationSeconds: 15, renewTime: "$RENEW"}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: kdeck-e2e-dangling}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: view}
subjects: [{kind: ServiceAccount, namespace: $NS, name: no-such-sa}]
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: shop, namespace: $NS}
spec:
  rules:
  - http:
      paths: [{path: /, pathType: Prefix, backend: {service: {name: missing-svc, port: {number: 80}}}}]
---
apiVersion: v1
kind: Secret
metadata: {name: sh.helm.release.v1.billing.v3, namespace: $NS, labels: {owner: helm, name: billing, status: pending-upgrade, version: "3"}}
type: helm.sh/release.v1
data: {release: ZmFrZQ==}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: stuck, namespace: $NS, finalizers: ["example.com/hold"]}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}
EOF
$KP -n $NS delete pvc stuck --wait=false >/dev/null
echo "waiting for the rollout deadline and a failed backup run"
$KP -n $NS wait deploy/bad-image --for=jsonpath='{.status.conditions[?(@.type=="Progressing")].reason}'=ProgressDeadlineExceeded --timeout=180s >/dev/null
for i in $(seq 1 30); do $KP -n $NS get jobs -o name 2>/dev/null | grep -q . && break; $KP -n $NS wait --for=jsonpath='{.status.lastScheduleTime}' cronjob/nightly-backup --timeout=10s >/dev/null 2>&1 || true; done
$KP -n $NS wait --for=condition=Failed job --all --timeout=180s >/dev/null

echo "--- kdeck silent"
$KP silent --fail-on none
$KP silent -o json --fail-on none > "$KDECK_HOME/silent.json"
fail=0
for id in SF-CRON-001 SF-WEBHOOK-001 SF-LEASE-001 SF-RBAC-001 SF-NET-001 SF-NET-002 SF-PDB-001 SF-ROLLOUT-001 SF-FINALIZER-001 SF-HELM-001; do
  if grep -q "\"check\": \"$id\"" "$KDECK_HOME/silent.json"; then echo "ok   $id"; else echo "MISS $id"; fail=1; fi
done

echo "--- guard"
$KP safety prod add "$($KP ctx -c)" >/dev/null
if $KP -n $NS scale deploy/web --replicas=2 </dev/null >/dev/null 2>&1; then echo "MISS guard: production write was not refused"; fail=1; else echo "ok   production write refused"; fi
KDECK_CONFIRM_CONTEXT=$($KP ctx -c) $KP -n $NS scale deploy/web --replicas=1 >/dev/null && echo "ok   confirmed write allowed"
$KP history | grep -q "scale deploy/web" && echo "ok   history recorded" || { echo "MISS history"; fail=1; }

echo "--- shortcuts"
[ "$(KDECK_DEBUG_ALIAS=1 $KP -n $NS kgpo 2>&1 >/dev/null | head -1)" = "kdeck: kgpo -> -n $NS get pods" ] && echo "ok   kgpo expands" || { echo "MISS kgpo"; fail=1; }
$KP -n $NS po web >/dev/null && echo "ok   kdeck po regex"

exit $fail

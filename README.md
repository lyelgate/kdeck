# kdeck

One binary to run Kubernetes end to end: **all of kubectl**, built in and unchanged, plus shortcuts, context and namespace switching, everyday helpers, **silent-failure detection**, a **krew-compatible plugin manager**, and **Linode LKE** management.

```text
kdeck get pods                      # any kubectl command, identical behaviour
kdeck kgpoa                         # shortcut: get pods --all-namespaces
kdeck ctx prod && kdeck ns payments    # switch context and namespace ("-" = previous)
kdeck bad                           # unhealthy pods, all namespaces, worst first
kdeck silent                        # what is quietly broken, and since when
kdeck plugin install neat           # krew plugins without krew
kdeck linode lke kubeconfig 12345 --merge --set-current   # LKE cluster -> kubectl
```

**Documentation:** <https://lyelgate.github.io/kdeck/> · **Licence:** MIT

- [Install](#install)
- [Quick start](#quick-start)
- [kubectl, built in](#kubectl-built-in)
- [Shortcuts and aliases](#shortcuts-and-aliases)
- [Contexts and namespaces](#contexts-and-namespaces)
- [Everyday helpers](#everyday-helpers)
- [Silent-failure detection](#silent-failure-detection-kdeck-silent)
- [Plugins (krew-compatible)](#plugins-krew-compatible)
- [Supported Kubernetes versions](#supported-kubernetes-versions)
- [Linode LKE](#linode-lke)
- [Regex everywhere](#regex-everywhere)
- [One pod by regex: dklog abc](#one-pod-by-regex-dklog-abc)
- [Stern-style logs](#stern-style-logs-kdeck-tail-also-kdeck-stern)
- [Port-forward shortcuts](#port-forward-shortcuts-kdeck-pf)
- [Clusters on AWS, Google Cloud and Azure](#clusters-on-aws-google-cloud-and-azure)
- [Teleport plugin](#teleport-plugin-tsh)
- [Setup and diagnostics](#setup-and-diagnostics)
- [Platforms](#platforms)
- [Production safety](#production-safety)
- [Many clusters](#many-clusters)
- [Troubleshooting](#troubleshooting)
- [Cost](#cost)
- [Update and settings](#update-and-settings)
- [Shell integration](#shell-integration)
- [Configuration reference](#configuration-reference)
- [Security notes](#security-notes)
- [Development](#development)
- [Limitations and roadmap](#limitations-and-roadmap)
- [License](#license)

## Install

| Platform | Command |
| --- | --- |
| macOS, Linux (script) | `curl -fsSL https://raw.githubusercontent.com/lyelgate/kdeck/main/install.sh \| sh` (checksum-verified) |
| Debian, Ubuntu | `curl -fLO https://github.com/lyelgate/kdeck/releases/download/v0.1.0/kdeck_0.1.0_linux_amd64.deb && sudo apt install ./kdeck_0.1.0_linux_amd64.deb` |
| Fedora, RHEL, Rocky, Amazon Linux | `sudo dnf install https://github.com/lyelgate/kdeck/releases/download/v0.1.0/kdeck_0.1.0_linux_amd64.rpm` |
| Alpine | `apk add --allow-untrusted kdeck_0.1.0_linux_amd64.apk` (from the release page) |
| Windows (PowerShell) | `irm https://raw.githubusercontent.com/lyelgate/kdeck/main/install.ps1 \| iex` |
| Container | `docker run --rm ghcr.io/lyelgate/kdeck about` |
| Homebrew, Scoop | Coming: `brew install lyelgate/tap/kdeck` and the `lyelgate` Scoop bucket are not published yet |

Every [release](https://github.com/lyelgate/kdeck/releases) has archives for Linux, macOS and Windows on amd64 and arm64 (`kdeck_<version>_<os>_<arch>.tar.gz`, `.zip` on Windows), `.deb`, `.rpm` and `.apk` packages for both CPUs (use `arm64` in the file name on ARM), `checksums.txt` signed with cosign, and an SBOM per archive. The Linux packages install shell completion for bash, zsh and fish. A macOS binary downloaded with a browser is quarantined by Gatekeeper; clear it with `xattr -d com.apple.quarantine kdeck`, or install with the script.

Then run `kdeck setup`. To build from source (Go 1.26 or newer; the binary is about 65 MB because it contains all of kubectl):

```sh
git clone https://github.com/lyelgate/kdeck && cd kdeck
make install            # builds and copies kdeck to ~/go/bin (or set PREFIX)
kdeck about             # version, embedded kubectl version, home directory
```

## Quick start

```sh
eval "$(kdeck shell-init zsh)"     # adds: k, kgpo, kdpo, kctx, kns, ... and completion
k who                           # where am I and who does the API server think I am
k ctx                           # pick a context (fzf if installed)
k ns                            # pick a namespace
k pods                          # pods, problems first
k silent                        # silent failures across the cluster
```

## kubectl, built in

kdeck embeds `k8s.io/kubectl` (v0.37.2) as a library, so **every kubectl command, flag, output format and behaviour is identical**: `get`, `describe`, `apply`, `diff`, `edit`, `logs`, `exec`, `port-forward`, `cp`, `rollout`, `scale`, `top`, `debug`, `auth`, `kustomize`, `events`, `config`, `explain`, `wait`, and the rest. Global flags (`--context`, `-n`, `--kubeconfig`, `--as`, ...) work everywhere, including on kdeck's own commands.

kubectl's own `~/.kube/kuberc` preferences and aliases still apply. A symlink named `kubectl` pointing at kdeck makes it a drop-in kubectl replacement.

## Shortcuts and aliases

A shortcut is expanded only when the word is **not** a real command, so nothing kubectl does is ever hijacked. Order of precedence: your aliases, verb shortcuts, then combinations. See what any word does with `kdeck alias explain <word>`; list everything with `kdeck alias list` (add `--all` for every combination).

**Full reference:** [docs/SHORTCUTS.md](docs/SHORTCUTS.md) lists every shortcut and alias (about 1,150) with the full command it stands for: shell aliases, command aliases such as `dklog`, verb shortcuts, the combination grammar and every combination word. The same list is in the binary:

```sh
kdeck alias reference            # everything, grouped
kdeck alias reference logs       # only rows matching a regex
kdeck help kgpo                  # "kgpo = kubectl get pods", then the help of kubectl get
kdeck help dklog                 # "dklog = kdeck log", then its help
```

`docs/SHORTCUTS.md` is generated by `make docs` (`kdeck alias reference --markdown`), and a test fails when it is out of date.

### Combination grammar (kubectl-aliases compatible)

Words follow `[k][sys]<cmd>[<resource>][<modifiers>...]`, the same grammar as [kubectl-aliases](https://github.com/ahmetb/kubectl-aliases). The leading `k` is optional inside kdeck (`kdeck gpo` = `kdeck kgpo`).

| Part | Codes |
| --- | --- |
| `sys` | `--namespace=kube-system` |
| cmd | `g` get · `d` describe · `rm` delete · `a` apply -f · `ex` exec -i -t · `lo` logs -f · `lop` logs -f -p · `p` proxy · `pf` port-forward |
| resource (g, d, rm only) | `po` pods · `dep` deployment · `sts` statefulset · `ds` daemonset · `rs` replicaset · `svc` service · `ing` ingress · `cm` configmap · `sec` secret · `no` nodes · `ns` namespaces · `job` · `cj` cronjob · `pvc` · `pv` · `hpa` · `sa` serviceaccount · `ev` events · `ep` endpointslices · `pdb` · `np` networkpolicy · `crd` · `sc` storageclass · `lease` |
| modifiers (any order) | `oyaml` `ojson` `owide` (one of) · `a` or `all` (--all-namespaces) · `w` (--watch) · `sl` (--show-labels) |
| value modifiers (at most one; takes your next argument) | `l` (-l) · `n` (--namespace) · `f` (--recursive -f) |

| You type | Runs |
| --- | --- |
| `kgpo` | `kubectl get pods` |
| `kgpoa` | `kubectl get pods --all-namespaces` |
| `kgpooyaml web-0` | `kubectl get pods -o yaml web-0` |
| `kgdepw` | `kubectl get deployment --watch` |
| `kgnoowide` | `kubectl get nodes -o wide` |
| `ksysgpo` | `kubectl get pods --namespace=kube-system` |
| `kdpol app=web` | `kubectl describe pods -l app=web` |
| `kgpon prod` | `kubectl get pods --namespace prod` |
| `krmdep api` | `kubectl delete deployment api` |
| `klop web-0` | `kubectl logs -f -p web-0` |
| `kex web-0 -- sh` | `kubectl exec -i -t web-0 -- sh` |
| `kpf svc/web 8080:80` | `kubectl port-forward svc/web 8080:80` |

Invalid combinations (two output formats, `w` with yaml/json, `all` with `sys` or `n`, two value modifiers) are not expanded.

### Verb shortcuts

| Word | Runs | Word | Runs |
| --- | --- | --- | --- |
| `l` | logs | `rr` | rollout restart |
| `lf` | logs -f | `rs` | rollout status |
| `lp` | logs -p | `rh` | rollout history |
| `e` | exec -it | `ru` | rollout undo |
| `ed` | edit | `ro` | rollout |
| `c` | create | `sc` | scale |
| `ak` | apply -k | `t` / `tp` / `tn` | top / top pods / top nodes |
| `df` | diff -f | `x` | explain |
| `ar` | api-resources | `lb` / `an` | label / annotate |
| `dr` | drain --ignore-daemonsets --delete-emptydir-data | `co` / `uc` | cordon / uncordon |
| `ta` | taint | `ev` / `evw` | events / events --types=Warning |
| `ci` | cluster-info | `v` | version |
| `cfg` | config | `w` | wait |
| `dbg` | debug | `can` / `canl` | auth can-i / auth can-i --list |

### Your own aliases

```sh
kdeck alias set kgr 'get pods --field-selector=status.phase=Running'
kdeck kgr -A
kdeck alias rm kgr
```

Stored in `~/.kdeck/config.yaml`; they take precedence over built-in shortcuts and are included in `kdeck shell-init`.

### Without shell aliases (busybox style)

`kdeck shortcuts install` creates symlinks (`k`, `kgpo`, `kdpo`, ...) to the kdeck binary in `~/.kdeck/shortcuts`. Put that directory on PATH and the shortcut names work as commands in any shell, script or CI job. `--all` links every combination.

Debug expansion with `KDECK_DEBUG_ALIAS=1 kdeck kgpoa`.

## Contexts and namespaces

| Command | Does |
| --- | --- |
| `kdeck ctx` | Pick a context with fzf; without fzf (or with `KDECK_NO_FZF=1`), list them |
| `kdeck ctx NAME` | Switch context |
| `kdeck ctx -` | Back to the previous context |
| `kdeck ctx -c` | Print the current context |
| `kdeck ctx rename OLD NEW` / `kdeck ctx rm NAME` | Rename or delete a context |
| `kdeck ns` | Pick or list namespaces (from the API) |
| `kdeck ns NAME` | Set the current context's namespace (`--force` if it does not exist yet) |
| `kdeck ns -` | Back to the previous namespace for this context |
| `kdeck ns -c` | Print the current namespace |

The previous context and per-context namespace are remembered in `~/.kdeck/state.json`.

## Everyday helpers

| Command | Does |
| --- | --- |
| `kdeck who` | Context, cluster, server, namespace, the identity the API server sees (SelfSubjectReview), server version |
| `kdeck pods [regex] [-A] [-l sel] [--bad]` | Pods sorted problems first, with the real status (`CrashLoopBackOff`, `ImagePullBackOff`, `OOMKilled`) and restarts |
| `kdeck bad` | Unhealthy pods in all namespaces |
| `kdeck nodes` | Readiness, cordon state, kubelet version, pods used/allocatable, CPU and memory requested vs allocatable |
| `kdeck sh POD-REGEX [-c c] [-- cmd]` | Shell into the best matching pod: bash if present, otherwise sh |
| `kdeck tail POD-REGEX or TYPE/NAME` | Stern-style multi-pod logs (see below) |
| `kdeck restart TYPE/NAME...` | Rolling restart, then wait for each rollout to finish |
| `kdeck images [-A]` | Every image in use and how many pods run it |
| `kdeck neat TYPE/NAME` | YAML without status, managedFields, uid, resourceVersion and last-applied, ready to re-apply |
| `kdeck decode SECRET [KEY]` | A Secret's values decoded (prints secret material) |
| `kdeck clean [-A] [--completed] [--yes]` | Delete Failed and Evicted pods; dry run unless `--yes` |
| `kdeck each -l SEL -- CMD...` | Run a command in every running pod of a selector |
| `kdeck netshoot` | Throwaway troubleshooting pod (dig, curl, tcpdump), removed on exit |
| `kdeck usage [-A] [--by cpu]` | Top pods by memory or CPU |

## Silent-failure detection (`kdeck silent`)

Finds failures that produce **no crash and no alert**: backups that stopped succeeding, certificates about to expire, controllers that stopped renewing their lease, webhooks and Services with nothing behind them, HPAs frozen because metrics are down, PDBs that will block every drain. Every finding has a severity, **since when** (taken from the API, never guessed; "unknown" when the API does not record it), the evidence, and a next step. Read-only.

```text
$ kdeck silent
SEVERITY  CHECK           OBJECT                                        SINCE      SUMMARY
critical  SF-CRON-001     broken/CronJob/nightly-backup                 2m24s ago  never succeeded; 3 scheduled runs since creation
critical  SF-WEBHOOK-001  ValidatingWebhookConfiguration/kdeck-e2e-policy  unknown    webhook strict.example.com: Service broken/deleted-webhook does not exist; failurePolicy=Fail: matching API requests are rejected
high      SF-LEASE-001    broken/Lease/ghost-controller                 62m ago    lease not renewed for 62m (duration 15s); the holder stopped renewing recently
medium    SF-PDB-001      broken/PodDisruptionBudget/web                2m24s ago  allows 0 disruptions; node drains and upgrades will block
...
11 findings (2 critical, 6 high, 3 medium) in 109ms · 2 skipped (--explain-skipped)
```

### Checks

| ID | Category | Detects | `since` from |
| --- | --- | --- | --- |
| SF-CRON-001 | backup | CronJob missed N successful runs (honours `timeZone`; backups are critical) | `status.lastSuccessfulTime` |
| SF-BACKUP-001 | backup | Velero schedules with no successful backup, failures in a row, unavailable BackupStorageLocation | last completed Backup |
| SF-CERT-001 | certificate | Expiring or expired certificates in Secrets (`tls.crt`, `ca.crt`); **sensitive, needs `--sensitive`** | certificate `NotAfter` |
| SF-CERT-002 | certificate | Expiring CA bundles in webhooks, APIServices and CRD conversion webhooks | certificate `NotAfter` |
| SF-RBAC-001 | rbac | Bindings to a missing Role/ClusterRole or a missing ServiceAccount (high when a running pod relies on it) | - |
| SF-LEASE-001 | control | Leader-election leases not renewed (high if recent, low if long dead) | `spec.renewTime` |
| SF-API-001 | control | Aggregated APIs `Available=False` (critical for metrics.k8s.io when HPAs exist) | condition transition |
| SF-WEBHOOK-001 | webhook | Admission webhooks pointing at a missing Service or one with no ready endpoints (`Fail` = critical, `Ignore` = silently skipped) | - |
| SF-NET-001 | network | Services with a selector and no ready endpoints (skips workloads scaled to zero on purpose) | pods' Ready transition |
| SF-NET-002 | network | Ingress backends naming a missing Service or port; HTTPRoutes not Accepted or ResolvedRefs=False | route condition |
| SF-HPA-001 | workload | HPAs with ScalingActive or AbleToScale False | condition transition |
| SF-PDB-001 | workload | PDBs allowing zero disruptions | DisruptionAllowed transition |
| SF-ROLLOUT-001 | workload | Deployments past their progress deadline; ReplicaSets that cannot create pods (quota, missing SA, webhook denial) | condition transition |
| SF-FINALIZER-001 | workload | Namespaces, PVCs, PVs and pods stuck in deletion | `deletionTimestamp` |
| SF-VOL-001 | storage | VolumeAttachments with attach/detach errors or never attached | `attachError.time` |
| SF-CERT-003 | certificate | API server serving certificate expiring | certificate expiry |
| SF-CERT-004 | certificate | Certificate signing requests pending (kubelet ones are high) | creation |
| SF-ROLLOUT-002 | workload | StatefulSet/DaemonSet rollouts that never finish | - |
| SF-VOL-002 | storage | PVC filesystem resize pending | condition transition |
| SF-VOL-003 | storage | Volume nearly full, or filesystem smaller than its PVC (kubelet stats; `--sensitive`) | - |
| SF-VOL-004 | storage | Released/Failed PVs still billing | - |
| SF-NODE-001 / 002 | node | Cordoned and forgotten / NotReady for over an hour | taint time / condition transition |
| SF-QUOTA-001 | workload | ResourceQuota at its limit | - |
| SF-POD-001 | workload | 100+ failed or evicted pods left in a namespace | oldest pod |
| SF-HELM-001 | gitops | Helm release stuck pending (blocks every upgrade) or failed; metadata only, no Secret data | release Secret creation |
| SF-GITOPS-001 | gitops | Argo CD apps Degraded/OutOfSync; Flux Kustomizations, HelmReleases, GitRepositories not Ready or suspended | condition transition |
| SF-API-002 | control | Deprecated APIs still being called (API server metrics), high when removed in the next minor | - |

**In-cluster mode:** `kdeck silent serve` re-runs the checks every interval and serves `/metrics` (`silent_finding`, `silent_finding_since_timestamp_seconds`, `silent_findings_total`, `silent_check_skipped`, `silent_last_run_timestamp_seconds`), `/findings` and `/healthz`; undated findings get a first-seen time. `kdeck silent manifest [--sensitive] [--prometheus-rule] | kubectl apply -f -` installs it with read-only RBAC.

`kdeck silent --list` prints the catalogue. Checks whose API is not installed (for example Velero) are listed with `--explain-skipped`.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-n NS` | all namespaces | Limit namespaced checks (cluster-scoped checks still run) |
| `--check IDs,categories` | all non-sensitive | e.g. `--check cert,backup` or `--check SF-PDB-001` |
| `--skip IDs,categories` | none | Exclude checks |
| `--sensitive` | off | Enable checks that read Secrets |
| `--since 24h` / `7d` | none | Only failures that began within the window (`--include-unknown` keeps undated ones) |
| `--min-severity` | low | Hide findings below this level |
| `--fail-on` | critical | Exit 1 when an unsuppressed finding is at or above this level (`none` disables) |
| `-o` | table | `table`, `wide` (evidence + next step), `json`, `markdown` |
| `--show-suppressed` | off | Include suppressed findings |
| `--explain-skipped` | off | List checks that could not run and why |

CI example: `kdeck silent -o json --fail-on high > silent.json`.

### Tuning and suppressing

```yaml
# ~/.kdeck/config.yaml
silent:
  checks:
    SF-CRON-001: {thresholds: {missedRuns: 5}}
    SF-CERT-002: {thresholds: {criticalDays: 14, highDays: 30, mediumDays: 45}}
    SF-NET-001:  {thresholds: {graceMinutes: 30}}
    SF-HPA-001:  {enabled: false}
  excludeNamespaces: ["sandbox-*"]
  backupLabels: ["app.kubernetes.io/component=backup"]   # CronJobs treated as backups
  suppressions:
  - check: SF-PDB-001
    object: payments/PodDisruptionBudget/api     # glob patterns allowed, or a fingerprint
    reason: single replica by design until Q1
    until: 2027-01-31                            # expired suppressions stop applying
```

Or on the object itself, so it travels with GitOps:

```yaml
metadata:
  annotations:
    silent.lyelgate.github.io/ignore: "SF-PDB-001; until=2027-01-31; reason=single replica by design"
```

Thresholds per check: `graceMinutes` (NET-001, HPA-001, PDB-001, FINALIZER-001, VOL-001, RBAC-001), `missedRuns` (CRON-001), `durationFactor` (LEASE-001), `criticalDays`/`highDays`/`mediumDays` (CERT-001, CERT-002).

## Plugins (krew-compatible)

kdeck reads the [krew index](https://github.com/kubernetes-sigs/krew-index) directly; krew itself is not needed. Plugins install into `~/.kdeck` and run as `kdeck <name>`. Plugins installed earlier with krew (`~/.krew/bin`) keep working.

| Command | Does |
| --- | --- |
| `kdeck plugin update` | Download the index |
| `kdeck plugin search [query]` | Search names and descriptions |
| `kdeck plugin info NAME` | Version, homepage, platforms, caveats |
| `kdeck plugin install NAME...` | Download, **verify sha256**, unpack, link `kubectl-<name>` |
| `kdeck plugin list` | kdeck-managed, krew-managed and any `kubectl-*` on PATH |
| `kdeck plugin upgrade [NAME...]` | Upgrade to the index version |
| `kdeck plugin uninstall NAME...` | Remove |

Plugins only run when no built-in or kdeck command matches, so a plugin can never shadow a kdeck command. Archives are checked for path traversal before unpacking.

## Linode LKE

Uses the official `linodego` SDK. The token comes from `--token`, `LINODE_TOKEN`, `LINODE_CLI_TOKEN`, or the default user in the linode-cli config file. kdeck never stores it. Anything that creates, changes or deletes resources needs `--yes`.

| Command | Does |
| --- | --- |
| `kdeck linode lke ls` | Clusters: ID, label, region, version, status, HA, tags |
| `kdeck linode lke get ID` | One cluster plus node pools |
| `kdeck linode lke kubeconfig ID` | Print the kubeconfig; `-f FILE` to write it; `--merge [--set-current] [--context-name]` to merge into your kubeconfig (backup first, entries renamed to `lke-<label>` so clusters never collide) |
| `kdeck linode lke pools ID` | Node pools: type, ready/count, autoscaler range |
| `kdeck linode lke scale ID POOL COUNT --yes` | Resize a pool |
| `kdeck linode lke create --label --region --version --pool TYPE:COUNT [--ha] --yes` | Create a cluster |
| `kdeck linode lke delete ID --yes` | Delete a cluster (warns that volumes remain) |
| `kdeck linode lke versions` | Kubernetes versions offered |
| `kdeck linode regions` / `types` | Regions (with LKE availability) and node plans with prices |
| `kdeck linode volumes ls [--unattached]` | Volumes; `--unattached` shows ones billing with no node |
| `kdeck linode nodebalancers` / `instances` | NodeBalancers (from LoadBalancer Services) and instances |
| `kdeck linode cli ARGS...` | Anything else, passed to `linode-cli` if installed |

From zero to kubectl in one line: `kdeck linode lke kubeconfig 12345 --merge --set-current && kdeck get nodes`.

## Regex everywhere

Every name argument is a **regular expression with smart case**: lower-case patterns ignore case, a pattern with an upper-case letter matches exactly. `api` matches `payments-API-7d9f`; `^web-[0-9]+$` matches `web-12` only.

| Where | Regex applies to |
| --- | --- |
| `kdeck log/desc/img/po/pod/sh/pf/tail` | pod (or Service) names; containers with `-c` |
| `kdeck pods`, `kdeck nodes`, `kdeck images` | names, node names, image references |
| `kdeck find REGEX [kinds]` | names (and labels with `--labels`) across many kinds, any CRD |
| `kdeck restart`, `kdeck each`, `kdeck clean --match` | workload / pod names |
| `kdeck ctx`, `kdeck ns`, `kdeck tu`, `kdeck eks/gke/aks use` | context, namespace, Teleport and cloud cluster names (exact name wins) |
| `kdeck silent -m`, `kdeck plugin search`, `kdeck alias list` | findings, plugins, shortcuts |
| `kdeck tail -i/-e` | log lines to include / exclude |

## One pod by regex: `dklog abc`

These commands pick **one pod**: an exact name wins, otherwise the best running and ready match; with several matches fzf opens on a terminal, otherwise the best one is used and the rest are named. `--all` acts on every match. Pod names cannot contain `_` (Kubernetes allows lower-case letters, digits, `-` and `.`).

| Command | Short name (`dk` = deck) | Does |
| --- | --- | --- |
| `kdeck log abc [-f] [-p] [-c re] [--all]` | `dklog` | Logs (all containers prefixed; `--all` = every match, stern-style) |
| `kdeck desc abc [--all]` | `dkdesc` | `kubectl describe pod` |
| `kdeck img abc [--all]` | `dkimg` | Each container's image and the digest actually running |
| `kdeck po abc [-o yaml]` | `dkpo` | `kubectl get pods -o wide` for every match |
| `kdeck pod abc` | `dkpod` | Detail card: node, IP, owner, QoS, containers, restarts, last crash, recent warnings |
| `kdeck sh abc [-- cmd]` | `dksh` | Shell into the best match |
| `kdeck pf abc [ports]` | `dkpf` | Port-forward (below) |
| `kdeck tail abc` | `dktail` | Stern-style logs (below) |
| `kdeck ns re` / `kdeck ctx re` | `dkns` / `dkctx` | Switch namespace / context |

`kdeck shortcuts install` creates all of these as real commands (symlinks on macOS and Linux, `.cmd` shims on Windows), so `dklog abc` works directly in any shell.

## Stern-style logs: `kdeck tail` (also `kdeck stern`)

Built in, no stern binary needed. Follows every pod matching a regex and every container matching `-c`; in follow mode it watches the API, so pods from scale-ups and rollouts join automatically and restarted containers resume without repeating history. Colour per pod.

```sh
kdeck tail api                        # every pod with "api" in the name
kdeck tail '^web-' -c nginx           # nginx containers only
kdeck tail . -A -i error -e healthz   # all pods, lines with "error", without "healthz"
kdeck tail deploy/api --since 10m     # pods of a workload
kdeck tail api -l tier=backend -t     # regex + selector, with timestamps
```

Flags: `-c` container regex, `-E` exclude pods, `-i`/`-e` include/exclude lines (repeatable), `-l` selector, `--since`, `--tail`, `--no-follow`, `-t`, `--init-containers`, `--color auto|always|never`, `-A`.

## Port-forward shortcuts: `kdeck pf`

```sh
kdeck pf grafana              # finds svc/grafana, forwards all its ports (80 -> localhost:8080)
kdeck pf api 9000:8080        # Service matching "api", explicit ports
kdeck pf '^web-0$' --pod      # a pod instead of a Service
kdeck pf deploy/api 8080      # TYPE/NAME goes straight to kubectl
kdeck pf grafana --address 0.0.0.0
```

Services are preferred over pods; ports below 1024 map to +8000 locally so no root is needed; several matches open the picker or are listed.

## Clusters on AWS, Google Cloud and Azure

kdeck uses each provider's official CLI, so your existing logins, SSO and profiles work. Connecting backs up the kubeconfig first; `--name` gives the context a short name.

| Command | Uses | Does |
| --- | --- | --- |
| `kdeck eks ls [re] [--region] [--profile]` | `aws` | List EKS clusters |
| `kdeck eks use re [--region] [--name prod]` | `aws eks update-kubeconfig` | Connect and switch |
| `kdeck gke ls [re] [--project]` | `gcloud` | List GKE clusters |
| `kdeck gke use re [--name]` | `gcloud container clusters get-credentials` | Connect (needs `gke-gcloud-auth-plugin`) |
| `kdeck aks ls [re] [--subscription]` | `az` | List AKS clusters |
| `kdeck aks use re [--name] [--kubelogin]` | `az aks get-credentials` (+ `kubelogin convert-kubeconfig -l azurecli`) | Connect, Entra ID clusters too |
| `kdeck ctx import FILE [--name] [--set-current]` | - | Merge any kubeconfig (backup, no overwrites) |

Credential plugins in kubeconfigs (`aws`, `gke-gcloud-auth-plugin`, `kubelogin`, `tsh`) work unchanged because kdeck uses client-go; `kdeck doctor` reports any that are missing.

## Teleport plugin (`tsh`)

Set up once; it asks only for the proxy:

```sh
kdeck setup                    # or: kdeck tsh setup tp.example.com:443
```

Then:

| Short | Full | Does |
| --- | --- | --- |
| `kdeck tl` | `kdeck tsh login` | `tsh login --proxy=<saved>` (plus saved `--user` / `--auth`) |
| `kdeck tls [re]` | `kdeck tsh ls` | Kubernetes clusters you can reach, filtered by regex (logs in first if needed) |
| `kdeck tu re` | `kdeck tsh use` | `tsh kube login <match>` and switch kubectl (logs in first if needed) |
| `kdeck ts` | `kdeck tsh status` | Session status |
| `kdeck tlo` | `kdeck tsh logout` | Log out |
| | `kdeck tsh -- ARGS` | Anything else, passed to tsh |

kdeck wraps the official tsh binary (install from goteleport.com/download, `brew install teleport`, or `winget install Teleport.Teleport`) and never handles Teleport credentials itself.

## Setup and diagnostics

- `kdeck setup`: one question (Teleport proxy, Enter to skip), then the tools found and the exact line for your shell; `--write-rc` appends it for bash, zsh or fish.
- `kdeck doctor [--all-contexts]`: kubeconfig, auth type and credential plugin per context, cluster reachability, whether the cluster version is supported (1.31 to 1.38; warns outside), tools, Teleport session.

## Platforms

| OS | CPU | Shells |
| --- | --- | --- |
| macOS | Apple silicon (arm64), Intel (amd64) | zsh, bash, fish, PowerShell |
| Linux, including WSL | amd64, arm64 | bash, zsh, fish, PowerShell |
| Windows | amd64, arm64 | PowerShell, cmd.exe |

`make dist` builds all six binaries. On Windows, `kdeck shortcuts install` writes `.cmd` shims (no admin rights needed) and `kdeck shell-init powershell` / `kdeck shell-init cmd` generate PowerShell functions or doskey macros.

## Supported Kubernetes versions

kdeck supports clusters from **Kubernetes 1.31 to 1.38** (1.31 up to one minor above the embedded kubectl, currently v0.37.2). CI runs the end-to-end suite (`hack/e2e.sh`) on kind for every minor from 1.31 to 1.37:

| Kubernetes | kind node image tested |
| --- | --- |
| 1.31 | v1.31.14 |
| 1.32 | v1.32.11 |
| 1.33 | v1.33.12 |
| 1.34 | v1.34.11 |
| 1.35 | v1.35.8 |
| 1.36 | v1.36.4 |
| 1.37 | v1.37.0 |

This is wider than kubectl's own policy (one minor either side). kdeck only calls long-stable APIs (SelfSubjectReview v1 from 1.28, admissionregistration v1, batch/v1 CronJob, policy/v1 PDB, coordination v1 Lease), so older clusters behave the same; `kdeck doctor` warns when a cluster is outside the supported range. 1.38 is supported by kubectl's skew policy but has no kind image yet. When the embedded kubectl moves to a new minor, the top of the range moves with it; raising the oldest supported version means changing `setup.OldestSupportedMinor`, the CI matrix and this table together.

## Production safety

| Setting | Effect |
| --- | --- |
| `kdeck safety prod add 'prod\|live'` | Writes (`apply`, `delete`, `scale`, `drain`, `rollout restart`, `edit`, `kdeck restart`, `kdeck clean --yes`, ...) on matching contexts need the context name typed back. `apply` shows `kubectl diff` first (`kdeck safety diff off` to skip) |
| `kdeck safety ro add '^audit-'` | Matching contexts refuse writes **and** exec (`exec`, `kdeck sh`, `kdeck each`, `kdeck node-sh`) |
| `kdeck --ro ...` or `KDECK_RO=1` | One-off read-only |
| `KDECK_CONFIRM_CONTEXT=<ctx>` | Confirms production writes in scripts and CI |
| `kdeck history [regex] [--undo]` | Every write and exec run through kdeck, with exit code and an undo command where known (scale back to the previous replica count, rollout undo, uncordon) |

`--dry-run`, `diff` and `--help` always pass. `kdeck safety` lists every context with its protection.

## Many clusters

| Command | Does |
| --- | --- |
| `kdeck -C 'prod-.*' get pods -l app=api` | Runs on every matching context in parallel and merges output with a CONTEXT column; works for kdeck commands too (`kdeck -C . silent`, `kdeck -C . bad`). Writes across contexts need the count typed (or `KDECK_CONFIRM_MULTI=N`); any read-only context in the set blocks writes |
| `kh prod` (`kdeck here`) | This terminal only switches context (`-n ns` too); other terminals keep theirs. `kh --off` returns to the shared kubeconfig |
| `kdeck prompt` | `ctx/ns` for your prompt: red on production, blue when read-only; reads only the kubeconfig, so it is fast |
| `kdeck profile save NAME [--teleport CLUSTER]` / `kdeck profile NAME` | Teleport login, context and namespace in one step |
| `kdeck compare A B` (`ctx:ns`) | Drift in images, replicas, requests/limits, env names, config refs and volumes between two namespaces or clusters; `-m` to filter workloads |

## Troubleshooting

| Command | Does |
| --- | --- |
| `kdeck why POD-REGEX` | Plain-language diagnosis with evidence and next steps: unschedulable (reason), image pull errors (not found / unauthorized / network), crash loops with exit-code meaning (137 OOM, 127, 143 ...), missing config, failing readiness probes, stuck mounts, eviction, stuck termination |
| `kdeck trace URL` | Ingress or HTTPRoute → TLS secret → Service port → endpoints → pods → container port, marking the first broken link |
| `kdeck fit [regex] [-A] [--problems]` | Live usage vs requests and limits (metrics-server): OOM risk, CPU throttling, over/under-requested, missing requests |
| `kdeck timeline deploy/api [--since 30m]` | Rollouts, pod creation, restarts with exit codes and events of a workload, its ReplicaSets and pods, in time order |
| `kdeck node-sh NODE-REGEX` | Root shell on a node via `kubectl debug` (host at `/`), debug pod deleted afterwards |
| `kdeck edit-secret NAME` | Secret values decoded in your editor, re-encoded on save, temp file wiped |

## Cost

- `kdeck cost`: monthly estimate per namespace on any cluster. Node prices are shared by each pod's dominant request (CPU or memory share of the node); unrequested capacity shows as idle; PVCs add storage. On LKE prices come from the Linode API; elsewhere set `cost.nodePrices` (by `node.kubernetes.io/instance-type`), `cost.defaultNodePrice` and `cost.storagePerGB`.
- `kdeck linode cost [re]`: list-price cost per LKE cluster (nodes, HA control plane, attached volumes) plus unattached volumes and NodeBalancers, using region prices.
- `kdeck linode leftovers [--k8s-contexts re]`: unattached volumes and NodeBalancers that no LoadBalancer Service in the given contexts points at, with monthly cost.
- `kdeck linode lke upgrade ID [--version] --yes`, `kdeck linode lke recycle ID [--pool] --yes`, `kdeck linode lke autoscale ID POOL --min --max --yes`, `kdeck linode firewalls`.

## Update and settings

`kdeck update [--check]` replaces the binary with the latest release after verifying its sha256 (Homebrew installs: `brew upgrade kdeck`). `kdeck settings export > kdeck.yaml` and `kdeck settings import kdeck.yaml` carry aliases, safety rules, profiles, Teleport, silent tuning and cost prices to another machine; kdeck stores no tokens.

Releases are built by `.goreleaser.yaml` on a `v*` tag (`.github/workflows/release.yml`): six binaries, archives, checksums, SBOM, cosign keyless signatures, Linux packages and the image (Homebrew and Scoop once their repositories exist). CI (`.github/workflows/ci.yml`) runs unit tests on Linux, macOS and Windows, builds every release package with GoReleaser and installs the `.deb`, `.rpm` and `.apk` in Debian, Rocky Linux and Alpine, and runs `hack/e2e.sh` on kind. Renovate groups the `k8s.io/*` modules so kubectl updates land as one PR.

## Shell integration

```sh
echo 'eval "$(kdeck shell-init zsh)"'  >> ~/.zshrc                 # zsh
echo 'eval "$(kdeck shell-init bash)"' >> ~/.bashrc                # bash
echo 'kdeck shell-init fish | source'  >> ~/.config/fish/config.fish
Add-Content $PROFILE 'kdeck shell-init powershell | Out-String | Invoke-Expression'   # PowerShell
kdeck shell-init cmd > "%USERPROFILE%\kdeck-macros.cmd"                                  # cmd.exe (see file)
```

Adds `k` for kdeck (`--name` to change it), helpers (`kctx`, `kns`, `kwho`, `kbad`, `ksf`, `ksh`, `ktail`, `kneat`), 62 common combinations (`--all` for about 950), your own aliases, and completion for `kdeck` and `k`. Completion alone: `kdeck completion bash|zsh|fish|powershell`.

## Configuration reference

Everything lives in `~/.kdeck` (override with `KDECK_HOME`):

| Path | Content |
| --- | --- |
| `config.yaml` | `aliases`, `silent` (checks, thresholds, suppressions, excludeNamespaces, backupLabels), `linode.defaultRegion` |
| `state.json` | Previous context and per-context namespace |
| `bin/` | Plugin links (`kubectl-<name>`), added to PATH when kdeck runs |
| `store/`, `receipts/`, `index/` | Installed plugin files, install records, krew index copy |
| `shortcuts/` | Symlinks created by `kdeck shortcuts install` |

Environment: `KDECK_HOME`, `KDECK_NO_FZF=1` (never open the picker), `KDECK_DEBUG_ALIAS=1` (print expansions), `LINODE_TOKEN`, `LINODE_URL` (alternative API endpoint, used by tests).

## Security notes

- `kdeck silent` is read-only. It reads Secrets only with `--sensitive`, parses certificates in memory, and never prints secret data. It needs only `get`/`list` permissions; forbidden checks are skipped with a reason instead of failing the run.
- `kdeck decode` prints secret material by design. `kdeck clean` deletes only with `--yes`. Linode changes need `--yes`.
- Plugin downloads are verified against the index sha256 and refused on mismatch.
- `kdeck linode lke kubeconfig --merge` backs up your kubeconfig before writing (`<file>.kdeck-backup-<timestamp>`).
- Object names are stripped of control characters before printing.

## Development

```sh
make test     # unit tests: shortcut grammar, all checks (fake clientset), suppressions,
              # cron/timezone maths, certificates, kubeconfig merge, krew platform
              # matching and archive safety, neat
make vet
make build    # bin/kdeck
```

End-to-end test on a throwaway kind cluster, which never touches your kubeconfig:

```sh
kind create cluster --name kdeck-e2e --kubeconfig /tmp/kdeck-e2e.kubeconfig
export KUBECONFIG=/tmp/kdeck-e2e.kubeconfig KDECK_HOME=/tmp/kdeck-e2e-home
kdeck get nodes && kdeck kgpoa && kdeck who && kdeck nodes && kdeck ns kube-system && kdeck ns -
# apply broken scenarios (failing CronJob, webhook with no backend, stale Lease,
# dangling RoleBinding, PDB with minAvailable=replicas, Ingress to a missing
# Service, Deployment with a bad image, PVC stuck on a finalizer, expired TLS Secret)
kdeck silent && kdeck silent --sensitive --check certificate
kind delete cluster --name kdeck-e2e --kubeconfig /tmp/kdeck-e2e.kubeconfig
```

On 1 Nov 2026 this found all 12 planted failures in 109 ms on kind v1.37.0.

Layout:

```text
cmd/kdeck/               entry point
internal/app/         embeds kubectl, adds kdeck commands, alias expansion, plugin dispatch
internal/alias/       shortcut grammar, verb shortcuts, user aliases
internal/kctx/        ctx and ns
internal/quick/       everyday helpers
internal/silent/      snapshot, 15 checks, engine, suppressions, output
internal/plugin/      krew-compatible plugin manager
internal/linode/      LKE and Linode resources
internal/shell/       shell-init, alias, shortcuts
internal/config/      ~/.kdeck config and state
```

## Limitations and roadmap

- Not built: telemetry-silence checks (SF-TELEMETRY-001), and persisting `kdeck silent serve` first-seen times across restarts.
- `kdeck plugin install` has been tested with unit tests and a live index download, but no real plugin was installed during development.
- Linode commands were tested against a local mock of the API, not a real account.
- The binary is large (about 65 MB stripped, 98 MB unstripped) because it contains kubectl.

## License

MIT. See [LICENSE](LICENSE).

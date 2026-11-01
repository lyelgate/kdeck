# kubepilot (`kp`)

One binary to run Kubernetes end to end: **all of kubectl**, built in and unchanged, plus shortcuts, context and namespace switching, everyday helpers, **silent-failure detection**, a **krew-compatible plugin manager**, and **Linode LKE** management.

```text
kp get pods                      # any kubectl command, identical behaviour
kp kgpoa                         # shortcut: get pods --all-namespaces
kp ctx prod && kp ns payments    # switch context and namespace ("-" = previous)
kp bad                           # unhealthy pods, all namespaces, worst first
kp silent                        # what is quietly broken, and since when
kp plugin install neat           # krew plugins without krew
kp linode lke kubeconfig 12345 --merge --set-current   # LKE cluster -> kubectl
```

- [Install](#install)
- [Quick start](#quick-start)
- [kubectl, built in](#kubectl-built-in)
- [Shortcuts and aliases](#shortcuts-and-aliases)
- [Contexts and namespaces](#contexts-and-namespaces)
- [Everyday helpers](#everyday-helpers)
- [Silent-failure detection](#silent-failure-detection-kp-silent)
- [Plugins (krew-compatible)](#plugins-krew-compatible)
- [Linode LKE](#linode-lke)
- [Regex everywhere](#regex-everywhere)
- [One pod by regex: kplog abc](#one-pod-by-regex-kplog-abc)
- [Stern-style logs](#stern-style-logs-kp-tail-also-kp-stern)
- [Port-forward shortcuts](#port-forward-shortcuts-kp-pf)
- [Clusters on AWS, Google Cloud and Azure](#clusters-on-aws-google-cloud-and-azure)
- [Teleport plugin](#teleport-plugin-tsh)
- [Setup and diagnostics](#setup-and-diagnostics)
- [Platforms](#platforms)
- [Shell integration](#shell-integration)
- [Configuration reference](#configuration-reference)
- [Security notes](#security-notes)
- [Development](#development)
- [Limitations and roadmap](#limitations-and-roadmap)

## Install

Requires Go 1.26 or newer to build. The binary is about 65 MB (stripped) because it contains all of kubectl.

```sh
git clone <this repo> kubepilot && cd kubepilot
make install            # builds and copies kp to ~/go/bin (or set PREFIX)
kp about                # version, embedded kubectl version, home directory
```

Cross-compile release binaries for Linux, macOS and Windows (amd64 and arm64):

```sh
make dist VERSION=v0.1.0   # writes dist/kp-<os>-<arch>[.exe]
```

## Quick start

```sh
eval "$(kp shell-init zsh)"     # adds: k, kgpo, kdpo, kctx, kns, ... and completion
k who                           # where am I and who does the API server think I am
k ctx                           # pick a context (fzf if installed)
k ns                            # pick a namespace
k pods                          # pods, problems first
k silent                        # silent failures across the cluster
```

## kubectl, built in

kp embeds `k8s.io/kubectl` (v0.37.2) as a library, so **every kubectl command, flag, output format and behaviour is identical**: `get`, `describe`, `apply`, `diff`, `edit`, `logs`, `exec`, `port-forward`, `cp`, `rollout`, `scale`, `top`, `debug`, `auth`, `kustomize`, `events`, `config`, `explain`, `wait`, and the rest. Global flags (`--context`, `-n`, `--kubeconfig`, `--as`, ...) work everywhere, including on kp's own commands.

kubectl's own `~/.kube/kuberc` preferences and aliases still apply. A symlink named `kubectl` pointing at kp makes it a drop-in kubectl replacement.

## Shortcuts and aliases

A shortcut is expanded only when the word is **not** a real command, so nothing kubectl does is ever hijacked. Order of precedence: your aliases, verb shortcuts, then combinations. See what any word does with `kp alias explain <word>`; list everything with `kp alias list` (add `--all` for every combination).

### Combination grammar (kubectl-aliases compatible)

Words follow `[k][sys]<cmd>[<resource>][<modifiers>...]`, the same grammar as [kubectl-aliases](https://github.com/ahmetb/kubectl-aliases). The leading `k` is optional inside kp (`kp gpo` = `kp kgpo`).

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
kp alias set kgr 'get pods --field-selector=status.phase=Running'
kp kgr -A
kp alias rm kgr
```

Stored in `~/.kp/config.yaml`; they take precedence over built-in shortcuts and are included in `kp shell-init`.

### Without shell aliases (busybox style)

`kp shortcuts install` creates symlinks (`k`, `kgpo`, `kdpo`, ...) to the kp binary in `~/.kp/shortcuts`. Put that directory on PATH and the shortcut names work as commands in any shell, script or CI job. `--all` links every combination.

Debug expansion with `KP_DEBUG_ALIAS=1 kp kgpoa`.

## Contexts and namespaces

| Command | Does |
| --- | --- |
| `kp ctx` | Pick a context with fzf; without fzf (or with `KP_NO_FZF=1`), list them |
| `kp ctx NAME` | Switch context |
| `kp ctx -` | Back to the previous context |
| `kp ctx -c` | Print the current context |
| `kp ctx rename OLD NEW` / `kp ctx rm NAME` | Rename or delete a context |
| `kp ns` | Pick or list namespaces (from the API) |
| `kp ns NAME` | Set the current context's namespace (`--force` if it does not exist yet) |
| `kp ns -` | Back to the previous namespace for this context |
| `kp ns -c` | Print the current namespace |

The previous context and per-context namespace are remembered in `~/.kp/state.json`.

## Everyday helpers

| Command | Does |
| --- | --- |
| `kp who` | Context, cluster, server, namespace, the identity the API server sees (SelfSubjectReview), server version |
| `kp pods [regex] [-A] [-l sel] [--bad]` | Pods sorted problems first, with the real status (`CrashLoopBackOff`, `ImagePullBackOff`, `OOMKilled`) and restarts |
| `kp bad` | Unhealthy pods in all namespaces |
| `kp nodes` | Readiness, cordon state, kubelet version, pods used/allocatable, CPU and memory requested vs allocatable |
| `kp sh POD-REGEX [-c c] [-- cmd]` | Shell into the best matching pod: bash if present, otherwise sh |
| `kp tail POD-REGEX or TYPE/NAME` | Stern-style multi-pod logs (see below) |
| `kp restart TYPE/NAME...` | Rolling restart, then wait for each rollout to finish |
| `kp images [-A]` | Every image in use and how many pods run it |
| `kp neat TYPE/NAME` | YAML without status, managedFields, uid, resourceVersion and last-applied, ready to re-apply |
| `kp decode SECRET [KEY]` | A Secret's values decoded (prints secret material) |
| `kp clean [-A] [--completed] [--yes]` | Delete Failed and Evicted pods; dry run unless `--yes` |
| `kp each -l SEL -- CMD...` | Run a command in every running pod of a selector |
| `kp netshoot` | Throwaway troubleshooting pod (dig, curl, tcpdump), removed on exit |
| `kp usage [-A] [--by cpu]` | Top pods by memory or CPU |

## Silent-failure detection (`kp silent`)

Finds failures that produce **no crash and no alert**: backups that stopped succeeding, certificates about to expire, controllers that stopped renewing their lease, webhooks and Services with nothing behind them, HPAs frozen because metrics are down, PDBs that will block every drain. Every finding has a severity, **since when** (taken from the API, never guessed; "unknown" when the API does not record it), the evidence, and a next step. Read-only.

```text
$ kp silent
SEVERITY  CHECK           OBJECT                                        SINCE      SUMMARY
critical  SF-CRON-001     broken/CronJob/nightly-backup                 2m24s ago  never succeeded; 3 scheduled runs since creation
critical  SF-WEBHOOK-001  ValidatingWebhookConfiguration/kp-e2e-policy  unknown    webhook strict.example.com: Service broken/deleted-webhook does not exist; failurePolicy=Fail: matching API requests are rejected
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

`kp silent --list` prints the catalogue. Checks whose API is not installed (for example Velero) are listed with `--explain-skipped`.

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

CI example: `kp silent -o json --fail-on high > silent.json`.

### Tuning and suppressing

```yaml
# ~/.kp/config.yaml
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

kp reads the [krew index](https://github.com/kubernetes-sigs/krew-index) directly; krew itself is not needed. Plugins install into `~/.kp` and run as `kp <name>`. Plugins installed earlier with krew (`~/.krew/bin`) keep working.

| Command | Does |
| --- | --- |
| `kp plugin update` | Download the index |
| `kp plugin search [query]` | Search names and descriptions |
| `kp plugin info NAME` | Version, homepage, platforms, caveats |
| `kp plugin install NAME...` | Download, **verify sha256**, unpack, link `kubectl-<name>` |
| `kp plugin list` | kp-managed, krew-managed and any `kubectl-*` on PATH |
| `kp plugin upgrade [NAME...]` | Upgrade to the index version |
| `kp plugin uninstall NAME...` | Remove |

Plugins only run when no built-in or kp command matches, so a plugin can never shadow a kp command. Archives are checked for path traversal before unpacking.

## Linode LKE

Uses the official `linodego` SDK. The token comes from `--token`, `LINODE_TOKEN`, `LINODE_CLI_TOKEN`, or the default user in the linode-cli config file. kp never stores it. Anything that creates, changes or deletes resources needs `--yes`.

| Command | Does |
| --- | --- |
| `kp linode lke ls` | Clusters: ID, label, region, version, status, HA, tags |
| `kp linode lke get ID` | One cluster plus node pools |
| `kp linode lke kubeconfig ID` | Print the kubeconfig; `-f FILE` to write it; `--merge [--set-current] [--context-name]` to merge into your kubeconfig (backup first, entries renamed to `lke-<label>` so clusters never collide) |
| `kp linode lke pools ID` | Node pools: type, ready/count, autoscaler range |
| `kp linode lke scale ID POOL COUNT --yes` | Resize a pool |
| `kp linode lke create --label --region --version --pool TYPE:COUNT [--ha] --yes` | Create a cluster |
| `kp linode lke delete ID --yes` | Delete a cluster (warns that volumes remain) |
| `kp linode lke versions` | Kubernetes versions offered |
| `kp linode regions` / `types` | Regions (with LKE availability) and node plans with prices |
| `kp linode volumes ls [--unattached]` | Volumes; `--unattached` shows ones billing with no node |
| `kp linode nodebalancers` / `instances` | NodeBalancers (from LoadBalancer Services) and instances |
| `kp linode cli ARGS...` | Anything else, passed to `linode-cli` if installed |

From zero to kubectl in one line: `kp linode lke kubeconfig 12345 --merge --set-current && kp get nodes`.

## Regex everywhere

Every name argument is a **regular expression with smart case**: lower-case patterns ignore case, a pattern with an upper-case letter matches exactly. `api` matches `payments-API-7d9f`; `^web-[0-9]+$` matches `web-12` only.

| Where | Regex applies to |
| --- | --- |
| `kp log/desc/img/po/pod/sh/pf/tail` | pod (or Service) names; containers with `-c` |
| `kp pods`, `kp nodes`, `kp images` | names, node names, image references |
| `kp find REGEX [kinds]` | names (and labels with `--labels`) across many kinds, any CRD |
| `kp restart`, `kp each`, `kp clean --match` | workload / pod names |
| `kp ctx`, `kp ns`, `kp tu`, `kp eks/gke/aks use` | context, namespace, Teleport and cloud cluster names (exact name wins) |
| `kp silent -m`, `kp plugin search`, `kp alias list` | findings, plugins, shortcuts |
| `kp tail -i/-e` | log lines to include / exclude |

## One pod by regex: `kplog abc`

These commands pick **one pod**: an exact name wins, otherwise the best running and ready match; with several matches fzf opens on a terminal, otherwise the best one is used and the rest are named. `--all` acts on every match. Pod names cannot contain `_` (Kubernetes allows lower-case letters, digits, `-` and `.`).

| Command | kp-prefixed name | Does |
| --- | --- | --- |
| `kp log abc [-f] [-p] [-c re] [--all]` | `kplog` | Logs (all containers prefixed; `--all` = every match, stern-style) |
| `kp desc abc [--all]` | `kpdesc` | `kubectl describe pod` |
| `kp img abc [--all]` | `kpimg` | Each container's image and the digest actually running |
| `kp po abc [-o yaml]` | `kppo` | `kubectl get pods -o wide` for every match |
| `kp pod abc` | `kppod` | Detail card: node, IP, owner, QoS, containers, restarts, last crash, recent warnings |
| `kp sh abc [-- cmd]` | `kpsh` | Shell into the best match |
| `kp pf abc [ports]` | `kppf` | Port-forward (below) |
| `kp tail abc` | `kptail` | Stern-style logs (below) |
| `kp ns re` / `kp ctx re` | `kpns` / `kpctx` | Switch namespace / context |

`kp shortcuts install` creates all of these as real commands (symlinks on macOS and Linux, `.cmd` shims on Windows), so `kplog abc` works directly in any shell.

## Stern-style logs: `kp tail` (also `kp stern`)

Built in, no stern binary needed. Follows every pod matching a regex and every container matching `-c`; in follow mode it watches the API, so pods from scale-ups and rollouts join automatically and restarted containers resume without repeating history. Colour per pod.

```sh
kp tail api                        # every pod with "api" in the name
kp tail '^web-' -c nginx           # nginx containers only
kp tail . -A -i error -e healthz   # all pods, lines with "error", without "healthz"
kp tail deploy/api --since 10m     # pods of a workload
kp tail api -l tier=backend -t     # regex + selector, with timestamps
```

Flags: `-c` container regex, `-E` exclude pods, `-i`/`-e` include/exclude lines (repeatable), `-l` selector, `--since`, `--tail`, `--no-follow`, `-t`, `--init-containers`, `--color auto|always|never`, `-A`.

## Port-forward shortcuts: `kp pf`

```sh
kp pf grafana              # finds svc/grafana, forwards all its ports (80 -> localhost:8080)
kp pf api 9000:8080        # Service matching "api", explicit ports
kp pf '^web-0$' --pod      # a pod instead of a Service
kp pf deploy/api 8080      # TYPE/NAME goes straight to kubectl
kp pf grafana --address 0.0.0.0
```

Services are preferred over pods; ports below 1024 map to +8000 locally so no root is needed; several matches open the picker or are listed.

## Clusters on AWS, Google Cloud and Azure

kp uses each provider's official CLI, so your existing logins, SSO and profiles work. Connecting backs up the kubeconfig first; `--name` gives the context a short name.

| Command | Uses | Does |
| --- | --- | --- |
| `kp eks ls [re] [--region] [--profile]` | `aws` | List EKS clusters |
| `kp eks use re [--region] [--name prod]` | `aws eks update-kubeconfig` | Connect and switch |
| `kp gke ls [re] [--project]` | `gcloud` | List GKE clusters |
| `kp gke use re [--name]` | `gcloud container clusters get-credentials` | Connect (needs `gke-gcloud-auth-plugin`) |
| `kp aks ls [re] [--subscription]` | `az` | List AKS clusters |
| `kp aks use re [--name] [--kubelogin]` | `az aks get-credentials` (+ `kubelogin convert-kubeconfig -l azurecli`) | Connect, Entra ID clusters too |
| `kp ctx import FILE [--name] [--set-current]` | - | Merge any kubeconfig (backup, no overwrites) |

Credential plugins in kubeconfigs (`aws`, `gke-gcloud-auth-plugin`, `kubelogin`, `tsh`) work unchanged because kp uses client-go; `kp doctor` reports any that are missing.

## Teleport plugin (`tsh`)

Set up once; it asks only for the proxy:

```sh
kp setup                    # or: kp tsh setup tp.example.com:443
```

Then:

| Short | Full | Does |
| --- | --- | --- |
| `kp tl` | `kp tsh login` | `tsh login --proxy=<saved>` (plus saved `--user` / `--auth`) |
| `kp tls [re]` | `kp tsh ls` | Kubernetes clusters you can reach, filtered by regex (logs in first if needed) |
| `kp tu re` | `kp tsh use` | `tsh kube login <match>` and switch kubectl (logs in first if needed) |
| `kp ts` | `kp tsh status` | Session status |
| `kp tlo` | `kp tsh logout` | Log out |
| | `kp tsh -- ARGS` | Anything else, passed to tsh |

kp wraps the official tsh binary (install from goteleport.com/download, `brew install teleport`, or `winget install Teleport.Teleport`) and never handles Teleport credentials itself.

## Setup and diagnostics

- `kp setup`: one question (Teleport proxy, Enter to skip), then the tools found and the exact line for your shell; `--write-rc` appends it for bash, zsh or fish.
- `kp doctor [--all-contexts]`: kubeconfig, auth type and credential plugin per context, cluster reachability, kubectl/cluster version skew (warns outside ±1 minor), tools, Teleport session.

## Platforms

| OS | CPU | Shells |
| --- | --- | --- |
| macOS | Apple silicon (arm64), Intel (amd64) | zsh, bash, fish, PowerShell |
| Linux, including WSL | amd64, arm64 | bash, zsh, fish, PowerShell |
| Windows | amd64, arm64 | PowerShell, cmd.exe |

`make dist` builds all six binaries. On Windows, `kp shortcuts install` writes `.cmd` shims (no admin rights needed) and `kp shell-init powershell` / `kp shell-init cmd` generate PowerShell functions or doskey macros.

## Shell integration

```sh
echo 'eval "$(kp shell-init zsh)"'  >> ~/.zshrc                 # zsh
echo 'eval "$(kp shell-init bash)"' >> ~/.bashrc                # bash
echo 'kp shell-init fish | source'  >> ~/.config/fish/config.fish
Add-Content $PROFILE 'kp shell-init powershell | Out-String | Invoke-Expression'   # PowerShell
kp shell-init cmd > "%USERPROFILE%\kp-macros.cmd"                                  # cmd.exe (see file)
```

Adds `k` for kp (`--name` to change it), helpers (`kctx`, `kns`, `kwho`, `kbad`, `ksf`, `ksh`, `ktail`, `kneat`), 62 common combinations (`--all` for about 950), your own aliases, and completion for `kp` and `k`. Completion alone: `kp completion bash|zsh|fish|powershell`.

## Configuration reference

Everything lives in `~/.kp` (override with `KP_HOME`):

| Path | Content |
| --- | --- |
| `config.yaml` | `aliases`, `silent` (checks, thresholds, suppressions, excludeNamespaces, backupLabels), `linode.defaultRegion` |
| `state.json` | Previous context and per-context namespace |
| `bin/` | Plugin links (`kubectl-<name>`), added to PATH when kp runs |
| `store/`, `receipts/`, `index/` | Installed plugin files, install records, krew index copy |
| `shortcuts/` | Symlinks created by `kp shortcuts install` |

Environment: `KP_HOME`, `KP_NO_FZF=1` (never open the picker), `KP_DEBUG_ALIAS=1` (print expansions), `LINODE_TOKEN`, `LINODE_URL` (alternative API endpoint, used by tests).

## Security notes

- `kp silent` is read-only. It reads Secrets only with `--sensitive`, parses certificates in memory, and never prints secret data. It needs only `get`/`list` permissions; forbidden checks are skipped with a reason instead of failing the run.
- `kp decode` prints secret material by design. `kp clean` deletes only with `--yes`. Linode changes need `--yes`.
- Plugin downloads are verified against the index sha256 and refused on mismatch.
- `kp linode lke kubeconfig --merge` backs up your kubeconfig before writing (`<file>.kp-backup-<timestamp>`).
- Object names are stripped of control characters before printing.

## Development

```sh
make test     # unit tests: shortcut grammar, all checks (fake clientset), suppressions,
              # cron/timezone maths, certificates, kubeconfig merge, krew platform
              # matching and archive safety, neat
make vet
make build    # bin/kp
```

End-to-end test on a throwaway kind cluster, which never touches your kubeconfig:

```sh
kind create cluster --name kp-e2e --kubeconfig /tmp/kp-e2e.kubeconfig
export KUBECONFIG=/tmp/kp-e2e.kubeconfig KP_HOME=/tmp/kp-e2e-home
kp get nodes && kp kgpoa && kp who && kp nodes && kp ns kube-system && kp ns -
# apply broken scenarios (failing CronJob, webhook with no backend, stale Lease,
# dangling RoleBinding, PDB with minAvailable=replicas, Ingress to a missing
# Service, Deployment with a bad image, PVC stuck on a finalizer, expired TLS Secret)
kp silent && kp silent --sensitive --check certificate
kind delete cluster --name kp-e2e --kubeconfig /tmp/kp-e2e.kubeconfig
```

On 1 Nov 2026 this found all 12 planted failures in 109 ms on kind v1.37.0.

Layout:

```text
cmd/kp/               entry point
internal/app/         embeds kubectl, adds kp commands, alias expansion, plugin dispatch
internal/alias/       shortcut grammar, verb shortcuts, user aliases
internal/kctx/        ctx and ns
internal/quick/       everyday helpers
internal/silent/      snapshot, 15 checks, engine, suppressions, output
internal/plugin/      krew-compatible plugin manager
internal/linode/      LKE and Linode resources
internal/shell/       shell-init, alias, shortcuts
internal/config/      ~/.kp config and state
```

## Limitations and roadmap

- Silent checks from the design that are not built yet: API server certificate, kubelet CSR rotation, StatefulSet/DaemonSet rollouts, filesystem-smaller-than-PVC, volume nearly full, Released PVs, forgotten cordons, quota exhaustion, failed-pod build-up, telemetry silence, and an in-cluster mode with Prometheus metrics.
- `kp plugin install` has been tested with unit tests and a live index download, but no real plugin was installed during development.
- Linode commands were tested against a local mock of the API, not a real account.
- The binary is large (about 65 MB stripped, 98 MB unstripped) because it contains kubectl.

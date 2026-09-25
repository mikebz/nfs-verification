# 08: Cluster traits and preflight architecture: discovery, capabilities, and runner separation

Author: mikebz@
Created: 2026-09-21
Updated: 2026-09-21
Status: designed, Step 9 / [PR #92](https://github.com/mikebz/nfs-verification/pull/92).
Serves: PROV-07, PROV-08, DATA-07, DATA-11, CHAOS-01, CHAOS-02, CHAOS-05, CHAOS-06, CHAOS-07, OBS-03, OBS-04, OBS-05, OBS-06, OBS-07, SEC-02. Requirements in [`01-test-plan.md`](01-test-plan.md) Section 0, Section 3, and Section 4.1.
Builds on: [`01-test-plan.md`](01-test-plan.md), [`02-provisioning-design.md`](02-provisioning-design.md), [`03-chaos-operations-design.md`](03-chaos-operations-design.md), [`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md), [`06-observability-design.md`](06-observability-design.md), and [`07-security-design.md`](07-security-design.md).

---

## 1. Why this domain exists

The test harness exists to verify what a client observes of an NFS RWX volume on Kubernetes. To do that without taking down the cluster, the harness must know what kind of cluster it is driving before it injects a fault or makes an assertion.

Today, static properties of the cluster, the server, and the container image are re-evaluated repeatedly across three places:

1. **Fragile probes on the critical fault path**: Finding F-026 and Finding F-027 demonstrated that probing static traits at fault injection time is inherently fragile. Inside a server pod, a container exec that succeeded on a freshly started container failed after hours of uptime due to file descriptor access restrictions. Probing the process holding port 2049 once during preflight resolved a problem that had blocked CHAOS-01, DATA-12, and DATA-13 for months.
2. **Expensive runtime discovery that delays blocked verdicts**: Nine separate cases across `chaos_test.go`, `obs_test.go`, and `prov_test.go` inspect server pod ownership at runtime. Each case first provisions an RWX PVC, schedules two client pods, waits up to two minutes for images to pull and volumes to mount, only to check `s.target.Controller == ""` and report blocked. Discovering controller management during preflight saves minutes of cluster setup per case.
3. **Repetitive run habits in documentation**: The harness implementation already supports discovery and caching: [`pkg/framework/config.go`](../pkg/framework/config.go) discovers storage classes and server pods when empty, and [`test/e2e/main_test.go`](../test/e2e/main_test.go) reuses `artifacts/preflight-<context>.json`. What remained repetitive was the README's run examples, which pasted `FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"` across every target.

The goal of this design is not typing convenience; it is **execution reliability and eliminating redundant runtime discovery**:
- **What discover-once buys**: Removes execs from the critical fault path, eliminates several minutes of throwaway pod and claim provisioning before blocked cases, and catches undersized worker nodes before they reboot mid-run.
- **When a cached answer is valid**: Preflight results are cached per kubeconfig context. A cache is valid while the underlying cluster, node pool, and `-tools-image` remain unchanged.
- **What happens when a trait is missing**: The suite must never silently downgrade a missing deployment precondition into an unexercised pass. The invariant governing this change is: **discovery moves, the verdict does not.**

---

## 2. Taxonomy: Traits vs. Policies vs. Runner Controls

Configuration in this repository divides into three distinct categories:

| Category | Definition | Lifecycle | Examples |
|---|---|---|---|
| **Cluster Traits** | Immutable properties of the cluster, its worker nodes, its storage class, and its server workload. Discovered automatically by preflight. | Evaluated once per cluster context; cached in `environment.json`. | Schedulable node count, worker node capacity, StorageClass reclaim policy, server process name, server controller management, proxy RBAC permissions, tools image capabilities. |
| **Policy Assertions** | Expectations declared by the operator to enforce that a deployment satisfies a specific requirement, rather than merely recording what it does. | Provided to preflight or test suite as assertions; evaluated against discovered traits. | Mandatory lease/grace profile (`-profile=tuned`), enforced root-squash behavior (`-root-squash=on`). |
| **Runner Controls** | Operational parameters governing test execution, verbosity, output locations, and cache management. | Passed per test invocation to `make` or `go test`. | Test selection (`-run`, `CASE=...`, `-short`), output directories (`-artifacts-dir`, `-run-id`), triage options (`-keep-objects`), cache control (`-refresh-preflight`, `-env-file`). |

The guiding rule from AGENTS.md holds: *Discovery over declaration: if the cluster can answer it, discover it. A flag exists only for what a cluster genuinely cannot answer.*

---

## 3. What preflight strictly evaluates

Preflight evaluates all properties of the cluster and server that do not vary mid-run. All evaluations must be strictly non-destructive.

### 3.1 Worker node capacity floor (F-002)

Finding F-002 showed that 2GB worker nodes (`e2-small` on GKE) cannot host the suite: platform system daemons consume most of the memory, pushing nodes into kernel memory pressure and causing spontaneous node reboots during test runs.

- **Field checked**: Preflight inspects `node.Status.Capacity["memory"]` on all schedulable nodes.
- **Threshold**: **3.5 GiB** (~3,758,096 KiB).
  - An `e2-medium` node (4GB advertised capacity) reports capacity around 4,025,932 KiB (~3.84 GiB). GKE platform reservations and eviction thresholds leave *allocatable* memory at 2.8–3.0 GiB. Gating on allocatable memory at 4GiB would fail the reference clusters; gating on capacity at 3.5 GiB cleanly rejects 2GB nodes (`e2-small` ~ 1.9GiB capacity) while safely passing `e2-medium` and larger workers.
- **Verdict**: If any schedulable node reports capacity below 3.5 GiB, preflight stops hard: `schedulable node has insufficient memory capacity (< 3.5GiB)`.
- **Allocatable recorded**: `node.Status.Allocatable["memory"]` is recorded in [`NodeInfo`](../pkg/env/environment.go) for triage and triage runbooks, but not gated on, because daemon reserves vary across cloud platforms and bare metal.
- **Decision**: Upgrades F-002's open suggestion of a warning into a hard preflight stop.

### 3.2 Server workload controller management

Nine cases across the suite delete the server pod to test failover and recovery (PROV-07, PROV-08, CHAOS-02, CHAOS-05, CHAOS-06, CHAOS-07, OBS-03, OBS-04, OBS-07). If the server pod is unmanaged (a standalone pod without a Deployment, StatefulSet, or DaemonSet controller), deleting it destroys the export permanently.

- **Preflight evaluation**: Preflight inspects the `OwnerReferences` of discovered server pods. If a controller reference (`controller: true`) is present, it records the controller kind and name in `ServerInfo.Controller` and sets `ServerControlled: true` in [`Capabilities`](../pkg/framework/capabilities.go).
- **Workload-level scope**: Like `ServerInfo.Process` (which records the process name rather than a transient PID), `ServerControlled` describes the *workload specification*, not an ephemeral pod UID. If a chaos case deletes a pod and a replacement pod appears, the workload controller relationship remains valid.
- **Verdict preserved**: If `ServerControlled` is false:
  - Preflight logs an explicit warning note in `environment.json` and on stderr.
  - The nine server-deletion cases check this capability and immediately report **`blocked`**, preserving the exact triage message: `blocked: server pod %s has no controller, so deleting it would not bring it back`.
  - The cases do not waste time creating test claims or pods before reporting blocked.

### 3.3 Tools image capabilities on the mounted share

DATA-07 verifies direct I/O, and DATA-11 verifies hole punching. These properties depend on two things: the binary in `-tools-image` supporting the flag, and the underlying NFS mount supporting the operation.

- **Preflight evaluation**: Preflight already creates two test pods (`a` and `b`) mounting an RWX PVC. While these pods are up, preflight runs non-destructive probes **against a temporary file on the mounted NFS share** (`/mnt/share/probe-direct.tmp` and `/mnt/share/probe-fallocate.tmp`):
  - Probes `dd if=/dev/zero of=/mnt/share/probe-direct.tmp bs=4k count=1 oflag=direct`.
  - Probes `fallocate -p -o 0 -l 4096 /mnt/share/probe-fallocate.tmp`.
  - Removes the probe files.
- **Separation of causes**:
  - If the tool binary itself lacks the flag (e.g. `fallocate: unrecognized option`), preflight records that the tool is absent from `-tools-image`. DATA-11 reports **`blocked`** naming `-tools-image`.
  - If the tool supports the flag but the NFS mount returns `ENOTSUP` or `EINVAL`, preflight records that the share does not support the operation. DATA-07 or DATA-11 runs its case logic to assert or document the protocol boundary.
- **Workstation tools excluded**: `LocktoolReady` is a property of `bin/` on the runner workstation built by `make locktool`. It is not a cluster trait and is not probed by preflight; lock cases continue to check local binary availability directly.
- **Cache invalidation**: The preflight cache key incorporates the `-tools-image` name. Changing the tools image automatically invalidates cached tool capability records.

### 3.4 API proxy RBAC permissions

OBS-05 and OBS-06 read the kubelet Summary API via `nodes/proxy`. OBS-07 reads Prometheus metrics via `pods/proxy`.

- **Preflight evaluation**: Preflight performs probe `GET`s or `SelfSubjectAccessReview` checks for `nodes/proxy` on the schedulable nodes and `pods/proxy` on the discovered server pod.
- **Capabilities recorded**: `CanProxyNodes` and `CanProxyPods` in [`Capabilities`](../pkg/framework/capabilities.go).
- **Verdict preserved**: If permissions are missing, preflight prints a warning note. OBS-05, OBS-06, and OBS-07 report **`blocked`** naming the missing RBAC permission, rather than failing with unhandled HTTP 403 Forbidden errors.

### 3.5 Static PersistentVolume admission

OBS-04 tests mount failure surfacing by manufacturing a broken static PersistentVolume pointing at an unroutable server address. On clusters enforcing restrictive admission webhooks (e.g. OPA Gatekeeper, Kyverno) or restricted RBAC, static PV creation is denied.

- **Preflight evaluation**: Preflight issues a dry-run create request (`dryRun: ["All"]`) for a dummy static PersistentVolume.
- **Why dry-run instead of access review**: A `SelfSubjectAccessReview` verifies only Kubernetes RBAC; it misses admission webhooks, which are the primary mechanism that blocks static PVs on managed cloud clusters.
- **Capability recorded**: `CanCreateStaticPV` in [`Capabilities`](../pkg/framework/capabilities.go).
- **Verdict preserved**: If static PV creation is denied, OBS-04 reports **`blocked: this cluster does not allow a static NFS PersistentVolume`** without attempting a live creation.

### 3.6 StorageClass reclaim policy and root-squash baseline

- **StorageClass Reclaim Policy**: Preflight inspects the candidate StorageClass and records `reclaimPolicy` (`Delete` or `Retain`) in [`Environment`](../pkg/env/environment.go). PROV-01 skips with an explanatory note when `Retain` is configured, pointing to PROV-06.
- **Root-Squash Baseline**: During two-pod mount verification, preflight attempts a root `chown` on a probe file on the share.
  - If the probe pod cannot run as root due to Pod Security Standards, preflight records root-squash as `unknown (restricted PSS)` without failing.
  - If permitted, it records whether root was squashed in [`Environment`](../pkg/env/environment.go). SEC-02 uses this recorded baseline when running with `-root-squash=auto`.

---

## 4. The Non-Destructive Invariant

README states: *Only `make unit` and `make preflight` leave the cluster alone.*

Standardizing cluster traits into preflight strictly preserves this invariant:
- Preflight may create, mount, write to, and delete its own ephemeral test PVC and pods in namespace `default`.
- Preflight must **never** signal host processes, delete server pods, alter node taints, or induce network partitions.
- Properties that can only be measured by destroying state remain inside their respective test groups:
  - Grace period log announcements (`-grace-enter-pattern`) only appear on restart; observing them remains in CHAOS-05 and OBS-03.
  - Actual lock state survival across a server restart remains in CHAOS-02 and CHAOS-06.

Preflight evaluates the *preconditions* for those tests (whether the server process is named and whether a controller will restart the pod), never the fault recovery itself.

---

## 5. The Invariant: Discovery Moves, the Verdict Does Not

Moving checks from test cases to preflight must never change what the suite reports about a cluster:

| Classification | Meaning | Action | Example |
|---|---|---|---|
| **Hard Preflight Failure** | Cluster cannot host the suite safely or violates Section 0 preconditions. | Suite exits non-zero; no tests run. | < 2 schedulable nodes; worker capacity < 3.5 GiB; no RWX StorageClass; node agent cannot schedule. |
| **Capability Skip (`requireCap`)** | Legitimate optional platform or CSI feature not present in this cluster. | Test reports `skipped` naming the missing capability. | Single-stack cluster (`!DualStack`); StorageClass lacks expansion (`!CanExpand`); VolumeSnapshot CRD missing (`!CanSnapshot`). |
| **Blocked Verdict (`reportBlocked`)** | Cluster configuration or deployment arrangement prevents an assertion from executing, representing a finding. | Test reports `blocked` naming the root cause. | Server pod has no controller; kubeconfig lacks proxy RBAC; `-tools-image` lacks `fallocate -p`. |

When a preflight trait is missing for a blocked condition:
1. Preflight records the trait as false and prints an explicit warning in the preflight summary.
2. The affected test case queries the preflight environment and immediately calls `t.Skipf("blocked: ...")` naming the specific cause, preserving the exact triage message without spinning up throwaway pods.

---

## 6. Execution, Caching, and Invalidation

### Operator workflow
1. **Initial discovery**: `make preflight [CLUSTER_FLAGS]` verifies Section 0 preconditions, discovers cluster traits, and writes both `artifacts/<run-id>/environment.json` and `artifacts/preflight-<context>.json` (cached for up to 8 hours).
2. **Subsequent test runs**: `make test-prov`, `make test-chaos`, or `make test-case CASE=...` run cleanly without repeating cluster trait flags. `TestMain` automatically loads the cached environment.

### Invalidation boundaries
A cached preflight record is invalidated when:
- **Time expires**: Older than `-preflight-max-age` (default 8 hours).
- **Explicit refresh**: The operator passes `-refresh-preflight`.
- **Tools image change**: The `-tools-image` flag differs from the image recorded in the cached environment.
- **Kubeconfig context change**: The cache is keyed by kubeconfig context name (`preflight-<context>.json`).

If an operator resizes a node pool or alters server controllers, `-refresh-preflight` forces a complete re-evaluation.

---

## 7. Interface and Schema Updates

Changes to the Go codebase supporting this design land in PR 2:

- **[`pkg/env/environment.go`](../pkg/env/environment.go)**:
  - Add `Controller` to `ServerInfo` describing the workload controller managing the server pod.
  - Add `CapacityMemoryBytes` and `AllocatableMemoryBytes` to `NodeInfo`.
  - Add `RootSquashed` to `Environment` capturing the baseline root-squash behavior.
- **[`pkg/framework/capabilities.go`](../pkg/framework/capabilities.go)**:
  - Add `ServerControlled`: true when the server pod is managed by a Kubernetes workload controller.
  - Add `DirectIO` and `HolePunch`: tool capabilities probed from `-tools-image`.
  - Add `CanProxyNodes` and `CanProxyPods`: RBAC proxy access permissions.
  - Add `CanCreateStaticPV`: permission to create static PersistentVolumes.
- **[`pkg/preflight/preflight.go`](../pkg/preflight/preflight.go)**:
  - Enforce `node.Status.Capacity["memory"] >= 3.5GiB` in `describeNodes()`.
  - Execute direct I/O, hole punching, and dry-run PV probes during the two-pod mount check.
- **Documentation and Run blocks**:
  - Update [`README.md`](../README.md) to clean up repetitive `FLAGS` examples across category targets.

---

## 8. Open questions

1. **Restricted Pod Security Admission (PSA)**: On clusters enforcing `restricted` PSA on the `default` namespace, client pods cannot run as root. The root-squash probe handles this by recording `unknown (restricted PSS)`, but should preflight also record whether client pods can run as root as a distinct capability?
2. **Dynamic node pool resizing**: Node capacity is probed across currently schedulable nodes. If a cluster autoscaler adds nodes mid-run with a different machine type, preflight's initial capacity check will not reflect the new nodes. Does the suite need a per-case node sizing guard, or is the Section 0 preflight check sufficient?

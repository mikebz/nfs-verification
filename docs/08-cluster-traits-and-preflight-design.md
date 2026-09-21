# 08: Cluster traits and preflight architecture: discovery, capabilities, and runner separation

Author: mikebz@
Created: 2026-09-21
Updated: 2026-09-21
Status: designed. Proposes standardizing cluster trait evaluation in preflight and separating cluster discovery from test runner controls.
Serves: Section 0 (Preconditions), Section 4.1 (Harness design), and cross-category execution across PROV, DATA, CHAOS, OBS, SEC.
Builds on: [`01-test-plan.md`](01-test-plan.md), [`02-provisioning-design.md`](02-provisioning-design.md), [`03-chaos-operations-design.md`](03-chaos-operations-design.md), [`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md), [`06-observability-design.md`](06-observability-design.md), and [`07-security-design.md`](07-security-design.md).

---

## 1. Why this domain exists

Cluster and environment traits are currently handled across three conflicting mechanisms:

1. **Ad-hoc runtime discovery during case execution**: Cases discover static cluster or image properties mid-test. For example, nine test cases across `chaos_test.go`, `obs_test.go`, and `prov_test.go` inspect server pod ownership at runtime and skip if the pod has no controller. DATA-07 probes direct I/O support (`dd oflag=direct`) inside the pod. DATA-11 probes hole punching (`fallocate -p`). Lock cases check whether a matching `locktool` binary exists for the target node architecture. OBS-04 attempts to manufacture a static PersistentVolume and catches admission failures.
2. **Repetitive CLI flags piped through the runner**: The test targets (`make test-prov`, `make test-data`, `make test-chaos`, `make test-obs`, `make test-sec`) require a wall of repetitive flags describing cluster traits (`FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"`), even when running single cases.
3. **Preflight Section 0 checks**: Preflight verifies baseline cluster viability, creates an RWX volume, verifies simultaneous mounting from two nodes, and writes `environment.json`.

Moving the NFS server process identification to preflight (Finding F-026, Finding F-027) resolved a fragile runtime probe: asking a pod at fault injection time for an answer that is an immutable property of the image.

That change highlighted a broader architectural need: **standardize all static cluster traits into preflight discovery, record them in the environment record and capability set, and decouple them completely from test runner invocation.**

This design establishes:
- What belongs strictly in preflight evaluation.
- The separation between cluster traits, policy expectations, and test runner controls.
- How capabilities gate tests cleanly without masking deployment defects.
- How the cached preflight lifecycle simplifies daily development.

---

## 2. Taxonomy: Traits vs. Policies vs. Runner Controls

Configuration in this repository divides into three distinct categories:

| Category | Definition | Lifecycle | Examples |
|---|---|---|---|
| **Cluster Traits** | Immutable properties of the cluster, its nodes, its storage class, and its server workload. Discovered automatically by preflight. | Evaluated once per cluster context; cached in `environment.json`. | Schedulable node count, allocatable memory per node, StorageClass reclaim policy, server process name, server controller existence, proxy RBAC permissions, tools image capabilities. |
| **Policy Assertions** | Expectations declared by the operator to enforce that a deployment satisfies a specific requirement, rather than merely recording what it does. | Provided to preflight or test suite as assertions; evaluated against discovered traits. | Mandatory lease/grace profile (`-profile=tuned`), enforced root-squash behavior (`-root-squash=on`). |
| **Runner Controls** | Operational parameters governing test execution, verbosity, output locations, and cache management. | Passed per test invocation to `make` or `go test`. | Test selection (`-run`, `CASE=...`, `-short`), output directories (`-artifacts-dir`, `-run-id`), triage options (`-keep-objects`), cache control (`-refresh-preflight`, `-env-file`). |

The guiding rule from AGENTS.md holds: *Discovery over declaration: if the cluster can answer it, discover it. A flag exists only for what a cluster genuinely cannot answer.*

Under this architecture, flags describing cluster traits are passed only when a trait cannot be discovered (or to preflight as hints). Test execution targets (`make test-*`) run cleanly without trait flags, reading discovered traits from the cached environment record.

---

## 3. What preflight strictly evaluates

Preflight evaluates all cluster properties that do not vary mid-run. These evaluations must be non-destructive: they may create and delete ephemeral test resources in `default`, but they never disturb existing workloads.

### 3.1 Worker node allocatable memory (F-002)

Finding F-002 showed that 2GB worker nodes (`e2-small` on GKE) cannot host the suite: platform system daemons consume most of the memory, causing nodes to reboot mid-run under workload pressure. A rebooting node mimics storage failures and costs days of triage.

- **Preflight evaluation**: During node enumeration, preflight inspects `node.Status.Allocatable` memory for all schedulable nodes.
- **Verdict**: If any schedulable node reports allocatable memory below 4GiB, preflight stops with a hard failure: `schedulable node has insufficient allocatable memory (< 4GiB)`.
- **Outcome**: Protects cluster stability and stops invalid runs before workloads start.

### 3.2 Server workload controller existence

Nine chaos, provisioning, and observability cases delete the NFS server pod to test failover, grace entry, and restart durability. If the server pod was created standalone without a controller, deleting it causes a permanent outage.

- **Preflight evaluation**: Preflight inspects the `OwnerReferences` of discovered server pods. If a controller reference (`controller: true`) is present, it records the controller kind and name in `ServerInfo` and sets `ServerControlled` in `Capabilities`.
- **Verdict**: If no controller exists, preflight records a note. Cases that delete the server pod gate on `requireCap(t, f.Caps.ServerControlled, ...)` and skip cleanly upfront, rather than creating test pods and claims only to block mid-test.

### 3.3 Tools image and architecture capabilities

Client pods execute tools inside `-tools-image` (default `alpine:3.20`). Three features are currently probed at case time:
1. `dd oflag=direct`: probed in DATA-07.
2. `fallocate -p` (hole punching): probed in DATA-11.
3. Node architecture matching for cross-compiled `locktool` binaries (`linux/amd64`, `linux/arm64`).

- **Preflight evaluation**: Preflight already creates two test pods (`a` and `b`) to verify simultaneous RWX mounting. During that phase, preflight runs non-destructive checks inside pod `a`:
  - Executes `dd if=/dev/zero of=/dev/null count=1 oflag=direct` to test direct I/O support.
  - Executes `fallocate -p -o 0 -l 4096 /dev/null` to test hole punch support.
  - Verifies that `locktool` binaries exist on the runner workstation matching the node architectures reported in `NodeInfo`.
- **Capabilities set**: `DirectIO`, `HolePunch`, `LocktoolReady`.
- **Outcome**: DATA-07 and DATA-11 skip immediately by capability when unsupported, removing ad-hoc shell probes from test bodies.

### 3.4 API proxy RBAC permissions

OBS-05 and OBS-06 read the kubelet Summary API via the API server node proxy (`nodes/proxy`). OBS-07 reads the server pod's Prometheus metrics endpoint via the API server pod proxy (`pods/proxy`).

- **Preflight evaluation**: Preflight performs authorization checks using `SelfSubjectAccessReview` (or probe `GET`s) for `nodes/proxy` on the schedulable nodes and `pods/proxy` on the discovered server pod.
- **Capabilities set**: `CanProxyNodes`, `CanProxyPods`.
- **Outcome**: If missing, preflight records a note. Observability cases gate on these capabilities rather than failing with unexpected HTTP 403 Forbidden errors.

### 3.5 Static PersistentVolume creation permissions

OBS-04 creates a static broken PersistentVolume to verify that mount failures surface as Kubernetes Events. On clusters with restrictive admission webhooks or strict RBAC, static PV creation is rejected.

- **Preflight evaluation**: Preflight performs an authorization check (`SelfSubjectAccessReview` for `persistentvolumes` create) or a dry-run PV creation (`dryRun: ["All"]`).
- **Capability set**: `CanCreateStaticPV`.
- **Outcome**: OBS-04 gates on `requireCap(t, f.Caps.CanCreateStaticPV, ...)` instead of handling admission errors dynamically.

### 3.6 StorageClass reclaim policy and export root-squash default

- **StorageClass Reclaim Policy**: Preflight inspects the candidate StorageClass and records whether its `reclaimPolicy` is `Delete` or `Retain`. PROV-01 expects `Delete`; PROV-06 covers `Retain`. Recorded in the environment record.
- **Root-Squash Default**: During the two-pod mount verification, preflight tests whether root `chown` is permitted on the test share and records `RootSquashed` (true/false) in the environment record. SEC-02 uses this baseline when `-root-squash=auto`.

---

## 4. The Non-Destructive Invariant

README states: *Only `make unit` and `make preflight` leave the cluster alone.*

Standardizing cluster traits into preflight must preserve this invariant:
- Preflight may create, mount, write to, and delete its own ephemeral test PVC and pods in namespace `default`.
- Preflight must **never** signal host processes, delete server pods, alter node taints, or induce network partitions.
- Properties that can only be measured by destroying state remain inside their respective test groups:
  - Grace period duration and wording verification remain in CHAOS-05 and OBS-03.
  - Actual lock survival across a kill remains in CHAOS-02 and CHAOS-06.
  - Outage recovery bounds remain in the chaos suite.

Preflight evaluates the *preconditions* for those tests (e.g. whether the server process is identifiable and whether a controller will restart the pod), never the fault recovery itself.

---

## 5. Skip vs. Blocked vs. Hard Failure Semantics

Standardizing traits in preflight requires maintaining the distinction between platform capabilities, deployment defects, and invalid cluster shapes:

| Classification | Meaning | Action | Example |
|---|---|---|---|
| **Hard Preflight Failure** | Cluster cannot host the suite or violates Section 0 invariants. | Suite exits non-zero; no tests run. | < 2 schedulable nodes; allocatable RAM < 4GB; no RWX StorageClass; node agent cannot schedule. |
| **Capability Skip (`requireCap`)** | Legitimate feature or optional platform capability not present in this cluster. | Test reports `skipped` naming the missing capability. | Cluster is single-stack (`!DualStack`); StorageClass does not advertise expansion (`!CanExpand`); VolumeSnapshot CRD missing (`!CanSnapshot`). |
| **Blocked Verdict (`reportBlocked`)** | Cluster configuration or deployment arrangement prevents an assertion from executing, representing a finding. | Test reports `blocked` naming the configuration cause. | Server pod has no controller so delete chaos cannot test recovery; kubeconfig lacks proxy RBAC to read kubelet stats. |

To prevent masking deployment defects:
- When a trait like `ServerControlled` or `CanProxyNodes` is false, preflight prints an explicit warning note in its summary.
- The test cases requiring those traits report `blocked` (or skip with an explicit `blocked:` prefix), ensuring they are tracked in findings rather than disappearing into passing runs.

---

## 6. Execution and Caching Lifecycle

Standardizing cluster traits into preflight decouples cluster configuration from routine test execution:

```
Step 1: Discover & Cache (Once per cluster context or on configuration change)
  make preflight FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
     │
     ▼ writes
  artifacts/<run-id>/environment.json
  artifacts/preflight-<context>.json (cached for 8h)

Step 2: Run Tests (Zero cluster trait flags needed)
  make test-prov
  make test-data
  make test-chaos
  make test-case CASE=TestDataConcurrentAppendToOneFile
```

### Cache resolution flow:
1. If `-env-file` is passed, the test runner loads that exact environment record.
2. Otherwise, the runner checks `artifacts/preflight-<context>.json`. If valid and within `-preflight-max-age` (default 8 hours), it loads the cached environment and capabilities.
3. If the cache is missing or expired, `TestMain` runs preflight automatically using discovered defaults.
4. Passing `-refresh-preflight` forces full rediscovery regardless of cache age.

---

## 7. State and Interface Representation

### 7.1 Environment additions

The environment record (`environment.json`) tracks discovered traits in prose-mapped fields:
- `NodeInfo.AllocatableMemoryBytes`: Total allocatable memory reported by kubelet.
- `ServerInfo.Controller`: The owner controller managing the server pod (e.g. `StatefulSet/nfs-server`).
- `Environment.StorageClassReclaimPolicy`: The reclaim policy of the verified RWX class (`Delete` or `Retain`).
- `Environment.RootSquashed`: Boolean indicating whether root `chown` was squashed during preflight file probes.
- `Environment.ToolsCapabilities`: Probed capabilities of the tools image (direct I/O, hole punching).
- `Environment.ProxyAccess`: Boolean flags indicating proxy read access on nodes and pods.

### 7.2 Capability additions

The capability structure (`framework.Capabilities`) adds:
- `ServerControlled`: Discovered server pod is managed by a Kubernetes controller.
- `DirectIO`: Tools image supports `oflag=direct`.
- `HolePunch`: Tools image supports `fallocate -p`.
- `LocktoolReady`: Precompiled `locktool` binary matches the node architectures.
- `CanProxyNodes`: Caller credentials have `get` permission on `nodes/proxy`.
- `CanProxyPods`: Caller credentials have `get` permission on `pods/proxy`.
- `CanCreateStaticPV`: Caller credentials have permission to create static PersistentVolumes.

### 7.3 Runner flags simplification

Cluster-descriptive flags are reserved for preflight invocation (or fallback when undiscoverable):
- `-storage-class`
- `-lease-seconds`, `-grace-seconds`
- `-server-namespace`, `-server-selector`
- `-profile`
- `-root-squash`

Test runner flags focus strictly on execution:
- Selection: `-run`, `CASE=...`, `-short`
- Operational: `-artifacts-dir`, `-run-id`, `-keep-objects`
- Cluster target: `-kubeconfig`, `-context`
- Cache control: `-refresh-preflight`, `-preflight-max-age`, `-env-file`

---

## 8. Open questions

1. **Static PV probe mechanism**: Should preflight use `SelfSubjectAccessReview` or a dry-run `Create` (`dryRun: ["All"]`)? A dry-run create catches admission webhooks (e.g. OPA Gatekeeper, Kyverno) that RBAC authorization checks miss.
2. **Tools image cache invalidation**: Should a change to `-tools-image` invalidate the preflight cache? Hashing the tools image string into the cache filename or key would prevent stale tool capability flags.
3. **Policy flag enforcement**: When an operator specifies `-profile=tuned` on a test target, should `TestMain` fail if the cached preflight was run on a default profile? Yes: policy assertions must be verified against the loaded environment.

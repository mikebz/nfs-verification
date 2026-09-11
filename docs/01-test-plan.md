# E2E Test Plan: NFS RWX Persistent Volumes on Kubernetes

Version: 1.0 (v1 scope)
Mode: GENERATE
Scope note: this plan is written against a userspace NFS server architecture. NFS-Ganesha is the reference implementation used to derive failure modes, but no case depends on Ganesha-specific APIs, config syntax, or binaries. Every assertion is made through the NFS protocol, the Kubernetes API, or the pod filesystem.

---

## Section 0: Preconditions and hard stop

The suite refuses to run unless all of the following hold. `make preflight` exits non-zero with a specific reason on any failure. No test executes after a preflight failure.

| Check | Failure message |
|---|---|
| A StorageClass exists whose PVs support `ReadWriteMany` | `no RWX-capable StorageClass found` |
| A PVC on that class binds within 120s | `RWX PVC did not bind` |
| Two pods on two different nodes both mount it read-write | `RWX PVC not simultaneously mountable` |
| The mount is NFS and negotiates 4.1 | `mount is not nfs4 vers=4.1` |
| Cluster has >= 2 schedulable worker nodes | `insufficient nodes for cross-node cases` |
| Privileged DaemonSet can be scheduled | `node-level assertions unavailable (Autopilot?)` |

Preflight also records, and does not require as input:

- Server implementation and version, read from the server pod image tag and, where exposed, from the server's own version endpoint.
- CSI driver version, Kubernetes version, node kernel, container runtime, per node.
- Actual mount options as the driver sets them, read from `/proc/mounts` on the node.
- Server fan-out: number of server pods, and export-to-PVC mapping.
- IP families present (single-stack vs dual-stack).
- Lease and grace values in force, matched against the two profiles in Section 3.8. Unset or off-profile values fail preflight, because every timing assertion depends on them.
- Whether delegations are enabled. Gates CHAOS-18.
- Whether the recovery state path is backed by persistent storage or ephemeral pod storage. **Recorded for triage only.** Lock survival is still measured empirically by CHAOS-02 and CHAOS-06, not inferred from this. The value exists so that a reclaim failure is diagnosed in one minute instead of one day.
- Mount propagation mode on the CSI node plugin. Recorded, not gated: if it were wrong, nothing would mount and the simultaneous-mount check above already catches it.

These land in `artifacts/<run-id>/environment.json` and are attached to every failure report. Discovery over declaration: a plan that requires humans to type version numbers correctly will be wrong within one sprint.

---

## Section 1: Inputs

### Fixed for v1

| Input | Value |
|---|---|
| Sharing protocol | NFS 4.1 only. v3 and v4.0 deferred to v2. v4.2 out of scope for v1. |
| Primary platform | GKE Standard |
| Secondary platform | Bare metal |
| Portability rule | No distro-specific APIs. Kubernetes API plus portable Linux binaries (fio, dd, flock, stat) in test containers. |
| Harness | Go, `client-go`, standard `testing` package. No Ginkgo. |
| Execution | Local `make` targets with GitHub Actions CI for hermetic checks (fmt, vet, unit, build). |
| Topology | 3 control plane, >= 2 workers. CNodes and DNodes colocated. No dedicated storage network. |
| Upgrade testing | Out of scope, deferred to v2 |
| Multi-cluster | Out of scope |
| Rights | Destructive and chaos operations permitted; clusters are disposable |

### Discovered at runtime

Server version, CSI version, kernel, mount options, fan-out, IP families. See Section 0.

### Deliberately not fixed

HA mechanism is a black box. No case asserts how failover happens. Every failover case asserts only client-observable behavior derived from the protocol.

---

## Section 2: Architecture of the system under test

### 2.1 Data path, stated end to end

A single 4KiB write from an application pod:

| Hop | Location | Cardinality | Effect if it dies mid-write |
|---|---|---|---|
| Application `write(2)` | Pod, node A | Per-pod | Write never issued |
| Client page cache | Node A kernel | Per-node | Unflushed data lost. Legal: not durable until COMMIT. |
| Kernel NFS client (`nfs4`) | Node A kernel | Per-node | Hard mount blocks and retries indefinitely |
| CNI, node network | Cluster | Per-node | Client blocks, retransmits, recovers when path returns |
| Service or endpoint fronting the server | Cluster | Per-cluster | Client cannot reach server. Behavior depends on whether the endpoint IP is stable across failover. |
| NFS server process | Server pod | **Singleton per export set** | In-flight uncommitted writes lost. Client retries. Server re-enters grace on restart. |
| Server-side metadata cache | Server pod | Same singleton | Cache state lost. Not durable, so no data loss, but a known crash surface. |
| Backing block volume (RWO) | One node | Per-volume, single-attach | Export unavailable until the volume re-attaches elsewhere |
| Block replication | SDS | Per-volume | Depends on replica count |

Two things fall out of this table and drive the whole plan:

1. The server is a singleton in the data path. There is no cross-node coherence protocol to test, because there is only one coherence point.
2. The backing volume is single-attach. Volume detach and re-attach latency is on the failover critical path and is a distinct failure mode from server process death.

### 2.2 Architecture archetype

**Archetype A: sharing gateway over a single-writer volume.** A userspace server process exports a filesystem over NFS; the backing block volume is attached to exactly one node; clients reach the gateway over the pod or node network.

Hybrid element from B: if the server is packaged and versioned independently of the CSI driver, cross-product version skew and defect routing apply. Preflight records both versions; if they come from different release trains, the SKEW cases in Section 3.7 are enabled.

Explicitly **not** archetypes C or D. There is no distributed lock manager, no cluster filesystem, no split-brain surface between clients. Cases asserting POSIX coherence across nodes are out of scope by construction and any such case in a prior plan should be deleted, not rewritten.

### 2.3 Required answers

| Question | Answer |
|---|---|
| Coherence boundary | The single server process. All clients serialize through it. |
| Consistency model | NFS protocol semantics: close-to-open for regular files, byte-range locks for finer coordination. Not POSIX across clients. |
| Single point of failure | The server process. Scope is per-export-set: if fan-out is 1, it is cluster-wide for all RWX volumes. Preflight determines which. |
| Singleton in the data path | Yes, the server. Failover trigger is opaque by design. Recovery includes a grace period during which clients may reclaim state but may not acquire new state. |
| Lock state ownership | The server. Survival across restart depends on the recovery backend and is treated as unknown. Cases assert reclaim succeeds, not how. |
| Lock semantics | Advisory. Visible across nodes only because all clients serialize through the one server. |
| Client mount type | Kernel `nfs4`, hard mount. Blocks indefinitely rather than returning EIO. Verified in preflight, not assumed. |
| Network path | Pod or node network, no dedicated storage network, no isolation from tenant traffic. NetworkPolicy or CNI restart interrupts the data path. |
| Version ownership | Determined at preflight. If independently versioned, defect routing per Section 4.3. |
| Semantic claim | Protocol semantics, not POSIX. The server is an NFS server; the guarantee is whatever NFSv4.1 specifies (RFC 8881), not what a local filesystem provides. |

### 2.4 Consequences for test emphasis

| Vector | Weight | Rationale |
|---|---|---|
| Gateway kill, node loss, partition, grace behavior | **Heaviest** | Singleton in the data path |
| Lock reclaim after failover | **Heavy** | Lock state survival is the least-verifiable property in this archetype |
| Stale handle handling on clients | **Heavy** | Direct consequence of server restart with new state |
| Close-to-open verification | Medium | Bounded by protocol; assert the guarantee, and assert the absence of stronger ones |
| Server memory and cache behavior under load | Medium-heavy | Userspace server, historically the dominant production failure |
| Split-brain, DLM, cluster filesystem coherence | **Zero** | Not this architecture |
| Cross-product version skew | Conditional | Enabled only if versions are independent |

---

## Section 3: Coverage

Case ID scheme: `CATEGORY-NN`, matching the test category (`PROV`, `DATA`, `CHAOS`, `SCALE`, `OBS`, `SEC`, `SKEW`).

### 3.1 Provisioning and lifecycle (PROV)

| ID | Case | Expected |
|---|---|---|
| PROV-01 | Dynamic provision RWX PVC, bind, mount, write, delete | PV created and removed; backing directory or volume reclaimed |
| PROV-02 | Provision 20 RWX PVCs concurrently | All bind; no duplicate export IDs; no server restart |
| PROV-03 | Delete PVC while a pod still has it mounted | PVC stays Terminating; deletion completes only after unmount |
| PROV-04 | Volume expansion, if the driver advertises it | Capacity visible in pod; existing data intact; if unsupported, API rejects cleanly |
| PROV-05 | Snapshot and restore, if advertised | Restored volume mounts RWX and content matches; if unsupported, clean rejection |
| PROV-06 | Reclaim policy Retain | PV persists after PVC deletion; data readable when rebound |
| PROV-07 | Provision while the server pod is down | PVC stays Pending; binds after recovery; no orphaned export |
| PROV-08 | Delete PVC while the server pod is down | No orphaned export or leaked backing storage after recovery |
| PROV-09 | Rapid create/delete churn, 100 cycles | No export ID exhaustion, no fd leak, no server RSS growth beyond ceiling |
| PROV-10 | Provision with a 1000-character volume name or unusual characters | Clean rejection or correct handling; no malformed export config |
| PROV-11 | Two-stage expansion under active I/O: grow the backing block volume, then grow the share | Client `df` reflects new capacity with no unmount, no server restart, and no I/O error. **Shape depends on fan-out**: with one server per volume this is a block resize plus filesystem grow; with a shared server it is a quota or export change and the block device may not move at all. Preflight decides which assertion applies. |

### 3.2 Concurrency and data integrity (DATA)

Assertions here are calibrated to the protocol claim in 2.3. Do not tighten them.

| ID | Case | Expected |
|---|---|---|
| DATA-01 | N pods, N files, partitioned by file, checksummed | All checksums match; no cross-contamination |
| DATA-02 | N pods append to one file with `O_APPEND` | No lost or interleaved records; byte count exact |
| DATA-03 | Close-to-open: pod A writes and closes, pod B opens and reads | B sees A's data |
| DATA-04 | Negative: pod A writes without closing, pod B reads | B may see stale data. Test asserts this is **not a failure**. Documents the boundary. |
| DATA-05 | `flock` and `fcntl` byte-range locks across pods on different nodes | Mutual exclusion holds; second acquirer blocks |
| DATA-06 | Lock held by a pod that is force-deleted | Lock released within one lease period; new acquirer succeeds |
| DATA-07 | Same file opened `O_DIRECT` by two pods | Writes land; no corruption; reads reflect committed data |
| DATA-08 | `noac` mount variant, cross-pod visibility without close | Immediate visibility, confirming the difference from DATA-04 |
| DATA-09 | Rename, unlink, and re-create while another pod holds the file open | Open fd remains valid; silly-rename behavior correct |
| DATA-10 | Large directory (100k entries) readdir concurrent with deletes | Listing completes, no server crash, no use-after-free. Derived from a documented directory-chunk reuse crash in READDIR under cache pressure. |
| DATA-11 | Sparse file write, hole punch, read back | Correct zero regions; reported size consistent. Hole punching is NFSv4.2 (`DEALLOCATE`, RFC 7862); on the 4.1 mount Section 0 pins, the punch is **recorded as unsupported, not asserted**, and the case fails only if a punch reports success without zeroing. |
| DATA-12 | fsync and COMMIT durability: write, fsync, kill server | Post-recovery data present |
| DATA-13 | Negative durability: write without fsync, kill server | Data may be absent. Asserted as acceptable, documented. |
| DATA-14 | Mixed 70/30 read/write, 4KiB to 1GiB files, 20 pods, 1h | Zero checksum mismatches |

### 3.3 Resiliency and chaos (CHAOS)

Every case in this section runs with active I/O and asserts against the SLO table in Section 3.8.

| ID | Case | Expected |
|---|---|---|
| CHAOS-01 | SIGKILL the server process during active write | I/O blocks, does not error; resumes within restart SLO; committed data intact |
| CHAOS-02 | Delete the server pod during active write | As above, plus locks reclaimed |
| CHAOS-03 | Hard-stop the node hosting the server | I/O resumes within node-loss SLO; backing volume re-attaches |
| CHAOS-04 | Network partition the server from clients, then heal | Clients block, then recover; no data loss |
| CHAOS-05 | Repeated failover, 5 cycles inside 10 minutes | Each cycle recovers; grace does not enter a re-entry loop. Derived from reports of clients stalled for hours after repeated grace entry during address takeover. |
| CHAOS-06 | Failover while a client holds byte-range locks | All locks reclaimed; no conflicting lock granted to a different client during grace |
| CHAOS-07 | Failover while a second client attempts a *new* lock | New acquisition rejected during grace with a retryable error, then succeeds after grace ends. Never granted during grace. |
| CHAOS-08 | Client-side: server restarts with new state | Client handles stale handles without permanent EIO; no manual remount required |
| CHAOS-09 | kubelet restart on a client node with mounts active | Mounts survive; I/O resumes |
| CHAOS-10 | CSI node plugin pod eviction with mounts active | Existing mounts unaffected; new mounts queue and succeed after recovery |
| CHAOS-11 | CNI restart on a client node | I/O blocks then resumes; no unmount |
| CHAOS-12 | NetworkPolicy applied that blocks the data path, then removed | Clients block, then recover cleanly |
| CHAOS-13 | Backing block volume disconnect during write | Errors surface as retryable; no silent corruption |
| CHAOS-14 | **Colocation deadlock**: server pod scheduled on the same node as its clients, under memory pressure | No reclaim deadlock. Specific to hyperconverged CNode/DNode topology: the local client blocks in page reclaim waiting on a local server that needs memory to progress. |
| CHAOS-15 | Client node OOM with dirty pages on the NFS mount | Bounded failure; no node-level hang |
| CHAOS-16 | 24h chaos soak: randomized kills, partitions, evictions | Zero data corruption; zero unrecovered mounts; zero core dumps |
| CHAOS-17 | Recovery state store lost or corrupted, then server restarts | Bounded, honest failure: clients fail to reclaim and locks are lost, but no file data corruption, no permanent client hang, and the loss is observable in metrics or logs. Silently granting conflicting locks after state loss is the failure being hunted. |
| CHAOS-18 | Delegation held by client A, client B opens the same file conflicting | Delegation recalled and returned within the recall timeout; B proceeds; A sees no corruption. **Skipped, not failed, when delegations are disabled**, which is a common default. Ties to a reported crash on the delegation return path. |

Blanket rule: any core dump on any server pod fails the run. Cores are collected into run artifacts.

### 3.4 Scale and performance (SCALE)

| ID | Case | Expected |
|---|---|---|
| SCALE-01 | Max RWX mounts per node, ramp to failure | Documented ceiling; failures are clean, not kernel hangs |
| SCALE-02 | 50 RWX volumes per cluster | All mountable; server RSS below ceiling |
| SCALE-03 | Pod fan-out 1, 10, 50, 100 on one volume | Throughput degradation curve recorded; no cliff or timeout |
| SCALE-04 | Small-file write storm (1M files, 1-64KiB) | Server RSS bounded, no OOMKill. Derived from repeated production reports of unbounded memory growth under small-file write load. |
| SCALE-05 | Metadata-heavy: 100k stat/create/unlink per minute | No server restart; latency recorded |
| SCALE-06 | Noisy neighbor across exports on a shared server (**enabled only if fan-out > 1**) | One export's load does not starve another beyond a stated bound |
| SCALE-07 | Sustained 8h throughput soak | No degradation trend beyond 10%; no leak |

### 3.5 Observability (OBS)

| ID | Case | Expected |
|---|---|---|
| OBS-01 | Server unavailable | Alert fires within SLO; correct severity and target |
| OBS-02 | Failover event | Event is observable in metrics or logs with a timestamp; measurable duration |
| OBS-03 | Grace period entry and exit | Both observable; duration measurable. Required to make CHAOS-05 diagnosable. |
| OBS-04 | Mount failure on a client | Surfaced as a Kubernetes Event on the pod with an actionable reason |
| OBS-05 | Server memory approaching ceiling | Alert fires before OOMKill |
| OBS-06 | Volume near capacity | Alert fires; `df` inside the pod agrees with the control plane |
| OBS-07 | Metrics survive server restart | Counters either reset cleanly or persist; no gaps that hide an outage |

### 3.6 Security and identity (SEC)

| ID | Case | Expected |
|---|---|---|
| SEC-01 | UID/GID preservation across pods | Ownership as written. Derived from reports of v4 clients mapping local IDs to `nobody` on modern kernels. |
| SEC-02 | `root_squash` enabled: root-owned pod writes | Squashed to anonymous as configured |
| SEC-03 | Pod `securityContext.fsGroup` interaction | Group access correct; no unexpected chown storm on large volumes |
| SEC-04 | Export access rules through the cluster Service path | Per-client rules still apply. Derived from an open report that connections carrying no client identity are treated as the proxy host, degrading per-IP rules to the global access type. **High priority: Kubernetes always inserts a Service.** |
| SEC-05 | Denied client attempts mount | Rejected, not silently granted |
| SEC-06 | Dual-stack client identity (**skipped unless both IP families present**) | Client identity consistent. Derived from an open report of inconsistent normalization between IPv4 and IPv4-mapped IPv6. |
| SEC-07 | Two pods with identical client identity after restart | No state collision; no lost locks |
| SEC-08 | Data path confidentiality, stated | Records whether NFS traffic on the shared pod network is cleartext, and whether any transport encryption is in effect. This is a **finding, not a pass/fail**: with no dedicated storage network, cleartext NFS shares a fabric with tenant traffic, and that fact belongs in the record whether or not it is acceptable. |
| SEC-09 | Server pod under the platform's admission policy | Server runs with the capability set it actually needs and no more. A file-handle-based backend needs `CAP_DAC_READ_SEARCH` for `open_by_handle_at(2)`; if the policy strips it, file handle operations fail with EPERM. Conditional on the backend using that path. |

### 3.7 Version skew (SKEW, conditional)

Enabled only when preflight determines the server and CSI driver are independently versioned.

| ID | Case | Expected |
|---|---|---|
| SKEW-01 | CSI driver N with server N-1 | Provision, mount, I/O all work, or fail with an explicit compatibility error |
| SKEW-02 | CSI driver N-1 with server N | As above |
| SKEW-03 | Server restarted into a different minor version with clients mounted | Clients recover; state reclaimed or cleanly re-established |

### 3.8 SLO table

Referenced by every CHAOS case. Values live in `pkg/slo/slo.go` and are changed in one place.

#### Lease and grace configuration

Failover SLOs are meaningless without pinned lease and grace values, because grace is the dominant term. Common defaults are a 60s lease and a 90s grace period, which is *above* the 60s restart target: on stock settings the plan fails its own SLO with no defect present. Two configurations are therefore run, both recorded in `environment.json`:

| Config | Lease | Grace | SLO applied |
|---|---|---|---|
| `tuned` | 20s | 30s | 60s restart, 90s node loss |
| `default` | 60s | 90s | grace + 30s |

The `default` config is not a formality. It documents what a customer who changes nothing actually experiences, and it is the configuration most escalations will arrive on. Preflight reads the live values and fails if they are unset or outside both profiles, rather than letting a third value silently invalidate every timing assertion.

Note on determinism: a server may lift grace early once it concludes no further clients will reclaim. CHAOS-07 therefore holds one client with outstanding un-reclaimed state so that grace is observably enforced when the probe runs. Without that, the case passes vacuously.

#### Targets

| Event | Metric | Target | Basis |
|---|---|---|---|
| Server restart in place (process kill, pod delete) | Time to first successful I/O | **60s p99, fail above** | Ratified |
| Server reschedule after graceful node drain | Time to first successful I/O | **60s p99, fail above** | Ratified |
| Server reschedule after ungraceful node loss | Time to first successful I/O | **90s p99, fail above** | Ratified, subject to the floor note below |
| Any failover | I/O errors on hard mounts | 0 | Protocol: hard mounts block, not fail |
| Any failover | Committed writes lost | 0 | Protocol: post-COMMIT durability |
| Any failover | Locks reclaimed | 100% | Protocol: grace period exists for this |
| Any failover | New lock granted during grace to a different client | 0 | Protocol: grace bars new state acquisition |
| Any failover | Grace exit | within 2x lease, no re-entry loop | Convention: grace runs about two lease periods |

**Floor note: 90s for ungraceful node loss is not reachable on Kubernetes defaults.** Two platform defaults sit in front of the storage system and neither has anything to do with NFS:

1. Kubernetes automatically adds tolerations for `node.kubernetes.io/not-ready` and `node.kubernetes.io/unreachable` with `tolerationSeconds=300` unless the pod sets them explicitly, so a pod on a dead node stays bound for five minutes before eviction.
2. Once evicted, an RWO backing volume must detach from the dead node before it can attach elsewhere. The attach-detach controller waits `maxWaitForUnmountDuration` (6 minutes) before force-detaching, and that value is not currently configurable.

Worst case on stock settings is therefore around 11 minutes, not 90 seconds. Two required configuration changes on the test cluster, both recorded in `environment.json` and asserted at preflight:

- The server workload sets `tolerationSeconds: 30` explicitly on both taints.
- Ungraceful node loss applies the `node.kubernetes.io/out-of-service=nodeshutdown:NoExecute` taint to trigger immediate volume detach, which is the GA path for exactly this case.

If either is absent, CHAOS-03 is reported as **blocked on cluster configuration**, not failed. Distinguishing the two matters: a 6-minute recovery caused by a controller default is a deployment defect, and filing it against the NFS server wastes a week.

---

## Section 4: Automation and execution

### 4.1 Harness design

- Language: Go. `client-go` for orchestration, standard `testing` for structure, table-driven cases. No Ginkgo.
- The harness runs **on the operator's workstation**, outside the cluster. It authenticates with a kubeconfig, creates namespaces, PVCs, pods and DaemonSets, injects faults, collects logs, and asserts. No component of the suite is deployed into the cluster ahead of time.
- The harness is a Kubernetes client, not an NFS client. It never mounts the share itself. All I/O comes from in-cluster pods running portable binaries: `fio` for load, `dd` for simple patterns, `flock` and a small static Go binary for lock semantics, `sha256sum` for verification.
- Optional and off by default: `-external-mount=<server>:<path>` lets the workstation mount the export directly with a plain Linux NFS client. Used only for triage step 6 (reproducing outside Kubernetes to route a defect upstream). It is skipped, never failed, when the server is unreachable from the workstation, which is the normal case on a private GKE cluster.
- Node-level assertions (`/proc/mounts`, dmesg, process signals) go through a privileged DaemonSet with host namespace access. This is the only privileged component, and its absence is a preflight failure rather than a silent skip.
- No distro-specific APIs. Chaos is expressed as Kubernetes operations (pod delete, node cordon and drain, NetworkPolicy apply, node shutdown via whatever the platform provides) behind an interface with a GKE implementation and a bare-metal implementation.
- Every case is skippable by capability, never by platform name. `if !caps.CanKillNode { t.Skip() }`, never `if platform == "gke"`.

### 4.2 Execution by Category

| Target | Contents | Budget |
|---|---|---|
| `make preflight` | Section 0 only | < 3 min |
| `make test-prov` | All PROV cases | < 45 min, 2 nodes |
| `make test-data` | All DATA cases except the DATA-14 soak | < 45 min, 2 nodes |
| `make test-chaos` | All CHAOS cases | < 4 h, 4 nodes |
| `make test-obs` | All OBS cases | < 45 min, 2 nodes |
| `make test-sec` | All SEC cases | < 30 min, 2 nodes |
| `make test-e2e` | All categories end to end | < 5 h, 4 nodes |

E2E tests are organized strictly by category. Each category has its own test file, Makefile target, and corresponding `Test<Category>...` function prefix.

**Soak is not a category**, and the table above has one row per category for that reason. DATA-14 is a DATA case: it carries the `TestData` prefix, lives in the DATA file, and is listed in Section 3.2 with the rest of them.

What it has separately is a runtime. It runs for an hour across 20 pods, which `make test-data` cannot hold inside a 45 minute budget, so `make test-data-soak` runs that one case with `-timeout=120m`. That is a runtime exception inside one category, not a seventh category, and it is why the target is named for the case's own category rather than for what the case does. DATA-14 also needs `-fio-image` and skips without it, wherever it is run from.

`make test-data` injects faults: DATA-12 and DATA-13 kill the NFS server process. `make test-prov` and `make test-obs` are already in that position.

### 4.3 Triage runbook

Every failure produces `artifacts/<run-id>/` containing `environment.json`, server logs, client pod logs, `/proc/mounts` from every involved node, dmesg, Kubernetes Events, any core dumps, and a timeline of injected faults.

Triage order:

1. **Is it the harness?** Re-run the single case in isolation. Flaky-on-isolation means harness bug; file against the suite.
2. **Which side of the mount?** Compare client `/proc/mounts` and dmesg against server logs at the fault timestamp. Client stuck with a healthy server means client or network. Server restarted means server.
3. **Is it grace?** Look for repeated grace entry in the window. Grace re-entry loops present as "hung client, healthy server" and are the single most common false diagnosis in this architecture.
4. **Provisioning or data path?** Provisioning failures go to the CSI driver owner. Data path failures go to the server owner.
5. **Version skew?** Compare the two versions in `environment.json`. If they are independently versioned and differ from the last green run, suspect skew first.
6. **Reproduce minimally**, then file with the artifact bundle attached. A failure filed without `environment.json` will be closed as unreproducible.

Defect routing when the sharing layer is independently versioned: reproduce outside Kubernetes against the server directly with a plain Linux NFS client. Reproducing outside Kubernetes routes the bug upstream to the server. Not reproducing routes it to the CSI driver or the packaging.

---

## Appendix A: Cases derived from the upstream issue tracker

Each maps to a real reported defect class, not speculation.

| Source pattern | Case |
|---|---|
| Lock requests denied after grace following server address takeover | CHAOS-06, CHAOS-07 |
| Client I/O stalled for hours after network-induced grace churn | CHAOS-05 |
| Failover measured in hundreds of seconds | SLO table, CHAOS-01 through CHAOS-03 |
| Unbounded memory growth to OOM under small-file writes | SCALE-04, OBS-05 |
| Use-after-free in directory-chunk reuse during READDIR under cache pressure | DATA-10 |
| Crash on the delegation return path | CHAOS-08, core-dump blanket rule |
| Lock acquisition failure tied to open-owner state | SEC-07, DATA-06 |
| v4 clients mapping local IDs to `nobody` | SEC-01 |
| Per-client export rules degrading to global access behind a proxy | SEC-04 |
| Inconsistent IPv4 / IPv4-mapped IPv6 client identity normalization | SEC-06 |
| Deleting the server deployment renders outstanding PVs unusable | PROV-07, PROV-08, SCALE-06 |

## Appendix B: Explicitly out of scope for v1

- NFS v3, v4.0, v4.2. Deferred to v2.
- Upgrade of any component. Deferred to v2.
- Multi-cluster and cross-region.
- Kerberos and `sec=krb5*`. AUTH_SYS only.
- RPC-over-TLS (RFC 9289), certificate rotation under load, and TLS handshake storm cases. Deferred to v2 and **contingent on SEC-08**: server-side support is not a given, and the Linux client side needs both an `xprtsec=` capable kernel and a handshake helper daemon on the node, neither of which is controllable on a managed node image. Probe first, then plan. CNI-level encryption (WireGuard, IPsec) is a platform property and belongs in the platform suite, not here.
- pNFS and RDMA transports.
- GKE Autopilot (no privileged pods, so no node-level assertions).
- Any case asserting POSIX coherence across clients. Out of scope by architecture, not by budget.

## Appendix C: Unratified assumptions

These are decisions the plan made in the absence of an input. Each is a one-line change.

1. Workload profile: 20 pods per volume, 4KiB to 1GiB files, 70/30 read/write.
2. Scale targets: 50 volumes per cluster, 10 mounts per node.
3. AUTH_SYS with `root_squash` on.
4. Node auto-repair and auto-upgrade disabled on the test node pool. If they are not, chaos results are invalid.
5. Server workload sets `tolerationSeconds: 30`, and ungraceful node-loss testing applies the out-of-service taint. Without both, CHAOS-03 is blocked rather than failed. See the floor note in Section 3.8.

Resolved since draft: failover SLO (60s process and graceful, 90s ungraceful node loss, against the `tuned` lease and grace profile); harness location (workstation, external, Kubernetes client only); lease and grace pinned to two explicit profiles rather than left at vendor defaults.

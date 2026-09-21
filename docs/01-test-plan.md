# E2E Test Plan: NFS RWX Persistent Volumes on Kubernetes

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-21
Version: 1.0 (v1 scope)

This is the requirements and delivery document: what gets verified, why, the
delivery order, and what is shipped or left to do (Section 5). Cases that are
done carry a ✅ at the start of their description in Section 3; a case with no
mark is not written yet. How the harness works is the repository
[`README.md`](../README.md), which also lists every other document here.

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
| Cluster has >= 2 schedulable nodes | `insufficient nodes for cross-node cases` |
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
| Topology | >= 2 schedulable nodes. Either dedicated workers alongside 3 control plane, or 3 nodes that each serve the API and the workloads, which is what GDC ships. The suite counts nodes it can schedule on and does not read role labels. CNodes and DNodes colocated. No dedicated storage network. |
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
| Semantic claim | Protocol semantics, not POSIX. The server is an NFS server; the guarantee is whatever NFSv4.1 specifies ([RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html)), not what a local filesystem provides. |

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

How volume provisioning, lifecycle, expansion, snapshots, and reclaim policies are verified is in
[`02-provisioning-design.md`](02-provisioning-design.md).

| ID | Case | Expected |
|---|---|---|
| PROV-01 | ✅ Dynamic provision RWX PVC, bind, mount, write, delete | PV created and removed; backing directory or volume reclaimed |
| PROV-02 | ✅ Provision 20 RWX PVCs concurrently | All bind; no duplicate export IDs; no server restart |
| PROV-03 | ✅ Delete PVC while a pod still has it mounted | PVC stays Terminating; deletion completes only after unmount |
| PROV-04 | ✅ Volume expansion, if the driver advertises it | Capacity visible in pod; existing data intact; if unsupported, API rejects cleanly |
| PROV-05 | ✅ Snapshot and restore, if advertised | Restored volume mounts RWX and content matches; if unsupported, clean rejection |
| PROV-06 | ✅ Reclaim policy Retain | PV persists after PVC deletion; data readable when rebound |
| PROV-07 | ✅ Provision while the server pod is down | PVC stays Pending; binds after recovery; no orphaned export |
| PROV-08 | ✅ Delete PVC while the server pod is down | No orphaned export or leaked backing storage after recovery |
| PROV-09 | ✅ Rapid create/delete churn, 100 cycles | No export ID exhaustion, no fd leak, no server RSS growth beyond ceiling |
| PROV-10 | ✅ Volume name edge cases: names the API will not store are rejected, names it will store work | Two tables of names, each posted to the API exactly as written, with nothing about a name judged by the harness. Names the API must refuse (1000 chars, one char past the limit, uppercase, underscore) must come back `Invalid` or `BadRequest`, labelled as the apiserver barrier it is and not evidence about NFS. Names it must store (the longest one, and a short one of dots and digits) each bind, mount on two nodes, carry a file between them and delete with no server restart. The export the provisioner mints is recorded as evidence and not asserted on: how a driver names an export is its own business, one volume cannot show a naming collision, and an export malformed enough to matter cannot be mounted |
| PROV-11 | ✅ Two-stage expansion under active I/O: grow the backing block volume, then grow the share | Client `df` reflects new capacity with no unmount, no server restart, and no I/O error. **Shape depends on fan-out**: with one server per volume this is a block resize plus filesystem grow; with a shared server it is a quota or export change and the block device may not move at all. Preflight decides which assertion applies. |

### 3.2 Concurrency and data integrity (DATA)

Assertions here are calibrated to the protocol claim in 2.3. Do not tighten them.

How data path consistency, locking, caching, and durability are verified is in
[`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md).

| ID | Case | Expected |
|---|---|---|
| DATA-01 | ✅ N pods, N files, partitioned by file, checksummed | All checksums match; no cross-contamination |
| DATA-02 | ✅ N pods append to one file with `O_APPEND` | No lost or interleaved records; byte count exact. **Carries a caveat**: NFSv4.1 has no append operation, so a client implements `O_APPEND` by writing at the offset it believes to be end of file. An exact count under concurrent appends from several clients is an implementation property, not a protocol guarantee, and the case says so in its failure message so a failure reaches the boundary discussion rather than the server owner. A torn record is corruption under any reading and is failed without a caveat. Still open: whether the count belongs in the fast gate at all. |
| DATA-03 | ✅ Close-to-open: pod A writes and closes, pod B opens and reads | B sees A's data |
| DATA-04 | ✅ Negative: pod A writes without closing, pod B reads | B may see stale data. Test asserts this is **not a failure**. Documents the boundary. |
| DATA-05 | ✅ `flock` and `fcntl` byte-range locks across pods on different nodes | Mutual exclusion holds; second acquirer blocks |
| DATA-06 | ✅ Lock held by a pod that is force-deleted | Lock released within one lease period; new acquirer succeeds |
| DATA-07 | ✅ Same file opened `O_DIRECT` by two pods | Writes land; no corruption; reads reflect committed data |
| DATA-08 | ✅ `noac` mount variant, cross-pod visibility without close | Immediate visibility, confirming the difference from DATA-04 |
| DATA-09 | ✅ Rename, unlink, and re-create while another pod holds the file open | Open fd remains valid; silly-rename behavior correct |
| DATA-10 | ✅ Large directory (100k entries) readdir concurrent with deletes | Listing completes, no server crash, no use-after-free. Derived from a documented directory-chunk reuse crash in READDIR under cache pressure. |
| DATA-11 | ✅ Sparse file write, hole punch, read back | Correct zero regions; reported size consistent. Hole punching is NFSv4.2 (`DEALLOCATE`, [RFC 7862](https://www.rfc-editor.org/rfc/rfc7862.html)); on the 4.1 mount Section 0 pins, the punch is **recorded as unsupported, not asserted**, and the case fails only if a punch reports success without zeroing. |
| DATA-12 | ✅ fsync and COMMIT durability: write, fsync, kill server | Post-recovery data present |
| DATA-13 | ✅ Negative durability: write without fsync, kill server | Data may be absent. Asserted as acceptable, documented. |
| DATA-14 | Mixed 70/30 read/write, 4KiB to 1GiB files, 20 pods, 1h | Zero checksum mismatches. **Deferred, not implemented.** See below. |


**DATA-14 is deferred and this suite is not doing soak testing for now.** Four
reasons, and none of them is that the case is wrong:

- **The suite already has its soak, and it is SCALE-07**, a sustained 8h
  throughput run in Section 3.4. DATA-14 sits between that and SCALE-03's pod
  fan-out: twenty pods under sustained mixed load for an hour is a scale
  question wearing a data path number. Doing it here, ahead of the section it
  belongs to, would mean two soaks to reconcile when they disagree.
- **It needs an image nobody has supplied.** `fio` is not ours to build, so the
  case can only run behind an operator-named image, and none has been named in
  any run so far.
- **It asks for tens of gigabytes.** Twenty pods at up to 1GiB across four files
  each is 80GiB in the worst case, which is two orders of magnitude more than
  any other case in the plan asks of an export.
- **An hour fits inside no category budget.** Section 4.2 gives every category
  target 45 minutes or less except CHAOS. An hour-long case either breaks that
  or needs a target of its own, and a target of its own reads as a soak
  category, which this suite does not have.

What the deferral costs: nothing that runs today covers sustained mixed
read/write load at scale. DATA-01 covers concurrent writers with cross-verified
checksums, and CHAOS-01, CHAOS-02 and CHAOS-05 cover content survival across
failovers, but all of them are minutes, not hours. That gap is real and it is
SCALE-07's to close.


### 3.3 Resiliency and chaos (CHAOS)

Every case in this section runs with active I/O and asserts against the SLO table in Section 3.8.

| ID | Case | Expected |
|---|---|---|
| CHAOS-01 | ✅ SIGKILL the server process during active write | I/O blocks, does not error; resumes within restart SLO; committed data intact |
| CHAOS-02 | ✅ Delete the server pod during active write | As above, plus locks reclaimed |
| CHAOS-03 | Hard-stop the node hosting the server | I/O resumes within node-loss SLO; backing volume re-attaches |
| CHAOS-04 | Network partition the server from clients, then heal | Clients block, then recover; no data loss |
| CHAOS-05 | ✅ Repeated failover, 5 cycles inside 10 minutes | Each cycle recovers; grace does not enter a re-entry loop. Derived from reports of clients stalled for hours after repeated grace entry during address takeover. |
| CHAOS-06 | ✅ Failover while a client holds byte-range locks | All locks reclaimed; no conflicting lock granted to a different client during grace |
| CHAOS-07 | ✅ Failover while a second client attempts a *new* lock | New acquisition rejected during grace with a retryable error, then succeeds after grace ends. Never granted during grace. |
| CHAOS-08 | Client-side: server restarts with new state | Client handles stale handles without permanent EIO; no manual remount required |
| CHAOS-09 | kubelet restart on a client node with mounts active | Mounts survive; I/O resumes |
| CHAOS-10 | CSI node plugin pod eviction with mounts active | Existing mounts unaffected; new mounts queue and succeed after recovery |
| CHAOS-11 | CNI restart on a client node | I/O blocks then resumes; no unmount |
| CHAOS-12 | NetworkPolicy applied that blocks the data path, then removed | Clients block, then recover cleanly |
| CHAOS-13 | Backing block volume disconnect during write | Errors surface as retryable; no silent corruption |
| CHAOS-14 | **Colocation deadlock**: server pod scheduled on the same node as its clients, under memory pressure | No reclaim deadlock. Specific to hyperconverged CNode/DNode topology: the local client blocks in page reclaim waiting on a local server that needs memory to progress. |
| CHAOS-15 | Client node OOM with dirty pages on the NFS mount | Bounded failure; no node-level hang |
| CHAOS-16 | 24h chaos soak: randomized kills, partitions, evictions | Zero data corruption; zero unrecovered mounts; zero core dumps |
| CHAOS-17 | Recovery state store lost or corrupted, then server restarts | Bounded, honest failure: clients fail to reclaim and locks are lost, but no file data corruption, no permanent client hang, and the loss is observable in metrics or logs. Silently granting conflicting locks after state loss is the failure being hunted. Needs `-recovery-state-path`: there is no portable way to find the recovery state directory, and the case skips without it rather than guessing at an implementation's layout. |
| CHAOS-18 | Delegation held by client A, client B opens the same file conflicting | Delegation recalled and returned within the recall timeout; B proceeds; A sees no corruption. **Skipped, not failed, when delegations are disabled**, which is a common default. Ties to a reported crash on the delegation return path. |

Blanket rule: any core dump on any server pod fails the run. Cores are collected into run artifacts.

How chaos faults are injected, measured, and verified across failovers is in
[`03-chaos-operations-design.md`](03-chaos-operations-design.md).

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
| OBS-01 | Server unavailable | The deployment provides an availability signal that can represent NFS reachability: a readiness probe targeting the NFS service, with the Service's endpoints following it. Fails when the only in-cluster signal is the container's lifecycle, which cannot represent a server that is wedged rather than dead. The behavioral half, that the signal moves when a running server stops answering, needs a fault that does not kill the container and lands in step 10. |
| OBS-02 | ✅ Failover event | Event is observable in metrics or logs with a timestamp; measurable duration |
| OBS-03 | ✅ Grace period entry and exit | Both observable; duration measurable. Required to make CHAOS-05 diagnosable. |
| OBS-04 | ✅ Mount failure on a client | Surfaced as a Kubernetes Event on the pod with an actionable reason |
| OBS-05 | Server memory approaching ceiling | The server container declares a memory limit, its working set is readable against that limit with a timestamp, and the reading moves when the server is worked. An OOMKill, if one occurs, is visible with a timestamp. Fails when no limit is declared: an undeclared ceiling is one nobody can monitor against. **Not manufactured**: no case drives the server to its limit. |
| OBS-06 | ✅ Volume near capacity | The control plane reports this volume's usage, it agrees with `df` inside the pod within a stated tolerance and freshness, and both move together when the workload writes. Fails when the CSI driver reports no usage for the volume, and fails when the reported total is not the claim's capacity: an export with no per-volume quota is measuring the backing filesystem, so no threshold on that number describes this claim. The agreement comparison is still run and recorded either way. |
| OBS-07 | ✅ Metrics survive server restart | The server's own metrics answer before a restart and after it, and its counters either reset cleanly or carry on. A gap that shows the outage is not a defect. Counters that read identically across the restart settle nothing: a fresh process that re-derives the same numbers is indistinguishable from one that kept them, so that outcome is reported and claimed as neither ([F-024](findings.md)). Fails when the server publishes no metrics endpoint: a deployment that says nothing about NFS has nothing that could survive anything, and no case substitutes a container-level signal for it. |

**Alerting rules are out of scope.** Thresholds, durations, severities and
routing are organization-specific and problem-specific, and a portable suite
that asserted on them would fail a healthy storage system for a threshold set
differently, and would have to find or install a monitoring stack it does not
own. The cases above verify the **data an alert is built on**: that it
exists, is current, and agrees with what the workload sees. A deployment missing
that data cannot be alerted on by anyone, whatever rules they write, so each
case fails rather than skipping when the data is absent.

How the data is read, and what a green run does and does not establish, is in
[`06-observability-design.md`](06-observability-design.md).

### 3.6 Security and identity (SEC)

| ID | Case | Expected |
|---|---|---|
| SEC-01 | ✅ UID/GID preservation across pods | Ownership as written. Derived from reports of v4 clients mapping local IDs to `nobody` on modern kernels. |
| SEC-02 | ✅ Who may change a file's ownership | An owner is refused a `chown` of its own file: changing a file's owner requires privilege everywhere, and owning it is not privilege ([`chown(2)`](https://man7.org/linux/man-pages/man2/chown.2.html)). What the server does with a client claiming uid 0 is a deployment choice, so it is asserted only against `-root-squash` where that states the intent, and otherwise recorded — but whichever rule is in force must be applied to every client alike, since a half-squash makes what a workload may do depend on where it was scheduled. The probe is the operation, never what `stat` displays: F-021 is this case reading a client-side mapping as the export's policy. |
| SEC-03 | ✅ Pod `securityContext.fsGroup` interaction | Group access correct; no unexpected chown storm on large volumes |
| SEC-04 | ✅ Client state isolation between nodes | One node losing its mount must not discard another node's locks, and the survivor must still be able to read and write. NFSv4.1 holds state per client, so if two nodes are one client to the server, either one's departure takes the other's state with it and the survivor is never told. Derived from an open report that connections carrying no client identity are treated as the proxy host; the access-control half of that report is measured end to end by SEC-05, which attempts the mount rather than inspecting addresses. **High priority: Kubernetes always inserts a Service.** |
| SEC-05 | ✅ Denied client attempts mount | Rejected, not silently granted |
| SEC-06 | ✅ Dual-stack client identity (**skipped unless both IP families present**) | Client identity consistent. Derived from an open report of inconsistent normalization between IPv4 and IPv4-mapped IPv6. |
| SEC-07 | ✅ Two pods with identical client identity after restart | No state collision; no lost locks |
| SEC-08 | ✅ Data path confidentiality, stated | Records whether NFS traffic on the shared pod network is cleartext, and whether any transport encryption is in effect. This is a **finding, not a pass/fail**: with no dedicated storage network, cleartext NFS shares a fabric with tenant traffic, and that fact belongs in the record whether or not it is acceptable. |
| SEC-09 | ✅ Server pod under the platform's admission policy | Server runs with the capability set it actually needs and no more. A file-handle-based backend needs `CAP_DAC_READ_SEARCH` for `open_by_handle_at(2)`; if the policy strips it, file handle operations fail with EPERM. Conditional on the backend using that path. |

How each of these is probed, and what a green run does and does not establish,
is in [`07-security-design.md`](07-security-design.md).

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

1. Kubernetes automatically adds tolerations for `node.kubernetes.io/not-ready` and `node.kubernetes.io/unreachable` with `tolerationSeconds=300` unless the pod sets them explicitly ([taints and tolerations](https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/)), so a pod on a dead node stays bound for five minutes before eviction.
2. Once evicted, an RWO backing volume must detach from the dead node before it can attach elsewhere. The attach-detach controller waits `maxWaitForUnmountDuration` (6 minutes) before force-detaching, and that value is not currently configurable.

Worst case on stock settings is therefore around 11 minutes, not 90 seconds. Two required configuration changes on the test cluster, both recorded in `environment.json` and asserted at preflight:

- The server workload sets `tolerationSeconds: 30` explicitly on both taints.
- Ungraceful node loss applies the `node.kubernetes.io/out-of-service=nodeshutdown:NoExecute` taint to trigger immediate volume detach, which is the GA path for exactly this case ([non-graceful node shutdown](https://kubernetes.io/docs/concepts/architecture/nodes/)).

If either is absent, CHAOS-03 is reported as **blocked on cluster configuration**, not failed. Distinguishing the two matters: a 6-minute recovery caused by a controller default is a deployment defect, and filing it against the NFS server wastes a week.

---

## Section 4: Automation and execution

### 4.1 Harness design

- Language: Go. `client-go` for orchestration, standard `testing` for structure, table-driven cases. No Ginkgo.
- The harness runs **on the operator's workstation**, outside the cluster. It authenticates with a kubeconfig, creates PVCs, pods and DaemonSets, injects faults, collects logs, and asserts. It creates no namespaces: everything lands in `default`. No component of the suite is deployed into the cluster ahead of time.
- The harness is a Kubernetes client, not an NFS client. It never mounts the share itself. All I/O comes from in-cluster pods running portable binaries: `dd` for simple patterns, `flock` and `locktool`, a small static Go binary in this repository, for lock semantics, `sha256sum` for verification, `stat` and `df` for what the client sees. `fio` arrives with the scale cases, from an operator-supplied image.
- Triage step 6, reproducing outside Kubernetes against the server with a plain Linux NFS client, is done by hand. An earlier draft of this plan gave the harness an `-external-mount` flag for it; no such flag exists, because the server is normally unreachable from the workstation on a private cluster and a flag nothing uses is a flag that rots.
- Node-level assertions (`/proc/mounts`, `/proc/locks`, dmesg, process signals) go through a privileged DaemonSet with host namespace access. This is the only privileged component, and its absence is a preflight failure rather than a silent skip.
- No distro-specific APIs. Chaos is expressed as Kubernetes operations or a signal through the node agent, so the same case runs on GKE and on bare metal. `pkg/chaos` holds two operations today, both portable; the per-platform interface arrives with node power in step 10, which is the one thing that genuinely differs.
- Every case is skippable by capability, never by platform name. `if !caps.CanKillNode { t.Skip() }`, never `if platform == "gke"`.

**Conventions that hold for every case, whatever section it comes from.** These
were settled while building the phases below and are stated here rather than in
any one design doc, because a convention that lives inside a phase is one the
next phase has to rediscover:

- **The suite counts nodes it can schedule on, never nodes with a role label.** `SchedulableNodes` returns exactly those: Ready, not cordoned, and carrying no taint a test pod would have to tolerate. A control-plane node that does not want workloads carries a `NoSchedule` taint and is excluded by that; one that does not is a node its operator runs workloads on, and a three-node cluster where every node serves both the API and the workloads is a supported topology, not a degraded one. Filtering on the label instead left such a cluster with nothing and stopped preflight for the wrong reason. Where a design document written before this still says "worker", read it as this: the term carries no role meaning anywhere in the suite.
- **Fault operations live in `pkg/chaos`, not on the per-case fixture.** Faults are the one thing a reader should be able to enumerate in one file, and a fault that records itself cannot be forgotten by the case that injected it.
- **Targets are resolved live, at case time, never from the cached environment record.** Preflight names the pod that was there when it ran; the chaos cases move pods around. A stale target either fails to act or acts on the wrong thing.
- **A fault that was not injected is never measured.** An operation that cannot identify its target, or that matched nothing, is an error, and the case reports blocked. A recovery measured from a fault that never landed passes for the wrong reason.
- **A node runs at most one NFS server process, and the suite assumes it.** A signal is delivered by matching the process name across the whole node, so a node carrying two servers would take one fault and lose both, and the case would attribute a two-server outage to a one-server fault. Nothing in Kubernetes prevents that arrangement: a server pod has its own network namespace, so two of them on one node would each bind 2049 without conflict. What makes the assumption safe against the deployment under test is the provisioner, which advertises a Service address and [refuses to provision at all](https://github.com/kubernetes-sigs/nfs-ganesha-server-and-external-provisioner/blob/master/pkg/volume/provision.go#L425-L444) when that Service resolves to more than one endpoint, capping its fan-out at one pod per cluster. A deployment that genuinely runs several servers needs the signal scoped to the pid discovery already records, and that is work for the case that needs it rather than ahead of it.
- **A baseline that could not be read blocks the assertion that needs it, not the case that carries it.** A before-and-after reading whose "before" is unavailable returns an error, never a zero: two zeros compare equal and the assertion passes without having measured anything. The assertion reports blocked on its own, so that the rest of the case, which is usually about something else entirely, still returns a verdict. This is why the server restart count refuses to answer where discovery found no server pods, and where the pods it found have not reported a container yet.
- **Both ends of any measurement come from one clock.** Times that will be compared are read from the same pod, never one from a pod and one from the workstation: a few seconds of skew is invisible and moves every number. Where two clocks are unavoidable, because the two ends are on different nodes by construction, the window is narrowed by a guard band in `pkg/slo` and only an unambiguous violation is reported.
- **A case waits past its target rather than up to it.** Stopping at the SLO reports "timed out" where the case could report how long recovery actually took, and the second is what a defect report needs.
- **A tool the image may not carry is probed before it is used**, and its absence reports blocked, naming the flag that fixes it. A missing tool is never a protocol finding.
- **Timing and correctness bounds live in `pkg/slo`**, against the profile preflight pinned. No case carries a literal.
- **What a case reports** (passed, failed, blocked, skipped) is defined in the repository `README.md`, and the same four words mean the same four things in every section.
- **A case names the file it argues from, and the harness keeps a copy.** The two things every claim rests on are destroyed by teardown: the workload's record stream, which lives on the pod's own filesystem, and the file on the share a data case is about. Both are copied into the bundle before anything is deleted, on a pass as well as a failure, because a passing case's numbers are exactly the ones nobody can re-derive later. One named file per registration, capped, and nothing walks the share: a rule that collected a directory would try to bring DATA-10's hundred thousand entries home.

### 4.2 Execution by Category

| Target | Contents | Budget |
|---|---|---|
| `make preflight` | Section 0 only | < 3 min |
| `make test-prov` | All PROV cases | < 45 min, 2 nodes |
| `make test-data` | All DATA cases | < 45 min, 2 nodes |
| `make test-chaos` | All CHAOS cases | < 4 h, 4 nodes |
| `make test-obs` | All OBS cases | < 45 min, 2 nodes |
| `make test-sec` | All SEC cases | < 30 min, 2 nodes |
| `make test-e2e` | All categories end to end | < 5 h, 4 nodes |

E2E tests are organized strictly by category. Each category has its own test file, Makefile target, and corresponding `Test<Category>...` function prefix.

**Soak is not a category**, and the table above has one row per category. The one case that would have needed a target of its own, DATA-14, is deferred; Section 3.2 says why.

`make test-data` injects faults: DATA-12 and DATA-13 kill the NFS server process. `make test-prov` and `make test-obs` are already in that position.

### 4.3 Triage runbook

Every failure produces `artifacts/<run-id>/` containing `environment.json`, server logs, client pod logs, `/proc/mounts` from every involved node, dmesg, Kubernetes Events, any core dumps, and a timeline of injected faults.

Every run, failed or not, also leaves the files each case named as its evidence: the workload's record stream, which is the input to every recovery number, and the one file on the share a data case is making a claim about. They are listed in `evidence.txt` with what was captured and what was cut off at the size cap, because a truncated file and a whole one are indistinguishable from their bytes.

Triage order:

1. **Is it the harness?** Re-run the single case in isolation. Flaky-on-isolation means harness bug; file against the suite.
2. **Which side of the mount?** Compare client `/proc/mounts` and dmesg against server logs at the fault timestamp. Client stuck with a healthy server means client or network. Server restarted means server.
3. **Is it grace?** Look for repeated grace entry in the window. Grace re-entry loops present as "hung client, healthy server" and are the single most common false diagnosis in this architecture.
4. **Provisioning or data path?** Provisioning failures go to the CSI driver owner. Data path failures go to the server owner.
5. **Version skew?** Compare the two versions in `environment.json`. If they are independently versioned and differ from the last green run, suspect skew first.
6. **Reproduce minimally**, then file with the artifact bundle attached. A failure filed without `environment.json` will be closed as unreproducible.

Before filing, read [`findings.md`](findings.md). It is the citation of record
for this suite: a case that reports blocked, a constant that is the value it is,
or a teardown step that looks like more work than it should be usually has an
`F-NNN` behind it, and several of the failure modes this runbook hunts have
already been met once.

Defect routing when the sharing layer is independently versioned: reproduce outside Kubernetes against the server directly with a plain Linux NFS client. Reproducing outside Kubernetes routes the bug upstream to the server. Not reproducing routes it to the CSI driver or the packaging.

---

## Section 5: Delivery order and progress

### 5.1 Progress summary

Forty-two cases are in the tree today out of 66 core cases (63.6%) and 69 total
cases (60.9%) including conditional skew testing. Each case's status is
marked directly in Section 3: shipped cases carry a ✅, and the rest carry nothing.

| Category | Shipped | Deferred | Remaining | Total | Status |
|---|---|---|---|---|---|
| PROV | 11 | 0 | 0 | 11 | Complete (Steps 1, 2, 5) |
| DATA | 13 | 1 | 0 | 14 | Complete (DATA-14 deferred to SCALE-07) (Steps 1, 2, 2b, 6) |
| CHAOS | 5 | 0 | 13 | 18 | In progress (Steps 3, 4 done; Step 10 remaining) |
| OBS | 4 | 0 | 3 | 7 | In progress (Steps 2b, 4 done; Step 7 in progress; OBS-01 half in Step 10) |
| SEC | 9 | 0 | 0 | 9 | Complete (Steps 2, 2b, 8). SEC-05 is red against this deployment ([F-018](findings.md)) and SEC-06 skips on a single-stack cluster |
| SCALE | 0 | 0 | 7 | 7 | Not started (Step 9) |
| SKEW | 0 | 0 | 3 | 3 | Not started (Step 11, conditional on independent versions) |
| **Total** | **42** | **1** | **26** | **69** | **42 / 66 core cases shipped (63.6%)** |

### 5.2 What the latest runs returned

Delivery says what exists. This says how it did, on the two deployments it has
been run against. Both are three-worker GKE clusters, Kubernetes
v1.37.0-gke.2941000, Container-Optimized OS with kernel 6.12.94+, `e2-medium`
nodes, StorageClass `nfs`, profile `default` (lease 60s, grace 90s). They differ
in the server image: `gke-w1` runs the upstream `nfs-server-provisioner` v4.0.8,
`gke-w2` a locally built `nfs-provisioner:15.3` (Ganesha V15.3-mb). Results from
two deployments are not results about NFS, and every red below is a statement
about these deployments.

**Whole suite on both clusters, 2026-09-17, `make test-e2e`, run concurrently:
31 passed, 7 failed, 4 blocked, 1 skipped** over the forty-three cases in the
tree. `gke-w1` (run `w1-e2e-20260916-2015`) took 67 minutes, `gke-w2` (run
`w2-e2e-20260916-2015`) 70 minutes. **The two clusters returned the same verdict
on every case**, which is the first evidence here that any of these results is a
property of the architecture rather than of one deployment.

That reconciles with the previous whole-suite run (2026-09-13, `gke-w1`, 35
cases: 26 passed, 5 failed, 4 blocked) with nothing unexplained: the same five
reds, the same four blocked, plus SEC-05 and OBS-07, both of which were already
red in the category runs that shipped them.

None of the reds is a defect in the storage server, and each one has a finding:

| Case | Result | Whose problem |
|---|---|---|
| SEC-05 | fail | A node with no claim mounted another claim's export and read its bytes. The exports carry no client rules, nothing in the network path restricts who may connect, and `no_root_squash` is set, so the access control around a claim ends at the mount. [F-018](findings.md) |
| DATA-02 | fail | Four clients appending to one file landed 150 of 200 records and tore none, one appender losing its whole contribution. Identical on both clusters, a different appender each time. NFSv4.1 has no append operation, so this goes to the boundary discussion rather than to the server owner, and it is intermittent: whole-suite runs are 5 for 5, data-only runs 0 for 2. [F-016](findings.md) |
| OBS-03 | fail | Neither provisioner announces grace in its container log stream, so there is no signal to observe. [F-008](findings.md), and [F-022](findings.md) for where the signal actually is |
| OBS-06 | fail | The export has no per-volume quota, so both capacity sources describe the backing filesystem rather than the claim. Both agreed on used bytes and on the movement; only the total is meaningless. [F-009](findings.md) |
| OBS-07 | fail, for different reasons | On `gke-w1` no endpoint exists to scrape, verdict `absent` ([F-023](findings.md)). On `gke-w2`, whose exposer and scrape annotations were both switched on by hand and so is no longer as-shipped, the case reached its later steps for the first time and returned `never-resumed` over metric families the server had not yet recreated, which is a harness defect rather than a deployment one ([F-025](findings.md)) |
| PROV-04, PROV-11 | fail | The StorageClass advertises `allowVolumeExpansion` and nothing implements it. [F-004](findings.md) |

The four blocked are CHAOS-01, DATA-12 and DATA-13, which need a process name
neither image lets the harness discover, and CHAOS-07, which needs the grace
window OBS-03 could not see. DATA-11's hole-punch subtest is blocked on busybox
`fallocate`. SEC-06 skips because both clusters are single-stack. **Blocked and
skipped are not passes**: those vectors are unexercised here, and a green run
against these provisioners is not evidence that grace behaves.

Failover recovery agreed closely across the two clusters and stayed inside the
2m0s budget for the default profile: CHAOS-02, CHAOS-06 and OBS-02 recovered in
1m43s to 1m46s on both. CHAOS-05's five cycles took 8m48s on `gke-w1` and 9m6s
on `gke-w2`, whose worst single cycle, 1m58s, is the narrowest margin against
the budget recorded so far and is worth watching rather than acting on.

### 5.3 Delivery steps

One step is one pull request, or a short run of them. Later steps depend only on
earlier ones. Steps are numbered independently of the documents; a step's design
doc, where it has one, is named in its row.

| Step | Scope | Status |
|---|---|---|
| 1 | Approach, harness skeleton, preflight (Section 0), PROV-01, DATA-03, DATA-05 `flock` | done, [PR #1](https://github.com/mikebz/nfs-verification/pull/1) (PROV-01 documented in [doc 02](02-provisioning-design.md)) |
| 2 | Cases needing nothing new from the harness: PROV-03, PROV-04, DATA-01, DATA-04, SEC-01 (PROV cases documented in [doc 02](02-provisioning-design.md), SEC-01 in [doc 07](07-security-design.md)) | done, [PR #3](https://github.com/mikebz/nfs-verification/pull/3) |
| 2b | The three held back from step 2: DATA-02, OBS-04, SEC-02 (SEC-02 documented in [doc 07](07-security-design.md), OBS-04 in [doc 06](06-observability-design.md)) | done, [PR #4](https://github.com/mikebz/nfs-verification/pull/4) |
| 3 | `pkg/chaos`, CHAOS-01, CHAOS-02, the SLO measurement path, fault timelines. [Design](03-chaos-operations-design.md) (serves all CHAOS cases) | done, [PR #4](https://github.com/mikebz/nfs-verification/pull/4) |
| 4 | Grace and lock reclaim: CHAOS-05, CHAOS-06, CHAOS-07. [Design](04-grace-and-lock-reclaim-design.md) (historical; consolidated in [doc 03](03-chaos-operations-design.md) and [doc 06](06-observability-design.md)) | done, [PR #8](https://github.com/mikebz/nfs-verification/pull/8) |
| 5 | Close out PROV: PROV-02, PROV-05 to PROV-11. [Design](02-provisioning-design.md) (serves all PROV cases) | done, [PR #10](https://github.com/mikebz/nfs-verification/pull/10) |
| 6 | Close out DATA: DATA-06 to DATA-13, `locktool`. [Design](05-data-path-and-locktool-design.md) (serves all DATA cases) | done except the soak, [PR #12](https://github.com/mikebz/nfs-verification/pull/12) onward. DATA-10 has now been run and passes (2026-09-13); DATA-14 is deferred (Section 3.2) |
| 7 | OBS: OBS-01 through OBS-07. [Design](06-observability-design.md) | **in progress**, two of three PRs done: the kubelet stats reader and OBS-06 ([PR #29](https://github.com/mikebz/nfs-verification/pull/29)), red on the quota check ([F-009](findings.md)); then the server metrics reader and OBS-07 ([PR #74](https://github.com/mikebz/nfs-verification/pull/74)), run against `gke-w1` and `gke-w2` as shipped, red on both because neither deployment declares an endpoint ([F-023](findings.md)), and run end to end on `gke-w2` with Ganesha's exposer enabled by hand, where the first green was a false one ([F-024](findings.md)). The whole-suite runs of 2026-09-17 reached OBS-07's later steps for the first time, on a `gke-w2` that has since also been annotated by hand, and found the case scrapes before the server has recreated its traffic-driven metric families ([F-025](findings.md)): fixing that is the open harness item. OBS-01 and OBS-05 are left. Section 3.5 stays open either way: OBS-01's behavioral half needs a fault from step 10 |
| 8 | SEC: SEC-01 through SEC-09. [Design](07-security-design.md) | **in review**, [PR #57](https://github.com/mikebz/nfs-verification/pull/57): SEC-03 to SEC-09 are in the tree and the whole group was run against `gke-w1` (run `20260914-011748`, 2m45s). SEC-05 is red and stays red ([F-018](findings.md)), SEC-06 skips on a single-stack cluster, the rest pass. Review also rewrote three cases: SEC-04, which no longer reads the server at all and now asks a third node whether the locks survived; SEC-02, which probes a privileged operation rather than what `stat` prints ([F-021](findings.md)); and SEC-03, which now asks its question behind a gated directory, correcting [F-020](findings.md) |
| 9 | Close out SCALE: SCALE-01 to SCALE-07 | not started |
| 10 | Close out CHAOS: CHAOS-03, CHAOS-04, CHAOS-08 to CHAOS-18, and OBS-01's behavioral half | not started |
| 11 | SKEW-01 to SKEW-03, conditional on preflight finding independent versioning | not started |

### 5.4 Why this order

Steps 1 to 4 built the foundation: the harness, the eleven cases that need no
fault, the fault-injection package, and the grace and lock reclaim path.

From step 5 on, **one section of the test plan is closed out at a time** rather
than interleaving fault injection across domains. PROV first, because the
lifecycle has to be trustworthy before anything built on it means much; DATA
next, because the data path is what the protocol actually guarantees; then OBS
and SEC; then SCALE; then CHAOS last, because the most invasive
platform-dependent faults are worth running only once a failure elsewhere can be
ruled out; then SKEW, if preflight finds the server and driver independently
versioned.

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
| Per-client export rules degrading to global access behind a proxy | SEC-05 for the access-control consequence, SEC-04 for the state-isolation one |
| Inconsistent IPv4 / IPv4-mapped IPv6 client identity normalization | SEC-06 |
| Deleting the server deployment renders outstanding PVs unusable | PROV-07, PROV-08, SCALE-06 |

## Appendix B: Explicitly out of scope for v1

- NFS v3, v4.0, v4.2. Deferred to v2.
- Upgrade of any component. Deferred to v2.
- Multi-cluster and cross-region.
- Kerberos and `sec=krb5*`. AUTH_SYS only.
- RPC-over-TLS ([RFC 9289](https://www.rfc-editor.org/rfc/rfc9289.html)), certificate rotation under load, and TLS handshake storm cases. Deferred to v2 and **contingent on SEC-08**: server-side support is not a given, and the Linux client side needs both an `xprtsec=` capable kernel and a handshake helper daemon on the node, neither of which is controllable on a managed node image. Probe first, then plan. CNI-level encryption (WireGuard, IPsec) is a platform property and belongs in the platform suite, not here.
- pNFS and RDMA transports.
- GKE Autopilot (no privileged pods, so no node-level assertions).
- Any case asserting POSIX coherence across clients. Out of scope by architecture, not by budget.

## Appendix C: Unratified assumptions

These are decisions the plan made in the absence of an input. Each is a one-line change.

1. Workload profile: 20 pods per volume, 4KiB to 1GiB files, 70/30 read/write.
2. Scale targets: 50 volumes per cluster, 10 mounts per node.
3. AUTH_SYS. Whether root is squashed is no longer assumed: `-root-squash` states it where somebody knows, SEC-02 records it otherwise, and either way SEC-02 requires the same rule on every client.
4. Node auto-repair and auto-upgrade disabled on the test node pool. If they are not, chaos results are invalid.
5. Server workload sets `tolerationSeconds: 30`, and ungraceful node-loss testing applies the out-of-service taint. Without both, CHAOS-03 is blocked rather than failed. See the floor note in Section 3.8.

Resolved since draft: failover SLO (60s process and graceful, 90s ungraceful node loss, against the `tuned` lease and grace profile); harness location (workstation, external, Kubernetes client only); lease and grace pinned to two explicit profiles rather than left at vendor defaults; `root_squash`, which was assumed on and is a deployment choice no case may invent.

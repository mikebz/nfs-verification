# 03: Resiliency and chaos: fault injection, failover recovery, and lock reclaim

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-14
Status: **in progress.** Serves the complete Resiliency and Chaos test group:
CHAOS-01, CHAOS-02, CHAOS-05, CHAOS-06, and CHAOS-07 shipped (Steps 3, 4, 6);
CHAOS-03, CHAOS-04, and CHAOS-08 through CHAOS-18 planned for delivery step 10.
Serves: CHAOS-01 through CHAOS-18. Requirements in [`01-test-plan.md`](01-test-plan.md)
Section 3.3, targets in Section 3.8, weighting in Section 2.4.
Builds on [`01-test-plan.md`](01-test-plan.md). Consolidates chaos design ownership
across the repository, superseding the chaos sections of [`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md).

---

## 1. Why this domain exists

The NFS server is the singleton in the data path (test plan Section 2.1): every
RWX client serializes through one server process, so what a client observes when that
process dies, stalls, or relocates is the heaviest-weighted question in the plan.
Before this test group was built, the suite could assert steady state and nothing else:
it had no mechanism to injure the system and no way to measure what happened next.

Failover in this architecture is an opaque black box (test plan Section 2.3). No case
asserts *how* failover happens (e.g. Pacemaker, Kubernetes StatefulSet controller,
cloud volume re-attachment). Every chaos case asserts only client-observable behavior
derived from the protocol:
1. **Recovery**: time until client I/O resumes, measured against the pinned `pkg/slo` profile.
2. **Error behavior**: zero I/O errors across server failovers on `hard` NFSv4.1 mounts (storage/network faults such as CHAOS-13 surface bounded retryable errors).
3. **Durability**: zero loss of data acknowledged as committed before the fault.
4. **State preservation**: all advisory locks held before failover successfully reclaimed given a healthy recovery store (with CHAOS-17 verifying an honest, non-conflicting failure when the store is corrupted).
5. **Grace period enforcement**: new locks refused during grace, preventing state corruption.

Done means eighteen repeatable resiliency cases that drive real cluster and storage
faults, with every recovery verified from client pods on independent worker nodes.

## 2. The fault injection framework (`pkg/chaos`)

Fault injection operations live in `pkg/chaos`, isolated from the test fixtures so that
every fault is explicitly enumerated, safe-guarded, and tracked:

- **Live target resolution (`ServerTarget`)**: Targets are resolved live at case execution
  time via the Kubernetes API, never from cached preflight records. Chaos operations move
  pods and change IPs; stale targets either act on the wrong pod or fail silently.
- **Process signaling (`KillServerProcess`)**: Signals the NFS server process on its host
  node through the privileged node agent (`nsenter` into the host PID namespace).
  - *Safety check*: Lists matching PIDs first and errors if nothing matched.
  - *Reject list*: Refuses generic process names (`sh`, `bash`, `wrapper`). Killing a shell
    wrapper takes down unrelated node infrastructure, turning a targeted test into an
    uncontrolled outage.
- **Graceful pod deletion (`DeleteServerPod`)**: Deletes the server pod with a standard grace
  period via the Kubernetes API.
  - *Safety check*: Inspects `ownerReferences` and refuses any pod with no controller. An
    unmanaged pod would never be recreated, turning a recovery test into a permanent outage.
- **Fault timeline recording**: The framework records a timestamped `FaultEvent` for each
  successful fault operation. On test failure, `CollectArtifacts` bundles the recorded faults
  and cluster state into the run's artifact directory (`artifacts/<run-id>/`) for post-failure triage.
- **The record stream is kept**: the workload's log is the input to every recovery number
  here, and it lives on the writer pod's own filesystem, so it used to go with the pod at
  teardown. The workload now registers it as case evidence, which is copied into the bundle
  on a pass as well as a failure. A passing CHAOS-05 carries five recovery measurements, and
  without the log none of them can be re-derived; F-017 is a run whose numbers had to be.

### Platform and network fault roadmap (Step 10)
The remaining operations close out the CHAOS matrix in Step 10:
- **Node hard-stop (`CHAOS-03`)**: Executes node-level power-off (`-node-power-cmd` or cloud API).
  Requires the server pod to set `tolerationSeconds: 30` on both `node.kubernetes.io/not-ready` and
  `node.kubernetes.io/unreachable` taints, and the harness applies
  `node.kubernetes.io/out-of-service=nodeshutdown:NoExecute` to trigger immediate RWO volume detach.
- **Network partitioning (`CHAOS-04`, `CHAOS-12`)**: Injects bidirectional packet drops between
  client nodes and server pods via node agent iptables rules or temporary `NetworkPolicy`.
- **Component eviction and restarts (`CHAOS-09`, `CHAOS-10`, `CHAOS-11`)**: Restarts kubelet,
  evicts the CSI node plugin daemonset pod, or restarts the CNI plugin while NFS mounts are active.
- **Deadlock and resource exhaustion (`CHAOS-14`, `CHAOS-15`, `CHAOS-17`)**: Hyperconverged
  colocation deadlock under memory pressure, client node OOM with dirty NFS pages, and recovery
  state directory loss (`-recovery-state-path`).

## 3. The measurement path and silence detection

Every chaos case runs a dedicated workload in a client pod while the fault is injected:

- **Workload cadence**: One 4KiB record written per second with `conv=fsync`, logging
  `OK|ERR <index> <epoch>` on the pod's **own local filesystem**, never on the share.
  - *Local logging*: A stalled NFS mount must never stall the harness's ability to read
    workload progress.
  - *Resolution*: 1 second. Sufficient for 60s+ SLO budgets without filling the volume.
- **Outage detection as a silence (`StallAfter`)**:
  - *The flaw in timestamp comparison (F-014)*: An earlier draft measured recovery from the
    first write timestamped at or after the fault. Because deleted pods continue serving
    during termination grace, and timestamps have 1-second granularity, this returned writes
    committed *before* service was lost, falsely reporting 100s failovers as `0s`.
  - *The silence rule*: On a hard mount, an outage is a silence. Recovery is measured by
    finding the silence gap in the write log (`LoadReport.StallAfter`) against `slo.LoadStallFloor`.
- **Zero I/O errors on hard mounts**:
  - Under `hard` mount options (`nfs(5)`), the Linux client retries RPCs indefinitely when the
    server is unreachable.
  - Returning an I/O error (`EIO`) to an application during a server failover on a `hard` mount
    violates the client mount contract (`nfs(5)`), not a slow recovery. Errors are asserted strictly to be zero.
- **Durability content sweep**:
  - Once client I/O resumes, a verifier pod on an **independent worker node** reads back every
    record index acknowledged before the fault.
  - Reading across worker nodes ensures the verification crosses the server and backing disk
    rather than hitting the writer's local page cache.
  - Records are verified for full content and length, returning exhaustive verdicts (`correct`,
    `absent`, `short`, `wrong`).

## 4. Grace and lock reclaim across failover

Grace is the interval after a restart in which the server accepts reclaims of state that
existed before the crash and refuses all new state acquisitions (RFC 8881 Section 8.4.2):

```
Fault Injected
     │
     ▼
Server Down ──► Server Restart ──► Grace Window Starts ──► Grace Window Ends ──► Normal Operation
(RPCs block)    (Grace Entry)     (Reclaims allowed;      (Grace Exit)          (New state allowed)
                                   new locks rejected)
```

- **Why grace matters**: It is the dominant term in every recovery target. If an NFS server enters
  grace repeatedly during address takeover, clients stall for hours—the single most common false
  diagnosis in this architecture (triage runbook Section 4.3).
- **Grace observation (`pkg/framework/grace.go`)**:
  - Read from the server pod's container log stream using container runtime timestamps.
  - Uses an exit-first keyword classification to avoid mistaking negative exit phrases for entries.
  - If the server announces no grace in logs, `CHAOS-07` reports blocked and `OBS-03` fails (F-008).
- **Lock reclaim verification (`CHAOS-06`)**:
  - *Whole-file locks*: Taken using `flock -x`. The Linux kernel simulates `flock` via whole-file
    POSIX locks on the wire (RFC 8881 Section 9).
  - *Byte-range locks*: Taken using `cmd/locktool hold`, coordinating disjoint byte ranges across
    pods on distinct nodes.
  - *Dual-end assertion*: Verified both from the holder (descriptor remains valid and holding)
    and from conflicting clients (refused by server during and after recovery). Client `/proc/locks`
    is inspected via the privileged node agent.
- **Rejection of new locks during grace (`CHAOS-07`)**:
  - A probe client attempts a new lock once per second during failover.
  - RFC 8881 bars new state acquisition during grace: any grant inside the grace window is a
    protocol violation.
  - *Un-reclaimed state requirement*: A server may lift grace early if no clients have state to
    reclaim. CHAOS-07 deliberately holds one un-reclaimed lock across failover so that grace is
    actively enforced when the probe runs; otherwise, the case passes vacuously.
- **Multi-cycle repeated failovers (`CHAOS-05`)**:
  - Injects five sequential failover cycles.
  - *Cadence*: Each cycle is injected only after the previous cycle has fully recovered.
  - *Per-cycle assertions*: Each cycle must recover within SLO and enter grace at most once
    (checking for grace re-entry loops without requiring an observed exit; OBS-03 owns the grace
    entry/exit assertion). A cumulative wall-clock budget would hide an outage that took four
    minutes if others were fast.

## 5. What these cases assert

Conventions shared across the suite (one clock per measurement, waiting past target, bounds
from `pkg/slo`) live in [`01-test-plan.md`](01-test-plan.md) Section 4.1. The table below lists
the core assertions across the Resiliency & Chaos test group:

| Case | Assertion | Source & Basis |
|---|---|---|
| **CHAOS-01** | ✅ Server process SIGKILL during write: recovery within SLO, 0 errors, post-COMMIT data intact | RFC 8881 Sec 18.3 (`COMMIT`), `nfs(5)` hard mount retry |
| **CHAOS-02** | ✅ Server pod graceful delete during write: recovery within SLO, locks reclaimed | Kubernetes graceful pod termination, RFC 8881 Sec 8.4.2 |
| **CHAOS-03** | Hard-stop node hosting server: recovery within node-loss SLO, RWO volume re-attaches | Kubernetes out-of-service taint, non-graceful shutdown GA |
| **CHAOS-04** | Network partition server from clients, then heal: I/O blocks then resumes, no corruption | CNI network isolation, TCP retransmit recovery |
| **CHAOS-05** | ✅ Repeated failover (5 cycles): each cycle recovers, no grace re-entry loop | RFC 8881 Sec 8.4.2, grace stability under churn |
| **CHAOS-06** | ✅ Failover with held locks: 100% whole-file locks reclaimed (disjoint byte ranges asserted if supported), conflicting clients blocked | RFC 8881 Sec 9 (LOCK) & Sec 8.4.2 (Reclaim), `locktool` |
| **CHAOS-07** | ✅ New lock attempt during grace: 0 grants inside grace window (blocked/refused), grant succeeded after grace | RFC 8881 Sec 8.4.2 (Grace state exclusivity) |
| **CHAOS-08** | Client handles stale handles (`ESTALE`) after server restart with new state without permanent hang | RFC 8881 filehandle persistence |
| **CHAOS-09** | Kubelet restart on client node with active mounts: mounts survive, I/O resumes | Kubelet volume manager mount tracking |
| **CHAOS-10** | CSI node plugin eviction with active mounts: existing mounts unaffected, new mounts queue | CSI architecture, kernel mount independence |
| **CHAOS-11** | CNI restart on client node: I/O blocks then resumes cleanly | CNI interface churn, kernel TCP recovery |
| **CHAOS-12** | NetworkPolicy applied blocking NFS port 2049, then removed: clients recover | Kubernetes NetworkPolicy data path filtering |
| **CHAOS-13** | Backing block volume disconnect during write: errors surface as retryable, no silent corruption | Underlying SDS / RWO attach stability |
| **CHAOS-14** | Colocation deadlock: server pod scheduled on same node as clients under memory pressure | Hyperconverged topology page reclaim safety |
| **CHAOS-15** | Client node OOM with dirty pages on NFS mount: bounded failure, no node-level hang | Linux VM dirty page throttling and OOM safety |
| **CHAOS-16** | 24h chaos soak: randomized kills, partitions, evictions: 0 corruption, 0 unrecovered mounts | Systemic reliability under sustained chaos |
| **CHAOS-17** | Recovery state store lost or corrupted: bounded honest failure, no conflicting locks | RFC 8881 recovery backend integrity |
| **CHAOS-18** | Delegation recall under conflicting open: delegation recalled within timeout | RFC 8881 Sec 10.4 (Delegations); skipped if disabled |

## 6. Detailed case walkthroughs (Shipped cases)

### CHAOS-01: Server process SIGKILL under active write
- **Steps**:
  1. Start write load in client pod writing 1 record/s with `conv=fsync`.
  2. Locate server process PID on host node via `ServerTarget`.
  3. Send `SIGKILL` to server PID via privileged node agent (`KillServerProcess`).
  4. Wait for client write silence to end (`StallAfter`) and assert recovery duration is within restart SLO.
  5. Assert zero `ERR` lines in client workload log.
  6. From an independent worker node, sweep all records acknowledged before the kill and verify SHA256 checksums.

### CHAOS-02: Delete server pod under active write
- **Steps**:
  1. Start active workload and let it commit writes.
  2. Take a lock on a shared file from the writer pod and confirm the second client pod (verifier) is refused.
  3. Delete the server pod gracefully via `DeleteServerPod`.
  4. Measure client I/O recovery by finding the silence gap in the workload log via `StallAfter` and assert it is within the restart SLO.
  5. Assert zero I/O errors across the failover and verify all pre-fault writes are intact from an independent worker node.
  6. Verify the writer still holds its lock and the second client is still refused.

### CHAOS-05: Repeated failovers (5 cycles)
- **Steps**:
  1. Start write load and initialize grace observation.
  2. For cycle = 1 to 5:
     a. Delete server pod gracefully.
     b. Wait for client workload to recover (`StallAfter` silence gap within restart SLO).
     c. Read server container log stream through Kubernetes API to count grace entries.
     d. Assert that grace was entered at most once during this cycle (no re-entry loops; OBS-03 owns the entry/exit assertion).
     e. Allow short stabilization before the next injection.
  3. Verify all records committed before the first fault survived on stable storage.

### CHAOS-06: Lock reclaim across failover (whole-file and byte-range)
- **Steps**:
  1. Start active workload and take four whole-file locks (`flock`) across two clients on separate nodes (three held by writer, one by verifier).
  2. Subtest takes disjoint byte ranges on a separate file using `locktool`: `[0, 4096)` held by writer, `[8192, 12288)` held by verifier.
  3. Verify cross-node exclusion for all whole-file locks and byte ranges before any fault is injected.
  4. Delete server pod gracefully and assert recovery duration within restart SLO.
  5. Assert each whole-file lock holder still reports held, and cross-node probes are refused. Assert 100% reclaim fraction.
  6. Query the server with `F_GETLK` from the opposite client to assert each byte range is still held by its original owner with unmodified boundaries.
  7. Read `/proc/locks` on each holder's node to verify client kernel matches server state.
  8. Deploy a third client pod after failover to probe `[0, 4096)` and `[8192, 12288)`, verifying conflicting acquisitions are refused.

### CHAOS-07: New lock attempted during grace
- **Steps**:
  1. Client pod A acquires lock and keeps outstanding state.
  2. Delete server pod to trigger restart and grace window.
  3. Client pod B runs `lock-probe.sh`, attempting new lock acquisition once per second.
  4. Observer reads server pod container logs through the Kubernetes API to identify grace entry and exit timestamps.
  5. Assert that no lock was granted to client B inside the narrowed grace window (`ClockSkewGuard`) (attempts may be refused or kernel-blocked).
  6. Assert that client B successfully acquires the lock after grace exit.

## 7. Key decisions

- **Graceful pod deletion vs. process SIGKILL**: `CHAOS-01` tests abrupt process death; `CHAOS-02`
  tests Kubernetes pod rescheduling. Both are necessary because an abrupt kill leaves host
  page cache intact, while pod relocation unmounts the backing volume.
- **Outage measured as silence, not timestamp subtraction**: Avoids F-014's false-zero recovery bug.
- **Un-reclaimed state held throughout grace probe**: Prevents premature grace lifting, ensuring
  the server actively enforces state protection.
- **Multi-node guards (`requireCap(t, f.Caps.MultiNode)`)**: Every test reading back data or
  testing locks across clients requires multiple schedulable workers; single-node clusters skip
  honestly rather than failing.
- **Blanket core dump rule (Step 10 target)**: Any server core dump during any chaos case fails
  the suite immediately; automated core sweep and artifact packaging will land in Step 10.

## 8. What real runs taught

- **[F-001](findings.md) (Unmount before PVC deletion)**: Hard mounts retry indefinitely when an
  export is deleted while still mounted. The node agent and teardown ordering ensure pods leave
  the API and mounts clear before claims are touched.
- **[F-008](findings.md) (Unannounced grace)**: Some userspace provisioners never log grace entry
  or exit. In that environment, `CHAOS-07` reports blocked because no window can be established,
  and `OBS-03` reports the missing signal.
- **[F-014](findings.md) (Silence measurement)**: Failover measurements must detect silence gaps
  (`StallAfter`), not first write timestamps.

## 9. Sources

- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 8 (State Management),
  Section 8.4.2 (Server Failure and Recovery), Section 9 (File Locking), Section 18.3 (`COMMIT`).
- [`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) for hard mount retry semantics.
- Kubernetes [Non-graceful node shutdown](https://kubernetes.io/docs/concepts/architecture/nodes/#non-graceful-node-shutdown)
  and [Taints and Tolerations](https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/).

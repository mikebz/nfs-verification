# Implementation plan

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-13

**A working document, and a temporary one.** It holds the delivery order, what
is shipped, and what is left to do: no design decisions, no test approach, no
restatement of the requirements. Those have homes, and a copy here is how these
documents drifted the first time. It sits in the root rather than in `docs/`, and
carries no number, because it is not part of the record those documents keep: it
goes away when the last step lands.

- What gets verified: [`docs/01-test-plan.md`](docs/01-test-plan.md).
- How the harness works, and every flag: the repository
  [`README.md`](README.md).
- Why a phase is built the way it is: that phase's design doc, listed in the
  [`README.md`](README.md).
- What a real run taught: [`docs/findings.md`](docs/findings.md).

Rule for the whole effort: **small, reviewable changes.** One vector per change,
each landing with the harness pieces it needs and nothing more. A change that
adds a helper no case calls yet does not land. A change estimated at 1000 lines
or more lands as a design doc first ([`AGENTS.md`](AGENTS.md)).

## Progress summary

Thirty-five cases are merged in the tree today out of 66 core cases (53.0%) and
69 total cases (50.7%) including conditional skew testing.

| Category | Shipped | Deferred | Remaining | Total | Status |
|---|---|---|---|---|---|
| PROV | 11 | 0 | 0 | 11 | Complete (Steps 1, 2, 5) |
| DATA | 13 | 1 | 0 | 14 | Complete (DATA-14 deferred to SCALE-07) (Steps 1, 2, 2b, 6) |
| CHAOS | 5 | 0 | 13 | 18 | In progress (Steps 3, 4 done; Step 10 remaining) |
| OBS | 4 | 0 | 3 | 7 | In progress (Steps 2b, 4 done; Step 7 in progress; OBS-01 half in Step 10) |
| SEC | 2 | 0 | 7 | 9 | In progress (Steps 2, 2b done; Step 8 remaining) |
| SCALE | 0 | 0 | 7 | 7 | Not started (Step 9) |
| SKEW | 0 | 0 | 3 | 3 | Not started (Step 11, conditional on independent versions) |
| **Total** | **35** | **1** | **33** | **69** | **35 / 66 core cases shipped (53.0%)** |

## Delivery order

One step is one pull request, or a short run of them. Later steps depend only on
earlier ones. Steps are numbered independently of the documents; a step's design
doc, where it has one, is named in its row.

| Step | Scope | Status |
|---|---|---|
| 1 | Approach, harness skeleton, preflight (Section 0), PROV-01, DATA-03, DATA-05 `flock` | done, [PR #1](https://github.com/mikebz/nfs-verification/pull/1) |
| 2 | Cases needing nothing new from the harness: PROV-03, PROV-04, DATA-01, DATA-04, SEC-01 | done, [PR #3](https://github.com/mikebz/nfs-verification/pull/3) |
| 2b | The three held back from step 2: DATA-02, OBS-04, SEC-02 | done, [PR #4](https://github.com/mikebz/nfs-verification/pull/4) |
| 3 | `pkg/chaos`, CHAOS-01, CHAOS-02, the SLO measurement path, fault timelines. [Design](docs/03-chaos-operations-design.md) | done, [PR #4](https://github.com/mikebz/nfs-verification/pull/4) |
| 4 | Grace and lock reclaim: CHAOS-05, CHAOS-06, CHAOS-07, OBS-02, OBS-03. [Design](docs/04-grace-and-lock-reclaim-design.md) | done, [PR #8](https://github.com/mikebz/nfs-verification/pull/8) |
| 5 | Close out PROV: PROV-02, PROV-05 to PROV-11 | done, [PR #10](https://github.com/mikebz/nfs-verification/pull/10) |
| 6 | Close out DATA: DATA-06 to DATA-13, `locktool`. [Design](docs/05-data-path-and-locktool-design.md) | done except the soak, [PR #12](https://github.com/mikebz/nfs-verification/pull/12) onward. DATA-10 has not been run; DATA-14 is deferred (test plan Section 3.2) |
| 7 | OBS: OBS-05, OBS-06, OBS-07 and the half of OBS-01 that needs no fault. [Design](docs/06-observability-design.md) | **in progress**, first of three PRs done ([PR #29](https://github.com/mikebz/nfs-verification/pull/29)): the kubelet stats reader and OBS-06, red on the quota check ([F-009](docs/findings.md)). Section 3.5 stays open either way: OBS-01's behavioral half needs a fault from step 10 |
| 8 | Close out SEC: SEC-03 to SEC-09 | not started |
| 9 | Close out SCALE: SCALE-01 to SCALE-07 | not started |
| 10 | Close out CHAOS: CHAOS-03, CHAOS-04, CHAOS-08 to CHAOS-18, and OBS-01's behavioral half | not started |
| 11 | SKEW-01 to SKEW-03, conditional on preflight finding independent versioning | not started |

## Shipped cases (35)

| ID | Case | Category | Step |
|---|---|---|---|
| PROV-01 | Dynamic provision, bind, mount, write, delete, backing volume reclaimed | PROV | 1 |
| PROV-02 | Provision 20 RWX PVCs concurrently; no duplicate export IDs or paths | PROV | 5 |
| PROV-03 | Delete a claim a pod still mounts; it stays Terminating until the mount is gone | PROV | 2 |
| PROV-04 | Volume expansion, or a clean rejection when the class does not advertise it | PROV | 2 |
| PROV-05 | Snapshot and restore verified from two nodes, or clean rejection where no snapshot class names the driver | PROV | 5 |
| PROV-06 | Reclaim policy Retain: PV persists and rebinds with data intact | PROV | 5 |
| PROV-07 | Provision while server pod is down; recovers cleanly once server returns | PROV | 5 |
| PROV-08 | Delete claim while server pod is down; completes deletion once server returns | PROV | 5 |
| PROV-09 | Rapid create/delete churn (100 cycles); no export ID or fd exhaustion | PROV | 5 |
| PROV-10 | Volume name edge cases: invalid names rejected at admission, and a 253-character name binds, mounts and exports correctly | PROV | 5 |
| PROV-11 | Two-stage volume expansion under active I/O; zero I/O errors | PROV | 5 |
| DATA-01 | Four pods writing at once, four files, cross-verified checksums | DATA | 2 |
| DATA-02 | Four pods appending to one file through a held-open descriptor | DATA | 2b |
| DATA-03 | Close-to-open across two nodes | DATA | 1 |
| DATA-04 | The negative of DATA-03: what a reader may see before the writer closes | DATA | 2 |
| DATA-05 | flock and byte-range mutual exclusion across two nodes | DATA | 1, 6 |
| DATA-06 | A byte-range lock held by a force-deleted pod, released inside one lease | DATA | 6 |
| DATA-07 | One file written and read O_DIRECT by two pods on two nodes | DATA | 6 |
| DATA-08 | The same export mounted twice, once with noac: visibility without a close | DATA | 6 |
| DATA-09 | Silly rename, cross-node unlink and rename under a held descriptor | DATA | 6 |
| DATA-10 | A 100k-entry directory listed while another pod deletes from it | DATA | 6 |
| DATA-11 | Sparse write and read back; the hole punch recorded, not asserted, on 4.1 | DATA | 6 |
| DATA-12 | fsync durability: every committed record intact after a server kill | DATA | 6 |
| DATA-13 | Negative durability: un-fsynced records may be absent, never wrong | DATA | 6 |
| SEC-01 | uid and gid preservation across pods on two nodes | SEC | 2 |
| SEC-02 | What the export does to a root-owned write, and whether it does it coherently | SEC | 2b |
| OBS-02 | A failover reaches the operator with a timestamp and a measurable duration | OBS | 4 |
| OBS-03 | Grace entry and exit are both observable, and the window is measurable | OBS | 4 |
| OBS-04 | A mount that cannot succeed reaches the operator as a Kubernetes Event | OBS | 2b |
| OBS-06 | The control plane reports this volume's usage, it agrees with `df` in the pod, and both move with the workload | OBS | 7 |
| CHAOS-01 | SIGKILL the server process during an active write | CHAOS | 3 |
| CHAOS-02 | Delete the server pod during an active write, with a lock held across it | CHAOS | 3 |
| CHAOS-05 | Five failovers in a row, each recovering on its own and entering grace once | CHAOS | 4 |
| CHAOS-06 | Whole-file and disjoint byte-range locks across a failover, read from both ends | CHAOS | 4, 6 |
| CHAOS-07 | A second client attempting a new lock while the server is in grace | CHAOS | 4 |

Several are deliberately careful about what they blame. SEC-01 reports
**blocked**, with the export's own error in the message, when an ordinary uid
has nowhere to write on the share: that is deployment configuration, and filing
it against the storage system wastes a week. PROV-04 records what `df` reports
rather than asserting on it, because an export with no per-volume quota shows
every client the whole backing filesystem and cannot show a capacity change at
all; it still asserts that the control plane grew, that the data survived, and
that the client never restarted. SEC-02 records what the export does to root
unless `-root-squash` says what it was configured to do, and asserts coherence
between clients either way. DATA-02 asserts record integrity outright but
carries a caveat on the record count, because NFSv4.1 has no append operation
and an exact count under concurrent appends is an implementation property; the
failure message routes it to the boundary discussion rather than to the server
owner.

CHAOS-05 records the wall clock over its five cycles and does not assert on it:
on the default profile grace alone is ninety seconds, so five lawful recoveries
do not fit inside the ten minutes the plan names, and a case that asserted it
would fail with no defect present. Each cycle is asserted against the restart
SLO instead. CHAOS-06 takes locks in both shapes: whole-file, which a Linux
NFSv4 client sends to the server as a lock over the whole byte range, and
disjoint sub-file ranges from two clients on one file, which is the only thing a
byte range adds. It reads both ends of every lock afterwards, the server through
`F_GETLK` and the client through `/proc/locks`, because a client that believes
it holds a range the server has forgotten is visible only as the disagreement
between them. CHAOS-07 reports **blocked** when the server does not make grace
observable, because there is then no window to place a lock grant inside or
outside of, and OBS-03 is the case that fails for that missing signal.

The chaos cases report **blocked** when the cluster gives them nothing to
injure: no server pods discovered, a server whose process name lives in an image
entrypoint, or a server pod no controller owns, which would not come back. A
case that measures a recovery from a fault that was never injected passes for
the wrong reason, which is worse than a case that does not run.

## What is left to do (33 cases)

### Step 7: Observability (In progress — 3 cases remaining)
Design doc: [`docs/06-observability-design.md`](docs/06-observability-design.md).

| ID | Case | Expected |
|---|---|---|
| OBS-01 | Server unavailable (configuration half) | The deployment provides an availability signal representing NFS reachability: readiness probe targeting NFS service, with endpoints following. Behavioral half (signal moves when server stalls) lands in Step 10. |
| OBS-05 | Server memory approaching ceiling | Server container declares a memory limit, working set is readable against limit with timestamp, reading moves under load. |
| OBS-07 | Metrics survive server restart | Server metrics endpoint answers before and after restart, counters reset cleanly or persist. |

### Step 8: Close out Security and Identity (7 cases)

| ID | Case | Expected |
|---|---|---|
| SEC-03 | Pod `securityContext.fsGroup` interaction | Group access correct; no unexpected chown storm on large volumes |
| SEC-04 | Export access rules through Service path | Per-client export rules still apply when connections traverse the Kubernetes Service |
| SEC-05 | Denied client attempts mount | Rejected cleanly, not silently granted |
| SEC-06 | Dual-stack client identity | Client identity consistent across IPv4 and IPv6; skipped unless both IP families present |
| SEC-07 | Two pods with identical client identity after restart | No state collision; no lost locks |
| SEC-08 | Data path confidentiality | Finding (not pass/fail): records whether NFS traffic on shared pod network is cleartext or encrypted |
| SEC-09 | Server pod under platform admission policy | Server runs with necessary capability set (`CAP_DAC_READ_SEARCH` if file-handle-based) and no more |

### Step 9: Close out Scale and Performance (7 cases)

| ID | Case | Expected |
|---|---|---|
| SCALE-01 | Max RWX mounts per node, ramp to failure | Documented ceiling; failures are clean, not kernel hangs |
| SCALE-02 | 50 RWX volumes per cluster | All mountable; server RSS below ceiling |
| SCALE-03 | Pod fan-out 1, 10, 50, 100 on one volume | Throughput degradation curve recorded; no cliff or timeout |
| SCALE-04 | Small-file write storm (1M files, 1-64KiB) | Server RSS bounded, no OOMKill under small-file writes |
| SCALE-05 | Metadata-heavy: 100k stat/create/unlink per min | No server restart; latency recorded |
| SCALE-06 | Noisy neighbor across exports (shared server, fan-out > 1) | One export's load does not starve another beyond stated bound; skipped if fan-out = 1 |
| SCALE-07 | Sustained 8h throughput soak | No degradation trend beyond 10%; no leak. Also covers deferred DATA-14 soak. |

### Step 10: Close out Resiliency and Chaos (13 cases + OBS-01 behavioral half)

| ID | Case | Expected |
|---|---|---|
| CHAOS-03 | Hard-stop node hosting server | I/O resumes within node-loss SLO; backing volume re-attaches. Requires node power / out-of-service taint. |
| CHAOS-04 | Network partition server from clients, then heal | Clients block, then recover; no data loss |
| CHAOS-08 | Client-side: server restarts with new state | Client handles stale handles without permanent EIO; no manual remount required |
| CHAOS-09 | kubelet restart on client node with mounts active | Mounts survive; I/O resumes |
| CHAOS-10 | CSI node plugin pod eviction with mounts active | Existing mounts unaffected; new mounts queue and succeed after recovery |
| CHAOS-11 | CNI restart on client node | I/O blocks then resumes; no unmount |
| CHAOS-12 | NetworkPolicy applied blocking data path, then removed | Clients block, then recover cleanly |
| CHAOS-13 | Backing block volume disconnect during write | Errors surface as retryable; no silent corruption |
| CHAOS-14 | Colocation deadlock under memory pressure | No reclaim deadlock in hyperconverged CNode/DNode topology |
| CHAOS-15 | Client node OOM with dirty pages on NFS mount | Bounded failure; no node-level hang |
| CHAOS-16 | 24h chaos soak: randomized kills, partitions, evictions | Zero data corruption; zero unrecovered mounts; zero core dumps |
| CHAOS-17 | Recovery state store lost or corrupted, then restart | Bounded, honest failure: reclaim fails, locks lost, but no data corruption or permanent client hang |
| CHAOS-18 | Delegation held by client A, client B conflicting open | Delegation recalled and returned within timeout; B proceeds; skipped if delegations disabled |
| OBS-01 | Server unavailable (behavioral half) | Availability signal moves when running server stops answering; requires fault that stalls without killing container |

### Step 11: Version Skew (3 cases, conditional)
Enabled only when preflight determines server and CSI driver are independently versioned.

| ID | Case | Expected |
|---|---|---|
| SKEW-01 | CSI driver N with server N-1 | Provision, mount, I/O all work, or fail with explicit compatibility error |
| SKEW-02 | CSI driver N-1 with server N | As above |
| SKEW-03 | Server restarted into different minor version with clients mounted | Clients recover; state reclaimed or cleanly re-established |

### Deferred cases

| ID | Case | Reason |
|---|---|---|
| DATA-14 | Mixed 70/30 read/write, 4KiB to 1GiB files, 20 pods, 1h | Deferred to SCALE-07 soak: fits no category budget, requires external fio image and ~80GiB storage; test plan Section 3.2 |

## Why this order

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

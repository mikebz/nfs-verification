# 03: Chaos operations and the failover measurement path

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-12
Status: shipped, delivery step 3 ([PR #4](https://github.com/mikebz/nfs-verification/pull/4))
Serves: CHAOS-01, CHAOS-02. Requirements in [`01-test-plan.md`](01-test-plan.md)
Section 3.3, targets in Section 3.8, weighting in Section 2.4.

This document was written after the implementation rather than before it, which
is the wrong order and is why [`../AGENTS.md`](../AGENTS.md) now requires the
doc first. It records what was decided, not what would be decided.

---

## 1. Why this phase exists

The NFS server is the singleton in the data path (test plan Section 2.1): every
RWX client serializes through one process, so what a client observes when that
process dies is the heaviest-weighted question in the plan. Before this step the
suite could assert steady state and nothing else. It had no way to injure the
system and no way to measure what happened next.

Done means three numbers a client can produce: how long until the next write
committed, how many I/O errors were seen, and which acknowledged writes survived.

## 2. What shipped

- `pkg/chaos`: two fault operations, each recording itself on the fixture's
  fault timeline, each refusing a target it cannot identify.
  - `KillServerProcess` signals the server process on its node through the
    privileged node agent. It lists matches first and errors when nothing
    matched.
  - `DeleteServerPod` deletes the server pod gracefully, and refuses a pod no
    controller owns, because that pod would not come back.
  - `ServerTarget` resolves the server pod live at case time, never from the
    cached preflight record.
- The measurement path: a workload in a client pod writing one 4KiB record per
  second with `conv=fsync`, logging `OK|ERR <index> <epoch>` on the pod's own
  filesystem, never on the share.
- CHAOS-01 (SIGKILL the server process under active write) and CHAOS-02 (delete
  the server pod under active write, with a lock held across the failover).
- The category split in `make`: chaos cases carry a name prefix, the fast path
  carries no fault injection.

The same workload log yields all three assertions, which is the point: a
separate poller, error probe and durability check could disagree with each
other, and one log cannot.

## 3. What these two cases assert

The general conventions this phase settled, and which now hold for every case in
every section, are in [`01-test-plan.md`](01-test-plan.md) Section 4.1: fault
operations in their own package, live target resolution, a fault that was not
injected is never measured, one clock per measurement, waiting past the target,
and bounds from `pkg/slo`. What is specific to CHAOS-01 and CHAOS-02:

| # | Assertion | Where it comes from, and how it is checked |
|---|---|---|
| 1 | Recovery is time to first successful client I/O, never time to pod `Ready` | Ready is not serving, and that number would flatter every result. It comes from the workload log; pod readiness is a diagnostic only |
| 2 | A logged success means the server committed the write | RFC 8881 Section 18.3: after `COMMIT` is acknowledged the data is on stable storage. The workload fsyncs before logging, and a unit test against a local directory asserts every logged success is a full-size file |
| 3 | I/O errors across a failover are zero | A protocol statement, not a tolerance: a `hard` mount blocks and retries rather than returning an error ([`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html)). Asserted separately from timing, and an unreadable log line fails the case rather than counting as a success |
| 4 | Every write acknowledged before the fault is readable after it | Swept from a pod on another node, so the read crosses the server rather than the writer's own page cache |
| 5 | A signal is never sent to a pattern that could match an unrelated process | Killing everything matching `sh` on a node is a worse outage than the one being measured. Unit tested both ways: shells and wrappers refused whether derived or passed by flag, real server names accepted |
| 6 | A server pod no controller owns is never deleted | It would not come back, and the case would measure a permanent outage against a recovery target |
| 7 | The workload never reports its progress through the filesystem under test | A stalled mount must not be able to stall the harness reading its own progress. Log and stop file are on the pod's own filesystem, stated in the script itself |
| 8 | No case asserts how failover happens | The HA mechanism is a black box (test plan Section 2.3). Every assertion reads a client, a claim or a Kubernetes object |

## 4. What the workload produces

The shapes are in the code and are not repeated here:
`pkg/framework/load.go` and `pkg/framework/scripts/write-load.sh` for the
workload and its log, `pkg/framework/sweep.go` for the record sweep,
`pkg/chaos/chaos.go` for the fault timeline.

What matters about them, and what a later change must not break:

- The log line's time is when the attempt **finished**, so the first success
  after a fault is the moment the outage ended.
- Resolution is one second, against targets of 60 seconds and up.
- A line the harness cannot parse is kept and fails the case. Reporting zero
  errors from a partly unreadable log is worse than reporting the problem.
- The set a fault may not lose is every index logged as committed at or before
  the fault time, and nothing weaker.
- The fault timeline is triage only. Nothing asserts on it.

The lock probe in step 4 reuses this log format unchanged, so one parser serves
both. Moving the log onto the share would break assertion 7 above, which is why
it is stated as an assertion rather than left to habit.

## 5. Decisions

**The process name comes from the flag, else the container command, and both are
checked.** The container command is the only place a cluster states what the
server runs as. A shell wrapper or an image entrypoint states nothing, and the
case reports blocked rather than guessing. The reject list of generic names
applies to operator input too: killing everything matching `sh` on a node takes
the node out, which is a worse outage than the one being measured.

**The pod delete is graceful.** The abrupt path is already CHAOS-01's signal, and
a force delete removes the pod from the API before the server stops, which
starts the measurement at the wrong moment.

**One write per second.** The targets are 60 to 120 seconds at one second of
resolution. A tighter loop fills the share without sharpening anything.

**CHAOS-02 asserts lock exclusivity after the failover, not the reclaim
mechanism.** A lock is taken before the fault and never released, so grace has
outstanding state to reclaim and the case cannot pass vacuously. Whether the
state survived is measured, never inferred from the recovery backend preflight
recorded.

Alternatives rejected, each for one reason:

| Option | Why not |
|---|---|
| A per-platform chaos interface now | Both operations here are Kubernetes operations or a node-agent signal, and neither differs per platform. The interface arrives with node power, which does |
| Measure recovery by polling from the harness | Starts the clock when the harness notices, not when the fault landed, and the client is where the outage is felt |
| A workload without fsync | A success would mean the client accepted the write, which says nothing about durability |
| Force delete the server pod | The measurement would start before the outage does |

## 6. Configuration

Flags only, through the `make` targets. Server namespace and selector are
discovered, with a recorded note when discovery had to guess; the server process
is derived from the container command; lease and grace are discovered or
preflight fails. Bad config fails loud: lease and grace outside the two profiles
fail preflight rather than measuring against an unknown base.

Not configurable, because each is a property of the measurement rather than of a
deployment: the write rate, the record size, the SLO targets, and the reject list
of process names.

## 7. What changed after this was written

- **Step numbering.** This doc originally called itself "phase 3" and later docs
  referred to it as "phase 2" after its file number. Both are gone: this is
  delivery step 3, designed in doc 03.
- **The durability check was upgraded.** Step 6 replaced the existence check
  these two cases used with a content-verifying sweep, so CHAOS-01 and CHAOS-02
  now assert bytes rather than presence. See
  [`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md).
- **Grace is now observable.** This phase could not tell grace from a wedged
  server. Step 4 added the observer; see
  [`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md).
- **Node and network faults moved.** They were "a later phase" here and are now
  step 10, after PROV, DATA, OBS, SEC and SCALE are closed out.

- **The conventions moved out.** What this phase settled about faults in
  general (their package, live targets, one clock, waiting past the target, a
  fault that was not injected is never measured) is in the test plan's Section
  4.1, where every later phase inherits it rather than rediscovering it here.

Open items this phase left, still open: the process name is not discoverable on
every server, and server discovery without a selector remains a heuristic whose
exclusion of the suite's own pods is unit tested, because getting it wrong points
the chaos cases at the harness.

## 8. Sources

- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 8.4.2 for what
  a server does on restart, and Section 18.3 for what `COMMIT` guarantees.
- [`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) for `hard` mount
  retry behaviour, which is assertion 3.
- Kubernetes [pod lifecycle and
  eviction](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/)
  for what a graceful delete does, and the [API
  reference](https://kubernetes.io/docs/reference/kubernetes-api/) for owner
  references, which is how assertion 6 is enforced.

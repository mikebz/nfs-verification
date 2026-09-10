# 02: Chaos operations and the failover measurement path

Author: Claude Code
Created: 2026-09-10
Updated: 2026-09-10

Phase boundary: the first fault injection in the suite, and the measurement it
feeds. Ends when CHAOS-01 and CHAOS-02 run against a real cluster and report a
recovery time, an error count and a durability result. Cadence: one pull
request for the phase, following the delivery order in
[`plan.md`](plan.md) step 3.

Written after the implementation, not before. The repository rule in
[`AGENTS.md`](../AGENTS.md) is that a change of this size lands as a design doc
first, approved before implementation code is written. That did not happen, and
the rule was added to `AGENTS.md` while the code was being written. This
document is the record the rule asks for, produced late, and it says what was
decided rather than what will be decided.

---

## 1. Problem and outcomes

The server is the singleton in the data path. Every RWX client serializes
through one process, so the plan's heaviest weight is on what a client observes
when that process dies. Until this phase the suite could assert steady-state
behavior and nothing else: it had no way to injure the system and no way to
measure what happened next.

This phase adds the two operations that injure a server, and the measurement
that turns "it came back" into a number that can be held against an SLO.

Observable outcomes that define done:

- A case can kill the server process on its node, or delete the server pod, and
  both operations record what they did.
- A case can state how long the client took to commit its next write after a
  fault, in seconds, against a target read from `pkg/slo`.
- A case can state how many I/O errors the client saw, and which writes the
  server had acknowledged before the fault are still there afterwards.
- A cluster that cannot be injured reports blocked, and says why.

Requirements: Section 3.3 of [`01-test-plan.md`](01-test-plan.md) for the cases,
Section 3.8 for the SLO table and the lease and grace profiles, Section 2.4 for
why this vector carries the heaviest weight.

Cases served by this phase: **CHAOS-01** (SIGKILL the server process during an
active write) and **CHAOS-02** (delete the server pod during an active write,
plus locks reclaimed). The operations and the measurement path are shared with
CHAOS-03 through CHAOS-13, which arrive in later phases.

---

## 2. Human-readable rules

Each line is testable, and a reviewer can accept or reject the phase from this
section without reading code.

1. A fault that was not injected is never measured. If the operation cannot
   identify its target or cannot act, the case reports blocked and no recovery
   number is produced.
2. A signal is never sent to a pattern that could match a process unrelated to
   the NFS server. This holds whether the pattern was derived or passed by an
   operator.
3. A server pod that no controller owns is never deleted, because it would not
   come back and the case would measure a permanent outage against a recovery
   target.
4. Recovery is measured as time to first successful I/O from a client, never as
   the time a server pod took to report Ready.
5. Both ends of a recovery measurement come from the same clock.
6. A logged success means the server committed the write, not that the client
   accepted it.
7. Across a failover on a hard mount, the client I/O error count is zero. An
   error is a protocol violation, not a slow recovery, and is reported
   separately from timing.
8. Every write the server acknowledged before a fault is readable after it,
   verified from a client that did not write it.
9. A timing bound is never a literal in a case. It comes from `pkg/slo` against
   the profile preflight pinned.
10. The workload never reports its own progress through the filesystem under
    test.
11. No case asserts how failover happens. The mechanism is a black box; only
    what a client can observe is asserted.
12. Chaos cases never run in the fast gate.

---

## 3. Scope

**In scope for this phase**

- Two fault operations: signal the server process on its node, and delete the
  server pod.
- Target resolution: find the server pod live, at case time, and name the
  process to signal.
- A workload that produces committed writes at a known rate and records the
  outcome and time of every attempt.
- Parsing that workload into the three assertions: recovery, errors, durability.
- The gate split that keeps chaos out of the fast path.
- CHAOS-01 and CHAOS-02.

**Out of scope (explicit non-goals)**

- Node power operations. CHAOS-03 needs a platform implementation per
  environment, and the plan's floor note makes it conditional on two cluster
  settings that preflight already records.
- Network faults. CHAOS-04, CHAOS-11 and CHAOS-12 need NetworkPolicy or CNI
  interference.
- Grace period observation. CHAOS-05 through CHAOS-07 and OBS-02 and OBS-03 need
  grace entry and exit to be observable, which is a later phase.
- Any assertion about which mechanism performed the failover.
- A general chaos framework. Two operations, called by two cases.

**Depends on**

- The node agent (privileged DaemonSet, host namespaces) for signalling a
  process. Preflight already fails without it.
- Server pod discovery, which exists and is a heuristic when no selector is
  passed.
- Lease and grace pinned to a known profile. Preflight fails otherwise, because
  grace is the dominant term in every recovery number.
- `pkg/slo`, which holds the targets.

---

## 4. Data contract

The phase produces two artifacts a case reads back, plus one it writes for
triage only.

### 4.1 The workload log

Produced by the workload inside a client pod, one line per write attempt,
append only.

| Field | Type | Meaning |
|---|---|---|
| outcome | enum, `OK` or `ERR` | whether the write committed |
| index | integer, from 1, monotonic | which attempt |
| at | integer, Unix seconds | when the attempt finished |

Natural identity: the index, within one workload run. The log is not shared
between workloads: each carries an id that keeps two cases apart.

Correctness-affecting semantics:

- `OK` means the write was committed by the server, not accepted by the client.
  Without that distinction a logged success would say nothing about post-COMMIT
  durability, which is the guarantee the durability rule rests on.
- `at` is the time the attempt **finished**, so the first `OK` after a fault is
  the moment the outage ended.
- `at` is read from the client pod's clock. The fault time is read from the same
  pod's clock immediately before the fault, so a recovery number never depends
  on a workstation and a node agreeing.
- Resolution is one second. Every target this feeds is 60 seconds or more.
- A line the harness cannot parse is kept, not dropped. An unreadable log means
  the error count cannot be trusted, and reporting zero errors from a log that
  was partly unreadable is worse than reporting the problem.

Ownership: the workload writes it, the case reads it. It lives on the pod's own
filesystem, never on the share, so that a stalled mount cannot stall the
harness reading its own progress. It is not the system of record for anything
beyond one case.

### 4.2 The records on the share

| Field | Type | Meaning |
|---|---|---|
| name | `rec-<index>` | matches the workload log index |
| size | 4096 bytes | a short write is a failed write |

A record that is absent or zero length counts as lost. The set a fault may not
lose is every index the log shows as `OK` at or before the fault time.

### 4.3 The fault timeline

One entry per injected fault: when, what action, which target, and detail
naming what was matched or owned. Triage only. It lands in the artifact bundle
and nothing asserts on it.

### 4.4 Evolution

Likely to be added next: a fault kind dimension, once node power and network
faults arrive and one case injects more than one kind. A per-attempt latency,
if a later phase measures degradation rather than outage. Both are new fields
on existing lines and are backward compatible with the parsing here.

What would force a breaking change: making the log a shared record across
workloads, or moving it onto the share. Neither is planned, and rule 10 exists
to stop the second.

The write strategy survives partial writes by construction: the log is append
only, one line per attempt, and a truncated final line is parsed as unreadable
rather than as a success.

---

## 5. Design and decisions

### 5.1 Where the operations live

[decided: a package holding the fault operations, imported by the cases]

The operations sit above the harness and below the cases. They take the case
fixture, so every one records its own fault timeline entry rather than trusting
a case to remember.

Alt: methods on the fixture. Rejected because the fixture is already the place
everything lands, and faults are the one thing a reader should be able to
enumerate in one file.

### 5.2 Target resolution

[decided: resolve the server pod live, at case time, never from the environment record]

Preflight is cached per cluster and names the pod that was there when it ran.
Chaos cases move pods around, including pods this suite deleted itself. A stale
target either fails to act or acts on the wrong thing.

The target carries the pod, its node, its owning controller if it has one, and
its container commands, which is where a process name comes from.

### 5.3 Naming the process to signal

[decided: the flag wins, otherwise the base name of the container command, and both are checked]

The only place a cluster states what a server runs as is the container command.
A server started through a shell wrapper, or one whose command lives in the
image entrypoint, does not state it at all, and the case reports blocked rather
than guessing.

The check is a reject list of names too generic to signal on, plus a minimum
length. It applies to a name derived from a command **and** to one passed by an
operator. An operator passing a generic name means one thing to the person
typing it and another to the node, and no NFS server is refused by the check.

[decided: look before signalling, and fail when nothing matches]

Listing the matching processes first is what makes rule 1 hold. A kill that
matched nothing must not read as a fault that was injected.

### 5.4 Deleting the server pod

[decided: refuse a pod no controller owns; delete gracefully]

Graceful, because the abrupt path is the signal in CHAOS-01, and a force delete
removes the pod from the API before the server stops, which starts the recovery
measurement at the wrong moment.

### 5.5 The measurement

[decided: one workload log yields all three assertions]

The alternative is three mechanisms: a poller for recovery, an error probe, and
a durability check written separately. One workload that writes, commits and
records gives all three from one source, and they cannot disagree with each
other.

Sequence, for both cases:

1. Resolve a target, or report blocked.
2. Create a client pod on one node and a verifier pod on another.
3. Start the workload, and wait until it has committed several writes, so the
   fault interrupts something rather than landing on a cold start.
4. Read the fault reference from the writer pod's clock.
5. Inject the fault, or report blocked.
6. Wait for the first committed write after the fault reference, waiting well
   past the target so an overrun is reported as a number rather than as a
   timeout.
7. Stop the workload and read the log: recovery against the target, error count
   against zero, and the committed set.
8. Verify the committed set from the verifier pod, so the read crosses the
   server rather than the writer's own cache.

[decided: one write per second]

The measurements are a 60 to 120 second bound at one second of resolution. A
tighter loop fills the share with records without sharpening any assertion.

[decided: wait past the target rather than up to it]

A case that gives up at the SLO reports "timed out" where it could report how
long recovery actually took. The second is what a defect report needs.

### 5.6 What CHAOS-02 adds

[decided: assert lock exclusivity after the failover, not the reclaim mechanism]

A lock is taken before the fault and never released, so grace has outstanding
state to reclaim and the case cannot pass vacuously. Afterwards the assertion
is that a client on another node is still refused. Whether the state survived is
measured, never inferred from the recovery backend preflight recorded.

### 5.7 Gating

[decided: the category lives in the test name and a `go test` pattern]

Chaos case names share a prefix; the fast gate excludes it and the chaos gate
selects it. No harness machinery until there is more than one thing to select.

---

## 6. Delivery phases

**MVP**: this phase. The two operations, the measurement path, CHAOS-01 and
CHAOS-02, and the gate split. It merges as one pull request.

Validated outside a test harness by running the chaos gate against a real GKE
cluster with a server workload owned by a controller, and reading three numbers
out of it: seconds to first committed write, error count, and lost committed
writes. A run that reports blocked is not a validation, and the reason it
reported blocked is the finding.

Later phases, intent only:

- Node and infrastructure faults, including the platform implementations for
  taking a node out.
- Grace entry and exit made observable, which is what makes repeated failover
  diagnosable.
- Network path faults.
- The soak cases, which reuse this workload for a much longer window.

These sharpen once the MVP has run against a real cluster. The first real run
of this phase is expected to change the shape of at least one of them.

---

## 7. Configuration

Source: command line flags, passed through the make targets. No config file,
because every value here is either discovered or is a single string.

| Flag | Default | Needed for |
|---|---|---|
| server namespace, server selector | discovered | naming the server pods; the fallback is a heuristic and preflight records that it guessed |
| server process | derived from the container command | naming the process to signal, when the image entrypoint hides it |
| lease seconds, grace seconds | discovered, or preflight fails | every timing target |
| profile | either accepted | pinning the run to one lease and grace profile |

Validation and behavior on bad config: fail loud. Lease and grace outside the
two profiles fail preflight rather than measuring against an unknown base. A
process name too generic to signal on is refused rather than used. A server
pod that cannot be found reports blocked rather than skipping quietly.

Intentionally not configurable: the write rate, the record size, the SLO
targets, and the reject list of process names. Each is a property of the
measurement rather than of a deployment, and a run that changed one would not
be comparable with any other.

Credentials: a kubeconfig, from the environment. Nothing is committed.

---

## 8. Deployment decisions

Runtime unit: a test binary run by an operator, on a workstation, outside the
cluster. Not a job, not a service, nothing deployed ahead of time. The harness
authenticates with a kubeconfig and creates what it needs.

Lifecycle: each case creates its objects under a label selector carrying the
run and the case, and teardown deletes exactly that selector, pods first and
gracefully, then claims. The workload is stopped by removing a file it polls,
and the case registers that stop before it can fail, so a failing case does not
leave a workload running.

Idempotency: no. Each run carries its own id, and objects from two runs cannot
collide by name.

Access scope: the suite needs to create and delete pods and claims in one
namespace, run commands in pods, read nodes, and delete a pod in the server's
namespace. The node agent is privileged, which is the only elevated component
and is a preflight failure when it cannot be scheduled.

---

## 9. Observability

This phase asserts on what a client observes. What it reports is what a person
needs to route a failure.

Per case, logged as it runs:

- The target: server pod, its node, and whether a controller owns it.
- The fault: what was matched, how many processes were signalled, and what they
  were.
- Recovery: seconds to first committed write, the target it was measured
  against, and the profile name.
- The longest gap between attempts, which is what a blocked client looks like
  from outside: no errors, no progress.
- The number of committed writes that survived.

In the artifact bundle on failure: the environment record, client and server
pod logs including previous-container logs, Kubernetes events, `/proc/mounts`
and dmesg from every involved node, and the fault timeline.

Not in this phase: metrics from the server, or any assertion that an alert
fired. Those are the OBS cases.

---

## 10. Alternatives considered

| Option | What it is | Why not chosen |
|---|---|---|
| A chaos operation interface with per-platform implementations | An interface now, with implementations added per environment | Both operations here are Kubernetes operations or a signal through the node agent, and neither differs per platform. The interface arrives with node power, which genuinely does. |
| Measure recovery by polling from the harness | The harness retries an I/O until it succeeds | Starts the clock when the harness notices, not when the fault landed, and the client is where the outage is actually felt. |
| Measure recovery from the server pod becoming Ready | Watch the pod, take the time it reports Ready | Ready is not serving. Rule 4 exists because that number would flatter every result. |
| Take the fault time from the workstation clock | Timestamp the fault locally | Compares two clocks. Skew of a few seconds is invisible and would silently move every measurement. |
| A workload without fsync | Write and count | A success would mean the client accepted the write, which says nothing about durability across a failover. |
| Force delete the server pod | Delete with grace period zero | Removes the pod from the API before the server stops, so the measurement starts before the outage does. |
| Skip a case when the fault cannot be injected | Treat an unidentifiable target as not applicable | Blocked and skipped read the same in a test runner but not to a person. The reason a cluster could not be injured is itself the finding. |

---

## 11. Open questions and risks

| Item | The constraint that decides it |
|---|---|
| The process name is not discoverable on every server. A server whose command lives in the image entrypoint needs the flag. | Whether the flag proves noisy across the deployments this suite meets. If it does, the alternative is reading the container's own process table through the node agent and matching it against the pod's cgroup, which is more machinery than the flag is worth today. |
| Server discovery is a heuristic without a selector. | Whether a real deployment is ever matched wrongly. The heuristic excludes anything the suite created, and that exclusion is unit tested, because getting it wrong points the chaos cases at the harness. |
| A shared server changes what these cases prove. With fan-out greater than one, killing one server affects one export set, not the cluster. | Preflight records fan-out. Whether a case must assert a blast radius rather than a recovery is a question for the first run against a shared server. |
| CHAOS-02 asserts lock exclusivity after failover, which may fail on a deployment with no persistent recovery state. | That is a real result, not a harness defect. The risk is that it is filed as one. The failure message names the recovery backend preflight recorded, which is where triage starts. |
| Neither case has been run against a real cluster. | The first run. Expect it to change the timing of step 3 in the sequence above, and possibly the write rate. |

---

## 12. Verification

One check per rule in section 2.

| Rule | Check |
|---|---|
| 1. A fault not injected is never measured | A target that cannot be resolved, a process name that cannot be derived, and a kill that matched nothing each produce an error, and the case reports blocked. Unit tested for the name; the others are error paths in the operations. |
| 2. Never signal a generic pattern | Unit tested both ways: shells and wrappers are refused whether derived or passed by flag; real server names are accepted. |
| 3. Never delete an unowned pod | The operation refuses a target with no controller, and the case reports blocked. |
| 4. Recovery is time to first successful I/O | The number comes from the workload log, never from pod status. Pod readiness is checked afterwards and only as a diagnostic. |
| 5. One clock | The fault reference is read from the writer pod immediately before the fault, and the recovery line comes from the same pod. |
| 6. A logged success is a committed write | The workload commits each record before logging it. Unit tested by running the workload against a local directory and asserting every logged success is a full-size file on disk. |
| 7. Zero I/O errors across a failover | Asserted separately from timing, against the protocol constant. An unreadable log line fails the case rather than being counted as a success. |
| 8. Committed writes survive | The committed set is swept from the verifier pod. An absent or zero-length record counts as lost. Unit tested for the sweep. |
| 9. No timing literals | Targets come from `pkg/slo`, which is unit tested including the profile where a 60 second target under a 90 second grace period is unachievable. |
| 10. The workload never reports through the share | The log and the stop file are on the pod's own filesystem. Stated in the workload script itself. |
| 11. No assertion on the failover mechanism | Reviewable from the cases: every assertion reads a client, a claim or a Kubernetes object, and none reads a server's internals. |
| 12. Chaos never runs in the fast gate | The fast gate excludes the chaos name prefix, and the chaos gate selects it. Visible in the make targets. |

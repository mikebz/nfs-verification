# 03: Grace, lock reclaim, and making a failover observable

Author: Claude Code
Created: 2026-09-10
Updated: 2026-09-10

Phase boundary: the first cases that assert on what happens *during* a
failover rather than on the fact that one ended. Ends when CHAOS-05, CHAOS-06,
CHAOS-07, OBS-02 and OBS-03 run against a real cluster and report a grace
window, a reclaim result and a repeated-failover result. Cadence: one pull
request for the phase, following [`plan.md`](plan.md) step 4, on top of step 3.

---

## 1. Problem and outcomes

Step 3 can injure the server and time the outage. It cannot see grace. Grace is
the interval after a restart in which the server accepts reclaims of state that
existed before the crash and refuses everything new, and it is the dominant
term in every recovery number the suite reports. A suite that cannot see it
reports a slow client and cannot say whether the server was enforcing a
protocol guarantee or had wedged.

The plan says so directly: OBS-03 exists to make CHAOS-05 diagnosable, and the
triage runbook lists "is it grace?" as the third question and calls a grace
re-entry loop the single most common false diagnosis in this architecture.

Observable outcomes that define done:

- A case can state when grace was entered and when it ended, with timestamps,
  or say that this server makes neither observable.
- A case can state that a lock held before a failover was still held after it,
  for every lock held, not for one.
- A case can state that a new lock was never granted while grace was in force.
- A case can put a server through five failovers and say whether each one
  recovered and whether grace was entered once per failover or more.
- A failover is reported as an operator-visible event with a timestamp and a
  measurable duration, or the case says that nothing outside the client noticed.

Requirements: Section 3.3 of [`01-test-plan.md`](01-test-plan.md) for CHAOS-05
through CHAOS-07, Section 3.5 for OBS-02 and OBS-03, Section 3.8 for the grace
exit bound and the lock rows of the SLO table, Section 4.3 for why grace is a
triage question rather than a curiosity.

Cases served by this phase: **CHAOS-05** (repeated failover), **CHAOS-06**
(failover with locks held), **CHAOS-07** (a new lock attempted during grace),
**OBS-02** (a failover is observable), **OBS-03** (grace entry and exit are
observable). The grace observer and the lock probe are shared with CHAOS-17 and
SEC-07, which arrive later.

---

## 2. Human-readable rules

Each line is testable, and a reviewer can accept or reject the phase from this
section without reading code.

1. Grace is read from what the server publishes, never inferred from the fact
   that a client stalled. A stalled client is the symptom this phase exists to
   tell apart from grace.
2. A case that needs a grace window and cannot observe one reports blocked. It
   never falls back to the configured grace value anchored at the fault: grace
   begins when the server restarts, which is later than the fault by an unknown
   amount, so a derived window ends after the real one and would report a
   lawful lock grant as a violation.
3. A timestamp from the Kubernetes API and a timestamp from inside a pod are
   two clocks. Where the two must be compared, the window is narrowed by a
   guard band, and only an event unambiguously inside the narrowed window is
   reported as a violation.
4. Lock reclaim is asserted from both ends: the holder still holds, and a
   different client is still refused. The holder alone proves nothing, because
   a client that has lost its lock does not find out until it uses it.
5. Reclaim is asserted for every lock the case took, not for one. The SLO row
   is 100%, and a case that checks one lock cannot report a fraction.
6. A new lock granted during grace is a failure whether the server granted it
   deliberately or lost the state that would have refused it. Neither reading
   is acceptable, and the case does not have to tell them apart to fail.
7. A probe that is refused because the server is down is not evidence that
   grace was enforced. The case establishes grace was in force from the
   server's own signal, then asks what the probe saw inside that window.
8. Repeated failover is measured per cycle. A total that fits inside a wall
   clock proves nothing if one cycle inside it took four minutes.
9. Grace entered more than once per failover is reported as a re-entry loop,
   which is a finding about the server and not about the client that stalled.
10. A case that injures the server is named for the chaos gate, whatever
    section of the plan it comes from. The fast gate holds no fault injection.
11. No case asserts how grace is implemented, only that its effect is
    observable and its rules hold at the client.
12. Every rule from phase 2 of [`02-chaos-operations-design.md`](02-chaos-operations-design.md)
    still holds. This phase adds to that contract, it does not amend it.

---

## 3. Scope

**In scope for this phase**

- A grace observer: read the server's log stream through the Kubernetes API,
  with timestamps, and classify the lines that announce grace entry and exit.
- A lock probe: a second client attempting a new lock at a known rate, logging
  the outcome and time of every attempt on its own filesystem.
- Multi-lock reclaim: several locks held across a failover, asserted from both
  ends afterwards.
- Repeated failover: a cycle that injects, waits for recovery and repeats, with
  per-cycle results.
- The five cases, and the gate naming that keeps them out of the fast path.

**Out of scope (explicit non-goals)**

- Byte-range locks below whole-file granularity. Nothing on a busybox image can
  take one. See 5.5.
- Scraping server metrics. A metrics endpoint needs a port and a metric name
  that no Kubernetes object states, and inventing a convention for it would put
  a guess in the middle of an observability assertion. Open, in section 11.
- Asserting that an alert fired. That is OBS-01, which needs a monitoring stack
  this suite does not deploy.
- Any new fault. The two operations from phase 3 are the two this phase uses.
- Node and network faults, which stay in step 5.

**Depends on**

- The fault operations, the workload and the recovery measurement from phase 3.
- Lease and grace pinned to a profile. Preflight already fails otherwise, and
  the grace exit bound is two lease periods.
- `pkg/slo`, which holds the lock rows and the grace exit bound already.
- Server pod discovery, which is where the log stream is read from.

---

## 4. Data contract

### 4.1 The grace observation

Produced by reading the server pod's log stream and classifying its lines. One
entry per transition.

| Field | Type | Meaning |
|---|---|---|
| kind | enum, `enter` or `exit` | which transition the line announces |
| at | timestamp, from the log stream | when the server printed it |
| line | text | the line itself, kept for the failure message |

Correctness-affecting semantics:

- `at` is the timestamp the container runtime attached to the line, not a time
  parsed out of the server's own wording. Server log formats differ, runtime
  timestamps do not, and a suite that parsed each server's format would be
  asserting on the thing it is least able to keep working.
- That timestamp is the kubelet's clock on the server's node. It is not the
  clock a client pod stamps its probe with. Rule 3 governs every comparison
  between the two.
- Entry and exit are two separate observations, not one interval with a
  duration. An entry with no exit is the re-entry loop symptom and has to be
  representable.
- A line that mentions grace but matches neither classification is kept and
  reported. An unclassified line is a gap in the observer, and reporting "grace
  was never observed" while holding a line that says otherwise is worse than
  reporting the gap.
- The log stream a case reads is the one belonging to the server pod that is
  live when the case reads it. After a pod delete that is a new pod, which is
  the one that entered grace. Where the container restarted in place, the
  previous container's log holds the pre-fault half and is read as well.

Ownership: the observer produces it, the cases read it. It is not persisted
beyond the run; the underlying log lines land in the artifact bundle, which
already collects server logs including previous-container logs.

### 4.2 The lock probe log

Produced inside a client pod, one line per attempt, append only, on the pod's
own filesystem. It carries the same three fields as the workload log from phase
3, in the same order and the same format, so one parser serves both.

| Field | Type | Meaning |
|---|---|---|
| outcome | enum, `OK` or `ERR` | `OK` means the lock was granted, `ERR` that it was not |
| index | integer, from 1, monotonic | which attempt |
| at | integer, Unix seconds, pod clock | when the attempt finished |

Correctness-affecting semantics:

- `OK` means granted and then immediately released. The probe must not hold
  what it was granted: a probe that kept the lock would change what the next
  attempt means.
- An attempt that blocks is not an attempt that failed, and the two are
  distinguished only by the gap between consecutive lines. A Linux NFS client
  handling the server's grace error retries in the kernel rather than returning
  it, so the expected shape during grace is a long gap and then a grant, not a
  run of refusals. The assertion is about grants, which is why it survives
  either shape.
- Each attempt is bounded, so a client blocked in the kernel cannot stall the
  probe past the end of the case.
- Resolution is one second, as in phase 3, against windows of thirty seconds
  and up.

### 4.3 The per-cycle failover record

Produced by the repeated failover case, one entry per cycle.

| Field | Type | Meaning |
|---|---|---|
| cycle | integer, from 1 | which failover |
| fault at | timestamp, writer pod clock | when the fault was injected |
| recovered after | duration | to the first committed write after the fault |
| grace entries | integer | grace entry lines observed since the previous cycle |

Grace entries per cycle is the whole point of the record: more than one is the
re-entry loop, and the count only means anything if it is attributed to one
cycle rather than summed over the run.

### 4.4 Evolution

Likely to be added next: a sub-file range on the lock probe once `locktool`
arrives with DATA-06, which is a new field on the probe rather than a new log;
and a grace source dimension when metrics become readable, which is a new field
on the observation. Both are backward compatible.

What would force a breaking change: parsing the server's own timestamp out of
its log text instead of taking the runtime's. Nothing here plans to.

---

## 5. Design and decisions

### 5.1 Where grace is read from

[decided: the server's log stream, through the Kubernetes API, with runtime timestamps]

Three channels were available. The Kubernetes API is the only one that is
guaranteed present, timestamped by something other than the process under test,
and readable without knowing anything about the server's deployment beyond
which pods it runs.

Alt: the server's metrics endpoint. Rejected for this phase because nothing in
the Kubernetes API states which port serves metrics or what the metric is
called, so it would need two flags and a convention, and the convention would
be a guess sitting in the middle of an observability assertion. Open in 11.

Alt: infer grace from the client. Rejected by rule 1. Inferring grace from a
stalled client makes the case unable to tell grace from the failure mode that
mimics it, which is the one distinction the triage runbook says matters.

### 5.2 Classifying a line as entry or exit

[decided: a word-level rule over lines mentioning grace, plus a pair of flags for a server that words it differently]

The rule is: a line mentioning grace is an exit if it also carries a word that
negates or ends it, and an entry otherwise. That ordering matters, because the
common exit wordings are the entry wording with a negation in front, and a rule
that tested for entry first would classify every exit as an entry and report a
re-entry loop on a healthy server.

This is the one piece of this phase that is implementation-specific in
practice, even though it depends on no implementation's API. It is unit tested
against the wordings of several servers, and a pair of flags exists for a server
whose wording the rule does not cover. They are set together or not at all, and
they replace the rule rather than adding to it: an operator who states the
wording owns it, and a run that stated only the entry wording would observe
every failover entering grace and never leaving it, which is the re-entry loop
symptom these cases exist to report honestly. A server that says nothing at all fails
OBS-03, which is the finding rather than a harness gap: an operator on that
deployment cannot see grace either.

[decided: an unclassified line mentioning grace is reported, not dropped]

Same reasoning as the unparsed workload line in phase 3. A gap in the observer
that presents as "grace was never observed" would be diagnosed as a server
defect.

### 5.3 Comparing two clocks

[decided: narrow the window by a guard band and only fail on an unambiguous violation]

The grace window is stamped by the kubelet on the server's node. The probe is
stamped by the client pod's clock. There is no way to put both on one clock:
the two ends are on different nodes by construction, because the probe has to
run on a client and grace happens on the server.

The guard band lives in `pkg/slo` next to the other constants, not as a
literal in a case. It narrows the window at both ends, so a grant near a
boundary is reported as a note rather than a failure, and a grant in the middle
of grace fails. The direction is deliberate: this phase would rather miss a
marginal violation than file a lawful one.

Alt: measure the offset between the two clocks by reading both. Rejected as
more machinery than the assertion needs. The guard band is a constant a
reviewer can argue with; a measured offset is a number nobody checks.

### 5.4 What the lock probe asserts

[decided: assert on grants, never on refusals]

A refusal proves nothing on its own: during an outage every attempt fails for
the ordinary reason that the server is not there. The assertion that carries
the protocol guarantee is that no grant lands inside the observed grace window,
and that a grant does land after it. Rule 7 is this decision.

[decided: hold un-reclaimed state through the window]

Section 3.8 of the test plan says a server may lift grace early once it
concludes no further clients will reclaim, and that CHAOS-07 must therefore
hold a client with outstanding state so grace is observably enforced when the
probe runs. The holder from the reclaim case serves that purpose: it holds
across the failover and is not released until the case ends.

### 5.5 Whole-file locks, not byte ranges

[decided: land CHAOS-06 on whole-file locks now, and extend it when locktool arrives]

CHAOS-06 in the plan says byte-range locks. Nothing on a busybox image can take
one: `flock` is whole-file, and the tool that can is the static Go binary the
plan schedules with DATA-06 in step 7, which needs an image this repository
does not build yet.

What a whole-file lock does exercise is the same protocol path. A Linux NFSv4
client sends a whole-file lock to the server as a lock over the whole byte
range, and it is reclaimed by the same mechanism as any other. So reclaim,
exclusivity and the grace bar on new state are all asserted now. What is not
asserted now is the sub-file dimension: two clients holding disjoint ranges of
one file, and both reclaiming. That arrives with `locktool`.

The case comment says which half it covers, and step 7 in `plan.md` carries the
extension, so the gap is recorded in both places rather than in a commit
message.

Alt: bring `locktool` forward into this phase. Rejected: it needs a build and a
registry the repository does not have, and it would double the size of a phase
that already needs a design document.

### 5.6 Repeated failover

[decided: five cycles, each one injected only after the previous recovered, with per-cycle assertions]

Injecting on a fixed cadence regardless of recovery would be a different case:
it would measure what happens when failovers overlap, which is worth doing and
is not what the plan asks for here.

[decided: assert per cycle, record the wall clock, and do not assert on it]

The plan's phrase is five cycles inside ten minutes. On the default profile
grace alone is ninety seconds, so five lawful recoveries do not fit in ten
minutes and a case that asserted the wall clock would fail with no defect
present, which is exactly the trap Section 3.8 describes for the restart target.
The wall clock is recorded. Rule 8 is the assertion that replaces it.

[decided: the re-entry check is a diagnostic that degrades, not a requirement]

Where grace is observable, more than one entry per cycle fails the case. Where
it is not, the case still asserts five recoveries and says the re-entry check
was unavailable, and OBS-03 is the case that fails for the missing signal. One
missing signal should produce one failure, not four.

### 5.7 What OBS-02 asserts

[decided: any timestamped channel an operator can reach counts, and the case records which one answered]

A failover that only the client noticed is invisible to whoever runs the
cluster. Two channels are read: the server's own log stream, and the Kubernetes
API, which timestamps the pod's disappearance and the new container's start.
Either satisfies the case; both are recorded.

The case fails on silence, and on a pair of timestamps that cannot be turned
into a duration. It does not fail when only Kubernetes answered, because that
is still an operator-visible event, but it says so: a restart count tells an
operator a pod restarted, not that NFS failed over, and the difference matters
when the server is one pod among many.

### 5.8 Gate naming

[decided: the chaos name prefix marks a case that injures the server, whatever section it comes from]

OBS-02 and OBS-03 inject a fault. Under the existing rule, which keys the gate
off the plan section, they would land in the fast path, and the fast path holds
no fault injection. The rule becomes: the prefix marks what the case does, not
where it sits in the plan, and the plan ID in the comment above each case
remains the only place the section is recorded.

Alt: add the nightly gate from Section 4.2 now. Rejected as machinery ahead of
need: it would take a pattern enumerating cases by section, which is the
harness machinery the plan says not to build until something needs it.

---

## 6. Delivery phases

**MVP**: this phase. The grace observer, the lock probe, the five cases, and
the gate naming. It merges as one pull request behind the design document.

Validated outside a test harness by running the chaos gate against a real
cluster and reading four things out of it: a grace entry and exit with
timestamps, a reclaim result for every lock taken, the outcome of a new lock
attempted inside the window, and five per-cycle recovery numbers. A run that
reports blocked is not a validation, and the reason it reported blocked is the
finding.

Later phases, intent only:

- `locktool` and the sub-file dimension of CHAOS-06, with DATA-06 in step 7.
- CHAOS-17, which loses the recovery state deliberately and needs exactly the
  observations this phase produces to tell a bounded failure from a silent one.
- SEC-07, two clients with the same identity after a restart, which is the
  reclaim path seen from the other side.
- Metrics as a second grace channel, if the first real run shows a deployment
  whose logs say nothing.

---

## 7. Configuration

Source: command line flags, passed through the make targets, as in phase 3.

| Flag | Default | Needed for |
|---|---|---|
| grace enter pattern, grace exit pattern | built-in wording rule | a server whose grace wording the rule does not cover; set together or not at all |
| everything from phase 3 | unchanged | the fault, the target and the profile |

Validation and behavior on bad config: fail loud. A pattern that does not
compile is a startup failure, not a case that quietly observes nothing, and so
is one of the pair without the other. A
server whose logs are unreadable, because the pod is gone and its predecessor's
logs went with it, is reported as such rather than as a server that never
entered grace.

Intentionally not configurable: the probe rate, the guard band, the number of
failover cycles, and the grace exit bound. Each is a property of the
measurement rather than of a deployment.

---

## 8. Deployment decisions

Unchanged from phase 3. Nothing new is deployed, no new privilege is needed:
the grace observer reads pod logs through the API, and the lock probe runs in a
client pod the case already creates.

One lifecycle addition: the probe and every lock holder are registered for stop
before the case can fail, as the workload already is, so a failing case does
not leave a lock held on the share by a pod that outlives it.

---

## 9. Observability

Per case, logged as it runs:

- The grace window: entry, exit, duration, and which channel reported it.
- Every unclassified line mentioning grace, so a gap in the observer is visible
  in the same output as the assertion it affected.
- Per lock: whether the holder still held it and whether the other client was
  still refused.
- Per cycle in the repeated case: fault time, recovery, and grace entries.
- For the observability cases: the duration each channel reported, next to the
  outage the client actually experienced.

In the artifact bundle on failure: everything phase 3 collects. Server logs are
already in it, which is where the grace lines are.

---

## 10. Alternatives considered

| Option | What it is | Why not chosen |
|---|---|---|
| Infer grace from the client's stall | Treat the outage window as the grace window | Cannot tell grace from the failure that mimics it, which is the distinction the triage runbook is built on. |
| Derive the window from the configured grace value | Anchor at the fault, add the profile's grace | Grace starts when the server restarts, not when the fault landed, so the derived window ends late and a lawful grant after real grace would fail the case. Rule 2. |
| Parse the server's own timestamp from its log text | Read the time the server printed | One format per implementation, each of which changes. The runtime's timestamp is on every line of every container. |
| Assert refusals during grace | Require the probe to be refused | A refusal is what an outage produces anyway. Only a grant carries the guarantee. |
| Hold the probe's lock once granted | Keep it, to show it was granted | Changes what every later attempt means, and leaves a lock held on the share. |
| One grace observation with a duration | Model the window as an interval | An entry with no exit is the re-entry symptom, and an interval type cannot represent it. |
| A nightly gate for the OBS cases | Add the Section 4.2 target now | Needs a pattern enumerating cases by plan section. The chaos prefix already marks the property that matters here, which is that the case injures the server. |
| Bring locktool forward | Build the byte-range tool in this phase | Needs an image build and a registry the repository does not have, and doubles the phase. |

---

## 11. Open questions and risks

| Item | The constraint that decides it |
|---|---|
| Metrics are the other channel the plan allows and this phase does not read. A server that reports grace only through metrics fails OBS-03 for a reason that is half harness gap. | Whether such a deployment shows up. If it does, the answer is a metrics URL flag and a metric name flag, and the failure message for OBS-03 should name them before that. |
| The wording rule is the implementation-specific part of an otherwise portable observer. | The first run against a server nobody here has read the logs of. The flag is the escape hatch; if it is needed on every deployment, the rule is wrong and the flag should become the documented path. |
| Whole-file locks stand in for byte ranges until step 7. | Whether reclaim behaves differently for a sub-file range on any server met in practice. There is no protocol reason it should; if it does, that is a finding. |
| A grant near a window boundary is reported as a note rather than a failure. | Whether real clusters put grants near boundaries at all. If they do, the guard band is hiding something and the offset should be measured instead. |
| Five failovers in a row may leave the cluster in a state the next case inherits, since the suite shares one namespace and one server. | Whether the repeated case can be run without disturbing what follows it. It waits for recovery after the last cycle; if that proves insufficient, the case owns waiting longer, not the cases after it. |
| None of the five has been run against a real cluster. | The first run. Expect it to change the wording rule and the probe's per-attempt bound. |

---

## 12. Verification

One check per rule in section 2.

| Rule | Check |
|---|---|
| 1. Grace is read from what the server publishes | The observer's only input is the log stream. No case computes a window from client behavior. |
| 2. No window, no measurement | CHAOS-07 reports blocked when the observer returns nothing, and OBS-03 is the case that fails for the missing signal. |
| 3. Two clocks are narrowed by a guard band | The guard band is a named constant in `pkg/slo`, applied at both ends. Unit tested on the window arithmetic, including a grant exactly on a boundary. |
| 4. Reclaim is asserted from both ends | Every reclaim assertion reads the holder's state and probes from a second client. Reviewable from the cases. |
| 5. Every lock, not one | The reclaim case takes locks on several files from two clients and reports a fraction against the SLO row. |
| 6. A grant during grace fails either way | The assertion is on the grant, and the failure message names both readings and the recovery backend preflight recorded. |
| 7. A refusal is not evidence | The window comes from the server's signal, and the probe is read inside it. A case with no window does not assert. |
| 8. Per cycle, not per run | The record is per cycle, and each cycle's recovery is asserted against the target. The wall clock is logged only. |
| 9. Re-entry is a finding about the server | The count is per cycle and its failure message says grace re-entry, not client stall. |
| 10. Fault injection is out of the fast gate | The fast gate excludes the chaos name prefix, and all five cases carry it. Visible in the make targets. |
| 11. No assertion on how grace is implemented | Every assertion reads a log line's existence, a lock outcome or a Kubernetes object. None reads server internals. |
| 12. Phase 3's rules still hold | The recovery path is phase 3's, unchanged, and the repeated case asserts against it five times. |

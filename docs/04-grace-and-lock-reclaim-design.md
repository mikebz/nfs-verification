# 04: Grace, lock reclaim, and making a failover observable

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-12
Status: shipped, delivery step 4 ([PR #8](https://github.com/mikebz/nfs-verification/pull/8))
Serves: CHAOS-05, CHAOS-06, CHAOS-07, OBS-02, OBS-03. Requirements in
[`01-test-plan.md`](01-test-plan.md) Sections 3.3, 3.5 and 3.8.
Builds on [`03-chaos-operations-design.md`](03-chaos-operations-design.md), whose
rules all still hold.

---

## 1. Why this phase exists

Step 3 could injure the server and time the outage. It could not see grace.

Grace is the interval after a restart in which the server accepts reclaims of
state that existed before the crash and refuses everything new (RFC 8881 Section
8.4.2). It is the dominant term in every recovery number this suite reports, and
a grace re-entry loop presents as a hung client in front of a healthy server,
which the triage runbook in test plan Section 4.3 calls the most common wrong
diagnosis in this architecture. A suite that cannot see grace reports a slow
client and cannot say whether the server was enforcing a protocol guarantee or
had wedged.

Done means: entry and exit with timestamps or an honest statement that this
server publishes neither; every lock held before a failover still held after it;
no new lock granted while grace is in force; five failovers each recovering on
their own; and a failover that reaches an operator with a timestamp and a
duration.

## 2. What shipped

- **The grace observer**: reads the server pod's log stream through the
  Kubernetes API, with the timestamp the container runtime attached to each
  line, and classifies lines as grace entry or exit. Previous-container logs are
  read too, so a restart in place keeps its pre-fault half.
- **The lock probe**: a second client attempting a new lock once per second,
  logging outcomes in step 3's `OK|ERR <index> <epoch>` format on its own
  filesystem, so one parser serves both.
- **Five cases**: CHAOS-05 (five failovers, per-cycle assertions), CHAOS-06
  (locks across a failover, read from both ends), CHAOS-07 (a new lock attempted
  during grace), OBS-02 (a failover an operator can see), OBS-03 (grace entry and
  exit measurable).
- **Gate naming**: the chaos prefix marks a case that injures the server whatever
  plan section it comes from, which is why OBS-02 and OBS-03 carry it.

## 3. What these five cases assert

The conventions they share with every other case, including the guard band for
two clocks, are in [`01-test-plan.md`](01-test-plan.md) Section 4.1. What is
specific here:

| # | Assertion | Where it comes from, and how it is checked |
|---|---|---|
| 1 | Grace is read from what the server publishes, never inferred from a stalled client | A stalled client is the symptom this phase exists to tell apart from grace. The observer's only input is the log stream |
| 2 | A case that needs a window and has none reports blocked | Grace begins when the server restarts, which is later than the fault by an unknown amount, so a window derived from the configured value ends late and would report a lawful grant as a violation. CHAOS-07 reports blocked; OBS-03 is the case that fails for the missing signal ([F-008](findings.md)) |
| 3 | Reclaim is asserted from both ends: the holder still holds, and a second client is still refused | A client that has lost its lock does not find out until it uses it, so the holder alone proves nothing |
| 4 | Every lock the case took, not one | The SLO row is 100%, and a case that checks one lock cannot report a fraction |
| 5 | A new lock granted during grace fails, whether the server granted it deliberately or lost the state that would have refused it | RFC 8881 Section 8.4.2 bars new state during grace. Neither reading is acceptable, and the case does not have to tell them apart to fail |
| 6 | A refusal during an outage is not evidence that grace was enforced | Every attempt fails while the server is down, for the ordinary reason. The window is established from the server's own signal first, and only then is the probe read inside it |
| 7 | Repeated failover is measured per cycle | A total that fits inside a wall clock proves nothing if one cycle inside it took four minutes |
| 8 | Grace entered more than once per failover is a re-entry loop, and a finding about the server | It presents as a hung client in front of a healthy server, which the triage runbook calls the most common wrong diagnosis here. The count is per cycle, and the failure message says grace re-entry, not client stall |
| 9 | No case asserts how grace is implemented | Every assertion reads a log line's existence, a lock outcome or a Kubernetes object |

## 4. What the observer and the probe produce

The shapes are in `pkg/framework/grace.go` and `pkg/framework/probes.go`, with
the bounds in `pkg/slo/slo.go`, and are not repeated here. What matters about
them:

- **Entry and exit are two observations, not one interval with a duration.** An
  entry with no exit is the re-entry symptom and has to be representable.
- **A line that mentions grace but classifies as neither is kept and reported.**
  Reporting "grace was never observed" while holding a line that says otherwise
  would be diagnosed as a server defect.
- **The probe's success means granted and immediately released.** A probe that
  kept what it was granted would change what the next attempt means.
- **An attempt that blocks is not an attempt that failed.** A Linux client
  handling the server's grace error retries in the kernel rather than returning
  it, so the expected shape during grace is a long gap and then a grant, not a
  run of refusals. The assertion is about grants, which is why it survives
  either shape.
- **The per-cycle record attributes grace entries to one cycle.** The count
  means nothing summed over a run.

The probe log reuses step 3's format, so one parser serves both.

## 5. Decisions

**Grace is read from the log stream through the Kubernetes API.** It is the only
channel guaranteed present, timestamped by something other than the process under
test, and readable without knowing anything about the deployment beyond which
pods it runs. The server's metrics endpoint was rejected for this phase: nothing
in the Kubernetes API states which port serves metrics or what the metric is
called, so it would need two flags and a convention, and the convention would be
a guess in the middle of an observability assertion.

**Timestamps come from the container runtime, not from the server's own
wording.** Log formats differ per implementation and change between versions;
runtime timestamps do not. A suite that parsed each server's format would be
asserting on the thing it is least able to keep working.

**Classification tests for exit first.** A line mentioning grace is an exit if it
also carries a word that negates or ends it, and an entry otherwise. The ordering
matters: common exit wordings are the entry wording with a negation in front, and
a rule that tested for entry first would report a re-entry loop on a healthy
server. `-grace-enter-pattern` and `-grace-exit-pattern` replace the rule for a
server it does not cover, and are set together or not at all: one alone would
observe every failover entering grace and never leaving it. A server that says
nothing at all fails OBS-03, and that is a finding rather than a harness gap,
because an operator on that deployment cannot see grace either. F-008 in
[`findings.md`](findings.md) is exactly this, met in the field.

**The two clocks here are unavoidable**, because the window is stamped by the
kubelet on the server's node and the probe by a client pod, which is on a
different node by construction. This is the case that produced the guard band
convention in the test plan: the band narrows the window at both ends, a
boundary grant is a note rather than a failure, and the direction is deliberate,
missing a marginal violation rather than filing a lawful one. A measured offset
was rejected as a number nobody checks, against a constant a reviewer can argue
with.

**The probe asserts on grants, never on refusals.** During an outage every
attempt fails for the ordinary reason that the server is not there. The
protocol guarantee is that no *grant* lands inside the observed window and that a
grant does land after it.

**Un-reclaimed state is held through the window.** A server may lift grace early
once it concludes no further clients will reclaim (test plan Section 3.8), so
CHAOS-07 holds a client with outstanding state across the whole case. Without
that, the case passes vacuously.

**Five cycles, each injected only after the previous recovered.** Injecting on a
fixed cadence regardless of recovery measures overlapping failovers, which is a
different case and not what the plan asks for here. The plan's phrase is five
cycles inside ten minutes; on the default profile grace alone is ninety seconds,
so five lawful recoveries do not fit and a case asserting the wall clock would
fail with no defect present. Per-cycle assertions replace it.

**The re-entry check degrades rather than blocking the case.** Where grace is
observable, more than one entry per cycle fails CHAOS-05. Where it is not, the
case still asserts five recoveries and says the check was unavailable. One
missing signal should produce one failure, not four.

**OBS-02 accepts any timestamped channel an operator can reach**, and records
which one answered. It fails on silence and on a pair of timestamps that cannot
be turned into a duration. It does not fail when only Kubernetes answered, but it
says so: a restart count tells an operator a pod restarted, not that NFS failed
over.

## 6. Whole-file locks, and what was deferred

CHAOS-06 in the test plan says byte-range locks. Nothing on a busybox image can
take one: the `flock` applet calls `flock(2)`, which has no range argument.

A whole-file lock is not a different mechanism. The Linux client simulates
`flock` with POSIX locks on the server, so `flock -x` from a pod already becomes
a `LOCK` over the whole range on the wire (RFC 8881 Section 9). Reclaim,
exclusivity and the grace bar on new state are therefore all asserted honestly
here. What is not asserted here is the sub-file dimension: two clients holding
disjoint ranges of one file. That arrived with `locktool` in step 6, designed in
[`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md),
which also carries the byte-range half of DATA-05.

Bringing `locktool` forward into this phase was rejected: it would have doubled a
phase that already needed a design document.

## 7. Configuration

`-grace-enter-pattern` and `-grace-exit-pattern`, set together or not at all;
everything else is step 3's. A pattern that does not compile is a startup
failure, not a case that quietly observes nothing. A server whose logs are
unreadable is reported as such rather than as a server that never entered grace.

Not configurable: the probe rate, the guard band, the number of failover cycles,
and the grace exit bound of two lease periods.

Nothing new is deployed and no new privilege is needed. One lifecycle addition:
the probe and every lock holder are registered for stop before the case can fail,
so a failing case does not leave a lock held by a pod that outlives it.

## 8. What changed after this was written

- **OBS-01's requirement moved.** This doc listed "asserting that an alert fired"
  as OBS-01's requirement, needing a monitoring stack the suite does not deploy.
  [`06-observability-design.md`](06-observability-design.md) replaced that: the
  suite verifies what the deployment publishes, never the alert rules.
- **The metrics channel opened.** Left open here, taken up by OBS-07 in step 7.
- **Node and network faults** were "step 5" here. After the delivery order was
  resorted to close out one plan section at a time, they are step 10.
- **F-008 happened.** The provisioner this project runs against announces no
  grace at all, so CHAOS-07 reports blocked and OBS-03 fails on it. The design
  anticipated this; the field entry records what it costs a run.

Still open: a deployment that reports grace only through metrics fails OBS-03 for
a reason that is half harness gap; the wording rule is the one
implementation-specific part of an otherwise portable observer; and whether a
sub-file range reclaims differently from a whole-file one on any real server.
There is no protocol reason it should, and if it does, that is a finding.

## 9. Sources

- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 8.4.2, Server
  Failure and Recovery, for grace and reclaim, and Section 9, File Locking and
  Share Reservations, for `LOCK`, `LOCKT` and `LOCKU` over byte ranges.
- [`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) and the
  [kernel NFS documentation](https://docs.kernel.org/filesystems/nfs/) for what
  the Linux client does with a grace error and how it simulates `flock`.
- Kubernetes [pod logs through the
  API](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/)
  including `previous`, which is the channel the observer reads, and
  [Events](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/event-v1/),
  which is OBS-02's second channel.

# 05: Closing out observability: the data an alert is built on

Author: Claude Code
Created: 2026-09-11
Updated: 2026-09-11

Phase boundary: the first cases that assert on the *inputs an operator would
build monitoring on*, rather than on what a client or a server log said. Ends
when OBS-01, OBS-05, OBS-06 and OBS-07 run against a real cluster and each one
reports a timestamped availability signal, a memory reading against a declared
ceiling, a capacity agreement and a continuity verdict, or names the source this
deployment does not serve. Cadence: three pull requests, following
[`plan.md`](plan.md) step 7, on top of step 6. Prior phases:
[`02-chaos-operations-design.md`](02-chaos-operations-design.md),
[`03-grace-and-lock-reclaim-design.md`](03-grace-and-lock-reclaim-design.md),
[`04-data-path-and-locktool-design.md`](04-data-path-and-locktool-design.md).
This is the next phase of 03, which built the log channel and left the metrics
channel open in its section 11.

---

## 1. Problem and outcomes [decided: verify the data, never the alert rules]

Steps 3 and 4 can injure the server, time the outage and read the server's log
stream. The test plan's Section 3.5 words four of its cases as "an alert fires".
This phase does not test that, and the distinction is the whole design.

An alert is a rule somebody wrote: a threshold, a `for` duration, a severity and
a routing policy. All four are organization-specific and problem-specific. A
suite that asserted on them would be asserting on the operator's paging policy,
would fail a healthy storage system for a threshold set differently, and would
have to either find a monitoring stack it does not own or install one. None of
that is a property of NFS RWX volumes on Kubernetes.

What *is* a property of the system under test is whether the data those rules
need exists at all: whether an outage is visible in queryable state with a
timestamp, whether the server's memory can be read against a declared ceiling,
whether a volume's usage is reported by the control plane and agrees with what
the workload sees, and whether those readings survive a server restart. A
deployment where the data is missing, stale, or wrong cannot be alerted on by
anyone, whatever rules they write. That is the finding this phase exists to
produce.

Observable outcomes that define done:

- A case can state when the server became unavailable and when it came back,
  from data the Kubernetes API serves, with timestamps, and compare that window
  against the outage the client actually experienced.
- A case can state the server's memory working set, the limit it is measured
  against, and whether the reading moves when the server is worked.
- A case can state what a pod's `df` says about a volume, what the control plane
  says about the same volume at the same moment, and by how much they disagree.
- A case can state whether each of those readings kept answering across a server
  restart, and which of the lawful shapes the values took.
- Every one of the four can say "this deployment does not serve that data" in a
  way that lands on the deployment owner rather than on the server owner.

Requirements: Section 3.5 of [`01-test-plan.md`](01-test-plan.md) for the four
cases, whose wording this phase changes in the same pull request (5.11), Section
3.8 for the observability bound, Section 4.2 for the category budget, Section
4.3 for where a failure gets routed. Findings that constrain this phase: F-008
(a deployment that publishes nothing is a finding about the deployment, and the
case fails rather than skips), F-002 (node sizing, which bounds what the memory
case may do), F-001 and F-003 (ordering around anything that unmounts).

Cases served: **OBS-01** (server unavailable), **OBS-05** (memory approaching a
ceiling), **OBS-06** (volume near capacity), **OBS-07** (readings survive a
server restart). The volume and memory readers are shared with SCALE-02 and
SCALE-04, which assert on server RSS and arrive in step 9.

---

## 2. Human-readable rules

Each line is testable, and a reviewer can accept or reject the phase from this
section without reading code.

1. The suite deploys no monitoring stack, ever, and takes no dependency on a
   hosted one. Every reading in this phase comes from the Kubernetes API server
   with the kubeconfig the suite already uses.
2. No case asserts that an alert fired, and no case reads an alert rule, a
   severity or a threshold. Those are the operator's, and they differ per
   organization and per problem.
3. What is asserted is that the input data exists, carries a timestamp, is
   current, and says the same thing the workload says. A number nobody can read
   and a number that is wrong are the same defect from an operator's chair.
4. A data source the suite cannot reach for its own reasons (RBAC, a missing
   optional API) is blocked, naming what was refused. A source that answers and
   reports nothing about the server or the volume under test is a failure. The
   first is a harness gap, the second is the finding. This is the F-008 rule.
5. A case that could not establish its own precondition reports blocked with the
   number it did reach. "The reading never moved because the server was never
   worked" is not a pass.
6. No case drives the server to OOMKill, and no case fills a filesystem it does
   not own. Both are precondition manufacturing that costs more than the
   assertion is worth.
7. Two readings of one quantity taken by two samplers at two moments agree
   within a stated tolerance derived from the slower sampler's period, or the
   case says by how much they disagreed. A raw equality assertion between `df`
   and the control plane fails on a healthy cluster.
8. No case invents a fault. The pod delete from phase 3 is the only fault this
   phase uses.
9. Timing bounds come from `pkg/slo`. Fractions and tolerances added here go
   there too, not into a case.
10. Every rule from phases 3 and 4 still holds. This phase adds readers, it does
    not amend the contract around the existing cases.

---

## 3. Scope

**In scope for this phase**

- A reader for the availability data Kubernetes serves about the server:
  pod conditions with their transition times, container statuses and restart
  counts, termination reasons, and the readiness of the server's endpoints.
- A reader for the kubelet Summary API: per-volume capacity and usage with the
  kubelet's own timestamp, and per-pod memory working set from the same call.
- A cross-check against `metrics.k8s.io` for the memory reading, where the
  aggregated API is served.
- A sampler that polls those readers across a fault and classifies what happened
  to each series.
- A bounded write that moves a volume's usage, and a bounded small-file storm
  that moves the server's working set. Both exist to show the readings track
  reality, and neither approaches a ceiling.
- The four cases, and the artifacts each writes before teardown.

**Out of scope (explicit non-goals)**

- Alert rules, thresholds, severities, and routing. Rule 2.
- Prometheus, Alertmanager, Grafana, kube-state-metrics, or any component the
  cluster does not already run. Installing one to pass an observability case
  tests the thing that was installed.
- Cloud Monitoring, Managed Service for Prometheus, and any other hosted
  backend. See 5.1: querying Google's managed collection means querying Cloud
  Monitoring through a frontend proxy the operator deploys, which is a
  dependency this suite will not take.
- Filling a volume to a capacity threshold, or driving the server to its memory
  ceiling. Rule 6, and see 5.6 and 5.7 for what replaces them.
- Any new fault. The pod delete from phase 3 is the one this phase uses, and no
  network partition is built here. CHAOS-04 still owns that in step 10.
- Any change to OBS-02, OBS-03 and OBS-04, which passed as written.

**Depends on**

- Server pod discovery, the fault timeline and the recovery measurement from
  phase 3.
- The directory population and deletion helpers from phase 6, which are the
  bounded small-file storm OBS-05 uses.
- `pkg/slo`, which already holds the observability bound, currently named
  `AlertSLO` and read by nothing (5.11 renames it).
- `get` on `nodes/proxy`, which is the only access this phase needs that the
  suite does not already exercise.

---

## 4. Data contract

Everything here is produced by a case, written to the artifact bundle before
teardown, and consumed by whoever reads a failure. None of it is stored between
runs. The system of record for every field is the cluster: the suite copies what
the API server or the kubelet said, and never computes a value it then presents
as measured.

### 4.1 The availability signal

One row per observed transition of the server's readiness, from the Kubernetes
API. This is the data an availability rule is written against, whatever the rule
says.

| Field | Type | Meaning |
|---|---|---|
| `Pod`, `Node` | string | Which server pod, and where. |
| `Ready` | bool | The pod's `Ready` condition. |
| `TransitionAt` | time.Time | The condition's `lastTransitionTime`, which is the API's own clock, not the workstation's. |
| `Endpoints` | int | How many addresses the server Service's EndpointSlices list as ready. Zero is the shape a rule on endpoint availability fires on. |
| `RestartCount` | int32 | From the container status. |
| `LastTerminated` | struct | Reason, exit code, started and finished times, from `lastState.terminated`. `OOMKilled` is a reason, not a separate field. |
| `ObservedAt` | time.Time | The workstation's clock at the poll, so a row can be placed on the fault timeline. |

Identity: `(Pod, TransitionAt)` for a transition, `(Pod, ObservedAt)` for a poll.
Both are kept: the API's timestamps are what an operator would see, and the
poll times are what let the case say when the suite could first have seen it.

### 4.2 The volume usage reading

Two sources at minimum, one shape, so that a disagreement reads as a table.

| Field | Type | Meaning |
|---|---|---|
| `Source` | enum | `pod-df` or `kubelet-summary`. |
| `Claim` | string | The PVC this reading is about. |
| `CapacityBytes` | int64 | Total, as that source reports it. |
| `UsedBytes` | int64 | Used, as that source reports it. |
| `AvailableBytes` | int64 | Available, as that source reports it. Recorded rather than derived: on a filesystem with reserved blocks, used plus available does not equal capacity, and deriving would invent agreement. |
| `At` | time.Time | The kubelet publishes its own timestamp per volume; `pod-df` carries the workstation clock at the exec. |

Identity: `(Claim, Source, At)`. Every comparison OBS-06 makes is between two
rows with the same `Claim` and different `Source`.

### 4.3 The memory reading

| Field | Type | Meaning |
|---|---|---|
| `Pod`, `Container` | string | Which container. A server pod with a sidecar has more than one, and only the one running the server is the subject. |
| `WorkingSetBytes` | int64 | From the kubelet Summary API, cross-checked against `metrics.k8s.io` where served. Both report working set for memory. |
| `LimitBytes` | int64 | From the pod spec's resource limits. Zero means no limit is declared, which is a blocked precondition, not a zero ceiling. |
| `Fraction` | float64 | `WorkingSetBytes / LimitBytes`, computed only when `LimitBytes` is non-zero. |
| `At` | time.Time | The source's own sample time. |
| `Source` | enum | `kubelet-summary` or `metrics-api`, so a disagreement between the two is visible rather than averaged. |

### 4.4 The sampled series and the continuity verdict

There is no time-series store in this phase, so the suite is the sampler: a
poller reads one of the readers above on a fixed interval across a fault and
keeps what it got.

| Field | Type | Meaning |
|---|---|---|
| `Reader` | string | Which reading this series is of. |
| `Samples` | []Sample | `{At time.Time, Value float64, Err error}`. A failed read is a sample with an error, kept rather than dropped: a source that stopped answering is the result. |
| `Interval` | time.Duration | The poll interval, which is the suite's own and therefore known rather than inferred. |

`ContinuityVerdict` is one of four, exhaustive over what a poller can see across
a restart:

| Verdict | Shape | Meaning |
|---|---|---|
| `resumed-reset` | Reads succeed on both sides, value lower after than before | The process restarted and its counter started again. |
| `resumed-continuous` | Reads succeed on both sides, value non-decreasing across | The reading outlived the process. Lawful for anything the kubelet reports about a container rather than the server about itself. |
| `never-resumed` | Reads succeed before, fail or return nothing after, past the resume bound | The source did not survive the restart. The failure this case is named for. |
| `absent` | No successful read in the whole window | The source does not serve this on this deployment. Blocked, not failed. |

The gap between the last good read before the restart and the first after is
recorded in every verdict. A gap is not a failure: an outage that leaves a hole
is an outage an operator can see, which is what Section 3.5 asks for.

### 4.5 Ownership and evolution

Producer: the cases. Consumer: whoever reads the bundle. There is no schema in a
database and no cross-run comparison, so the only compatibility question is what
`environment.json` gains, since a cached preflight record is replayed with
`-env-file` and must gate a rerun exactly as the original run did.

Added to `Capabilities`: `VolumeStats` (the kubelet Summary API returned an
entry for a suite claim) and `PodMemoryStats` (a working set reading was
available for the server container). Both go through `AsMap` and
`CapabilitiesFromMap` in the same change, because a capability that does not
round-trip gates a replayed run differently from the run it was recorded on,
silently.

Added to `Environment`: `serverMemoryLimitBytes`, recorded because a deployment
that declares no limit is a fact about the deployment worth carrying in every
failure report, and `volumeStatsSource`, which names the source that answered or
says none did.

Backward compatible: a record written before this phase decodes with both
capabilities false and both fields zero or empty, which gates the new cases off.
That is correct for a record taken before anything was probed.

Would force a breaking change: making a reading a list of sources rather than a
value with a source tag, if a third source ever arrives. Avoided now by putting
`Source` on the row instead of on the reader.

---

## 5. Design and decisions

### 5.1 What a GKE cluster actually serves, and why there is no Prometheus here [decided: no monitoring stack, no hosted backend]

The first revision of this doc assumed a Prometheus-compatible query API could
be found in the cluster. Checked rather than assumed, that is wrong for the
deployment this project runs against, and the correction removes a large part of
the phase.

Google Kubernetes Engine does not run an in-cluster Prometheus server. What it
runs is collection: the `gmp-collector` and `gke-metrics-agent` DaemonSets that
F-002 already observed on these nodes ship metrics out of the cluster. Google
Cloud Managed Service for Prometheus stores them in Cloud Monitoring, and the
Prometheus-compatible query API is served by a `frontend` Deployment the
operator installs, which is an authorizing proxy in front of Cloud Monitoring
rather than a store in the cluster. Querying it is therefore a dependency on
Cloud Monitoring, which this project does not have and does not want, and
installing anything to create one violates rule 1.

So the design reads what Kubernetes itself serves, which is enough for every
assertion the phase actually needs:

| Source | Serves | Present without any add-on |
|---|---|---|
| Kubernetes API: pod conditions, container statuses, Events, EndpointSlices | The availability signal (4.1) | Yes, it is the API server |
| kubelet Summary API, `/stats/summary` through `nodes/proxy` | Per-volume capacity and usage, per-container memory working set | Yes, it is the kubelet |
| `metrics.k8s.io` | Container working set | Where metrics-server or its equivalent is installed, which GKE does by default; used as a cross-check, never as the only source |

None of the three needs a flag, a credential, or a component. That is the point.

### 5.2 What replaces "an alert fired" [decided: assert on the input, with a bound]

Each case keeps the question the plan asks and changes what answers it.

| Case | Plan's wording | What this phase asserts |
|---|---|---|
| OBS-01 | Alert fires within SLO, correct severity and target | The outage is visible in the Kubernetes API within the bound, with timestamps that name the server, and the window it describes overlaps the outage the client measured |
| OBS-05 | Alert fires before OOMKill | The server declares a memory limit, its working set is readable against that limit with a timestamp, the reading moves when the server is worked, and an OOMKill, if one happens, is visible in the API with a timestamp |
| OBS-06 | Alert fires, `df` agrees with the control plane | The control plane reports this volume's usage, it agrees with `df` inside the pod within tolerance, and both move together when the workload writes |
| OBS-07 | Counters reset cleanly or persist, no gaps that hide an outage | Every reading above keeps answering across a server restart, and each series lands in one of the lawful shapes in 4.4 |

The severity and the target from OBS-01's wording are dropped, not reinterpreted.
A severity is a property of a rule this suite does not read. "Target" survives in
the weak sense that the data names the server pod, which is asserted.

### 5.3 How the harness reaches the kubelet [decided: the API server's proxy subresource]

The harness runs on a workstation. The Summary API is served by each kubelet.
Reaching it directly would mean the node's address, its port and its certificate,
none of which a workstation outside the cluster should need. The API server
already proxies it:
`RESTClient().Get().Resource("nodes").Name(n).SubResource("proxy").Suffix("stats","summary")`.
The typed node client carries no `ProxyGet`, so this is built on the REST client
the suite already has.

`metrics.k8s.io` is an aggregated API, read with `AbsPath` on the same client.
The response is decoded into a local struct in both cases. No new module
dependency: the suite decodes the handful of fields it reads rather than pulling
`k8s.io/metrics` and a cAdvisor type tree into a repository whose dependency
list is client-go and nothing else.

Access: `get` on `nodes/proxy`. A 403 is classified and reported as blocked
naming the verb, never as a deployment that does not publish volume stats.
Telling those two apart is rule 4, and it is the only place in this phase where
a permission gap could masquerade as a finding.

### 5.4 OBS-01: an outage that is visible in queryable state [decided: reuse the pod delete, assert on the API's own timestamps]

No new fault. The server pod is deleted with the operation from phase 3, and the
case reads the availability data either side of it.

Steps, in order:

1. Start a workload and record the server's readiness, endpoint count and
   restart count as a baseline.
2. Delete the server pod. Record it on the fault timeline.
3. Poll the availability reader while the phase 3 recovery measurement runs, so
   the client's outage and the API's account of it are collected over the same
   interval.
4. Assert that the API showed the server not ready, with a transition timestamp,
   and that the Service's ready endpoint count reached zero. A deployment where
   neither happened had an outage no rule could have fired on.
5. Assert the signal arrived within the bound. The window between the fault and
   the first not-ready observation is compared against
   `slo.AvailabilitySignalSLO` (5.11), which is the existing five minute value
   under a name that says what it now means.
6. Report the API's window next to the client's measured outage. The two are
   different clocks and different definitions, so the overlap is asserted and
   the difference is reported, never asserted to a margin narrower than the
   guard the suite already applies to two-clock comparisons.

How this differs from OBS-02, since a reviewer will ask: OBS-02 asks whether a
failover left a trace anywhere, and reads the server's log stream and pod starts
to answer. OBS-01 asks whether the *availability state* an operator watches went
false and came back, with timestamps, inside a bound. A deployment can pass one
and fail the other in both directions.

### 5.5 OBS-05: a ceiling that is declared, a reading that moves [decided: no ceiling chase]

The plan's expected result is "alert fires before OOMKill". Neither half is
this suite's to assert: the alert is a rule, and producing the OOMKill would
mean deliberately destroying the cluster's storage to observe an ordering.
Rule 6.

What the case asserts instead is that the ingredients exist:

1. The server container declares a memory limit. Without one there is no
   denominator, no fraction, and nothing for any threshold rule to sit below.
   Blocked, naming the container, and recorded in `environment.json`: a server
   with no declared limit cannot be alerted on for approaching one, by anyone.
2. A working set reading is available for that container, with a timestamp,
   from the Summary API. Where `metrics.k8s.io` also answers, both are recorded
   and a disagreement beyond tolerance is reported, since two control-plane
   sources that contradict each other are worth knowing about.
3. The reading tracks reality. A bounded small-file storm through the export,
   using the phase 6 population helper, must move the working set by a
   measurable margin (`slo.MemoryReadingDelta`). A reading pinned at a constant
   while the server is being worked is a broken reading, and a threshold rule on
   it would never fire.
4. No OOMKill occurred. If one did anyway, it is reported with its timestamp and
   the reading that preceded it, which is the ordering the plan cares about,
   observed rather than manufactured.

The storm is bounded by the case budget and by the claim's capacity, is
nowhere near the limit by construction, and its files are deleted before
teardown reaches the claim. If the reading does not move within the budget, the
case reports blocked with the delta it did see: rule 5.

### 5.6 OBS-06: two sources, one quantity, and a small write to prove they track [decided: no threshold fill]

The agreement half of this case was always the valuable half, and it is now the
whole case.

- `df` inside the pod, which is what the workload sees.
- The kubelet Summary API entry for the same claim, matched by `pvcRef`, which
  is what the control plane sees. No entry means the CSI driver does not
  implement `NodeGetVolumeStats`, which the CSI specification makes an optional
  node capability. Blocked, naming the driver: a volume the control plane cannot
  measure is one nobody can write a capacity rule for, and that is a finding
  about the deployment.

What "agrees" means, since two samplers at two moments never produce equal
numbers. The kubelet aggregates volume stats on a period that defaults to one
minute (`--volume-stats-agg-period`), so its answer is stale by up to that
period. The case reads `df` at a moment, waits for a kubelet reading whose own
timestamp is after that moment, and compares used bytes within a tolerance
expressed as a fraction of capacity (`slo.VolumeStatsAgreement`), with the
freshness requirement stated separately (`slo.VolumeStatsFreshness`). A
disagreement beyond tolerance fails, and the message prints both rows, both
timestamps and the delta in bytes, because the failure that matters here is a
control plane reporting a volume as nearly empty while the workload is getting
ENOSPC.

Then a bounded write, sized as a fraction of the claim large enough to be well
outside the tolerance and small enough to be nothing like a fill
(`slo.VolumeUsageDelta`), and the comparison is repeated. Two sources that agree
on a static number prove less than two sources that move together.

The quota precheck stays, with its role changed. `df`'s reported total is
compared against the claim's `status.capacity`. A directory-backed export with
no per-volume quota reports the backing filesystem's size, and that is recorded
as a finding rather than used as a gate: nobody on that deployment can write a
per-volume capacity rule, because there is no per-volume capacity to threshold.
It no longer gates a fill, because there is no longer a fill to gate.

### 5.7 OBS-07: the suite is the sampler [decided: poll across the restart, classify each series]

With no time-series store, "metrics survive a server restart" is answered by
polling the readers across a restart and seeing which of them keep answering.
That is a weaker claim than a retained series would support, and it is the one
that can be made without a monitoring backend. It is also the claim that matters
for the deployment: a reading that stops answering after a restart, or that
comes back attached to nothing, is unusable as an alert input no matter what
stores it.

Mechanism: start pollers on the availability reader, the volume reader and the
memory reader; delete the server pod with the phase 3 operation; wait for the
phase 3 recovery; stop the pollers and classify each series with the table in
4.4.

Fails on `never-resumed`, which is a source that did not survive the restart.
Passes on `resumed-reset` and `resumed-continuous`, which are the two shapes
Section 3.5 names as acceptable. Blocks on `absent`. The gap is recorded in
every verdict, and a restart that left no gap and no reset anywhere is logged
with the series named: on kubelet-reported values a continuous reading across a
container restart is expected, and failing it would file lawful cAdvisor
behavior as a defect.

### 5.8 What this phase does not claim

Stated plainly so nobody reads a green OBS run as more than it is.

- It does not show that anyone is paged. No rule is read and none is evaluated.
- It does not show that a threshold is set anywhere, or set sensibly.
- It does not show that the data is retained. Every reading here is live, and
  retention is a property of a backend this suite does not query.

What a green run does show is that an operator on this deployment has readable,
timely, correct inputs to build all of that on, and a red or blocked run names
exactly which input they do not have. Given that alerting policy is
organization-specific, that is the line where a portable suite stops.

### 5.9 Naming, gating and the category budget

Four cases, named for what they do: `TestObsServerOutageIsVisibleInAPI`,
`TestObsServerMemoryIsReadableAgainstLimit`,
`TestObsVolumeUsageAgreesWithControlPlane`,
`TestObsReadingsSurviveServerRestart`.

The repository sorts strictly by category, so all four are `TestObs` and run
under `make test-obs`, including the two that delete the server pod. The
Makefile already says this is where `test-obs` stands.

Section 4.2 budgets `make test-obs` at under 45 minutes. Dropping the sustained
outage and both threshold chases takes the expensive work out of this phase:
what remains is two pod deletes and two bounded writes, which is the shape of
cases already in that target. The budget is expected to hold, and it is measured
in the last pull request rather than asserted here. If it does not hold, the row
is edited in the test plan against the measured number, not overrun in silence.

### 5.10 Test plan wording, and the SLO constant [decided: edit Section 3.5 in this pull request]

Requirements live in the test plan, so the four rows in Section 3.5 change with
this design rather than in a commit message. Each keeps its case ID and its
subject and states what is verified: the data exists, is timely, and agrees.
A line is added under the table saying that alert rules, thresholds, severities
and routing are organization-specific and out of scope for this suite, so that
the next reader does not restore the old wording as a gap.

`pkg/slo.AlertSLO` is currently defined, commented as the deadline for an
availability alert to fire, and read by nothing. It is renamed
`AvailabilitySignalSLO` with the same five minute value and a comment that says
what it now bounds: how long after an outage the Kubernetes API may take to show
it. Renaming a constant nothing reads costs nothing, and leaving a constant
called `AlertSLO` in a suite that deliberately does not test alerts is exactly
the comment-that-contradicts-the-code problem the repository rules call out.

---

## 6. Delivery phases

Three pull requests, smallest runnable slice first. Each lands with the cases it
serves, per the one-vector rule.

**MVP, PR 1: the kubelet reach, and OBS-06.** The `nodes/proxy` Summary API
reader, the volume usage rows, the capability and environment fields, the quota
precheck as a recorded finding, the agreement assertion and the bounded write.
Chosen as the MVP because it needs no fault at all, exercises the whole reach
path, and produces a result on any cluster, including one with nothing installed
beyond Kubernetes. Validated outside a unit test by running
`make test-obs -run TestObsVolumeUsageAgreesWithControlPlane` against the GKE
cluster in F-008 and reading the table it writes.

**PR 2: OBS-01 and OBS-07.** The availability reader, the pollers, the
continuity classifier, both cases, and the SLO constant rename. They land
together because OBS-07's pollers are the availability reader and the volume
reader driven across the same fault OBS-01 uses, and splitting them would land a
classifier with one caller.

**PR 3: OBS-05.** The memory reading, the limit precondition, the
`metrics.k8s.io` cross-check, the bounded storm and the delta assertion.

Later phases sharpen after PR 1 reports from a real cluster. The likely finding
there, stated now so it is not a surprise, is that the CSI driver in use does
not implement `NodeGetVolumeStats`, which blocks OBS-06 and tells the project
something worth writing down.

---

## 7. Configuration

Source: command-line flags, as everything else in this repository. No config
file, no environment variables beyond `KUBECONFIG`, no credentials: every call
this phase makes goes through the API server with the kubeconfig the suite
already uses.

**No new flags.** This is a deliberate outcome of the redesign, and it is worth
stating, because the first revision proposed two. Every input this phase needs
is answerable by the cluster: which pod serves the export is discovered, which
volume is measured is the claim the case created, what the memory limit is comes
from the pod spec, and how long the kubelet's aggregation period is does not
need to be declared because the kubelet timestamps its own readings.

Intentionally not configurable:

- Alert names, thresholds, severities. Not read at all. Rule 2.
- The agreement tolerance, the freshness requirement, the two movement deltas
  and the resume bound. They are bounds, and bounds live in `pkg/slo` per the
  no-timing-literals rule.
- The poll interval, which is the suite's own and fixed until something needs it
  to vary.

---

## 8. Deployment decisions

Runtime unit: the same `go test` binary on a workstation. Nothing is deployed
into the cluster by this phase, and unlike the first revision it creates no
object beyond the pods and claims every case already creates.

Lifecycle:

- Pollers stop before teardown, and their samples are written to the bundle
  before any object is deleted. A poller that outlived its case would read
  through teardown and record a deletion as an outage.
- Each read is individually bounded. A kubelet that has stopped answering must
  not stall a case, and one sick node must not starve collection from the
  healthy ones, which is the existing rule for anything that reads a node.
- The two faults are the phase 3 pod delete, which brings its own recovery
  assertion and its own teardown ordering. Nothing in this phase changes it.

Access scope: `get` on `nodes/proxy`, read on `metrics.k8s.io`. Both classified
on failure so a permission gap reports blocked rather than being filed as a
deployment defect.

Cost: OBS-05 and OBS-06 each write a bounded amount of data through the export
and delete it before teardown. Neither approaches a capacity or a memory
ceiling, which is the difference between this revision and the first.

---

## 9. Observability

The harness's own output, written to `artifacts/<run-id>/` before teardown,
because a case that tore down its evidence is unreproducible (Section 4.3).

| File | Contents |
|---|---|
| `availability.txt` | The rows from 4.1 across the fault, with the API's transition times and the endpoint counts, next to the client's measured outage. |
| `volume-usage.txt` | The table from 4.2, both sources, both timestamps, the delta in bytes, before and after the bounded write. Written whether the case passed or not: a run where the sources differed by 2% is a different run from one where they agreed exactly, and the pass looks identical without it. |
| `server-memory.txt` | The readings from 4.3, the declared limit, the fraction, the movement under load, and any OOMKill with its timestamp. |
| `continuity.txt` | One verdict per series from 4.4, with the interval, the gap, and the last good read either side of the restart. |

The fault timeline is unchanged: the pod delete records itself through the
existing mechanism, which is what the availability window is read against.

Log lines follow the existing style: what was observed, from which source, with
the node, the pod and the profile named.

---

## 10. Alternatives considered

| Option | What it is | Why not chosen |
|---|---|---|
| Query a Prometheus-compatible API in the cluster | Read alerts and series from a monitoring stack | GKE runs no in-cluster Prometheus. Managed collection ships to Cloud Monitoring and its query API is a frontend proxy the operator deploys, so querying it is a Cloud Monitoring dependency. See 5.1. |
| Install Prometheus and alert rules for the run | Deploy the stack the old design needed | Tests the rules the suite just wrote, and leaves a monitoring stack in someone's cluster. Rule 1. |
| Assert on alert rules where a stack happens to exist | Read `/api/v1/rules` when it answers | Makes the suite's verdict depend on whether the operator happens to run Prometheus, and asserts on thresholds and severities that are organization-specific. Rule 2. |
| Query Cloud Monitoring directly | Use the hosted backend this cluster already ships to | A cloud dependency, credentials the suite does not have, and a result that would not reproduce on bare metal. The suite is portable by design. |
| A NetworkPolicy partition to sustain an outage for minutes | Make the outage long enough for a `for` duration to elapse | Only needed to make an alert rule fire. With no alert assertion, a pod delete produces the transition the API records, and the phase creates no new fault. |
| Fill a volume to a capacity threshold | Approach the number a rule would fire on | The threshold is the operator's, so approaching it proves nothing here, and on an export with no per-volume quota it fills the server's disk. A bounded write proves the two sources track. |
| Drive the server to its memory ceiling | Approach the number a rule would fire on | Same reason, plus it risks the cluster's storage and, on small workers, the node (F-002). |
| `metrics.k8s.io` as the only memory source | Read the aggregated API and nothing else | It is an add-on, absent on a cluster without metrics-server, while the kubelet Summary API is served by the kubelet itself. Used as a cross-check instead. |
| Read volume usage from `kubelet_volume_stats_used_bytes` | Take the number from a scrape | That series is derived from the Summary API. Reading the source directly gives the same number and needs no monitoring stack. |

---

## 11. Open questions and risks

| Item | The constraint that decides it |
|---|---|
| Whether the CSI driver in use implements `NodeGetVolumeStats`. It is optional in the CSI specification, and without it the kubelet reports no volume entry and OBS-06 blocks. | PR 1, against the F-008 cluster. A block there is a real finding: no per-volume usage data means no per-volume capacity monitoring for anyone. |
| Whether the export is directory-backed with no per-volume quota, in which case `df` reports the backing filesystem and the two sources may agree with each other while both describe the wrong thing. | The precheck in 5.6. If `df`'s total is not the claim's capacity, the agreement assertion still holds but the finding is recorded, and what a capacity rule could mean on that deployment is the open part. |
| Whether the server declares a memory limit at all. `nfs-server-provisioner` charts commonly ship without resource limits. | PR 3. No limit blocks OBS-05 and is recorded in `environment.json`. |
| Whether a bounded small-file storm moves the server's working set measurably within a case budget. | The delta reported by PR 3. If it does not move at all, either the reading is broken or the server's memory is not workload-driven, and the two are told apart by the same reading under the heavier SCALE-04 load in step 9. |
| `nodes/proxy` may be refused on a locked-down cluster, which blocks three of the four cases at once. | The first run. The classification is already in the design; what is open is whether a kubeconfig that can exec into pods but not proxy to nodes is common enough to need a second path. |
| The availability window from the API and the outage measured from a client pod are two clocks and two definitions. | The existing guard band. Overlap is asserted, the difference is reported, and nothing fails on a margin narrower than the guard. |
| This phase leaves the plan's original question, whether anyone is actually paged, unanswered by design. | Whether the project ever wants it. If it does, the answer is a separate, deployment-specific check against that deployment's own monitoring, not a case in a portable suite. Stated in 5.8 so it is a decision rather than a gap. |
| None of the four has been run against a real cluster. | The first run. Expect it to change the tolerance in 5.6 and the delta in 5.5. |

---

## 12. Verification

One check per rule in section 2, observable from a run.

| Rule | Check |
|---|---|
| 1. No stack, no hosted backend | The suite creates no Deployment, Service or CRD, and makes no call to any host other than the API server. Reviewable from the diff and from the absence of any new module dependency. |
| 2. No alert is read or asserted | No case reads a rule, a threshold or a severity, and the word does not appear in an assertion. `AlertSLO` is renamed so the vocabulary matches. Grep-able. |
| 3. The input is asserted: present, timestamped, current, correct | Each case fails on a missing reading, on a reading with no timestamp, on one older than the freshness bound, and on one that contradicts the workload's own view. Four distinct failure messages, unit tested on recorded source output. |
| 4. Unreachable is blocked, silent is failed | Unit tests on the classifier: a 403 on `nodes/proxy` and an absent `metrics.k8s.io` produce blocked naming the verb or the API; a Summary API that answers with no entry for the claim, and an API that never shows the server unready across a real outage, produce failures whose messages name the deployment. |
| 5. An unestablished precondition is blocked | OBS-05 with a reading that never moved reports blocked with the delta. OBS-07 with no successful read reports `absent` and blocks. |
| 6. No ceiling chase | Neither case has a code path that writes toward a capacity threshold or a memory limit. Both write a fixed bounded delta from `pkg/slo` and stop. Reviewable from the two constants and their call sites. |
| 7. Agreement has a stated tolerance | OBS-06 compares `df` against a kubelet reading whose timestamp is later, within a tolerance from `pkg/slo`. Unit tests: a reading that is fresh but outside tolerance fails, one inside tolerance but stale fails on freshness, and one inside both passes. |
| 8. No new fault | `pkg/chaos` gains nothing in this phase. The two cases that inject use `DeleteServerPod` unchanged. |
| 9. Bounds live in `pkg/slo` | The five constants are defined there with the existing ones, and no case in this phase contains a duration or a fraction literal. |
| 10. Phases 3 and 4 still hold | `make test-chaos` and `make test-data` pass unchanged across the three PRs, and OBS-02, OBS-03 and OBS-04 are not modified by any of them. |

Sources for the platform claims above: the Google Cloud Managed Service for
Prometheus documentation for managed collection storing in Cloud Monitoring and
for the `frontend` Deployment being the Prometheus-compatible query path
([query API and UI](https://docs.cloud.google.com/stackdriver/docs/managed-prometheus/query-api-ui),
[prometheus-engine](https://github.com/GoogleCloudPlatform/prometheus-engine));
the Kubernetes API reference for the `proxy` subresource on Nodes, for pod
conditions carrying `lastTransitionTime`, and for EndpointSlice readiness; the
kubelet Summary API types in `k8s.io/kubelet/pkg/apis/stats/v1alpha1` for
`pvcRef`, `capacityBytes`, `usedBytes`, `workingSetBytes` and the per-reading
timestamp, and the kubelet reference for `--volume-stats-agg-period`; the CSI
specification for `NodeGetVolumeStats` being an optional node capability; and
the metrics-server documentation for memory usage being reported as working set.

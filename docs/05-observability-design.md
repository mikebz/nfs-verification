# 05: Closing out observability: the data an operator can monitor on

Author: Claude Code
Created: 2026-09-11
Updated: 2026-09-11

Phase boundary: the cases that ask whether this deployment publishes anything an
operator could monitor its storage with. Ends when OBS-05, OBS-06 and OBS-07
run against a real cluster and each reports what the deployment serves or names
what it does not, and when OBS-01 reports whether the availability signal
represents NFS at all. Cadence: three pull requests, following
[`plan.md`](plan.md) step 7, on top of step 6. Prior phases:
[`02-chaos-operations-design.md`](02-chaos-operations-design.md),
[`03-grace-and-lock-reclaim-design.md`](03-grace-and-lock-reclaim-design.md),
[`04-data-path-and-locktool-design.md`](04-data-path-and-locktool-design.md).
This is the next phase of 03, which built the log channel and left the metrics
channel open in its section 11. It supersedes one line of that document: doc 03
section 3 lists "asserting that an alert fired" as OBS-01's requirement, needing
a monitoring stack the suite does not deploy. That is no longer the boundary,
for the reasons in section 1 here. Doc 03 itself is not edited, since a numbered
design doc records what was decided at the time; this paragraph is where the
change is recorded.

**Amendment, 2026-09-11, after the first run of OBS-06 (rule 3, and 5.2).** A
deployment that does not implement what a case is about now reports **blocked**
citing the finding that records it, rather than failing. The owner's decision,
taken after the run behind F-009: a limitation somebody has written down is a
different result from a defect, and a suite that cannot tell the two apart is
read as broken. The sections below are edited in place rather than left
contradicting each other, because two pull requests of this phase are still to
be implemented from them. What did not change is what counts as a limitation: an
absence the deployment could configure away. A number that contradicts another
number is still a failure, and 5.2 says where the line falls.

---

## 1. Problem and outcomes [decided: verify what the deployment publishes, never the alert rules]

Steps 3 and 4 can injure the server, time the outage and read its log stream.
The test plan words four of its Section 3.5 cases as "an alert fires". This
phase does not test that.

An alert is a rule somebody wrote: a threshold, a duration, a severity and a
routing policy, all organization-specific and problem-specific. A suite that
asserted on them would fail a healthy storage system for a threshold set
differently, and would have to find or install a monitoring stack it does not
own. What is a property of the system under test is whether the inputs those
rules need exist at all.

The second half of that is the harder half, and it is what this revision is
about. An input that exists because Kubernetes produces it for every workload
is not evidence about NFS. A case that deletes a pod and then observes that
Kubernetes noticed is green by construction, and green-by-construction is the
exact failure mode F-008 was written about: a result that looks like a pass and
says nothing about the storage system. A wrong assertion is worse than no
assertion, so every case here is built to be capable of failing on a deployment
that publishes nothing.

Observable outcomes that define done:

- A case can state whether the only in-cluster signal for this server's
  availability is its container's lifecycle, which cannot represent a server
  that is wedged rather than dead.
- A case can state the server's memory working set against a declared ceiling,
  and that the reading moves when the server is worked, or that no ceiling is
  declared.
- A case can state what a pod's `df` says about a volume, what the control plane
  says about the same volume, by how much they disagree, and that both move
  together when the workload writes.
- A case can state whether the server publishes any metrics of its own and
  whether they survive its restart, or that it publishes none.
- Every one of the four reports, rather than passing quietly, on a deployment
  that does not publish what it needs: blocked citing the finding that records
  the absence, and the message names the deployment rather than the NFS server's
  code. What each still fails on is a published number that contradicts another
  published number (5.2).

Requirements: Section 3.5 of [`01-test-plan.md`](01-test-plan.md) for the four
cases, whose wording this phase changes in the same pull request (5.13);
Section 4.2 for the category budget; Section 4.3 for where a failure gets
routed. Findings that constrain this phase: F-008, which is about grace signals and not
about metrics, cited here only for the rule it established, that a case whose
signal the deployment does not publish reports rather than passes, which the
amendment above routes to blocked citing a finding rather than to a failure; F-002 (node sizing, which bounds what the
memory case may do); F-001 and F-003 (ordering around anything that unmounts).

Cases served: **OBS-01** (server unavailable), **OBS-05** (memory approaching a
ceiling), **OBS-06** (volume near capacity), **OBS-07** (metrics survive a
restart). OBS-01 lands here in the half that needs no fault, and is completed in
step 10 by the fault that can fail it (5.6). The volume and memory reader is
shared with SCALE-02 and SCALE-04 in step 9.

---

## 2. Human-readable rules

Each line is testable, and a reviewer can accept or reject the phase from this
section without reading code.

1. The suite deploys no monitoring stack and takes no dependency on a hosted
   one. Every control-plane reading comes through the Kubernetes API server with
   the kubeconfig the suite already uses. The one workload-side reading, `df`
   inside a pod, comes from the existing exec path, because the point of it is to
   be what the workload sees rather than what the control plane says.
2. No case asserts that an alert fired, and no case reads an alert rule, a
   threshold or a severity. Those are the operator's.
3. **One verdict rule, used in every case.** A source the suite cannot reach for
   its own reasons is **blocked**. A deployment that does not publish, declare or
   implement what the case is about is **blocked**, naming the deployment and
   citing the `F-NNN` entry that records it. A precondition the case failed to
   create by its own actions is **blocked**, with the number reached. What a case
   still **fails** on is data: two published numbers that contradict each other,
   or one that does not move when the workload moves it. Nothing is a capability
   skip either way: a capability flag gates a case off before it runs and takes
   the finding with it, while every blocked verdict here is reached by probing
   live and says what was found.
4. No case passes on a signal its own fault produced. Deleting a pod moves every
   container-derived signal by construction, so no case asserts on one.
5. A case that reads a live value shows it moves. A frozen counter and a gauge
   that ignores its subject both exist and neither is usable. A case that reads
   configuration rather than a value is exempt and says so.
6. Build only what a case asserts on. No reader, endpoint or classifier lands
   for a series no remaining assertion reads.
7. No case drives the server to OOMKill, and no case fills a filesystem it does
   not own.
8. Two readings of one quantity taken by two samplers at two moments agree
   within a stated tolerance derived from the slower sampler's period, or the
   case says by how much they disagreed.
9. No bound is asserted that cannot fail. A timing assertion belongs in the
   Section 3.8 table with a fault that can violate it, or it is reported rather
   than asserted.
10. Every rule from phases 3 and 4 still holds. This phase adds two readers, it
    does not amend the contract around the existing cases.

---

## 3. Scope

**In scope for this phase**

- A reader for the kubelet Summary API: per-volume capacity and usage with the
  kubelet's own timestamp and its claim reference, and per-container memory
  working set from the same call. One reader, two cases.
- A probe for the NFS server's own metrics endpoint, and a minimal scanner for
  the Prometheus text format it would serve.
- Reads of objects the suite already uses: the server pod's spec, its container
  statuses, and the endpoints of the Service in front of it.
- A bounded write that moves a volume's usage, and bounded metadata churn from
  the phase 6 helper, which creates empty entries, to move the server's working
  set.
- The four cases, and the artifacts each writes before teardown.

**Out of scope (explicit non-goals)**

- Alert rules, thresholds, severities and routing. Rule 2.
- Prometheus, Alertmanager, Grafana, kube-state-metrics, Cloud Monitoring and
  Managed Service for Prometheus. See 5.1: querying Google's managed collection
  means querying Cloud Monitoring through a frontend proxy the operator deploys,
  which is a dependency this suite will not take.
- The kubelet's Prometheus endpoints, cAdvisor series, `metrics.k8s.io`, a
  polling sampler and a continuity classifier over them. All were in an earlier
  revision of this design and none is read by an assertion that survived. Rule
  6, and 5.11 says what each would have served.
- Filling a volume to a capacity threshold, or driving the server to its memory
  ceiling. Rule 7.
- Any new fault. Step 10 brings the one this phase needs and does not have
  (5.6).

**Depends on**

- Server pod discovery from phase 3.
- The directory population and deletion helpers from phase 6, which are the
  bounded storm OBS-05 uses.
- Read access through the API server's node proxy, for the kubelet, and through
  its pod proxy, for the server's own endpoint. The suite exercises neither
  today, so both are new access and both are named in 5.4 and section 8.

---

## 4. Data contract

Everything here is produced by a case, written to the artifact bundle before
teardown, and consumed by whoever reads a failure. None of it is stored between
runs. The system of record is the cluster: the suite copies what the API server,
the kubelet or the server said, and never computes a value it then presents as
measured.

### 4.1 The availability configuration

What OBS-01 reads. Configuration, not a measurement, which is why rule 5 exempts
it.

| Field | Type | Meaning |
|---|---|---|
| `Pod`, `Container` | string | The server pod and the container serving NFS. |
| `ReadinessProbe` | struct or absent | The probe as declared: its kind, its target port, its period, timeout and failure threshold. Absent is the interesting value. |
| `ProbeTargetsNFS` | bool | Whether the probe's target is the NFS service rather than something incidental. A probe on a sidecar's health port says nothing about whether NFS answers. |
| `Service` | string or absent | The Service in front of the server. Discovery records server pods and exports today and no Service, so this case defines it: among Services in the server's namespace, the ones whose selector matches the server pod's labels, and of those the one exposing the NFS port. None matching is recorded rather than guessed at, and the case then reports on the probe alone. |
| `ServicePorts` | []int32 | The ports that Service exposes, so the case can say which of them readiness is gating. |
| `EndpointsDrivenByReadiness` | bool | Whether the Service's endpoints follow the pod's readiness, which is what makes the signal reach a client's connection rather than only a dashboard. |
| `BlindWindow` | duration or unknown | Period times failure threshold: how long a server that stops answering stays marked Ready. Unknown when no probe is declared, which is the point. |

### 4.2 The volume usage reading

| Field | Type | Meaning |
|---|---|---|
| `Source` | enum | `pod-df` or `kubelet-summary`. |
| `Claim` | string | The claim this reading is about. |
| `CapacityBytes`, `UsedBytes`, `AvailableBytes` | int64 | As that source reports them. Available is recorded rather than derived: on a filesystem with reserved blocks, used plus available does not equal capacity, and deriving would invent agreement. |
| `At` | time.Time | The kubelet publishes its own timestamp per volume; the pod reading carries the workstation clock at the exec. |

Identity: the claim, the source and the time. Every comparison is between two
rows with the same claim and different sources.

### 4.3 The memory reading

| Field | Type | Meaning |
|---|---|---|
| `Pod`, `Container` | string | Which container. A pod with a sidecar has more than one, and only the one serving NFS is the subject. |
| `WorkingSetBytes` | int64 | From the same kubelet call as 4.2. |
| `LimitBytes` | int64 | From the pod spec. Zero means no limit is declared, which blocks the case citing the finding for it: an undeclared ceiling is a deployment property rather than a harness gap, and 5.2 row two says how it is reported. |
| `Fraction` | float64 | Working set over limit, computed only when a limit exists. |
| `At` | time.Time | The kubelet's sample time. |
| `OOMKills` | int32 | Terminations with that reason so far, from the container status. |

### 4.4 The server's own series, and the restart verdict

What OBS-07 reads, and only where the server publishes it.

| Field | Type | Meaning |
|---|---|---|
| `Name` | string | The series name, verbatim. |
| `Labels` | map[string]string | The label set, verbatim. |
| `Value` | float64 | The sample value. Counters and gauges are not distinguished by the scanner: what a series means is the case's assertion, not a property of the parse. |
| `At` | time.Time | The workstation clock at the read. The exposition format carries an optional timestamp and servers commonly omit it, so there is no source clock to prefer. |

The restart verdict is one of four, exhaustive over what two reads of **one
series from this endpoint** can show either side of a restart. It is not a
classification of every reading in this phase: volume usage and working set are
gauges that may legitimately fall for reasons that have nothing to do with a
restart, and neither is read by this case.

The two `resumed-` verdicts both pass, so the split between them labels the
record rather than deciding the case. That is deliberate: a server may publish
gauges as readily as counters, a gauge that fell is not evidence of anything,
and a verdict that failed on it would be asserting that every series a server
publishes must be a counter.

| Verdict | Shape | Meaning |
|---|---|---|
| `resumed-reset` | Series answers on both sides, values lower after | Consistent with a process that restarted and began counting again. |
| `resumed-continuous` | Series answers on both sides, values not lower | The series outlived the process. Lawful for anything the server derives from outside itself. |
| `never-resumed` | Answered before, not after, past the resume bound | The metrics did not survive the restart. |
| `absent` | The server publishes no endpoint to read | Blocks the case, citing the finding that records it. 5.9 says why nothing stands in for it. |

### 4.5 Ownership and evolution

Producer: the cases. Consumer: whoever reads the bundle. No schema in a database
and no cross-run comparison, so the only compatibility question is what
`environment.json` gains, since a cached preflight record is replayed and must
gate a rerun exactly as the original run did.

Added: the server's declared memory limit, the server's metrics endpoint or the
record that it declares none, and the readiness configuration from 4.1. All
three are recorded because they are facts about the deployment that every
failure report in every category benefits from carrying.

**Nothing is added to `Capabilities`.** An earlier revision proposed four
capability flags for these sources. A capability gates a case off, and every one
of these absences is a finding this phase exists to report, so gating on them
would hide the finding on exactly the runs that should report it. The cases
probe live and report what they found. This is rule 3's last sentence, and it
is the one piece of the contract that an implementer could get wrong without any
test noticing: a blocked verdict reached by probing carries the finding, and a
capability flag checked beforehand takes it away.

Backward compatible: a record written before this phase decodes with the three
new fields empty, and nothing gates on them.

---

## 5. Design and decisions

### 5.1 What a GKE cluster actually serves, and why there is no Prometheus here [decided: no monitoring stack, no hosted backend]

Checked rather than assumed. Google Kubernetes Engine runs no in-cluster
Prometheus server. It runs collection: the collector and metrics-agent
DaemonSets that F-002 already observed on these nodes ship metrics out of the
cluster. Google Cloud Managed Service for Prometheus stores them in Cloud
Monitoring, and the Prometheus-compatible query API is served by a frontend
Deployment the operator installs, which is an authorizing proxy in front of
Cloud Monitoring rather than a store in the cluster. Querying it is a dependency
on Cloud Monitoring, which this project does not have and does not want, and
installing anything to create one violates rule 1.

So this phase reads two things, both through the API server:

| Source | Serves | Present without any add-on |
|---|---|---|
| The Kubernetes API: pod spec, container statuses, Service endpoints | The availability configuration (4.1) | Yes, it is the API server |
| The kubelet's stats summary, through the node proxy | Per-volume capacity and usage, per-container working set (4.2, 4.3) | Yes, it is the kubelet |
| The server's own metrics endpoint, through the pod proxy | Whatever the server publishes about itself (4.4) | Only if the server publishes it |

### 5.2 One verdict rule [decided: an absence is blocked and cites its finding, a contradiction fails]

An earlier revision stated the verdict for a missing volume-stats entry four
different ways in four places. That is worse than choosing wrong, because an
implementer picks one at random and the suite reports differently depending on
which section they read. One rule, stated once, applied everywhere:

| What happened | Verdict | Why |
|---|---|---|
| The suite could not reach a source for its own reasons: the node proxy refused it, the kubeconfig lacks the verb | **blocked**, naming what was refused | Harness access. Nothing was learned about the deployment. |
| The deployment does not publish, declare or implement what the case is about: no metrics endpoint, no memory limit, no volume stats from the driver, no per-volume quota, no readiness probe that tests NFS | **blocked**, naming the deployment and citing the `F-NNN` entry | The finding is real and it is written down. It is a limitation of this deployment rather than a defect in what it does implement, and the two are triaged by different people. |
| The case could not create its own precondition: the storm did not move the reading inside the budget | **blocked**, with the number reached | The case did not establish what it needed. Rule 5 forbids calling that a pass. |
| The deployment publishes the input and the numbers do not hold up: two sources disagree beyond tolerance, or a reading does not move when the workload moves it | **fail**, naming both readings | Nothing here is absent. What is published is wrong, and an operator acting on it is acting on a number that does not describe their volume. |

The line between rows two and four is the whole of this rule. Row two is an
absence the deployment could configure away: turn the quota option on, declare a
memory limit, add a probe, expose an endpoint. Row four is a number that
contradicts another number, which no configuration change explains and which a
green run would hide. A case reaches row two by probing live and reporting what
it found, so nothing is gated off before it runs and nothing disappears on a
replay. Either way the message routes per Section 4.3: it names the deployment
and its configuration, not the NFS server's code.

**Every row-two verdict cites an entry in `findings.md`.** That is the
load-bearing half of the rule, because a blocked line scrolls past and the entry
is what survives the run. Where a case meets an absence nothing has recorded
yet, it reports blocked saying exactly that, and writing the entry is part of
closing out the run. F-008 is the precedent for the shape rather than for the
verdict: OBS-03 was written to fail on a server that never announces grace and
is not modified here, and a phase whose four cases are each expected to meet a
different absence on this provisioner is not the same situation as one case
meeting one.

The previously carved-out case is now row two like the rest. An export whose
reported total is not the claim's capacity publishes real numbers, which agree
with each other perfectly and describe the wrong filesystem. The absence behind
it is a per-volume quota, F-009 records it, and the agreement comparison is
still run and recorded so the result arrives with everything a reader needs.

### 5.3 What the outside channel covers, and what it cannot

When the server publishes nothing, its health and operations are reported
entirely by things watching it from outside: the kubelet watching a container.
That covers less of NFS than its volume of metrics suggests, and the boundary
decides what the cases in this phase can honestly claim.

| Question an operator asks | Answered from outside? |
|---|---|
| Is the server process running; did it restart, and when | Yes, from container status |
| How much CPU and memory is it using | Yes, from the kubelet |
| Was it OOMKilled | Yes, when a limit exists to kill against |
| How full is a volume | Yes, when the driver implements volume stats |
| Did a mount, unmount or provision succeed | Yes, but that measures the client and driver side, not the server |
| **Is the server answering NFS at all** | **No.** Nothing probes port 2049 on this chart |
| **NFS operation and error rates** | **No.** Nothing counts READ, WRITE, COMMIT or LOCK, or what error was returned |
| **Clients holding state, locks held, open files** | **No.** That lives in the server |
| **Is it in grace, did reclaims succeed** | **No.** A log line only, and F-008 established this server emits none |
| **Which export is busy or failing** | **No.** Exports are invisible from outside the process |

The line through the table: the outside channel reports the **container** and
the **Kubernetes storage plumbing**, and says nothing about the **NFS
protocol**. Two facts about this chart, read from its own StatefulSet template,
make it thinner still, and both are load-bearing for the cases below:

- **It declares no liveness or readiness probe.** Twelve ports are named, 2049
  among them, and nothing probes any of them. A pod's readiness therefore tracks
  the container's lifecycle: a server wedged but not exited reads Ready and keeps
  its endpoints, serving traffic that hangs. This is OBS-01's finding (5.6).
- **It sets no resource limits by default.** The chart applies resource settings
  only when values supply them. No memory limit means no ceiling to monitor
  against, and no kubelet OOMKill either: the node's OOM killer fires instead,
  which is less visible and worse for every other pod on that node. This is
  OBS-05's finding (5.7).

### 5.4 How the harness reaches what it reads [decided: the API server's proxy subresources, no exec and no scraper]

The harness runs on a workstation. The kubelet and the server both speak HTTP
inside the cluster, and the API server proxies to both: to a node for the
kubelet's stats, and to a pod for the server's own endpoint. Both are ordinary
`GET` requests on the client the suite already holds, authorized by the same
kubeconfig.

Nothing in this phase opens a shell, logs into a node, deploys a scraper, or
uses the privileged node agent. The node agent exists for `/proc/mounts`, dmesg
and process signals, and this phase does not touch it. Reaching the kubelet
directly instead would mean the node's address, its port and its certificate,
none of which a workstation outside the cluster should need.

The server's endpoint is addressed as a pod rather than through a Service: a
Service in front of a fan-out answers from whichever pod it picks, and the case
is about one server.

Access needed: read on the node proxy, and read on the pod proxy. A refusal on
either is classified as blocked naming the verb, never as a deployment that does
not publish. Rule 3, row one, and this is the only place in the phase where a
permission gap could masquerade as a finding.

The exposition format is one sample per line, so the scanner that reads the
server's endpoint pulls named series out of a streamed response and needs no
library, no expression language and no scrape configuration.

### 5.5 Showing a reading is live [decided: two reads where the value is a number, the recorded event time where it is a time]

"The number is there" is a weak claim. A counter frozen at a value and a usage
gauge that ignores writes both exist and neither is usable, so every case that
reads a live value also shows it moving.

A baseline read is needed where the value is a number and nothing else, because
a counter carries no time dimension: the same value means "busy cluster, nothing
just happened" or "quiet cluster, one operation", and only a second read tells
them apart. Where the value is itself a time, the suite already recorded when it
caused the event, and the comparison is against that:

| Reading | What establishes the change |
|---|---|
| A series from the server's own endpoint (4.4) | Two reads, before and after the restart |
| Volume usage on a claim the case created (4.2) | Two reads around a bounded write. The claim starts empty, so the absolute value is also assertable; the baseline is kept because it costs one call and survives a claim that did not start empty |
| The working set under load (4.3) | Samples across the storm, compared against the first |
| The fact that the server restarted | One read of the replacement, against the fault time already on the timeline. **Not the restart count**: the phase 3 fault deletes the pod and its controller creates a new one, whose count starts at zero, so a count assertion would fail on the healthy path. What identifies a replacement is a new pod identity and a start time after the fault. A count increase belongs to an in-place container restart, which is CHAOS-01's fault, not this one |

Magnitude is asserted only where the suite knows it: the bytes it wrote. The
clocks differ, the fault timeline being the workstation's and the container's
start time the node's, so those comparisons use the guard band the suite already
applies to two-clock comparisons. At these magnitudes it decides nothing, and
using it keeps this comparison shaped like every other one rather than being the
place that assumes the clocks agree.

### 5.6 OBS-01: does the availability signal represent NFS [decided: assert the signal, defer the fault to step 10]

The obvious construction is to delete the server pod and watch readiness go
false. It is also worthless: Kubernetes sets readiness false and drops the
endpoints for any deleted pod of any workload, so the case would pass on a
deployment with no monitoring at all. OBS-02 already reads pod starts after that
same fault. Rule 4 exists for this case.

The outage that matters is the one this chart cannot see: a server wedged but
still running. Its container is up, its counters advance, and with no readiness
probe its endpoints keep receiving clients that hang. So OBS-01 splits, and the
split is stated here rather than discovered when it fails to fail:

**In this phase, with no fault at all.** Read the availability configuration
(4.1) and assert that this deployment has a signal that can represent NFS
reachability: a readiness probe that targets the NFS service, with the Service's
endpoints following it. On a deployment with no such probe, report blocked
citing the finding for it, naming that the only in-cluster availability signal
is the container's lifecycle and that a wedged server is invisible to it. Record the blind window, which is the probe's
period times its failure threshold, or state that it is unbounded where no probe
exists. Where the suite cannot see a signal an operator may have outside the
cluster, the message says so, the way OBS-03's message names its flags.

**In step 10, with the fault that can fail it.** Stop the server process without
killing it, which the node agent can already do, and assert that the signal goes
false within a bound. That bound belongs in the Section 3.8 table at that point,
because there will then be a fault that can violate it. DATA-05's byte-range
half was deferred by doc 03 and delivered in step 6; this is the same shape.

What this costs: OBS-01 in step 7 asserts a configuration rather than a
behavior, and Section 3.5 is therefore not fully closed out by this step. That
is recorded in the plan rather than papered over.

### 5.7 OBS-05: a ceiling that is declared, a reading that moves [decided: no ceiling chase, and an undeclared ceiling is a finding]

The plan's expected result is "alert fires before OOMKill". The alert is a rule,
and producing the OOMKill would mean deliberately destroying the cluster's
storage to observe an ordering. Rule 7.

What the case asserts:

1. The server container declares a memory limit. Without one there is no
   denominator and nothing for any threshold to sit below, so the case reports
   **blocked** citing the finding, naming the container. Rule 3, row two: the
   deployment has not declared the ceiling it would be monitored against, and
   this chart declares none by default (5.3).
2. Its working set is readable against that limit, with a timestamp.
3. The reading tracks reality: a bounded workload through the export moves the
   working set by a measurable margin. What the stimulus is must be stated
   precisely, because an earlier revision called it a write storm and it is not
   one. The phase 6 population helper creates empty entries, so this is metadata
   churn, and what it exercises is the server's handle and export caches rather
   than its data path. SCALE-04 in step 9 is the case built around small-file
   writes, and this one does not duplicate it.
4. No OOMKill occurred. If one did anyway, it is reported with its timestamp and
   the reading that preceded it, which is the ordering the plan cares about,
   observed rather than manufactured.

The storm is bounded by the case budget and the claim's capacity, is nowhere
near any limit by construction, and its files are deleted before teardown
reaches the claim.

If the reading does not move inside the budget, the case reports **blocked**
with the delta it saw, and the reason it is not a failure is worth stating,
because the neighbouring case answers the same shape differently. Three things
produce a flat reading and this case cannot tell them apart: a broken input, a
server whose memory is not driven by metadata churn, and a working set that grew
and was reclaimed between two samples, which is lawful for a reclaimable gauge
sampled asynchronously. Failing would assert the first without evidence.
SCALE-04's heavier and longer load in step 9 is what distinguishes them.

OBS-06 is the contrasting case, and the contrast is the point: there the suite
knows exactly how many bytes it wrote and has `df` confirming they landed, so a
control plane reading that did not move is unambiguously wrong and fails.

### 5.8 OBS-06: two sources, one quantity, and a write to prove they track [decided: no threshold fill, and an absent reading is a finding]

- `df` inside the pod, which is what the workload sees.
- The kubelet's entry for the same claim, which is what the control plane sees.
  No entry means the CSI driver does not implement volume stats, which the CSI
  specification makes optional. The case reports **blocked** citing the finding,
  naming the driver: a volume the control plane cannot measure is one nobody can
  monitor for capacity. Rule 3, row two, and this is the verdict an earlier
  revision stated four ways.

What "agrees" means, since two samplers at two moments never produce equal
numbers. The kubelet aggregates volume stats on a period that defaults to one
minute, so its answer is stale by up to that period. The case reads `df` at a
moment, waits for a kubelet reading whose own timestamp is later, and compares
used bytes within a tolerance expressed as a fraction of capacity, with the
freshness requirement stated separately. Both bounds live in `pkg/slo`. A
disagreement beyond tolerance fails, and the message prints both rows, both
timestamps and the delta in bytes, because the failure that matters is a control
plane reporting a volume as nearly empty while the workload is getting ENOSPC.

Then a bounded write and the comparison is repeated. Its size is an absolute
number of bytes in `pkg/slo`, not a fraction of the claim: a fraction is tens of
gigabytes on a multi-terabyte claim and below the tolerance on a small one, so it
would be neither bounded nor meaningful across deployments. The comparison is
against the bytes actually written, which the write reports, rather than against
the size that was asked for. Two sources that agree on a static number prove
less than two that move together. A control plane reading that did not move
**fails**: `df` confirms the bytes landed and the suite knows how many it wrote,
so there is no second reading of a number that stayed put.

The quota check reports **blocked** citing F-009, and it runs last so that
everything above it is measured and recorded first. `df`'s reported total is compared against the
claim's capacity; a directory-backed export with no per-volume quota reports the
backing filesystem's size instead, and no threshold on that number says anything
about this claim. The cause is nameable on this provisioner, whose XFS quota
option is off by default, so an export is a subdirectory with no per-volume
limit at all.

Two readings that agree with each other about the wrong filesystem is exactly
the shape a passing case would hide, which is why this is row two of 5.2 rather
than a note, and why the entry it cites is the part that has to exist. The agreement table is still written, so the failure says both that the
sources agree and that what they agree about is not the volume.

### 5.9 OBS-07: the server's own metrics, and nothing standing in for them [decided: nothing stands in for a server that publishes nothing]

The plan's OBS-07 is about metrics surviving a restart, and the first question is
whose. An earlier revision let the case fall through to the kubelet's container
counters when the server published nothing, and pass. That is the F-008 shape
again: on this deployment the case would always pass, having observed that
cAdvisor kept answering, which is true of every container in the cluster and
says nothing about NFS.

So the case asserts on the server's own endpoint and nothing stands in for it:

1. Find the metrics port. A port named for metrics on the server's own
   container is the direct answer. A Service may name one instead, and there the
   Service's port number is not what the pod proxy needs: the proxy addresses the
   pod, so the Service's target port is resolved to the container's port first.
   A Service port that resolves to no declared container port is not probed, and
   is recorded as such rather than guessed at. Then read it through the pod
   proxy. No port declared anywhere, or a port
   that serves nothing readable, reports **blocked** citing the finding: this
   deployment publishes nothing about NFS itself, so there are no metrics to
   survive anything. The message names that, the way OBS-03 names a server that
   is silent about grace.
2. Where an endpoint answers: read the series, delete the server pod with the
   phase 3 operation, wait for the phase 3 recovery, read again, and classify
   with the table in 4.4.
3. Fail on `never-resumed`. Pass on `resumed-reset` and `resumed-continuous`,
   which are the two shapes Section 3.5 names as acceptable. Record the gap
   between the last good read and the first after: a hole that shows the outage
   is what an operator should see, not a defect.

The restart itself is read from container status, which already carries what is
needed: a replacement pod identity and a start time after the fault. Two
consequences the implementation must not get wrong, both of which follow from
the fault deleting the pod rather than restarting its container:

- **The restart count does not increase.** The replacement is a new pod and its
  count starts at zero. Anything asserting an increase fails on the healthy
  path. The count is the right signal for an in-place restart, which is
  CHAOS-01's fault, and the wrong one here.
- **The second read addresses a different pod.** The endpoint is reached through
  the pod proxy, so the post-restart read resolves the server again through
  discovery rather than reusing the old pod's name, and a series whose labels
  carry the pod identity will legitimately differ across the two reads. A case
  that compared the old labels would classify a healthy replacement as
  `never-resumed`.

Known consequence, stated rather than discovered: the provisioner this project
runs against declares eleven command-line flags and none concerns metrics, so
OBS-07 is expected to report blocked on this deployment, and the pass path will first be
exercised somewhere else. Section 11 carries what that means for confidence in
the scanner.

### 5.10 What this phase does not claim

- It does not show anyone is paged. No rule is read or evaluated.
- It does not show a threshold is set anywhere, or set sensibly.
- It does not show data is retained. Every reading is live.
- It does not show the availability signal detects every outage. OBS-01 asserts
  the signal exists and can represent NFS; whether it moves when a wedged server
  stops answering needs the step 10 fault (5.6).
- It does not observe NFS operations or NFS errors anywhere, on a deployment
  whose server publishes nothing. The table in 5.3 is the boundary, and OBS-07
  reports it rather than implying otherwise.

What a green run does show, stated no more strongly than the cases support: this
deployment declares a readiness signal wired to the NFS service, declares a
memory ceiling and reports a working set against it that responds to load,
reports per-volume usage that agrees with the workload's own view and tracks its
writes, and publishes server metrics that survive a restart. That is a real
answer, and on the deployment in front of this project most of it is expected to
come back red.

### 5.11 What is deliberately not built, and what it would have served

An earlier revision of this design proposed the kubelet's four Prometheus
endpoints, a text scanner over all of them, a polling sampler, a continuity
classifier across several series, and `metrics.k8s.io` as a cross-check. None
survives, because after the alert assertions were dropped no remaining
assertion reads them. Recorded here so the next reader does not rebuild them
without a case:

| Not built | What it would have served | Why nothing needs it |
|---|---|---|
| The kubelet's own metrics endpoint | Storage and CSI operation counters, as a differential on mount and provision | No OBS row is about the CSI operation path; those counters measure the driver, not the server. If a case ever needs them, it arrives with the case |
| cAdvisor series | The restart marker for OBS-07 | Container status already carries the start time and restart count |
| The resource metrics endpoint | A working set cross-check | Duplicates the stats summary this phase already reads |
| The probes endpoint | Probe counters for OBS-01 | Recorded-not-asserted on a chart with no probes, which is the finding OBS-01 already reports from configuration |
| `metrics.k8s.io` | A second memory source | Same number as the stats summary, with an add-on dependency the summary does not have |
| A polling sampler and continuity classifier | Verdicts across several series | OBS-07 is two reads either side of one restart |

The scanner survives, narrowed to the server's own endpoint, because OBS-07's
pass path reads it.

### 5.12 Naming, gating and the category budget

Four cases: `TestObsAvailabilitySignalRepresentsNFS`,
`TestObsServerMemoryIsReadableAgainstLimit`,
`TestObsVolumeUsageAgreesWithControlPlane`,
`TestObsServerMetricsSurviveRestart`.

All four are `TestObs` and run under `make test-obs`. Only OBS-07 injects a
fault, and it is the pod delete that target already carries for OBS-02 and
OBS-03. With the sustained outage, both threshold chases and the pollers gone,
the phase adds one fault-injecting case and three cheap ones, so the Section 4.2
budget of 45 minutes is expected to hold. Measured in the last pull request
rather than asserted here.

### 5.13 Test plan wording, and the SLO constant [decided: edit Section 3.5, and delete the constant]

Requirements live in the test plan, so the four rows in Section 3.5 change with
this design rather than in a commit message. Each keeps its ID and subject and
states the claim the case will stand on, with the reach path left here where it
belongs. A line under the table says alert rules are out of scope. OBS-01's row
says its behavioral half lands in step 10.

`AlertSLO` in `pkg/slo` is five minutes, commented as the deadline for an
availability alert to fire, and read by nothing. An earlier revision renamed it
and kept the value. The value was an alert's `for` duration, and nothing in this
phase can violate five minutes: a deleted pod goes NotReady in seconds, and
OBS-01 no longer makes a timing assertion at all. A bound that cannot fail is
not an SLO. **The constant is deleted**, and no row is added to Section 3.8. The
bound for the step 10 wedge fault is written when that fault exists and can
violate it.

---

## 6. Delivery phases

Three pull requests, smallest runnable slice first.

**MVP, PR 1: the kubelet reader, and OBS-06.** The stats summary reader, the
volume usage rows, the quota precheck as a recorded finding, the agreement
assertion, the bounded write and the quota check. It needs no fault and is the
case in this phase most directly about storage. It produces a result on any
cluster already meeting the suite's Section 0 preconditions, an RWX class and a
reachable server, which it needs in order to have a claim to measure; what it
does not need on top of those is any monitoring component. Validated outside a unit test by running it against
the GKE cluster in F-008 and reading the table it writes.

**PR 2: OBS-05.** The memory reading from the same reader, the declared-limit
assertion, the bounded storm and the movement assertion.

**PR 3: OBS-01 and OBS-07.** The availability configuration assertion, the
server metrics probe, the text scanner, and the restart comparison. They land
together because both ask what the server itself exposes, not because they share
code: they share none, and either could land alone.

Later phases sharpen after PR 1 reports from a real cluster. Step 10 picks up
OBS-01's behavioral half with the stop-without-exit fault.

---

## 7. Configuration

Source: command-line flags, as everything else in this repository. No config
file, no credentials: every call goes through the API server with the kubeconfig
the suite already uses.

**No new flags.** Every input is answerable by the cluster: which pod serves the
export is discovered, which volume is measured is the claim the case created,
the memory limit is in the pod spec, the readiness probe is in the pod spec, and
the kubelet timestamps its own readings so its aggregation period need not be
declared. The server's metrics port is discovered from the pod spec or the
Service in front of it; a deployment publishing on a port it declares nowhere is
where a flag would earn its place, and section 11 carries it as open rather than
adding one for a shape nobody has met.

Intentionally not configurable: alert names, thresholds and severities, which
are not read at all; the agreement tolerance, the freshness requirement, the two
movement deltas, the absolute write size and the resume bound, which are bounds
and live in `pkg/slo`;
and the sampling interval for the storm, which is fixed until something needs it
to vary.

---

## 8. Deployment decisions

Runtime unit: the same test binary on a workstation. Nothing is deployed into
the cluster, and no object is created beyond the pods and claims every case
already creates.

Lifecycle: readings stop before teardown and their tables are written to the
bundle before any object is deleted, since a reader still running through
teardown would record a deletion as an outage. Each read is individually
bounded, so a kubelet that has stopped answering cannot stall a case and one
sick node cannot starve collection from the healthy ones. The one fault is the
phase 3 pod delete, which brings its own recovery assertion and teardown
ordering, unchanged.

Access scope: read on the node proxy and on the pod proxy. Both classified on
failure so a permission gap reports blocked rather than being filed as a
deployment defect.

Cost: OBS-05 and OBS-06 each write a bounded amount through the export and
delete it before teardown. Neither approaches a ceiling.

---

## 9. Observability

Written to the run's artifact directory before teardown, because a case that
tore down its evidence is unreproducible (Section 4.3).

| File | Contents |
|---|---|
| `availability-signal.txt` | The configuration from 4.1: the probe or its absence, what it targets, the blind window, and whether endpoints follow it. |
| `volume-usage.txt` | The rows from 4.2, both sources, both timestamps, the delta in bytes, before and after the bounded write. Written whether the case passed or not: a run where the sources differed by 2% is a different run from one where they agreed exactly, and the pass looks identical without it. |
| `server-memory.txt` | The readings from 4.3, the declared limit, the fraction, the movement under load, and any OOMKill with its timestamp. |
| `server-metrics.txt` | What the server's endpoint served and the restart verdict, or the record that it declares no endpoint and which ports were examined. |

The fault timeline is unchanged: the pod delete records itself through the
existing mechanism.

---

## 10. Alternatives considered

| Option | What it is | Why not chosen |
|---|---|---|
| Query a Prometheus-compatible API in the cluster | Read series and alerts from a monitoring stack | GKE runs no in-cluster Prometheus, and querying managed collection is a Cloud Monitoring dependency. 5.1 |
| Install Prometheus and rules for the run | Deploy what the first design needed | Tests the rules the suite just wrote, and leaves a stack in someone's cluster. Rule 1 |
| Assert on alert rules where a stack happens to exist | Read them when they answer | Makes the verdict depend on whether the operator runs Prometheus, and asserts on thresholds that are organization-specific. Rule 2 |
| Delete the server pod and assert readiness went false | The obvious OBS-01 | Kubernetes does that for every deleted pod of every workload. Green by construction, and OBS-02 already reads that fault. Rule 4 |
| Let OBS-07 fall back to cAdvisor and pass | Assert the weaker thing and log it | Always passes on a server that publishes nothing, which is the silent green F-008 warned about. 5.9 |
| Gate these cases on capabilities | Skip where a source is absent | A replayed preflight then hides exactly the findings this phase exists to report. 4.5 |
| Keep the five-minute bound under a new name | Rename `AlertSLO` and assert it | It was an alert's `for` duration, nothing here can violate it, and a bound that cannot fail is not an SLO. 5.13 |
| Fill a volume to a threshold, or drive memory to its ceiling | Approach the number a rule would fire on | The threshold is the operator's, so approaching it proves nothing, and both risk the cluster. Rule 7 |
| A NetworkPolicy partition to sustain an outage | Outlast an alert rule's duration | Only needed to make a rule fire, and no rule is read |

---

## 11. Open questions and risks

| Item | The constraint that decides it |
|---|---|
| Three of the four cases are expected to meet an absence on the deployment this project runs against: no readiness probe, no memory limit, no server metrics endpoint. | The first run. If they do, that is the phase working as designed, and each reports blocked citing its `findings.md` entry, which is what must be written for them to cite. What must not happen is a result being read as a harness defect, which is why each message names the deployment and its configuration. |
| Whether the CSI driver implements volume stats, which decides OBS-06. | Settled by the first run: it does, and the two sources agree exactly. What the run found instead was an absent per-volume quota, recorded as F-009. |
| OBS-07's pass path will not be exercised on this cluster, so the scanner's only run here is the absent path. | Unit tests against recorded exposition text, and the first deployment with a server that publishes. Until then the pass path is covered by tests rather than by a run, and that is stated rather than implied. |
| OBS-01 asserts a configuration, not a behavior, until step 10 adds the wedge fault. | Step 10. The node agent already signals processes, so the operation is small; what is open is the bound, which gets a Section 3.8 row when the fault that can violate it exists. |
| A server publishing on a port it declares nowhere is invisible to OBS-07's probe, and no flag exists for it. | Whether such a deployment shows up. A flag then earns its place and its README row. |
| Node proxy and pod proxy may be refused independently on a locked-down cluster. | The first run. Each is classified separately, so a partial refusal blocks only what it blocks. |
| Whether a bounded storm moves the server's working set measurably inside a case budget. | PR 2. If it does not move at all, either the reading is broken or this server's memory is not workload-driven, and the heavier SCALE-04 load in step 9 tells those apart. |
| None of the four has been run against a real cluster. | The first run. Expect it to change the agreement tolerance and the movement deltas. |

---

## 12. Verification

One check per rule in section 2, observable from a run.

| Rule | Check |
|---|---|
| 1. No stack, no hosted backend | The suite creates no Deployment, Service or CRD, makes no call to any host but the API server, and adds no module dependency. Reviewable from the diff |
| 2. No alert is read or asserted | No case reads a rule, threshold or severity, and `AlertSLO` is deleted rather than renamed. Grep-able |
| 3. One verdict rule | Unit tests, one per row of the 5.2 table: a refused node proxy blocks naming the verb; an absent metrics endpoint, an absent memory limit, an absent volume-stats entry, an absent per-volume quota and an absent NFS-targeted probe each block with a message naming the deployment and citing a finding; a storm that moved nothing blocks with its delta; two published readings that disagree beyond tolerance fail. No case consults a capability, and none is added |
| 4. No case passes on a signal its own fault produced | OBS-01 injects no fault and asserts configuration. OBS-07's assertion is on the server's own series, which a pod delete does not produce. Reviewable from the two cases |
| 5. A live reading is shown to move | OBS-05, OBS-06 and OBS-07 each write a before and after into their table. OBS-06 fails on a control plane reading that did not move, since the bytes written are known and `df` confirms them, and that is row four rather than an absence; OBS-05 blocks with its delta, since no expected memory delta is known, and 5.7 says why the two differ; OBS-07 fails on a series that answered before the restart and not after. OBS-01 is the exempt case and records configuration only |
| 6. Only what a case asserts on is built | The phase adds two readers. The 5.11 table lists what was removed and what would have used it; a reviewer can check no code lands for any row of it |
| 7. No ceiling chase | Neither case has a path that writes toward a capacity threshold or a memory limit. OBS-06 writes an absolute byte count from `pkg/slo`, not a fraction of the claim, and OBS-05's churn is bounded by its budget |
| 8. Agreement has a stated tolerance | OBS-06 compares against a kubelet reading whose timestamp is later, within a tolerance from `pkg/slo`. Unit tests: fresh but outside tolerance fails, inside tolerance but stale fails on freshness, inside both passes |
| 9. No bound that cannot fail | No timing assertion is made in this phase and no row is added to Section 3.8. The step 10 bound arrives with the fault that can violate it |
| 10. Phases 3 and 4 still hold | `make test-chaos` and `make test-data` pass unchanged across the three PRs, and OBS-02, OBS-03 and OBS-04 are not modified |

Sources for the platform claims: the Google Cloud Managed Service for Prometheus
documentation for managed collection storing in Cloud Monitoring and for the
query frontend being the Prometheus-compatible path; the Kubernetes API
reference for the proxy subresources on Nodes and Pods, for pod conditions and
container statuses, and for endpoint readiness; the kubelet stats summary types
for the per-volume claim reference, capacity, usage, working set and timestamp,
and the kubelet reference for its volume statistics aggregation period; the CSI
specification for volume statistics being an optional node capability; and the
provisioner's own command-line flags and chart templates for the absence of a
metrics endpoint, of probes, and of default resource limits.

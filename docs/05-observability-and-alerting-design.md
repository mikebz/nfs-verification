# 05: Closing out observability: alerts, capacity, and metrics across a restart

Author: Claude Code
Created: 2026-09-11
Updated: 2026-09-11

Phase boundary: the first cases that assert on what the *monitoring system*
saw, rather than on what a client or a server log said. Ends when OBS-01,
OBS-05, OBS-06 and OBS-07 run against a real cluster and each one reports an
alert with a timestamp, a capacity agreement, a memory ceiling result and a
metrics continuity verdict, or says precisely which channel this deployment does
not have. Cadence: four pull requests, following [`plan.md`](plan.md) step 7, on
top of step 6. Prior phases:
[`02-chaos-operations-design.md`](02-chaos-operations-design.md),
[`03-grace-and-lock-reclaim-design.md`](03-grace-and-lock-reclaim-design.md),
[`04-data-path-and-locktool-design.md`](04-data-path-and-locktool-design.md).
This is the next phase of 03, which built the log channel and left the metrics
channel open in its section 11.

---

## 1. Problem and outcomes [decided: read the monitoring system the cluster already has, never deploy one]

Steps 3 and 4 can injure the server, time the outage and read the server's log
stream. Nothing in the suite has ever asked the question an operator asks first:
*did anybody get told?* OBS-02 and OBS-03 answer "was there a trace", which is
weaker. A deployment can leave a perfect trace in a log stream nobody reads and
still page nobody for a total storage outage.

This phase adds the second channel the test plan has always named and the suite
has never used: metrics and alerts. Four cases assert that an outage, a memory
ceiling and a full volume each reach a human, and that the metrics record
survives a server restart well enough to show that the outage happened.

Observable outcomes that define done:

- A case can state that a total outage of the export produced an alert naming
  the server, with a severity, within the SLO bound, or that it produced none.
- A case can state the fraction of its memory limit the server reached, and
  whether an alert fired before it got there.
- A case can state what a pod's `df` says about a volume, what the control plane
  says about the same volume at the same moment, and by how much they disagree.
- A case can state whether the metrics record shows a server restart at all, and
  which of the three lawful shapes it took.
- Every one of the four can say "this deployment has no such channel" in a way
  that lands on the deployment owner rather than on the server owner.

Requirements: Section 3.5 of [`01-test-plan.md`](01-test-plan.md) for the four
cases, Section 3.8 for the alert SLO, Section 4.2 for the category budget,
Section 4.3 for where a failure gets routed. Findings that constrain this phase:
F-008 (a server that publishes nothing is a finding about the deployment, and
the case fails rather than skips), F-002 (node sizing, which bounds what the
memory case may do), F-001 and F-003 (ordering around anything that unmounts).

Cases served: **OBS-01** (server unavailable, alert fires), **OBS-05** (memory
approaching ceiling, alert fires before OOMKill), **OBS-06** (volume near
capacity, alert fires and `df` agrees with the control plane), **OBS-07**
(metrics survive a server restart). The metrics client is shared with SCALE-02
and SCALE-04, which assert on server RSS and arrive in step 9. The network
partition operation is shared with CHAOS-04, which arrives in step 10.

---

## 2. Human-readable rules

Each line is testable, and a reviewer can accept or reject the phase from this
section without reading code.

1. The suite deploys no monitoring stack, ever. It reads the one the cluster
   has. A suite that installs Prometheus to pass an observability case is
   testing Prometheus.
2. A monitoring API that cannot be reached is blocked, naming the flag. A
   monitoring API that answers and has nothing to say about the server is a
   failure. The first is a harness gap, the second is the finding the case
   exists for. This is the F-008 rule, applied to the metrics channel.
3. No case invents an alert name, a metric name or a threshold. Alert rules are
   read from the monitoring system, thresholds are the deployment's own, and the
   only thing the suite states is which severity words mean "page someone".
4. An alert counts for a case only when it names the thing under test. An alert
   about an unrelated workload firing in the same window is not evidence.
5. A case that manufactures an outage restores the cluster before it asserts
   anything, on a context that a cancelled or timed-out case cannot take away.
   Assertions are made on collected data after the restore, never with the fault
   still in place.
6. No case drives the server to OOMKill on purpose. Approaching a ceiling is the
   precondition the plan names; crossing it adds nothing to the assertion and
   costs the cluster its storage.
7. No case fills a filesystem it does not own. A volume whose reported capacity
   is not the claim's capacity is not quota-enforced, and filling it fills the
   server's disk for every other export on it.
8. A case that could not establish its own precondition reports blocked with the
   number it did reach. "The server never got near its limit" is not a pass.
9. Two readings of the same quantity taken by two samplers at two moments agree
   within a stated tolerance derived from the slower sampler's period, or the
   case says by how much they disagreed. A raw equality assertion between `df`
   and the control plane would fail on a healthy cluster.
10. Timing bounds come from `pkg/slo`. The alert SLO is already there; the
    fractions and tolerances this phase adds go there too, not into a case.
11. Every rule from phases 3 and 4 still holds. This phase adds a fault, it does
    not amend the contract around the existing ones.

---

## 3. Scope

**In scope for this phase**

- A read-only client for a Prometheus-compatible HTTP API: instant query, range
  query, active alerts, loaded alerting rules.
- Discovery of that endpoint, confirmed by a probe rather than by naming
  convention alone, recorded in `environment.json` with a capability.
- A reading of the kubelet Summary API for per-volume usage, which is the
  control plane's own answer about a volume.
- A reading of `metrics.k8s.io` for a pod's working set, which is how the server
  memory case measures the thing the alert is about.
- One new fault: a bounded ingress partition of the server pod, which is how an
  outage is made to last long enough for an alert rule to fire.
- A bounded capacity fill and a bounded small-file storm, both of which stop at
  a fraction and neither of which crosses it.
- The four cases, and the artifacts each writes before teardown.

**Out of scope (explicit non-goals)**

- Deploying, configuring or repairing any monitoring component. See rule 1.
- Asserting the routing of an alert to a receiver. Alertmanager knows which
  receiver a firing alert landed in; asserting on that asserts the operator's
  paging policy, which is not a property of the storage system. Recorded, not
  asserted. See section 10.
- Writing alerting rules for a deployment that has none, then asserting they
  fire. That tests the rules the suite just wrote.
- OOMKilling the server on purpose (rule 6), which is SCALE-04's territory and
  even there is an assertion about bounded RSS, not an objective.
- Node-level or network-level faults beyond the single-pod ingress partition.
  CHAOS-04 inherits the operation and widens it in step 10.
- Any change to OBS-02 and OBS-04, which passed as written.

**Depends on**

- Server pod discovery and the fault timeline from phase 3.
- The recovery measurement from phase 3, which is how a partition is proven
  healed before anything is torn down.
- `Caps.CanNetworkPolicy`, which preflight already establishes by probe, and
  which gates OBS-01 alone.
- `pkg/slo`, which already holds `AlertSLO` and `ObservationMargin`.
- The directory population and deletion helpers from phase 6, which are the
  small-file storm OBS-05 needs.

---

## 4. Data contract

Everything in this section is produced by a case, written to the artifact bundle
before teardown, and consumed by whoever reads a failure. None of it is stored
between runs. The system of record for every field is the cluster: the suite
copies what a monitoring API or the kubelet said, and never computes a value it
then presents as measured.

### 4.1 The alert snapshot

One row per alert active at the moment of sampling, taken from the monitoring
system's active-alerts endpoint.

| Field | Type | Meaning |
|---|---|---|
| `Name` | string | The `alertname` label. Empty is possible and is recorded as such. |
| `State` | enum | `pending` or `firing`, as the API reports it. A pending alert has matched its expression but not yet outlasted its `for` duration. |
| `Labels` | map[string]string | Verbatim. The severity and the identity assertions both read from here. |
| `Annotations` | map[string]string | Verbatim, for the failure message only. Never asserted on: wording is the deployment's. |
| `ActiveAt` | time.Time | When the expression first matched, from the API. This is the monitoring system's clock. |
| `Value` | string | The sample value at evaluation, verbatim as a string. Not parsed into a float and not compared: it is a number whose units belong to someone else's expression. |
| `SampledAt` | time.Time | The workstation's clock when the snapshot was taken, so that a snapshot can be placed against the fault timeline. |

Identity: an alert is identified by its full label set, which is what the
monitoring system itself uses. Two alerts with the same name and different
labels are two alerts.

### 4.2 The alerting rule inventory

Read once per case, before any fault, from the loaded-rules endpoint. It is what
makes "no alert fired" mean something: a deployment with no rule that could ever
match the server has a different defect from one whose rule did not fire.

| Field | Type | Meaning |
|---|---|---|
| `Group` | string | Rule group name, for the report. |
| `Name` | string | Alert name. |
| `Query` | string | The expression, verbatim. Used for identity matching only when labels do not settle it, and never parsed. |
| `For` | time.Duration | How long the expression must hold before the alert fires. Read, not assumed: a rule whose `For` exceeds the alert SLO cannot meet it, and that is knowable before any fault is injected. |
| `Labels` | map[string]string | Static labels the rule attaches, including severity. |
| `Health` | string | The API's own view of the rule, verbatim. A rule in error state is reported, since it will never fire. |

### 4.3 The series sample and the continuity verdict

Produced by a range query over a window that contains a fault.

| Field | Type | Meaning |
|---|---|---|
| `Metric` | map[string]string | The series' labels, verbatim. |
| `Samples` | []Sample | `{At time.Time, Value float64}`, in the API's order, which is ascending by time. |
| `Step` | time.Duration | The step the range query asked for. |
| `Observed` | time.Duration | The median spacing between consecutive samples before the fault. This is the scrape interval as measured, and every gap bound in OBS-07 is a multiple of it. Not read from configuration: `/status/config` is not exposed on every deployment and the spacing is the ground truth anyway. |

`ContinuityVerdict` is one of four values, exhaustive over what a range query can
show across a restart:

| Verdict | Shape | Meaning |
|---|---|---|
| `resumed-reset` | Samples on both sides, counter lower after than before | The process restarted and its counter started again. The healthy shape for a process-scoped counter. |
| `resumed-continuous` | Samples on both sides, counter non-decreasing across | The series outlived the process. Lawful for a counter that is not process-scoped, such as one the kubelet exports about the container. |
| `never-resumed` | Samples before, none after, past the resume bound | Metrics did not survive the restart. This is the failure the case is named for. |
| `absent` | No samples in the window at all | The series does not exist on this deployment. Blocked, not failed: the case could not ask its question. |

The gap between the last sample before the restart and the first after is
recorded as a duration in every verdict. A gap is not itself a failure: an
outage that leaves a hole in the series is an outage an operator can see, which
is what the plan asks for.

### 4.4 The volume usage reading

Three sources, one shape, so that a disagreement is readable as a table rather
than as prose.

| Field | Type | Meaning |
|---|---|---|
| `Source` | enum | `pod-df`, `kubelet-summary`, or `prometheus`. |
| `CapacityBytes` | int64 | Total, as that source reports it. |
| `UsedBytes` | int64 | Used, as that source reports it. |
| `AvailableBytes` | int64 | Available, as that source reports it. Recorded separately rather than derived: on a filesystem with reserved blocks, used plus available does not equal capacity, and deriving would invent agreement. |
| `At` | time.Time | The source's own timestamp where it publishes one (the kubelet Summary API does), otherwise the workstation clock at the call. |
| `Claim` | string | The PVC this reading is about. |

Identity: `(Claim, Source, At)`. The comparison OBS-06 makes is always between
two rows with the same `Claim` and different `Source`.

### 4.5 The server memory reading

| Field | Type | Meaning |
|---|---|---|
| `Pod`, `Container` | string | Which container. A server pod with a sidecar has more than one, and only the one running the server is the subject. |
| `WorkingSetBytes` | int64 | From `metrics.k8s.io`, which reports working set for memory. |
| `LimitBytes` | int64 | From the pod spec's resource limits. Zero means no limit, which is a blocked precondition, not a zero ceiling. |
| `Fraction` | float64 | `WorkingSetBytes / LimitBytes`, computed only when `LimitBytes` is non-zero. |
| `At` | time.Time | Sample time as the metrics API reports it. |
| `OOMKills` | int32 | Count of container terminations with reason `OOMKilled` seen so far, from the pod status. Zero is the expected value for the whole case. |

### 4.6 Ownership and evolution

Producer: the cases. Consumer: whoever reads the bundle. There is no schema in a
database and no cross-run comparison, so the only compatibility question is what
`environment.json` gains, since a cached preflight record is replayed with
`-env-file` and must gate a rerun exactly as the original run did.

Added to `Environment`:

- `metricsEndpoint`: the service reference the probe confirmed, or empty.
- `metricsFlavor`: what the probe found, so a future non-Prometheus API can be
  told apart from an absent one without changing the field's meaning.
- `alertRuleCount` and `serverAlertRuleCount`: how many alerting rules were
  loaded, and how many of them name the server. The second is what turns a
  silent run into a finding.

Added to `Capabilities`: `MetricsAPI` (an endpoint answered the probe) and
`VolumeStats` (the kubelet Summary API returned an entry for a suite claim).
Both go through `AsMap` and `CapabilitiesFromMap` in the same change, since a
capability that does not round-trip gates a replayed run differently from the
run it was recorded on, silently.

Backward compatible: a record written before this phase decodes with both
capabilities false and both counts zero, which gates the four cases off. That is
the correct behavior for a record taken before anything was probed.

Would force a breaking change: making the metrics endpoint a list, if a
deployment ever splits alerts and metrics across two endpoints. Avoided now by
recording one confirmed endpoint and treating Alertmanager as out of scope, so
the field never has to mean two things at once.

---

## 5. Design and decisions

### 5.1 How the harness reaches an in-cluster HTTP endpoint [decided: the API server's proxy subresource]

The harness runs on a workstation. Prometheus, Alertmanager and the kubelet all
speak HTTP inside the cluster. Three ways to get there:

| Way | What it costs |
|---|---|
| `kubectl port-forward` equivalent over SPDY | A local listener per call, a goroutine to own it, and a failure mode (port in use, forward dies mid-query) that has nothing to do with the assertion. |
| A pod running `wget` against the endpoint | Another pod, an image that carries a usable HTTP client, and JSON parsed out of pod logs. |
| The API server's proxy subresource | One call on the client the suite already has. |

Decided: the proxy subresource. For a Service,
`CoreV1().Services(ns).ProxyGet(scheme, name, port, path, params)` returns a
response wrapper the suite reads directly. For the kubelet there is no typed
`ProxyGet` on the node client, so the node path is built on the REST client
(`Get().Resource("nodes").Name(n).SubResource("proxy").Suffix("stats","summary")`).
Reason: no listener, no image, no second credential, and the same kubeconfig
that every other call in the suite uses.

Cost of this choice, stated because it is the one that can block a run: it needs
`get` on `services/proxy` and on `nodes/proxy`. A kubeconfig without them gets a
403, and the case reports blocked naming the verb rather than reporting that the
cluster has no monitoring. Telling those two apart is the whole point of rule 2,
so the error is classified, not passed through.

`metrics.k8s.io` is an aggregated API, not a proxy: it is read with
`RESTClient().Get().AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces/<ns>/pods/<pod>")`
and decoded into a local struct. No new module dependency for any of this:
the Prometheus responses, the kubelet summary and the pod metrics are all JSON,
and the suite decodes the handful of fields it uses. Pulling in
`k8s.io/metrics` and a Prometheus client library to reach five fields each would
add two module trees to a repository whose dependency list is currently
client-go and nothing else.

### 5.2 Finding the monitoring endpoint [decided: discover, probe to confirm, one flag to override]

Discovery over declaration, with the same rule server discovery already follows:
a heuristic that guesses wrong is worse than no heuristic, so the guess is
confirmed before it is recorded.

1. If `-prometheus=namespace/name:port` is set, that is the endpoint. No search.
2. Otherwise list Services cluster-wide and shortlist any that carry
   `app.kubernetes.io/name=prometheus` or `app=prometheus`, or that expose a
   port named `web` or numbered 9090.
3. Probe each shortlisted candidate with `GET /api/v1/status/buildinfo`, which
   the Prometheus HTTP API answers and nothing else does. The first that answers
   with a decodable body is the endpoint.
4. Nothing answers: record no endpoint, set `MetricsAPI` false, and add a note
   naming what was tried. The four cases then report blocked naming
   `-prometheus`.

The probe is what keeps a Service named `prometheus-operated` that fronts
something else from being recorded as the monitoring system and then failing
four cases for it.

Known deployment shape worth naming here, because the first run will meet it: on
GKE with Google Managed Prometheus, collection is managed and there is no
query-serving Service in the cluster unless the operator deploys the query
frontend. Discovery will find nothing, and `-prometheus` pointed at a frontend
the operator deployed is the documented path. That is a property of the
deployment, and the blocked message says so rather than implying the cluster has
no monitoring.

### 5.3 No endpoint is blocked, an endpoint with nothing to say is a failure [decided: F-008 rule, applied to metrics]

This is the decision the phase turns on, so it is stated once and every case
follows it.

| What the run finds | Verdict | Why |
|---|---|---|
| No endpoint answers the probe | blocked, naming `-prometheus` | The suite cannot see the monitoring system. It may exist outside the cluster. Nothing was learned. |
| RBAC refuses the proxy | blocked, naming the verb | Harness access, not deployment behavior. |
| Endpoint answers, zero alerting rules loaded | fail | An operator on this deployment is paged for nothing, ever. |
| Endpoint answers, rules exist, none names the server | fail | Everything else on this cluster is watched and the storage is not. |
| Rules name the server, none fired inside the bound | fail | The case's own assertion. |

F-008 is the precedent: on a server that never announces grace, OBS-03 fails
rather than skips, precisely so that the finding lands on the record where an
operator sees it. The metrics channel gets the same treatment. The failure
message routes the finding: for the three failure rows above it names the
deployment and the monitoring configuration, not the NFS server, following
Section 4.3.

### 5.4 Which alert is about the server [decided: identity matching, not alert names]

The suite cannot know what a deployment calls its alerts, and a
`-availability-alert=NFSServerDown` flag would be a declaration of something the
cluster can answer. It is answered by matching identities.

The identity set is built from what discovery already knows: the server
namespace, every server pod name, the controller name, the node, the name of any
Service that exposes port 2049 in that namespace, and for OBS-06 the claim and
volume names the case created. An alert or a rule matches when any of its label
*values* equals a member of the set. A rule additionally matches when its query
string contains one, which catches a rule whose labels are generic but whose
expression is specific.

Severity is read from the `severity` label, which is the de facto convention and
is what every rule inventory in practice carries. Two things are asserted about
it, and only two:

- It is present. An alert with no severity cannot be routed, and a total outage
  that arrives unclassified is an alert nobody prioritizes.
- For OBS-01 it names a paging severity. The built-in set is `critical`, `page`,
  `sev1` and `p1`, matched case-insensitively, and `-critical-severity` states
  it for a deployment that words it differently. This mirrors
  `-grace-enter-pattern` exactly: a built-in rule for the common wording, a flag
  for the deployment it does not cover, and a failure message that names the
  flag before anyone has to go looking for it.

What "correct target" means, concretely: the alert names the server by one of
the identities above. It is not asserted that the alert reached a receiver.
Section 10 says why.

### 5.5 OBS-01: manufacturing an outage an alert can see [decided: a bounded ingress partition of the server pod]

An alert rule with a `for` duration of minutes cannot fire during a pod delete,
which the suite recovers from in under a minute. OBS-01 needs an outage that
lasts. Two candidates:

| Candidate | What happens | Why not, or why |
|---|---|---|
| Scale the server's controller to zero | Total outage, for as long as we choose | Mutates a workload the suite does not own, and the scrape target disappears with the pod, so the classic `up == 0` rule never fires: the series is simply gone. If the harness dies mid-case, the storage stays down and nothing in the cluster knows the original replica count except an artifact file. |
| Deny ingress to the server pod with a NetworkPolicy | Clients block, scrapes fail, the pod keeps running | The object is one the suite creates, labels as its own and deletes. `up` goes to 0 rather than disappearing, which is the condition availability rules are actually written against. If the harness dies, one labeled object is left, and it is deleted by name. |

Decided: the NetworkPolicy. Gated on `Caps.CanNetworkPolicy`, which preflight
already establishes by probe rather than by asking the CNI what it claims.

The policy denies all ingress to the server pod except from one source: the
`ipBlock` of its own node's internal address. That exception exists for one
reason, the kubelet's liveness and readiness probes, which originate on the node
and which a blanket deny would fail, turning an outage into a restart and
changing what the case measures. Kubernetes states that traffic from the node to
a pod is not reliably governed by NetworkPolicy and that the behavior is
implementation-defined, so this is a mitigation, not a guarantee. The case
therefore reads the server's restart count before and after: a restart during
the window is recorded next to the result, and the alert assertion still holds,
because a restarting server is an outage too.

Bounds and ordering, in the order the case performs them:

1. Read the rule inventory and the identity set. A rule whose `For` exceeds
   `slo.AlertSLO` is reported now: it cannot meet the SLO by construction, and
   that is knowable without a fault.
2. Snapshot active alerts, so an alert already firing before the fault is never
   counted as a response to it.
3. Apply the policy. Record it on the fault timeline.
4. Register the removal immediately, on a context derived with
   `context.WithoutCancel` from the case context and given its own timeout. A
   case that times out or panics with the export partitioned is the one failure
   mode this phase must not have.
5. Poll active alerts until one matches the server or the hold expires. The hold
   is `slo.AlertSLO + slo.ObservationMargin`: five minutes to pass, five more to
   measure how late a late alert was. It is never extended to make an alert
   fire. A partition held past the bound is no longer a test, it is an outage.
6. Remove the policy. Prove the export is healthy again with the phase 3
   recovery measurement, which is time to first successful I/O from a client
   pod, before anything is deleted.
7. Assert, on what was collected.

Client pods block uninterruptibly on a hard mount for the whole hold. That is
the intended behavior of the mount and the reason the recovery proof in step 6
comes before any pod deletion: F-001 and F-003 are both about deleting things
around a mount that has not come back.

### 5.6 OBS-05: a ceiling that exists, a fraction that is approached [decided: stop at the fraction, never at the kill]

The plan's expected result is "alert fires before OOMKill". The case does not
produce an OOMKill to prove it. Rule 6: crossing the ceiling adds nothing to the
assertion and costs the cluster its storage, and on a 2GB worker it costs the
node (F-002).

Preconditions, each blocked with its own message when absent:

- The server container declares a memory limit. Without one there is no ceiling
  and no threshold an alert could sit below, and the honest report is that this
  deployment cannot alert on a ceiling it has not set.
- A live working-set reading is available, from `metrics.k8s.io`, or from the
  monitoring endpoint's container working-set series where the aggregated API is
  not served.

The pressure is the small-file storm the plan already attributes the growth to:
repeated small writes through the export, using the directory population helper
from phase 6, from client pods, bounded by the case budget and by the claim's
capacity. The case samples the working set on a fixed interval and stops on the
first of: the fraction `slo.MemoryCeilingApproach` is reached, an alert matching
the server and firing on memory appears, or the budget expires.

Assertions:

- No OOMKill occurred at any point. If one did, it is compared against the alert:
  an alert whose `ActiveAt` precedes the kill satisfies the plan's expected
  result, and a kill with no preceding alert is the failure the case exists for.
- If the fraction was reached, an alert naming the server fired within
  `slo.AlertSLO` of the crossing.
- If the fraction was never reached, blocked, reporting the peak fraction and
  the bytes written. The case did not establish its own precondition, and rule 8
  forbids calling that a pass.

Cleanup: the storm's files are deleted before teardown reaches the claim, and
the deletion is bounded like every other node-touching operation.

### 5.7 OBS-06: two halves, and why the fill half can refuse to run [decided: no fill without a quota]

The case has an agreement half and an alert half, and they have different
preconditions. The agreement half needs nothing but a mounted claim. The alert
half needs the volume to approach capacity, which means writing until it does.

The refusal rule first, because it is the one that protects the cluster. A
directory-backed export with no per-volume quota reports the *backing
filesystem's* size to `df` inside the pod. Filling 85% of what `df` reports then
means filling 85% of the server's disk, for every export on it. The case
therefore compares what `df` reports against the claim's own
`status.capacity` before writing a byte, and refuses the fill when the two do not
agree within a tolerance. Blocked, naming the export as not quota-enforced. This
is the expected outcome on a directory-backed provisioner, and it is worth
having on the record: an operator there cannot alert on a per-volume capacity
threshold either, because there is no per-volume capacity.

The agreement half runs whatever the fill half does:

- `df` inside the pod, which is what the workload sees.
- The kubelet Summary API entry for the same claim, matched by `pvcRef`, which
  is what the control plane sees. Decided as the control plane source rather
  than a Prometheus series, because `kubelet_volume_stats_*` is derived from
  exactly this and a deployment without Prometheus can still answer the half.
  Where the monitoring endpoint exists, the Prometheus series is recorded as a
  third row for the table, not asserted on.
- No entry for the claim means the CSI driver does not implement
  `NodeGetVolumeStats`, which is optional in the CSI spec. Blocked, naming the
  driver. A volume the control plane cannot measure cannot be alerted on, which
  is the same class of finding as the one above.

What "agrees" means, since two samplers at two moments never produce equal
numbers. The kubelet aggregates volume stats on a period that defaults to one
minute, so its answer is stale by up to that period plus a scrape delay. The
case therefore reads `df` at a moment, waits for a kubelet reading whose own
timestamp is after that moment, and compares used bytes with a tolerance
expressed as a fraction of capacity (`slo.VolumeStatsAgreement`), with the
freshness requirement stated separately (`slo.VolumeStatsFreshness`). A
disagreement beyond the tolerance fails and the message prints the table, both
timestamps and the delta in bytes, because the interesting failure here is a
control plane that reports a volume as nearly empty while the workload is
getting ENOSPC.

The fill, where it is permitted, writes to `slo.CapacityAlertFraction` of the
claim in bounded chunks, checking free space before each chunk so that the
target is approached and never overshot, and deletes the fill file before
teardown. Then the alert half: an alert naming the claim or the volume fired
within `slo.AlertSLO` of the crossing.

### 5.8 OBS-07: which series, and the three lawful shapes [decided: the server's own series when it has one, the container's when it does not]

OBS-02 already reads two channels and says which one answered. OBS-07 does the
same thing for metrics, for the same reason: a server that exports nothing is a
different deployment from one that does, and a result that hides the difference
is worth less than one that names it.

Series selection, in order:

1. A series the server itself exports, if the monitoring system scrapes a target
   whose pod is the server. Found from the targets the endpoint reports, matched
   against the identity set from 5.4.
2. Otherwise the kubelet's own counter for the server container
   (`container_cpu_usage_seconds_total` for the container, with
   `container_start_time_seconds` alongside it as the restart marker), which
   exists wherever the kubelet is scraped and requires nothing of the server.

The case logs which channel answered. On channel 2 it is asserting something
weaker than the plan's wording, and it says so in the log line rather than in a
comment nobody reads at three in the morning.

The mechanism: sample the series over a window that starts before the fault,
delete the server pod with the existing operation from phase 3, wait for the
phase 3 recovery, then range-query the whole window and classify with the table
in 4.3. The scrape interval is measured from the pre-fault samples; the resume
bound is a multiple of it, named in `pkg/slo`, and never a literal.

What fails: `never-resumed`, which is metrics that did not survive the restart.
What passes: `resumed-reset` and `resumed-continuous`, which are the two shapes
the plan names as acceptable. What is blocked: `absent`. One extra check on
`resumed-continuous`: if the counter continued with no gap and no restart marker
anywhere in the window, the restart left no trace in metrics at all, and the case
logs that as the "no gaps that hide an outage" concern in the plan's wording,
naming the series. It is a log rather than a failure on the first landing,
because on channel 2 a continuous node-level counter across a container restart
is expected, and failing it would file a lawful cAdvisor behavior as a defect.

### 5.9 Naming, gating and the category budget [decided: TestObs prefix, and the budget row is edited, not overrun]

Four cases, named for what they do:
`TestObsAlertsOnServerUnavailable`, `TestObsAlertsBeforeMemoryCeiling`,
`TestObsAlertsOnVolumeNearCapacity`, `TestObsMetricsSurviveServerRestart`.

The repository sorts strictly by category since PR #11, so all four are `TestObs`
and all four run under `make test-obs`, including the two that injure the server.
The Makefile already says this is where `test-obs` stands.

Section 4.2 of the test plan budgets `make test-obs` at under 45 minutes. That
row was written when the target held OBS-02, OBS-03 and OBS-04. OBS-01 alone
holds a partition for up to ten minutes and then proves recovery. Six cases do
not fit 45 minutes, and a target that overruns its own stated contract is the
thing `plan.md` already called out for DATA. The budget row is therefore edited
in `01-test-plan.md`, in the implementation PR, against a measured runtime, not
against an estimate. If the measured number is unreasonable for one target, the
alternative on the table is a second target for the two fill-heavy cases, the
same answer DATA-14 got.

### 5.10 What the existing OBS cases gain

Nothing in OBS-02, OBS-03 or OBS-04 changes in this phase. One thing becomes
possible that section 11 of doc 03 left open: a server that announces grace only
through metrics. The metrics client this phase adds is the channel that would
read it. Wiring it into OBS-03 is deliberately not done here, because no
deployment met so far exports such a metric, and building the second channel
before anything publishes into it is the "helper no case calls" the repository
rules forbid. The open item moves from "no channel exists" to "a channel exists
and nothing publishes to it", which is a better place for it to sit.

---

## 6. Delivery phases

Four pull requests, smallest runnable slice first. Each lands with the cases it
serves, per the one-vector rule.

**MVP, PR 1: the metrics reach, and OBS-07.** The proxy-based client, endpoint
discovery with the buildinfo probe, the `environment.json` and capability
fields, the range query and the continuity classifier, and
`TestObsMetricsSurviveServerRestart`. Chosen as the MVP because it needs no new
fault (the pod delete already exists), writes nothing to the export, and
exercises the whole reach path end to end. Validated outside a unit test by
running `make test-obs -run TestObsMetricsSurviveServerRestart` against the GKE
cluster in F-008 and reading the verdict, whichever of the four it is. A blocked
result there is a real result: it says the deployment has no query endpoint, and
that is the first thing this phase needs to know.

**PR 2: OBS-06.** The kubelet Summary API reading, the quota precheck, the
agreement assertion, the bounded fill, the alert half. Lands second because its
agreement half runs on a cluster with no monitoring at all, so it produces a
result even if PR 1 comes back blocked everywhere.

**PR 3: OBS-01.** The ingress partition operation in `pkg/chaos`, the
restore-before-assert ordering, the rule inventory, severity and identity
matching, `-critical-severity`. Lands third because it is the only one that
makes the export unavailable for minutes, and it should go in after the reach
path has been proven by two cases that cannot hurt anything.

**PR 4: OBS-05.** The working-set reading, the ceiling preconditions, the
bounded storm, the ordering assertion against any OOMKill.

Later phases sharpen after PR 1 reports from a real cluster. In particular, if
no query endpoint is reachable on any cluster available to this project, PRs 2
through 4 are still worth landing for their non-alert halves, and the alert
halves become a documented blocked result rather than dead code. That decision
is made on evidence from PR 1, not now.

---

## 7. Configuration

Source: command-line flags, as everything else in this repository. No config
file, no environment variables beyond `KUBECONFIG`, no credentials: every call
this phase makes goes through the API server with the kubeconfig the suite
already uses, which is why the proxy subresource was chosen in 5.1.

New flags, both with a README row in the same change:

| Flag | Default | Why it exists |
|---|---|---|
| `-prometheus=namespace/name:port` | empty, discovery runs | Nothing in the Kubernetes API states which Service serves the monitoring query API, and a managed collector may have no in-cluster query endpoint at all. |
| `-critical-severity=<value>` | empty, built-in set | Nothing states which severity value a deployment uses to mean "page someone". Same shape as `-grace-enter-pattern`. |

Validation: `-prometheus` is parsed at startup and a malformed reference is a
startup failure, not a run that quietly discovers something else.
`-critical-severity` is lowercased and compared as a whole value.

Intentionally not configurable:

- Alert names. Matched by identity (5.4).
- Alert thresholds and `for` durations. They are the deployment's, and read.
- The scrape interval. Measured (4.3).
- The ceiling fraction, the capacity fraction, the agreement tolerance, the
  freshness requirement and the resume bound. All are bounds, and bounds live in
  `pkg/slo` per the no-timing-literals rule. `AlertSLO` is already there.
- The partition hold. Derived from `AlertSLO` and `ObservationMargin`.

---

## 8. Deployment decisions

Runtime unit: the same `go test` binary on a workstation. Nothing is deployed
into the cluster by this phase. The only object it creates that is not a pod or
a claim is one NetworkPolicy, labeled with the suite's run and case labels like
everything else, so that it is identifiable as the suite's from outside.

Lifecycle, in the two places it matters:

- **Restore outlives the case.** The partition's removal runs on a context
  derived with `context.WithoutCancel`, with its own timeout, registered
  immediately after the policy is applied. A case that times out with the export
  partitioned is the failure mode this phase must not have, and a deferred call
  on the case's own context is exactly that failure mode.
- **Teardown deletes the policy before it deletes pods.** A pod terminating
  behind a partition cannot unmount, and an unmount that cannot complete is
  F-003. Ordering: remove the policy, prove I/O works, then the existing
  teardown path runs unchanged.

Access scope: `get` on `services/proxy` and `nodes/proxy`, read on
`metrics.k8s.io`, and create/delete on `networkpolicies` in the test namespace.
Each is classified on failure (5.1) so that a permission gap reports blocked
rather than being reported as a deployment defect.

Cost visibility: none of this phase costs anything beyond the cluster time it
already uses, with one exception worth naming: OBS-05 writes small files until a
memory fraction is reached, and on a directory-backed export those files land on
the server's disk. Bounded by the case budget and by the claim's capacity, and
deleted before teardown.

---

## 9. Observability

The harness's own output, written to `artifacts/<run-id>/` before teardown,
because a case that tore down its evidence is unreproducible (Section 4.3).

Per case, in the bundle:

| File | Contents |
|---|---|
| `alert-rules.txt` | The rule inventory from 4.2, with a column saying which rules matched the identity set and which did not. This is the file that makes "no alert fired" actionable. |
| `alerts-before.json`, `alerts-after.json` | Alert snapshots from 4.1, taken before the fault and at the end of the hold. Verbatim. |
| `volume-usage.txt` | The three-source table from 4.4, with timestamps and deltas. Written whether OBS-06 passed or not: a run where the two sources differed by 2% is a different run from one where they agreed exactly, and the pass looks identical without it. |
| `server-memory.txt` | The sample series from 4.5, with the limit, the fraction reached and any OOMKill. |
| `metrics-continuity.txt` | The verdict from 4.3, the measured interval, the gap, and the first and last sample either side of the restart. |

Timeline: the partition apply and remove are recorded as fault events with the
existing mechanism, so the alert's `ActiveAt` can be read against the moment the
export went away. Those two clocks are the monitoring system's and the
workstation's, so the durations reported from them are reported, not asserted
against a bound narrower than the guard the suite already applies to two-clock
comparisons.

Log lines follow the existing style: what was observed, from which channel, with
the node, the pod and the profile named.

---

## 10. Alternatives considered

| Option | What it is | Why not chosen |
|---|---|---|
| Deploy Prometheus and alert rules for the run | Install the stack the cases need | Tests the rules the suite just wrote, and leaves a monitoring stack in someone's cluster. Rule 1. |
| Flags naming each alert (`-availability-alert`) | Operator states the alert names | Declaration of something the cluster can answer. Identity matching (5.4) answers it, and a wrong flag value fails a case for a typo. |
| Assert the alert reached a receiver, via Alertmanager | Read `/api/v2/alerts` and the receiver routing | Routing is the operator's paging policy, not a property of the storage system. A correct alert routed to a silenced receiver is a finding about the operator's on-call, and this suite has no business failing storage for it. Recorded as an open item instead. |
| Scale the server controller to zero for OBS-01 | Delete the replicas for the hold | Mutates a workload the suite does not own, and the scrape target disappears rather than failing, so the rule written against `up == 0` never fires. See 5.5. |
| Kill the server process for OBS-01 | Reuse the phase 3 signal | Recovery is under a minute, which is shorter than any real alert's `for`. Nothing fires and the case passes or fails for the wrong reason. |
| Drive the server to OOMKill in OBS-05 | Prove the alert precedes the kill directly | Costs the cluster its storage to prove an ordering that a fraction and a timestamp already establish. Rule 6. |
| Use `kubelet_volume_stats_used_bytes` as the control plane source | Read the volume usage from Prometheus | It is derived from the kubelet Summary API, so reading the source directly gives the same number without requiring a monitoring stack for the half that does not need one. Recorded as a third row when available. |
| Read the scrape interval from `/api/v1/status/config` | Ask Prometheus for its configuration | Not exposed on every deployment, often redacted, and the sample spacing is the ground truth regardless. Measured instead. |
| Port-forward to reach the endpoint | A local listener per query | A failure mode per query that has nothing to do with the assertion. See 5.1. |
| A generic "alerting backend" interface with a Prometheus implementation | Abstract over monitoring systems | No second implementation exists, and the repository rules forbid an abstraction layer ahead of a case that uses it. The `metricsFlavor` field leaves room to tell a second one apart later. |

---

## 11. Open questions and risks

| Item | The constraint that decides it |
|---|---|
| Whether any cluster available to this project serves a Prometheus-compatible query API at all. On GKE with managed collection there is no in-cluster query endpoint unless someone deploys the frontend. | PR 1, run against the F-008 cluster. A blocked result there decides whether the alert halves of PRs 2 through 4 are landing as code that has ever run. |
| `nfs-server-provisioner` exports no metrics of its own, so OBS-07 will almost certainly fall to the container-counter channel on the current cluster. | The target list from the endpoint, read in PR 1. If no deployment ever exports server metrics, the plan's wording for OBS-07 is about the platform's metrics, not the server's, and Section 3.5 should say so. |
| The export on the current cluster is directory-backed, so OBS-06's fill half is likely to report blocked on the quota precheck. | The `df` reading against the claim capacity, in PR 2. If it blocks, per-volume capacity alerting is not a property this deployment has, and that belongs in `findings.md`. |
| Whether a NetworkPolicy denying ingress also kills the server's liveness probe, turning the outage into a restart. The node-IP exception is a mitigation and Kubernetes states node-to-pod traffic is implementation-defined. | The restart count either side of the hold, in PR 3. If the server restarts on every run, the partition is the wrong operation for OBS-01 and the honest fallback is scale-to-zero with the replica count recorded in an artifact. |
| Whether the server ever approaches its memory limit under a bounded small-file storm, or whether it has a limit at all. | The peak fraction reported by PR 4. A server with no limit blocks the case; a server that never climbs blocks it too, and both are deployment facts worth recording. |
| Alert routing is not asserted, so a deployment whose rules fire into a silenced receiver passes OBS-01. | Whether anyone asks for it. The answer would be an Alertmanager endpoint flag and an assertion on the receiver, which is a policy assertion; it is not in this phase on purpose. |
| The `make test-obs` budget row cannot hold six cases at 45 minutes. | A measured runtime from the four PRs. The row is edited against that number, or the fill-heavy cases get their own target. Decided in the implementation PRs, not here. |
| The severity convention (`critical`) is a built-in guess with a flag behind it, exactly like the grace wording rule, which F-008 showed can miss entirely. | The first deployment with real rules. If the built-in set misses everywhere, the flag becomes the documented path rather than the escape hatch. |
| Two clocks again: alert `ActiveAt` is the monitoring system's, the fault timeline is the workstation's. | The same guard the suite already applies. Durations across the two are reported; nothing is failed on a margin narrower than the guard. |
| None of the four has been run against a real cluster. | The first run. Expect it to change the discovery shortlist and the agreement tolerance. |

---

## 12. Verification

One check per rule in section 2, observable from a run.

| Rule | Check |
|---|---|
| 1. No monitoring stack is deployed | The suite creates no Deployment, Service or CRD in this phase. The only object it creates beyond pods and claims is one NetworkPolicy, and it is deleted before the case returns. Reviewable from the diff. |
| 2. Unreachable is blocked, silent is failed | Unit tests on the classifier: a 403 on `services/proxy`, a connection failure and an empty discovery each produce blocked with the flag or verb named; a reachable endpoint with an empty rule set, and one whose rules never match the identity set, each produce a failure whose message names the deployment. |
| 3. Nothing is invented | No alert name, metric name or threshold appears as a literal in any case. The only stated value is the severity set, and it has a flag. Grep-able, and the `-critical-severity` README row says why it exists. |
| 4. An alert must name the thing under test | Unit tests on the matcher: an alert whose labels carry the server pod matches; one for an unrelated workload in the same namespace does not; a rule whose labels are generic but whose query names the server matches on the query only. |
| 5. Restore outlives the case, assertions come after | OBS-01's removal is registered on a `context.WithoutCancel` context immediately after apply. Unit test: a cancelled case context still runs the removal. The case's assertions all execute after the recovery proof, which is checkable by reading the case top to bottom. |
| 6. No deliberate OOM | OBS-05 stops on the first of three conditions and none of them is a kill. The sampled fraction at stop is in `server-memory.txt` for every run, and an OOMKill that happened anyway is reported with its timestamp against the alert's. |
| 7. No filling a filesystem the case does not own | OBS-06 reports blocked, naming the export as not quota-enforced, when `df`'s total does not match the claim's capacity within tolerance. Unit test on the precheck with a 1Gi claim against a 500Gi `df` reading. |
| 8. An unestablished precondition is blocked | OBS-05 with a peak fraction below the threshold reports blocked with the number, not a pass. OBS-07 with no samples reports `absent` and blocks. |
| 9. Agreement has a stated tolerance | OBS-06 compares a `df` reading against a kubelet reading whose timestamp is later, within a tolerance from `pkg/slo`, and the failure message prints both timestamps and the byte delta. Unit tests on the comparison, including a reading that is fresh but outside tolerance and one inside tolerance but stale. |
| 10. Bounds live in `pkg/slo` | The five new constants are defined there with the existing ones, and no case in this phase contains a duration or a fraction literal. |
| 11. Phases 3 and 4 still hold | `make test-chaos` and `make test-data` pass unchanged across the four PRs, and OBS-02, OBS-03 and OBS-04 are not modified by any of them. |

Sources for the platform claims above: the Prometheus HTTP API reference for
`/api/v1/query`, `/api/v1/query_range`, `/api/v1/alerts`, `/api/v1/rules` and
`/api/v1/status/buildinfo`; the Kubernetes API reference for the `proxy`
subresource on Services and Nodes; the kubelet Summary API types in
`k8s.io/kubelet/pkg/apis/stats/v1alpha1` for `pvcRef`, `capacityBytes`,
`usedBytes` and the per-volume timestamp, and the kubelet reference for
`--volume-stats-agg-period`; the CSI specification for `NodeGetVolumeStats`
being an optional node capability; the Kubernetes NetworkPolicy documentation
for `ipBlock` and for node-to-pod traffic being outside what NetworkPolicy
reliably governs; the metrics-server documentation for memory usage being
reported as working set; and the Google Cloud Managed Service for Prometheus
documentation for managed collection having no in-cluster query endpoint without
the query frontend.

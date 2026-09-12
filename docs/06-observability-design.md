# 06: Observability: the data an operator can monitor on

Author: mikebz@
Created: 2026-09-11
Updated: 2026-09-12
Status: **designed, not implemented.** Delivery step 7, next up.
Serves: OBS-05, OBS-06, OBS-07, and the half of OBS-01 that needs no fault.
Requirements in [`01-test-plan.md`](01-test-plan.md) Section 3.5.
Builds on [`03-chaos-operations-design.md`](03-chaos-operations-design.md),
[`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md) and
[`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md),
whose rules all still hold. It supersedes one line of doc 04, which listed
"asserting that an alert fired" as OBS-01's requirement.

---

## 1. The decision that shapes everything here

**The suite verifies what the deployment publishes, never the alert rules.**

Test plan Section 3.5 words four of its cases as "an alert fires". An alert is a
rule somebody wrote: a threshold, a duration, a severity and a routing policy,
all organization-specific. A suite asserting on them would fail a healthy storage
system for a threshold set differently, and would have to find or install a
monitoring stack it does not own. What *is* a property of the system under test
is whether the inputs those rules need exist at all.

The second half is the harder half. An input that exists because Kubernetes
produces it for every workload is not evidence about NFS. So **no case passes on
a signal its own fault produced**, and every case **fails rather than skipping**
when the deployment publishes nothing. That is F-008's rule applied uniformly.

Done means an operator on this deployment can answer four questions from data
that exists: is the server reachable, is it near its memory ceiling, is the
volume near capacity, and do the server's own metrics survive a restart.

## 2. Rules, and how each is checked

| # | Rule | Check |
|---|---|---|
| 1 | The suite deploys no monitoring stack and depends on no hosted one | Every control-plane reading goes through the API server with the existing kubeconfig. The one workload-side reading, `df` in a pod, is the existing exec path, because the point of it is to be what the workload sees |
| 2 | No case asserts that an alert fired, reads a rule, a threshold or a severity | Those are the operator's |
| 3 | One verdict rule, used everywhere (Section 4) | A source the suite cannot reach is blocked; a deployment that publishes nothing fails; a precondition the case failed to create is blocked, with the number reached. Nothing is a capability skip |
| 4 | No case passes on a signal its own fault produced | Deleting a pod moves every container-derived signal by construction, so no case asserts on one |
| 5 | A case that reads a live value shows it moves | A frozen counter and a gauge that ignores its subject both exist and neither is usable. A case reading configuration rather than a value is exempt and says so |
| 6 | Build only what a case asserts on | No reader, endpoint or classifier for a series no assertion reads. Section 6 lists what was cut |
| 7 | No case drives the server to OOMKill or fills a filesystem it does not own | The storm and the write are both bounded |
| 8 | Two readings of one quantity agree within a tolerance derived from the slower sampler's period, or the case says by how much they disagreed | OBS-06 |
| 9 | No bound is asserted that cannot fail | A timing assertion belongs in Section 3.8 with a fault that can violate it, or it is reported rather than asserted |
| 10 | Every rule from steps 3, 4 and 6 still holds | This phase adds two readers and amends no existing case's contract |

## 3. What this phase builds

Two readers, and nothing else:

- **The kubelet stats summary**, through the API server's node proxy: per-volume
  capacity and usage with the kubelet's own timestamp and claim reference, and
  per-container memory working set from the same call. One reader, two cases.
- **The server's own metrics endpoint**, through the API server's pod proxy, with
  a minimal scanner for the Prometheus exposition format.

Everything else comes from objects the suite already reads: the server pod's
spec, its container statuses, and the Service in front of it. No new flags, no
new fault, no new module dependency, and **no new capability**: a capability
would gate these cases off on exactly the runs whose findings matter.

Both proxy subresources are new access for this suite. Neither is exec, and
neither is a scraper running in the cluster.

## 4. The verdict rule

An earlier revision stated the verdict for a missing reading four different ways
in four places, which is worse than choosing wrong: an implementer picks one at
random. One rule:

| What happened | Verdict | Why |
|---|---|---|
| The suite could not reach a source for its own reasons: the node proxy refused it, the kubeconfig lacks the verb | **blocked**, naming what was refused | Harness access. Nothing was learned about the deployment |
| The deployment does not publish, declare or implement what the case is about: no metrics endpoint, no memory limit, no volume stats, no readiness probe that tests NFS | **fail**, naming the deployment | This is the finding. An operator here has no input, and no rule they write changes that |
| The case could not create its own precondition: the storm did not move the reading inside its budget | **blocked**, with the number reached | The case did not establish what it needed, and calling that a pass is rule 5's failure mode |

F-008 is the precedent for row two: on a server that never announces grace,
OBS-03 fails rather than skips, so the finding lands where an operator sees it.
There is no exception, including for an export whose reported total is the
backing filesystem rather than the claim: OBS-06 is about a volume near capacity,
and a number describing something else is not a measurement of this claim.

## 5. What the outside channel covers, and what it cannot

When the server publishes nothing, everything an operator sees comes from the
kubelet watching a container. That covers less of NFS than its volume of metrics
suggests, and the boundary decides what these cases can honestly claim.

| Question an operator asks | Answered from outside? |
|---|---|
| Is the process running; did it restart, and when | Yes, from container status |
| How much CPU and memory is it using | Yes, from the kubelet |
| Was it OOMKilled | Yes, when a limit exists to kill against |
| How full is a volume | Yes, when the driver implements volume stats |
| Did a mount, unmount or provision succeed | Yes, but that measures the client and driver side |
| **Is the server answering NFS at all** | **No.** Nothing probes port 2049 on this chart |
| **NFS operation and error rates** | **No.** Nothing counts READ, WRITE, COMMIT or LOCK |
| **Clients holding state, locks held, open files** | **No.** That lives in the server |
| **Is it in grace, did reclaims succeed** | **No.** A log line only, and F-008 established this server emits none |
| **Which export is busy or failing** | **No.** Exports are invisible from outside the process |

The line through the table: the outside channel reports the **container** and the
**Kubernetes storage plumbing**, and says nothing about the **NFS protocol**.

Two facts read from this chart's own StatefulSet template make it thinner still,
and both are load-bearing:

- **It declares no liveness or readiness probe.** Twelve ports are named, 2049
  among them, and nothing probes any of them. Readiness therefore tracks the
  container's lifecycle: a server wedged but not exited reads Ready and keeps its
  endpoints, serving clients that hang. That is OBS-01's finding.
- **It sets no resource limits by default.** No memory limit means no ceiling to
  monitor against and no kubelet OOMKill either: the node's OOM killer fires
  instead, which is less visible and worse for every other pod on that node. That
  is OBS-05's finding.

## 6. The four cases

**OBS-01: does the availability signal represent NFS.** The obvious construction,
delete the pod and watch readiness go false, is worthless: Kubernetes does that
for any deleted pod of any workload, so the case would pass on a deployment with
no monitoring at all (rule 4). The outage that matters is a server wedged but
still running. So the case splits:

- *This phase, no fault.* Read the readiness configuration and assert the
  deployment has a signal that can represent NFS reachability: a probe targeting
  the NFS service, with the Service's endpoints following it. Record the blind
  window, the probe's period times its failure threshold, or state that it is
  unbounded where no probe exists. Fail where the only in-cluster signal is the
  container's lifecycle.
- *Step 10, with the fault that can fail it.* Stop the server process without
  killing it, which the node agent can already do, and assert the signal goes
  false within a bound. That bound joins Section 3.8 then, because only then does
  a fault exist that can violate it.

Section 3.5 is therefore **not closed out by this step**, and the plan says so.

**OBS-05: a ceiling that is declared, and a reading that moves.** The plan's
wording is "alert fires before OOMKill"; producing the OOMKill would mean
deliberately destroying the cluster's storage to observe an ordering (rule 7).
The case asserts instead that the container declares a memory limit, that its
working set is readable against that limit with a timestamp, and that the reading
moves under a bounded metadata storm. No limit declared is a failure: an
undeclared ceiling is one nobody can monitor against.

**OBS-06: two sources, one quantity, and a write to prove they track.** The
control plane must report this volume's usage, it must agree with `df` inside the
pod within a tolerance derived from the kubelet's sampling period, and both must
move when the workload writes. Available is recorded rather than derived: on a
filesystem with reserved blocks, used plus available does not equal capacity, and
deriving would invent agreement. No volume stats from the driver is a failure;
the agreement comparison is run and recorded either way.

**OBS-07: the server's own metrics, with nothing standing in for them.** Two
reads of one series either side of a restart, and four exhaustive verdicts:

| Verdict | Shape | Meaning |
|---|---|---|
| `resumed-reset` | answers both sides, lower after | consistent with a process that restarted, passes |
| `resumed-continuous` | answers both sides, not lower | the series outlived the process; lawful, passes |
| `never-resumed` | answered before, not after, past the resume bound | the metrics did not survive |
| `absent` | the server publishes no endpoint | fails |

Both `resumed-` verdicts pass, so the split labels the record rather than
deciding the case: a gauge that fell is not evidence of anything, and a verdict
that failed on it would assert that every series must be a counter. `absent`
fails rather than blocks, and no container-level signal is substituted.

## 7. Decisions worth keeping

**No monitoring stack, and no hosted backend.** GKE runs no in-cluster
Prometheus: managed collection ships to Cloud Monitoring, and its query API is a
frontend proxy the operator deploys. Querying it is a cloud dependency the suite
refuses.

**The API server's proxy subresources, not exec and not a scraper.** The kubelet
and the server both speak HTTP inside the cluster and the API server proxies to
both, so the harness stays a Kubernetes client on a workstation.

**A reading is shown live in one of two ways**, and the case says which: where the
value is a number, two reads either side of a bounded action it must respond to;
where it is a time, the recorded event time.

**Nothing is added to `Capabilities`.** An earlier revision proposed four
capability flags for these sources. A capability gates a case off, and every one
of these absences is a finding this phase exists to report. This is the one piece
of the contract an implementer could get wrong with no test noticing.

**`AlertSLO` in `pkg/slo` is deleted rather than renamed.** Five minutes was an
alert's `for` duration, read by nothing, and nothing in this phase can violate
it. A bound that cannot fail is not an SLO (rule 9).

**Not built, and what each would have served**, so the next reader does not
rebuild them without a case:

| Not built | Would have served | Why nothing needs it |
|---|---|---|
| The kubelet's own metrics endpoint | CSI operation counters | No OBS row is about the driver's operation path |
| cAdvisor series | The restart marker for OBS-07 | Container status carries start time and restart count |
| The resource metrics endpoint | A working set cross-check | Duplicates the stats summary |
| The probes endpoint | Probe counters for OBS-01 | On a chart with no probes, the finding comes from configuration |
| `metrics.k8s.io` | A second memory source | Same number, plus an add-on dependency |
| A polling sampler and continuity classifier | Verdicts across many series | OBS-07 is two reads either side of one restart |

## 8. What this phase does not claim

It does not show anyone is paged, that a threshold is set or set sensibly, or
that any data is retained: every reading is live. It does not show the
availability signal detects every outage, because the wedge fault is step 10's.
And on a deployment whose server publishes nothing, it observes no NFS operation
and no NFS error anywhere; the table in Section 5 is the boundary, and OBS-07
fails rather than implying otherwise.

What a green run does show: this deployment declares a readiness signal wired to
the NFS service, declares a memory ceiling and reports a working set against it
that responds to load, reports per-volume usage that agrees with the workload's
own view and tracks its writes, and publishes server metrics that survive a
restart.

## 9. Delivery

Three pull requests, smallest runnable slice first, all `TestObs` under
`make test-obs`:

1. The kubelet reader and **OBS-06**. No fault, and the case in this phase most
   directly about storage.
2. **OBS-05**: the memory reading from the same reader, the declared-limit
   assertion, and the bounded storm.
3. **OBS-01 and OBS-07**: the availability configuration assertion, the metrics
   probe, the text scanner, and the restart comparison.

Only OBS-07 injects a fault, and it is the pod delete OBS-02 and OBS-03 already
carry, so the 45 minute category budget is expected to hold.

**Expect most of this to come back red** on the provisioner this project runs
against, which declares no probes, no resource limits and no metrics endpoint.
That is the phase working, and it belongs in [`findings.md`](findings.md) as one
entry.

## 10. Open

- A server publishing on a port it declares nowhere is where a flag would earn
  its place. None has been met, so none is added.
- The agreement tolerance in OBS-06 is derived from the kubelet's period and has
  not been checked against a real disagreement.
- The step 10 wedge fault, and the bound OBS-01's behavioral half will assert
  against.

## 11. Sources

- Kubernetes [node metrics data](https://kubernetes.io/docs/reference/instrumentation/node-metrics/)
  for the kubelet stats summary, and the
  [Pod API](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/)
  for probes, resource limits and container statuses.
- Kubernetes [probes](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/)
  and [Service endpoints](https://kubernetes.io/docs/concepts/services-networking/service/),
  for what readiness does to a client's connection, which is OBS-01's subject.
- The [CSI specification](https://github.com/container-storage-interface/spec/blob/master/spec.md),
  where `NodeGetVolumeStats` is an optional capability: a driver that omits it
  publishes no per-volume usage, which is what OBS-06 reports.
- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) for what the NFS
  protocol layer is that none of the outside signals reach.

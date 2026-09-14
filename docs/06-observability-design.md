# 06: Observability: health signals, telemetry, and capacity

Author: mikebz@
Created: 2026-09-11
Updated: 2026-09-14
Status: **in progress.** Delivery step 7 (OBS-06 shipped in [PR #29](https://github.com/mikebz/nfs-verification/pull/29);
OBS-01, OBS-05, OBS-07 designed). OBS-02 and OBS-03 shipped in Step 4 ([PR #8](https://github.com/mikebz/nfs-verification/pull/8));
OBS-04 shipped in Step 2b ([PR #4](https://github.com/mikebz/nfs-verification/pull/4)). Consolidated here to serve
the complete Observability test group.
Serves: OBS-01 through OBS-07. Requirements in [`01-test-plan.md`](01-test-plan.md) Section 3.5.
Builds on [`03-chaos-operations-design.md`](03-chaos-operations-design.md),
[`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md) and
[`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md),
whose rules all still hold. It consolidates observability design ownership across
the repository, superseding the OBS-02 and OBS-03 sections of doc 04 and
documenting OBS-04.

---

## 1. The decision that shapes everything here

**The suite verifies what the deployment publishes, never the alert rules.**

Test plan Section 3.5 words several of its cases as "an alert fires". An alert is a
rule somebody wrote: a threshold, a duration, a severity, and a routing policy,
all organization-specific. A suite asserting on them would fail a healthy storage
system for a threshold set differently, and would have to find or install a
monitoring stack it does not own. What *is* a property of the system under test
is whether the inputs those rules need exist at all.

The second half is the harder half. An input that exists because Kubernetes
produces it for every workload is not evidence about NFS. So **no case passes on
a signal its own fault produced**, and every case **fails rather than skipping**
when the deployment publishes nothing. That is F-008's rule applied uniformly.

Done means an operator on this deployment can answer seven operational questions
from telemetry data that actually exists:
1. Does the availability signal represent NFS reachability rather than just container death? (OBS-01)
2. Is a failover event observable with a timestamp and measurable duration? (OBS-02)
3. Are grace period entry and exit observable and bounded? (OBS-03)
4. Does an unmountable share surface an actionable warning on the client pod? (OBS-04)
5. Is the server container's memory ceiling declared and its working set readable? (OBS-05)
6. Does volume capacity and usage agree between the control plane and pod `df`, and does quota apply? (OBS-06)
7. Do the server's own metrics survive a restart? (OBS-07)

## 2. What the outside channel covers, and what it cannot

When an NFS server publishes no metrics and no grace announcements, everything an
operator sees comes from the outside: the kubelet watching a container, and the
Kubernetes API server tracking objects. That covers less of NFS than its volume of
events and metrics suggests, and the boundary decides what these cases can honestly claim.

| Question an operator asks | Answered from outside (control plane)? | Telemetry channel / condition |
|---|---|---|
| Is the process running; did it restart, and when | Yes | Container status (`ServerStartedAfter`) / Events (OBS-02) |
| How much CPU and memory is it using | Yes | Kubelet stats summary (OBS-05) |
| Was it OOMKilled | Yes, when a limit exists | Container termination reason |
| Did a client fail to mount a volume, and why | Yes | Kubelet Events on client pod (OBS-04) |
| How full is a volume | Yes, when driver implements stats | Kubelet stats summary (OBS-06) |
| **Is the server answering NFS at all** | **No** (lifecycle only) | Service readiness probe targeting port 2049 / health probe (OBS-01) |
| **Is it in grace, did reclaims succeed** | **No** (invisible to kubelet) | Container runtime log stream (OBS-03) |
| **NFS operation and error rates (READ, WRITE, LOCK)** | **No** (invisible to control plane) | Server metrics endpoint (OBS-07; conditional on server publishing series) |
| **Clients holding state, locks held, open files** | **No** (invisible to control plane) | Server metrics endpoint (OBS-07; conditional on server publishing series) |
| **Which export is busy or failing** | **No** | Server-side logs or per-export metrics |

The dividing line: the outside channel reports the **container** and the
**Kubernetes storage plumbing**, and says nothing about the **NFS protocol**.

Two facts read from common NFS provisioner chart templates make the outside channel
thinner still:
- **No liveness or readiness probe.** Ports are named (including 2049), but nothing probes
  them. Readiness tracks the container's lifecycle: a server wedged in kernel `D` state or
  a deadlocked userspace daemon reads `Ready` and keeps its Service endpoints, continuing to
  attract client traffic that hangs indefinitely. That is OBS-01's finding.
- **No resource limits by default.** Setting no memory limit means no ceiling to monitor
  against and no kubelet OOMKill either: the host node's OOM killer fires instead, which is
  less visible and destabilizes the whole node. That is OBS-05's finding.

## 3. Telemetry channels, readers, and configuration checks

The suite builds minimal readers and collectors, using existing workstation credentials without
requiring an in-cluster monitoring stack:

1. **The Kubelet Stats Summary Reader** (`pkg/framework/kubeletstats.go`):
   Accessed via the API server's node proxy (`/api/v1/nodes/<node>/proxy/stats/summary`).
   Returns per-volume capacity, used bytes, and available bytes with the kubelet's own
   timestamp and PVC reference (`OBS-06`), and container memory working set from the same
   call (`OBS-05`). No node agent or scraper is needed: it is a standard `GET` using client-go.
2. **The Grace Log Stream Observer** (`pkg/framework/grace.go`):
   Streams server container logs via the Kubernetes Pod API (`PodLogOptions{Timestamps: true}`).
   Crucially, timestamps come from the container runtime (RFC 3339 nano), not from the server's
   own log formatting. It classifies lines into grace entry or exit using an exit-first heuristic,
   reading previous-container logs across pod restarts (`OBS-03`).
3. **The Event Poller and Container Status Reader** (`pkg/framework/events.go` and `pkg/framework/status.go`):
   Polls Kubernetes `v1.Event` objects associated with client pods (`WaitPodEvent` in `events.go`)
   for `Warning` events such as `FailedMount` and `FailedAttachVolume` (`OBS-04`). For failover traces
   (`OBS-02`), reads container restart timestamps via `ServerStartedAfter` (`pkg/framework/status.go`)
   and server logs (`ServerLog`).
4. **The Server Metrics Pod Proxy Reader**:
   Accessed via the API server's pod proxy (`/api/v1/namespaces/<ns>/pods/<pod>:<port>/proxy/metrics`).
   Implements a minimal text scanner for the Prometheus exposition format, checking series
   survival and counter resets across server restarts (`OBS-07`).
5. **The Service Readiness Probe Configuration Checker**:
   Inspects the server pod's container spec and the backing Service's endpoints. Asserts that
   readiness is driven by an active probe targeting the NFS service (port 2049 or an NFS-aware health probe)
   rather than container lifecycle alone (`OBS-01`).

Both proxy subresources (`nodes/proxy` and `pods/proxy`) represent direct control-plane
access. Neither runs a scraper inside the cluster, and neither execs into nodes.

## 4. The uniform verdict contract

A missing telemetry reading or broken signal must be reported consistently across all cases.
An earlier draft mixed skip, fail, and block arbitrarily; the suite establishes this uniform contract:

| What happened | Verdict | Rationale |
|---|---|---|
| The suite could not reach a source for its own reasons: the node proxy returned 403, the kubeconfig lacks `get nodes/proxy`, or static PVs are forbidden | **blocked**, naming what was refused | Harness access gap. Nothing was learned about the deployment under test |
| The deployment does not publish, declare, or implement what the case verifies: no metrics endpoint, no memory limit, no volume stats, no quota, no readiness probe for NFS | **fail**, naming the deployment | Deployment defect or telemetry gap. An operator on this cluster has no data to alert on |
| The case could not create its own precondition: the workload storm did not move the reading within budget | **blocked**, with the number reached | The case failed to set up its test condition; reporting pass would be vacuous |

F-008 is the guiding precedent: when a server never announces grace, OBS-03 fails rather
than skips, so the lack of visibility is highlighted as a deployment defect. Similarly,
when an export reports the entire backing disk rather than the claim's provisioned size,
OBS-06 fails on its quota assertion (F-009).

Where secondary channel errors occur during multi-channel discovery (e.g., `ServerLog` returning
an error in OBS-02 while container start status is available), the harness logs the channel failure
as a diagnostic while asserting that at least one primary operator channel recorded the event.

## 5. What these seven cases assert

Shared conventions (one clock per measurement, waiting past the target, bounds from `pkg/slo`)
live in [`01-test-plan.md`](01-test-plan.md) Section 4.1. The table below covers the assertions
specific to the Observability test group:

| Case | Assertion | Source & Basis |
|---|---|---|
| **OBS-01** | The deployment declares an active readiness probe targeting the NFS service, and endpoints drop when NFS is unavailable | Kubernetes probes and Service endpoints. An active probe targeting port 2049 or an NFS health check is verified in configuration (Step 7); behavioral drop of endpoints when frozen is verified with a fault in Step 10 |
| **OBS-02** | Failover leaves a timestamped trace an operator can find; duration is measurable | Container start status (`ServerStartedAfter`) and server log stream (`ServerLog`). Fails if neither provides a timestamped record after the fault |
| **OBS-03** | Grace period entry and exit are observable with timestamps; duration is bounded by `2 * LeaseSeconds` | RFC 8881 Section 8.4.2. Read from runtime log timestamps; fails if unannounced (F-008) |
| **OBS-04** | A mount failure on a client pod surfaces as an actionable `Warning` Event naming the volume; pod does not report Ready | Kubelet mount logic. Fails if no mount failure event arrives within budget, if the event omits the volume name, or if container status reports Ready. Wording of the failure cause is logged as a diagnostic warning because kubelet controls event phrasing |
| **OBS-05** | The server container declares a memory limit, and its working set is readable and moves under load | Kubelet Summary API. Fails if no limit is declared; never manufactures an OOMKill |
| **OBS-06** | Kubelet volume usage agrees with pod `df` within tolerance, both move with writes, and quota applies | CSI `NodeGetVolumeStats` capability. Kubelet stats summary (`pkg/framework/kubeletstats.go`) compared against pod `df -P -k` (`pkg/framework/volumeusage.go`). Fails if unannounced or if reported total is backing disk (F-009) |
| **OBS-07** | Server metrics answer before and after restart; counters persist or reset cleanly | Prometheus metrics scraping. Fails if no metrics endpoint is exposed |

## 6. Detailed case walkthroughs

### OBS-01: Server unavailable (NFS readiness vs. container lifecycle)
- **Problem**: In-cluster clients access NFS via a Kubernetes Service. If the server process
  wedges in kernel `D` state or deadlocks, the container stays running, Kubernetes reports the pod
  `Ready`, and the Service continues routing new client connections to a dead export.
- **Design**:
  - *Configuration half (Step 7, designed)*: Inspects the StatefulSet/Pod container spec. Asserts that
    an active readiness probe exists, targeting port 2049 or an NFS-aware health script, and that
    the Service's endpoints track it. Fails if readiness merely mirrors container lifecycle.
  - *Behavioral half (Step 10, planned)*: Injects a freeze fault (SIGSTOP via node agent) without killing
    the container. Asserts that Service endpoints drop the pod within the probe's blind window
    (`periodSeconds * failureThreshold`).
  - *Scope boundary*: A TCP socket probe on port 2049 verifies listener reachability, not full NFS protocol
    correctness. The configuration check verifies that an active probe exists to detach dead containers;
    the Step 10 fault verifies the drop behavior under process hang.

### OBS-02: Failover event is observable
- **Problem**: When a failover happens, an operator needs to see when it started, when it finished,
  and how long it took, using standard operational tooling.
- **Design**:
  - Monitors two timestamped channels: the server container's log stream (`ServerLog`) and container
    restart status (`ServerStartedAfter`).
  - Asserts that at least one channel provides timestamped evidence after the fault. Fails on complete silence.
  - Records the client's measured outage duration against the time since the fault.
  - Logs whether the trace came from Kubernetes container restart status or NFS daemon logs.

### OBS-03: Grace period entry and exit
- **Problem**: Grace re-entry loops mimic hung clients in front of a healthy server (the most common
  misdiagnosis in NFS on Kubernetes, per triage runbook Section 4.3). A suite cannot evaluate failover
  correctness without observing grace.
- **Design**:
  - Uses `pkg/framework/grace.go` to stream logs via the Kubernetes Pod API.
  - Evaluates lines using container runtime timestamps, bypassing unstandardized server log formats.
  - Applies an **exit-first classification rule**: a line containing "grace" is checked for exit/lift
    keywords first, because common exit phrases contain the entry phrase prefixed with a negation.
  - Asserts grace entry is observed, followed by grace exit within `2 * LeaseSeconds`.
  - Fails if the server announces no grace (F-008).

### OBS-04: Client mount failure surfaces as actionable Event
- **Problem**: When a client pod cannot mount an NFS volume, kubelet retries in the background while
  the pod remains stuck in `ContainerCreating`. Without an event, the failure is completely silent.
- **Design**:
  - Manufactures a deterministic mount failure by creating a static PV pointing at an unroutable IP
    (RFC 5737 documentation prefix `192.0.2.0/24`) and creating a client pod referencing its claim.
  - Polls for a `Warning` Event on the pod with reason `FailedMount` or `FailedAttachVolume` using `WaitPodEvent`.
  - Asserts that the event message is actionable: it must name the PVC, PV, or pod. If the message omits
    the specific failure cause (e.g. mount, attach, timeout, or nfs), that fact is logged as a diagnostic
    warning rather than failing the case, because kubelet owns the message formatting.
  - Samples the pod status after receiving the event and asserts that no container reports `Ready`.

### OBS-05: Server memory ceiling and working set telemetry
- **Problem**: Userspace NFS servers historically suffer memory leaks or cache growth under small-file
  workloads. Without a declared memory limit, the pod cannot be monitored against a ceiling, and node
  OOM kills occur instead of container-level remediation.
- **Design**:
  - Asserts the server container declares `resources.limits.memory`. An undeclared ceiling fails the case.
  - Reads `container.memory.workingSetBytes` from the Kubelet Stats Summary via `nodes/proxy` (`pkg/framework/kubeletstats.go`).
  - Applies a bounded metadata workload (creating 1,000 files) and asserts that the working set gauge moves.
  - **Never manufactures an OOMKill**: deliberately exhausting memory would crash cluster storage and
    take down unrelated workloads (Rule 7).

### OBS-06: Volume near capacity (Control plane agreement and quota check)
- **Problem**: Storage alerts rely on the CSI driver publishing accurate volume usage to the kubelet.
  Furthermore, RWX shares often report the host node's root filesystem capacity rather than the claim's
  provisioned size, making capacity alerts meaningless.
- **Design**:
  - Reads `pvc.usedBytes` and `pvc.capacityBytes` from the Kubelet Stats Summary via `nodes/proxy`
    (`FreshKubeletUsage` in `pkg/framework/kubeletstats.go`).
  - Reads used and available space inside the pod using `df -P -k` over `pods/exec` (`ClaimUsage` in
    `pkg/framework/volumeusage.go`), converted to bytes.
  - Writes a 128 MiB verification file using `yes | head -c ... > ...; sync` (`WriteBytes` in `pkg/framework/io.go`).
  - Asserts that both the kubelet summary and `df` move by at least the movement floor.
  - Asserts that kubelet and `df` agree within a tight tolerance derived from the smaller of the provisioned
    claim size and the capacity the workload is shown.
  - **Asserts quota enforcement**: the reported capacity must match the claim size (e.g. 1 GiB). If it
    reports the 10 GiB backing filesystem, the case fails with [F-009](findings.md).

### OBS-07: Metrics survive server restart
- **Problem**: Server-side metrics (NFS RPC counts, active clients, lock counts) must survive server
  restarts or reset predictably so monitoring pipelines do not break during failover.
- **Design**:
  - Scrapes the server's metrics endpoint before a pod deletion and again after the server recovers.
  - Categorizes the series behavior into four mutually exclusive verdicts:
    - `resumed-reset`: series present before and after, counter reset lower (passes).
    - `resumed-continuous`: series present before and after, counter continuous (passes).
    - `never-resumed`: series answered before but failed after restart (fails).
    - `absent`: no metrics endpoint published by server (fails).
  - Fails if the server exposes no metrics endpoint.

## 7. Decisions worth keeping

- **No hosted monitoring stack**: GKE has no default in-cluster Prometheus; its managed collection
  ships to Cloud Monitoring, which requires cloud-specific APIs. Accessing Kubernetes proxy subresources
  keeps the harness cloud-agnostic.
- **API server proxy subresources (`nodes/proxy`, `pods/proxy`)**: Allows the harness to remain an
  out-of-cluster client-go application. Avoids deploying in-cluster scrapers, daemonsets, or exec helpers.
- **Live movement over static presence**: A frozen counter or a hardcoded gauge looks healthy to a
  simple query. Every numerical case (OBS-05, OBS-06) proves the gauge actually moves in response to writes.
- **No new Capabilities**: Missing telemetry or absent quotas must report `fail` or `blocked`. Adding
  a capability would turn deployment defects into silent skips on the clusters where verification matters most.
- **AlertSLO was deleted**: A bound that cannot be violated by a test fault is not an SLO. Alert rules
  are operator concerns; data availability is the storage system's concern.

## 8. What this document does not claim

- Does not claim that any human operator is paged, or that alert thresholds are set appropriately.
- Does not claim that NFS operation rates or lock states are visible from outside when a server exposes
  no metrics endpoint.
- Does not claim that availability probes catch deadlock conditions until the Step 10 freeze fault lands.

## 9. What real runs taught

- **[F-008](findings.md) (Grace unannounced)**: The reference `nfs-server-provisioner` logs no grace
  entry or exit messages. OBS-03 failed as designed, preventing silent misdiagnoses during chaos tests.
- **[F-009](findings.md) (Missing per-volume quota)**: On shared exports without filesystem quotas,
  both kubelet and `df` report the host's 10 GiB disk for a 1 GiB claim. OBS-06 fails on its quota check,
  proving that percentage-based capacity alerts on this deployment would be alerting on the wrong volume.

## 10. Sources

- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 8.4.2 (Server Failure and Recovery).
- Kubernetes [Node Metrics Data](https://kubernetes.io/docs/reference/instrumentation/node-metrics/)
  for the Kubelet Stats Summary format.
- Kubernetes [Pod API](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/)
  for log options, container statuses, and probe definitions.
- Kubernetes [probes](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/)
  and [Service endpoints](https://kubernetes.io/docs/concepts/services-networking/service/),
  for readiness probe behavior and Service endpoint tracking, which is OBS-01's subject.
- [CSI Specification](https://github.com/container-storage-interface/spec/blob/master/spec.md) for
  `NodeGetVolumeStats` optional capability.

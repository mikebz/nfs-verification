# Implementation plan

How the test plan in [`01-test-plan.md`](01-test-plan.md) gets built.
The test plan says what to verify; this file says in what order, in what shape,
and what each step is allowed to assume.

Rule for the whole effort: **small, reviewable changes.** One vector per change,
each landing with the harness pieces it needs and nothing more. A change that
adds a helper no case calls yet does not land.

---

## 1. Test approach

### Where the harness runs

On the operator's workstation, outside the cluster. It authenticates with a
kubeconfig, creates namespaces, PVCs, pods and DaemonSets, injects faults,
collects artifacts and asserts.

The harness is a **Kubernetes client, not an NFS client**. It never mounts the
share itself. Every byte of I/O in every assertion comes from a pod inside the
cluster, driven over `pods/exec`, using binaries that exist on any Linux image:

- `dd` writes and reads at a controlled block size and offset, and is the only
  portable way to ask for `O_DIRECT` or an fsync on each block.
- `sha256sum` produces the checksum that turns "the data came back" into an
  assertion, and it runs in a different pod from the writer so the comparison
  crosses the server rather than a local page cache.
- `flock` takes and probes an advisory whole-file lock, which is how mutual
  exclusion across nodes is tested without a lock daemon.
- `stat` reports size, ownership and timestamps as the client sees them, for
  the sparse-file, identity and squash cases.
- `df` reports capacity as the workload sees it, which the expansion and
  capacity cases compare against the control plane.
- `fio` arrives with the load cases (DATA-14 and the scale soaks). It is the
  only tool here that is not on a stock image, so it comes from a supplied
  `-fio-image` and its cases skip without one. Nothing in this change uses it:
  the lock and integrity cases need exact, inspectable operations, and `fio`
  would make the failure harder to read, not easier.
- `locktool`, a small static Go binary in this repository, arrives with DATA-06
  for `fcntl` byte-range locks, which busybox `flock` cannot express. It also
  extends CHAOS-06 to two clients holding disjoint ranges of one file; until
  then that case asserts reclaim on whole-file locks, which travel to the server
  as a lock over the whole range.

This is what keeps the suite portable between GKE Standard and bare metal, and
what keeps a workstation kernel out of the result.

### Structure

Go, `client-go`, the standard `testing` package, table-driven where the case has
a table. No Ginkgo. One test function per plan case, named for its plan ID
(`TestPROV01_...`), so a failure in CI names the case a human can look up.

### Discovery over declaration

Nothing that can be read from the cluster is passed in by hand. Preflight reads
the Kubernetes version, per-node kernel and runtime, the RWX StorageClass, the
CSI driver, the server fan-out and export-to-PVC mapping, the actual mount
options as the driver set them (from `/proc/mounts` on the node, not from what
the StorageClass asked for), IP families, mount propagation, and the recovery
state backend. It writes them to `artifacts/<run-id>/environment.json`, which is
attached to every failure report.

A plan that requires humans to type version numbers correctly is wrong within a
sprint. The flags that remain exist only for things a cluster genuinely cannot
answer, and each one is listed in the README with why.

### Namespaces and object naming

The suite creates no namespaces. Everything lands in `default`, including the
privileged node agent. Cases are kept apart by name and by label instead: every object a case creates is
named `nfsv-<case>-<run>-<what>` and labelled with the run and the case, and
teardown deletes exactly that label selector, pods first and then claims. The
trade is deliberate: a namespace per case is tidier, but it hides ownership
behind a generated name and it puts a namespace deletion, which is slow and
which can wedge on a stuck finalizer, on the path of every case.

### Objects come from manifests

Pods and the node agent DaemonSet are embedded YAML templates under
`pkg/framework/manifests`, rendered and decoded into typed objects. A manifest
reads like something a person would apply, and it can be diffed against what
was actually applied. Unit tests render every manifest and assert on the decoded
object, so an indentation slip fails on a workstation rather than against a
cluster.

### Categories and skipping

- The plan sorts cases into presubmit, nightly, soak and manual. Until there is
  more than one category of case in the repository, that lives in the comment
  above each test and in `go test -run`, not in harness machinery. The rule the
  categories exist to enforce still holds: the fast path holds no chaos, because
  chaos is slow, its failures need human triage, and red in the fast path trains
  people to ignore red.
- Cases skip **by capability, never by platform name**:
  `if !f.Caps.CanStopNode`, never `if platform == "gke"`. Capabilities are
  discovered at preflight and recorded in `environment.json`.
- A case blocked by cluster configuration reports **blocked**, not failed. A six
  minute recovery caused by a Kubernetes controller default is a deployment
  defect; filing it against the NFS server wastes a week.

### Assertion discipline

The guarantee under test is NFSv4.1, not POSIX. Close-to-open is asserted
directly (DATA-03); the absence of anything stronger is asserted just as
deliberately (DATA-04). Locks are advisory and visible across nodes only because
every client serializes through the one server. No case asserts coherence this
architecture does not provide, and no case asserts how failover happens: the HA
mechanism is a black box, and every failover case measures only what a client
can observe.

Timing bounds are never literals in a case. They come from `pkg/slo`, against a
lease/grace profile that preflight pins to `tuned` (20s/30s) or `default`
(60s/90s). A third value fails preflight rather than silently invalidating every
timing assertion.

### Preflight runs once per cluster

Preflight is a probe, not an assertion about this run: against an unchanged
cluster it answers the same way every time. A passing result is cached per
kubeconfig context under `artifacts/preflight-<context>.json` and reused until
it ages out, so a normal edit-and-rerun loop costs one probe, not one per run.
`-refresh-preflight` forces it, and it reruns automatically once the cache is
older than `-preflight-max-age`.

### Artifacts

A failed case writes `artifacts/<run-id>/<CASE-ID>/`: `environment.json`, client
and server pod logs including previous-container logs, Kubernetes Events,
`/proc/mounts` and dmesg from every involved node, and the injected-fault
timeline. A failure filed without that bundle gets closed as unreproducible, so
the bundle is written before teardown, not after.

---

## 2. Delivery order

Each step is one pull request. Later steps depend only on earlier ones.

| Step | Scope | Status |
|---|---|---|
| 1 | Approach, harness skeleton, preflight (Section 0), PROV-01, DATA-03, DATA-05 flock | done, [PR #1](https://github.com/mikebz/nfs-verification/pull/1) |
| 2 | Presubmit cases that need nothing the harness does not already have: PROV-03, PROV-04, DATA-01, DATA-04, SEC-01 | done, [PR #3](https://github.com/mikebz/nfs-verification/pull/3) |
| 2b | The three presubmit cases held back from step 2: DATA-02, OBS-04, SEC-02 | done, [PR #4](https://github.com/mikebz/nfs-verification/pull/4) |
| 3 | Chaos operations package plus CHAOS-01 and CHAOS-02, the SLO measurement path, fault timelines | done, [PR #4](https://github.com/mikebz/nfs-verification/pull/4) |
| 4 | Grace and lock reclaim: CHAOS-05, CHAOS-06, CHAOS-07, OBS-02, OBS-03, designed in [`03-grace-and-lock-reclaim-design.md`](03-grace-and-lock-reclaim-design.md) | **this change** |
| 5 | Node and infrastructure faults: CHAOS-03, CHAOS-08 to CHAOS-13, node power implementations | |
| 6 | Remaining provisioning: PROV-02, PROV-05 to PROV-08, PROV-10, PROV-11 | |
| 7 | Remaining data cases: DATA-06 to DATA-13, the locktool image for fcntl byte ranges, and the sub-file half of CHAOS-06 it makes possible | |
| 8 | Scale: SCALE-01, SCALE-02, SCALE-03, SCALE-05, SCALE-06 | |
| 9 | Security and observability: SEC-03 to SEC-09, OBS-01, OBS-05, OBS-06, OBS-07 | |
| 10 | Soak gate: PROV-09, DATA-14, CHAOS-14 to CHAOS-16, SCALE-04, SCALE-07 | |
| 11 | Version skew, conditional on preflight finding independent versioning: SKEW-01 to SKEW-03 | |

Order rationale: the heaviest weight in the plan is on the singleton in the data
path, so chaos comes early, right after there are enough presubmit cases to
prove the harness itself works. Scale and soak come last because they are the
cases most likely to be blocked by cluster capacity rather than by defects.

Steps 2b and 3 land together because the three cases held back from step 2 were
held back for reasons the chaos work settles: OBS-04 needs a fault, and SEC-02
needs a way to state what the export is configured to do. Step 2 was split
because those three cases could not yet be written so that a failure means what
it says, and a presubmit case that fails for its own reasons is worse than no
case at all:

- **DATA-02** carries the caveat in gap 1 below. It asserts an exact record
  count under concurrent `O_APPEND` writes, which is an implementation property
  and not something NFSv4.1 promises. It landed with a failure message that
  routes it to the boundary discussion rather than to the server owner, and
  with the record-integrity half of the case separated out, because a torn
  record is corruption under any reading of the protocol.
- **OBS-04** needs a mount that fails on purpose, which means a PV pointing at
  an export that is not there. It landed here rather than being hand-rolled in
  step 2.
- **SEC-02** asserts that root is squashed *as configured*. Nothing in the
  Kubernetes API states the export's squash setting, so the case takes it from
  `-root-squash` when the operator states it, and otherwise records what the
  export does without asserting a value it was never told. What it asserts
  either way is coherence: whatever the export does to root, it must do the
  same thing on every client.

---

## 3. What step 1 contains

**Packages**

- `pkg/slo` — every timing and correctness target, and the two lease/grace
  profiles. Unit-tested, including the fact that a 60s target under a 90s grace
  period is unachievable, which is why the default profile uses grace + 30s.
- `pkg/env` — the environment record and its JSON form.
- `pkg/framework` — clients, the per-case fixture, pod and PVC builders backed
  by embedded YAML manifests, exec, polling and measurement, flock helpers, the
  privileged node agent, artifact collection, gating.
- `pkg/preflight` — Section 0 checks and all discovery.
- `cmd/preflight` — `make preflight`, exits non-zero with a specific reason.

**Cases**

Test functions are named for what they do, not for their plan ID; the ID lives
in the comment above each one.

- PROV-01: provision, bind, mount, write, checksum, delete, and confirm the
  backing volume is reclaimed.
- DATA-03: close-to-open across two nodes.
- DATA-05: flock mutual exclusion across two nodes, and a clean handover on
  release.

Three cases is deliberate. They exercise the whole harness end to end (claim,
pod, exec, node agent, mount option verification, artifacts) without committing
reviewers to reading sixty cases before the plumbing is agreed.

**Not in step 1**: any fault injection, the SLO measurement path, the locktool
image, fio, snapshots, expansion. Those arrive with the cases that use them.

---

## 3a. What step 2 contains

Five cases, and only the harness pieces they call:

- PROV-03: delete a claim a pod still mounts. The claim stays Terminating with
  the `pvc-protection` finalizer, the mount keeps working underneath it, and the
  claim completes once the pod is gone. This is the protection that stands
  between an ordinary `kubectl delete pvc` and F-001 in
  [`findings.md`](findings.md), so it is worth a presubmit gate.
- PROV-04: volume expansion. Control-plane capacity grows, existing data
  survives, the client does not restart. `df` inside the pod is **recorded, not
  asserted**: a directory-backed export with no per-volume quota reports the
  whole backing filesystem to every client and cannot move, and telling that
  apart from a failed expansion needs the fan-out that PROV-11 works from. When
  the class does not advertise expansion, the case asserts the API rejects the
  request, because silently accepting a resize the driver will never perform
  leaves the claim wedged with nothing to fix it.
- DATA-01: four pods writing at once, four files, distinct seeds, each file
  verified by a pod that did not write it. Distinct checksums are asserted too, so two writers
  landing on one content cannot read as a pass.
- DATA-04: the negative of DATA-03. A reader on another node may see nothing or
  part of what an open writer has produced, and the case asserts that is **not**
  a failure while recording which one this deployment does. The one thing it
  does fail on is bytes nobody wrote.
- SEC-01: a pod running as an ordinary uid writes; a pod on another node reads
  the ownership back. Ownership is read from both clients, because a correct id
  on the writer and `nobody` on the reader is a per-client idmapper problem and
  routes to the node, not to the server.

**Harness added**: pod identity (`runAsUser`, `runAsGroup`) in the client pod
manifest, a writer that holds a descriptor open across execs, `stat` ownership,
`df` capacity, and PVC expansion. Each is called by a case in this change.

**Still not here**: fault injection, the SLO measurement path, the locktool
image, fio, snapshots.

---

## 3b. What steps 2b and 3 contain

Five more cases, and the first fault injection in the suite:

- DATA-02: four pods append to one file through one descriptor each, held open
  across the whole loop, which is the shape of a real log appender and the
  harder case. Record integrity is asserted outright; the exact count fails
  with the caveat that routes it.
- SEC-02: what the export does to a root-owned write, on two clients. The
  squash setting comes from `-root-squash` or is recorded rather than asserted;
  the coherence between clients is asserted either way.
- OBS-04: a volume pointing at an export that is not there, on an address
  RFC 5737 reserves for documentation, so the case manufactures a mount failure
  without touching the real export. It asserts the failure reaches the operator
  as a Kubernetes Event that names what could not be mounted. A pod stuck in
  ContainerCreating with nothing in its events is a silent failure.
- CHAOS-01: SIGKILL the server process during an active write.
- CHAOS-02: delete the server pod during an active write, with a lock held
  across the failover.

**The measurement path.** Both chaos cases run a workload that writes one 4KiB
record per second with `conv=fsync` and logs the outcome and time of every
attempt on the pod's own filesystem, never on the share. That log gives all
three assertions at once: time to first committed write after the fault
(against `pkg/slo`), the count of I/O errors (which must be zero, because a
hard mount blocks rather than failing), and the set of writes the server had
already acknowledged, every one of which must still be there afterwards when
read from a pod on another node. The fault time and the recovery time are both
read from the writer pod's clock, so the measurement never depends on the
workstation and a node agreeing.

**`pkg/chaos`.** Two operations, both expressed as Kubernetes operations or a
signal through the node agent, so the same case runs on GKE and on bare metal.
Both record what they did on the fault timeline, and both refuse to act on a
target they cannot identify: the process pattern is rejected when it would
match a shell wrapper, the pod delete is refused when no controller owns the
pod, and a kill that matched nothing is an error rather than a fault. A case
that measures recovery from a fault that never happened passes for the wrong
reason, which is worse than a case that does not run.

**Gates.** There are now slow cases to keep out of the fast path, so the
Section 4.2 split exists: `make test-presubmit` skips `TestChaos...` and
`make test-chaos` runs only those. The category lives in the test name and a
`go test` pattern, not in harness machinery.

**Still not here**: node power operations, the locktool image, fio, snapshots.
Grace and reclaim (CHAOS-05 to CHAOS-07) are step 4, and they are where lock
behaviour gets pinned down properly; CHAOS-02 asserts only that a lock held
across a failover is still exclusive afterwards.

---

## 3c. What step 4 contains

Five cases, designed first in
[`03-grace-and-lock-reclaim-design.md`](03-grace-and-lock-reclaim-design.md),
and the two harness pieces they call. Step 3 could injure the server and time
the outage; it could not see grace, which is the dominant term in every number
it reported and the third question in the triage runbook.

- CHAOS-05: five failovers in a row, each injected once the previous one has
  recovered. Every cycle is asserted on its own, because a total that fits
  inside a wall clock proves nothing if one cycle inside it took four minutes.
  Where grace is observable, grace entered more than once per cycle fails the
  case as a re-entry loop, which is the failure that presents as a hung client
  in front of a healthy server.
- CHAOS-06: locks held on several files across a failover, asserted afterwards
  from both ends: every holder still holds, and a client on another node is
  still refused. The reclaimed fraction is reported against the SLO row, which
  is 100%.
- CHAOS-07: a second client attempting a **new** lock while grace is in force.
  It must never be granted inside the window, and it must be granted after it.
  A client with outstanding un-reclaimed state is held across the whole case, so
  the server cannot lift grace early and let the case pass vacuously.
- OBS-02: a failover is visible to whoever runs the cluster, with a timestamp
  and a duration, through the server's log stream or the Kubernetes API. A
  failover only the client noticed is an observability defect.
- OBS-03: grace entry and exit are both observable and the window is
  measurable, against the two-lease bound in Section 3.8. This is the case the
  plan says CHAOS-05 needs to be diagnosable at all.

**Harness added**: a grace observer that reads the server's log stream through
the Kubernetes API and classifies the lines announcing grace entry and exit,
timestamped by the container runtime rather than by parsing each server's own
format; and a lock probe that attempts a new lock once per second from a second
client, logging outcomes on the pod's own filesystem in the same three-field
format as the workload, so one parser serves both.

**Whole-file, not byte ranges.** CHAOS-06 in the test plan says byte-range
locks, and nothing on a busybox image can take one. A whole-file lock travels
to an NFSv4 server as a lock over the whole range and is reclaimed by the same
mechanism, so reclaim, exclusivity and the grace bar on new state are all
asserted now. The sub-file dimension, two clients holding disjoint ranges of one
file, arrives with `locktool` in step 7.

**Gates.** The chaos name prefix now marks a case that injures the server,
whatever section of the plan it comes from: OBS-02 and OBS-03 inject a fault,
and the fast path holds no fault injection.

**Still not here**: node power operations, network faults, the locktool image,
metrics scraping, and any assertion that an alert fired.

---

## 4. Known gaps to settle as we go

0. **Read [`findings.md`](findings.md) before touching teardown.** F-001
   is a way to take a node out of service with two ordinary API calls in the
   wrong order, and it will be tempting to reintroduce.

1. **DATA-02 asserts more than NFSv4.1 guarantees.** The protocol has no append
   operation; a client implements `O_APPEND` by writing at the offset it
   believes to be end-of-file. Exact record counts under concurrent appends from
   several clients are an implementation property, not a protocol guarantee. The
   case carries that caveat in its failure message so a failure is routed to
   the boundary discussion before it is filed as a server defect, and it
   separates the two halves: a lost record is the flagged assertion, a torn
   record is corruption under any reading and is failed without a caveat. Still
   open: whether the count belongs in the presubmit gate at all, which is a
   question for the boundary discussion and not for this file.
2. **Lease and grace are not discoverable on every implementation.** Confirmed
   on the first real run: the in-cluster NFS provisioner exposes neither, so the
   run needs `-lease-seconds` and `-grace-seconds`. Preflight
   reads them from the server pod spec or a mounted ConfigMap and otherwise
   fails, asking for `-lease-seconds` and `-grace-seconds`. If that proves
   noisy in practice, the alternative is a small read-only probe against the
   server rather than a default value; a default value is not acceptable,
   because it silently invalidates every timing assertion.
3. **CHAOS-17 needs a path.** There is no portable way to find the recovery
   state directory, so that case will take `-recovery-state-path` and skip
   without it rather than guess at an implementation's layout.
4. **CHAOS-03 is blocked on stock cluster settings**, per the floor note in the
   plan. Preflight records both required settings; the case reports blocked, not
   failed, when either is missing.
5. **The export's squash setting is not discoverable.** Nothing in the
   Kubernetes API states it, so SEC-02 takes `-root-squash=on|off` and, without
   it, records what the export does rather than asserting a value nobody stated.
   The alternative would be a probe that infers the setting from the behaviour
   and then asserts the behaviour matches, which proves nothing.
6. **The server process name is not always discoverable.** CHAOS-01 derives it
   from the container command and refuses a shell wrapper, because killing
   everything matching `sh` on a node takes the node out. A server whose command
   lives in the image entrypoint needs `-server-process`, and the case reports
   blocked without it rather than killing something else.
7. **Server discovery is a heuristic** when `-server-selector` is not passed:
   pods exposing port 2049, or named for a known userspace server, excluding
   anything the suite itself created. Passing the selector is the supported
   path, and preflight says so in `environment.json` when it had to guess. The
   heuristic has a unit test, because getting it wrong means the chaos cases
   kill the harness instead of the server.

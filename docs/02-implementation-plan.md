# Implementation plan

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-12

How the test plan in [`01-test-plan.md`](01-test-plan.md) gets built. The test
plan says what to verify; this file says in what order, in what shape, and what
each step is allowed to assume. [`README.md`](README.md) is the map of all the
documents.

Rule for the whole effort: **small, reviewable changes.** One vector per change,
each landing with the harness pieces it needs and nothing more. A change that
adds a helper no case calls yet does not land.

---

## 1. Test approach

### Where the harness runs

On the operator's workstation, outside the cluster. It authenticates with a
kubeconfig, creates PVCs, pods and DaemonSets, injects faults,
collects artifacts and asserts. It creates no namespaces: everything lands in
`default`, kept apart by name and by label (see "Namespaces and object naming"
below).

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
a table. No Ginkgo. One test function per plan case, named for what the case does
(with the plan ID in the comment above it), so a failure in CI names what actually
failed.

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

- All E2E tests are organized strictly by category (`PROV`, `DATA`, `CHAOS`,
  `OBS`, `SEC`, and later `SCALE`, `SKEW`). Each category lives in its own test
  file, has its own target (`make test-prov`, `make test-data`, etc.), and
  corresponds to a `Test<Category>...` function prefix.
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

## 2. Delivery order and status

One step is one pull request, or a short run of them. Later steps depend only on
earlier ones. Steps are numbered independently of the design documents: a step's
design doc, where it has one, is named in its row.

| Step | Scope | Status |
|---|---|---|
| 1 | Approach, harness skeleton, preflight (Section 0), PROV-01, DATA-03, DATA-05 `flock` | done, [PR #1](https://github.com/mikebz/nfs-verification/pull/1) |
| 2 | Cases needing nothing new from the harness: PROV-03, PROV-04, DATA-01, DATA-04, SEC-01 | done, [PR #3](https://github.com/mikebz/nfs-verification/pull/3) |
| 2b | The three held back from step 2: DATA-02, OBS-04, SEC-02 | done, [PR #4](https://github.com/mikebz/nfs-verification/pull/4) |
| 3 | `pkg/chaos`, CHAOS-01, CHAOS-02, the SLO measurement path, fault timelines. Designed in [`03-chaos-operations-design.md`](03-chaos-operations-design.md), written after the fact | done, [PR #4](https://github.com/mikebz/nfs-verification/pull/4) |
| 4 | Grace and lock reclaim: CHAOS-05, CHAOS-06, CHAOS-07, OBS-02, OBS-03. Designed in [`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md) | done, [PR #8](https://github.com/mikebz/nfs-verification/pull/8) |
| 5 | Close out PROV: PROV-02, PROV-05 to PROV-11 | done, [PR #10](https://github.com/mikebz/nfs-verification/pull/10) |
| 6 | Close out DATA: DATA-06 to DATA-13, `locktool`. Designed in [`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md) | done except the soak, [PR #12](https://github.com/mikebz/nfs-verification/pull/12) onward. Run on GKE 2026-09-11: DATA-05 to DATA-09 and DATA-11 to DATA-13 pass, with CHAOS-06. DATA-11's punch half reports blocked on a busybox image, DATA-10 has not been run, DATA-14 is deferred (test plan Section 3.2) |
| 7 | OBS: OBS-05, OBS-06, OBS-07 and the half of OBS-01 that needs no fault. Designed in [`06-observability-design.md`](06-observability-design.md) | **in progress**, first of three PRs done ([PR #29](https://github.com/mikebz/nfs-verification/pull/29)): the kubelet stats reader and OBS-06, run on GKE 2026-09-11. The control plane publishes per-volume usage, it agrees with `df` exactly and both move with the write; the case fails its last assertion because the export has no per-volume quota, which is [F-009](findings.md). OBS-05 follows, then OBS-01 and OBS-07. Does not close out Section 3.5: OBS-01's behavioral half needs a fault from step 10 |
| 8 | Close out SEC: SEC-03 to SEC-09 | not started |
| 9 | Close out SCALE: SCALE-01 to SCALE-07 | not started |
| 10 | Close out CHAOS: CHAOS-03, CHAOS-04, CHAOS-08 to CHAOS-18 | not started |
| 11 | SKEW-01 to SKEW-03, conditional on preflight finding independent versioning | not started |

Thirty-four cases are in the tree today. [`../README.md`](../README.md) lists
them and says what each asserts.

### Why this order

Steps 1 to 4 built the foundation: the harness, the eleven cases that need no
fault, the fault-injection package, and the grace and lock reclaim path.

From step 5 on, the strategy is to **close out one section of the test plan at a
time** rather than interleave fault injection across domains. PROV first, because
the lifecycle has to be trustworthy before anything built on it means much; DATA
next, because the data path is what the protocol actually guarantees; then OBS
and SEC; then SCALE; then CHAOS last, because the most invasive
platform-dependent faults are worth running only once a failure elsewhere can be
ruled out; then SKEW, if preflight finds the server and driver independently
versioned.

### Why step 2 was split

Steps 2b and 3 landed together because the three cases held back from step 2 were
held back for reasons the chaos work settles. A case that fails for its own
reasons is worse than no case at all.

- **DATA-02** asserts an exact record count under concurrent `O_APPEND` writes,
  which is an implementation property and not something NFSv4.1 promises (gap 1
  in Section 4). It landed with a failure message that routes it to the boundary
  discussion rather than to the server owner, and with record integrity
  separated out, because a torn record is corruption under any reading.
- **OBS-04** needs a mount that fails on purpose, which means a PV pointing at an
  export that is not there, on an address [RFC
  5737](https://www.rfc-editor.org/rfc/rfc5737.html) reserves for documentation.
- **SEC-02** asserts that root is squashed *as configured*. Nothing in the
  Kubernetes API states the export's squash setting, so the case takes it from
  `-root-squash` or records what the export does. What it asserts either way is
  coherence between clients.

---

## 3. What each step delivered, or will

### Steps 1 to 4: the foundation

**Step 1** brought `pkg/slo` (targets and the two lease/grace profiles),
`pkg/env` (the environment record), `pkg/framework` (fixture, manifests, exec,
polling, `flock` helpers, node agent, artifacts) and `pkg/preflight`, with three
cases exercising the whole harness end to end. Three was deliberate: enough to
prove the plumbing, few enough that reviewers were not asked to read sixty cases
before agreeing to it.

**Step 2** added pod identity, a writer holding a descriptor open across execs,
`stat` ownership, `df` capacity and PVC expansion, each called by a case in the
same change.

**Steps 2b and 3** added the two fault operations, the measurement path that
turns one workload log into recovery, errors and durability, and the category
split in `make`. See
[`03-chaos-operations-design.md`](03-chaos-operations-design.md).

**Step 4** added the grace observer and the lock probe, and the rule that a case
injuring the server carries the chaos prefix whatever plan section it comes from.
That rule was later superseded by strict sorting by category
([PR #11](https://github.com/mikebz/nfs-verification/pull/11)). See
[`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md).

### Step 5: close out PROV

PROV-02 (20 concurrent claims, no duplicate export IDs), PROV-05 (snapshot and
restore, or clean rejection), PROV-06 (`Retain` and rebind with data intact),
PROV-07 and PROV-08 (provision and delete while the server is down), PROV-09
(100 create/delete cycles), PROV-10 (name edge cases), PROV-11 (two-stage
expansion under active I/O).

**Harness added**: snapshot manifests and capability probing, PV retention and
rebinding fixtures, and a high-churn lifecycle runner.

### Step 6: close out DATA

DATA-06 to DATA-13, `locktool`, the clone volume for `noac`, the directory
helpers, the tool probes, the force-delete-and-await-unmount helper, and the
content-verifying sweep that replaced the existence check everywhere. It also
landed the byte-range half of DATA-05 and the disjoint-range half of CHAOS-06,
both deferred to it by step 4, and the fix for `scripts/lock-probe.sh` passing
`flock -w` (F-006). DATA-14 is deferred. See
[`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md).

### Step 7: observability, in progress

Four cases asserting that an operator on this deployment has inputs to monitor
with, under one decision: **the suite verifies what the deployment publishes,
never the alert rules.** Two readers, no new flags, no new fault, no new
capability. See [`06-observability-design.md`](06-observability-design.md).

The first of three pull requests has landed: the kubelet stats reader, the
volume usage reading with its agreement, movement and quota checks, OBS-06, and
the `pkg/slo` bounds they use. `AlertSLO` went with it, as the design said it
should, because nothing read it and nothing could violate it. OBS-05 follows,
then OBS-01 and OBS-07.

The first run did what the design expected: the two sources agree and move
together, and the case still comes back red, because the export reports the
backing filesystem rather than the claim. That is F-009, and it is the phase
working rather than failing. Expect the rest to be red too on a provisioner
that declares no probes, no resource limits and no metrics endpoint.

### Steps 8 to 11: not started

The case list for each is in the test plan, and repeating it here is how these
documents drifted before. What each step means, and the harness it will need:

| Step | Section | What closing it out means | Harness it needs |
|---|---|---|---|
| 8, SEC | [3.6](01-test-plan.md#36-security-and-identity-sec) | `fsGroup` behaviour, export rules through the Service path, denied clients, dual-stack identity, identity collision after restart, confidentiality on the pod network, and the server's capability set under admission policy | `fsGroup` pod configurations, Service-backed access fixtures, dual-stack probes, traffic inspection |
| 9, SCALE | [3.4](01-test-plan.md#34-scale-and-performance-scale) | Mount ceiling per node, 50 volumes per cluster, pod fan-out curves, small-file and metadata storms, noisy neighbours across exports, and the 8h throughput soak | Batch volume and pod generators, metadata stress scripts, an `fio` orchestrator, noisy-neighbour fixtures |
| 10, CHAOS | [3.3](01-test-plan.md#33-resiliency-and-chaos-chaos) | Node power loss and the out-of-service taint, network partitions, stale filehandles, kubelet, CNI and CSI plugin restarts, colocation deadlock, client OOM, a corrupted state store, delegation recall, and the 24h soak. Also OBS-01's behavioral half, which needs the stop-without-exit fault | Node power operations per platform, taint management, a NetworkPolicy injector, a memory stress daemon, a long-running orchestrator |
| 11, SKEW | [3.7](01-test-plan.md#37-version-skew-skew-conditional) | Driver N against server N-1 and the reverse, and a server restarted into a different minor version under live mounts | Nothing beyond version discovery, which preflight already does |

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

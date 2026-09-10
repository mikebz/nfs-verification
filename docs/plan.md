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
  for `fcntl` byte-range locks, which busybox `flock` cannot express.

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
| 1 | Approach, harness skeleton, preflight (Section 0), PROV-01, DATA-03, DATA-05 flock | **this change** |
| 2 | Remaining presubmit cases: PROV-03, PROV-04, DATA-01, DATA-02, DATA-04, OBS-04, SEC-01, SEC-02 | next |
| 3 | Chaos operations package plus CHAOS-01 and CHAOS-02, the SLO measurement path, fault timelines | |
| 4 | Grace and lock reclaim: CHAOS-05, CHAOS-06, CHAOS-07, OBS-02, OBS-03 | |
| 5 | Node and infrastructure faults: CHAOS-03, CHAOS-08 to CHAOS-13, node power implementations | |
| 6 | Remaining provisioning: PROV-02, PROV-05 to PROV-08, PROV-10, PROV-11 | |
| 7 | Remaining data cases: DATA-06 to DATA-13, the locktool image for fcntl byte ranges | |
| 8 | Scale: SCALE-01, SCALE-02, SCALE-03, SCALE-05, SCALE-06 | |
| 9 | Security and observability: SEC-03 to SEC-09, OBS-01, OBS-05, OBS-06, OBS-07 | |
| 10 | Soak gate: PROV-09, DATA-14, CHAOS-14 to CHAOS-16, SCALE-04, SCALE-07 | |
| 11 | Version skew, conditional on preflight finding independent versioning: SKEW-01 to SKEW-03 | |

Order rationale: the heaviest weight in the plan is on the singleton in the data
path, so chaos comes early, right after there are enough presubmit cases to
prove the harness itself works. Scale and soak come last because they are the
cases most likely to be blocked by cluster capacity rather than by defects.

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

## 4. Known gaps to settle as we go

0. **Read [`findings.md`](findings.md) before touching teardown.** F-001
   is a way to take a node out of service with two ordinary API calls in the
   wrong order, and it will be tempting to reintroduce.

1. **DATA-02 asserts more than NFSv4.1 guarantees.** The protocol has no append
   operation; a client implements `O_APPEND` by writing at the offset it
   believes to be end-of-file. Exact record counts under concurrent appends from
   several clients are an implementation property, not a protocol guarantee. The
   case will carry that caveat in its failure message so a failure is routed to
   the boundary discussion before it is filed as a server defect.
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
5. **Server discovery is a heuristic** when `-server-selector` is not passed:
   pods exposing port 2049, or named for a known userspace server, excluding
   anything the suite itself created. Passing the selector is the supported
   path, and preflight says so in `environment.json` when it had to guess. The
   heuristic has a unit test, because getting it wrong means the chaos cases
   kill the harness instead of the server.

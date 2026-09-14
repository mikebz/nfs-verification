# nfs-verification

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-14

End-to-end verification of NFS RWX persistent volumes on Kubernetes.

**The point.** This is a test harness, not a product. It drives a real cluster
and asserts what a client can actually observe of an RWX NFS volume: that two
pods on two nodes see each other's writes, that a lock taken on one node
excludes the other, that data acknowledged before a fault is still there after
it, that the platform reports what it says it reports. Assertions come from what
NFSv4.1, the Kubernetes API and the CSI spec guarantee, and each case cites the
document it rests on. A deployment that cannot meet a correct assertion is a
finding about that deployment, never a reason to relax the assertion.

**The direction.** v1 covers provisioning, data integrity, chaos, observability
and security, with scale and version skew after them and the conditional
categories gated on what preflight discovers. Cases land one vector at a time,
each with the harness it needs and nothing more.

This repository holds the harness, preflight, the fault injection package, grace
observability, `locktool`, the kubelet stats reader, and the cases themselves.
Which cases exist, which are left, and what the last run returned are tracked in
the test plan ([`docs/01-test-plan.md`](docs/01-test-plan.md)); what those runs
taught, and why each result is what it is, is the findings log
([`docs/findings.md`](docs/findings.md)). Counts and results live there and not
here, so that this file stays true between runs.

## Documentation

Read this file first, then the test plan. Thirty minutes on those two is enough
to read any case in the repository; the design docs are reference material, not
prerequisites.

| Document | What it answers |
|---|---|
| [`docs/01-test-plan.md`](docs/01-test-plan.md) | What gets verified and why: the architecture under test, every case ID, the SLO table, and delivery order with shipped/remaining progress |
| [`docs/findings.md`](docs/findings.md) | What running against a real cluster taught, `F-001` upward: the index, with one file per finding in [`docs/findings/`](docs/findings/). **Read it before touching teardown, deletion, or anything that unmounts** |
| [`AGENTS.md`](AGENTS.md) | How to work here: change size, case conventions, the sources every assertion cites, what to claim when you are done |

One design doc per delivery group, in `docs/`. Each opens with a header saying
which cases it serves and whether it is designed, shipped or superseded, so both
the scope and the status are read there rather than mirrored here:

| Design doc | Delivery group |
|---|---|
| [`02-provisioning-design.md`](docs/02-provisioning-design.md) | Provisioning and volume lifecycle |
| [`03-chaos-operations-design.md`](docs/03-chaos-operations-design.md) | Node and pod faults |
| [`04-grace-and-lock-reclaim-design.md`](docs/04-grace-and-lock-reclaim-design.md) | Server restart, grace and lock reclaim, since absorbed by docs 03 and 06 |
| [`05-data-path-and-locktool-design.md`](docs/05-data-path-and-locktool-design.md) | Data path, byte-range locking and `locktool` |
| [`06-observability-design.md`](docs/06-observability-design.md) | Observability |
| [`07-security-design.md`](docs/07-security-design.md) | Security and client identity |

Every fact has one home: what a case must verify and how far delivery has got
are the test plan's, how the harness works and how to run it are this file's,
why a phase is shaped the way it is belongs to its design doc, and what a real
run taught is `findings.md`'s. A copy anywhere else is how these documents
drifted the first time, and **a result from a particular run is never this
file's**: it is out of date by the next merge.

A case reports one of four things. **Passed**: the assertion held. **Failed**:
the deployment did not do what the protocol, the Kubernetes API or the CSI spec
requires, which is a finding about the deployment. **Blocked**: the case could
not run, because the cluster or the tools image gave it nothing to assert on,
and the reason is itself the finding. **Skipped**: the cluster lacks a
capability the case needs, discovered at preflight.

## Layout

| Path | Contents |
|---|---|
| `pkg/slo` | Timing and correctness targets, and the two lease/grace profiles |
| `pkg/env` | The environment record written to `artifacts/<run-id>/environment.json` |
| `pkg/framework` | Clients, per-case fixture, pods and PVCs from embedded manifests, exec, locks and the lock probe, locktool delivery, the grace observer, the kubelet stats reader, the privileged node agent, artifact and evidence collection |
| `pkg/framework/manifests` | The YAML the suite applies, rendered and decoded into typed objects so it can be diffed against what was applied: the client pod and the node agent DaemonSet |
| `pkg/framework/scripts` | The shell the suite runs inside pods, as scripts rather than as Go strings |
| `pkg/chaos` | The fault operations the CHAOS cases inject |
| `pkg/preflight` | Section 0 checks and all discovery |
| `test/e2e` | The cases; each names its plan ID in the comment above it |
| `cmd/preflight` | `make preflight` |
| `cmd/locktool` | The byte-range lock tool the lock cases stream into a pod; `make locktool` |

[`AGENTS.md`](AGENTS.md) is the guide for working in this repository: how to
approach a change, how to write a case, what to claim when you are done.

## Running

```sh
make all                                                  # fmt, vet, unit, build, locktool
make check-fmt                                            # verify formatting without modifying
make unit                                                 # harness unit tests, no cluster
make locktool                                             # build bin/locktool-linux-{amd64,arm64}
make preflight      FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make test-prov      FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make test-data      FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make test-chaos     FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make test-obs       FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make test-sec       FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make test-e2e       FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make test-case      CASE=TestDataConcurrentAppendToOneFile FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make clean                                                # remove artifacts/ and bin/
```

Everything goes through `make`. The targets carry the flags, the timeouts and
the run ID, so a result reported from a target is one anyone else can reproduce.
`FLAGS` passes cluster-specific values through to the binary; `RUN_ID` overrides
the generated run ID. `make unit` needs no cluster, no network and no
kubeconfig, and unit tests must stay that way: a test under `pkg/` that needs a
cluster belongs in `test/e2e` behind a capability check.

**Worker nodes need at least 4GB of memory**, which on GKE means `e2-medium` or
larger. A 2GB node (`e2-small`) cannot host the suite: the platform's own system
daemons already account for most of that, and the nodes then reboot mid-run,
which from inside a case is indistinguishable from the storage failures the plan
is hunting — I/O that stalls, a mount that does not come back, a lock that is not
reclaimed. Nothing in the suite checks it. Section 0 of the test plan is the set
of conditions preflight enforces and refuses to run without, and this is not one
of them, so it is an operational prerequisite for whoever builds the cluster
rather than a failure that names itself. Teaching preflight to read allocatable
memory and say so is the open item on [F-002](docs/findings.md).

`make test-data` injects faults. DATA-12 and DATA-13 kill the NFS server
process, because the suite sorts strictly by category and they are DATA cases;
`test-prov` and `test-obs` are already in the same position. Only `make unit`
and `make preflight` leave the cluster alone.

Soak is not a category, and this suite is not doing soak testing for now.
DATA-14, the one case that would have needed an hour and a target of its own, is
deferred; Section 3.2 of the test plan says why, and the short version is that
the suite's soak is SCALE-07 and belongs to a section nobody is working yet.

E2E tests are organized strictly by category: `test-prov` runs provisioning cases
(`-run '^TestProv'`), `test-data` runs data consistency cases (`-run '^TestData'`),
`test-chaos` runs chaos cases (`-run '^TestChaos'`), `test-obs` runs observability
cases (`-run '^TestObs'`), and `test-sec` runs security cases (`-run '^TestSec'`).
`make test-e2e` runs all categories end to end.

The suite creates no namespaces. Everything lands in `default`. Objects are
named `nfsv-<case>-<run>-<what>` and labelled with the run and the case, so
teardown deletes exactly that selector, pods first and then claims. A namespace
per case would be tidier and is deliberately not done: it hides ownership behind
a generated name, and it puts a namespace deletion, which is slow and can wedge
on a stuck finalizer, on the path of every case. The full run
ID stays in the name: truncating it collides across runs and turns triage into
guesswork. The node agent is privileged by design, so a cluster enforcing a
restricted Pod Security level on `default` cannot run the suite as it stands.

OBS-06 reads the kubelet's stats summary through the API server's node proxy,
which is the control plane's own view of how full a volume is. That needs `get`
on `nodes/proxy` in the kubeconfig the suite runs with. Nothing is deployed for
it and no monitoring stack is involved: it is an ordinary `GET` on the client
the suite already holds. A kubeconfig without that verb reports the case blocked
and names it, rather than reporting the deployment as one that publishes
nothing.

`make locktool` cross-compiles `cmd/locktool` into `bin/locktool-linux-<arch>`
with `CGO_ENABLED=0`, one per node architecture. `make all` runs it, so a
contributor who never touches a cluster still compiles it. The lock cases read
the target node's architecture, stream the matching binary into the ordinary
client pod over `pods/exec`, and verify it by checksum on the far side; a node
whose architecture has no built binary reports blocked and names this target.
`bin/` is git-ignored, so the binaries are never committed.

Preflight runs once per cluster. A passing result is cached at
`<repo>/artifacts/preflight-<context>.json`, keyed by kubeconfig context, and
reused by later runs for `-preflight-max-age` (8h by default). A relative
`-artifacts-dir` is anchored to the repository root, so `make preflight` and
`go test ./test/e2e` agree on where the cache lives even though `go test` runs
from the package directory. Pass `-refresh-preflight`
to redo it, or `-env-file` to point at a specific record.

Nothing runs until preflight passes. Preflight writes
`artifacts/<run-id>/environment.json`; a failed case writes its own bundle under
`artifacts/<run-id>/<CASE-ID>/` with pod logs, Kubernetes Events, `/proc/mounts`
and dmesg from every involved node, and the injected-fault timeline.

Two things a case argues from do not survive to that point on their own: the
workload's record stream, which lives on the writer pod's filesystem so the
measurement never crosses the filesystem under test, and the file on the share a
data case is making a claim about. Both go with the pod and the claim at
teardown. A case therefore **names** what it argues from, and the harness copies
it into the same directory before anything is deleted, **whether the case passed
or failed** — a passing chaos case's five recovery numbers are precisely the ones
nobody can re-derive afterwards. One named file per registration, capped at 1MiB
each, and nothing walks the share: `evidence.txt` lists what arrived, where it
came from, and what was cut off at the cap, because a truncated file and a whole
one look the same from the bytes.

## Timeouts and budgets

Every wait in the harness is bounded. The bounds below are named constants, the
exported ones in `pkg/framework/wait.go` and the lowercase ones next to the
code they bound, and a new wait should take one of them rather than a literal. A
handful of helpers predate the rule and still pass their own: the workload, lock
and lock-probe helpers wait two minutes for a holder to report, `io.go` waits a
minute for a writer to start, and `pkg/preflight` derives `classProbeTimeout`
from `BindTimeout`. A timeout bug here does not look like a timeout bug: it
looks like a test runner killed by its own `-timeout` and a pile of leaked
claims.

| Constant | Value | What it bounds |
|---|---|---|
| `PollInterval` | 2s | Between polls, everywhere |
| `BindTimeout` | 120s | A claim reaching `Bound`, per Section 0 |
| `PodReadyTimeout` | 5m | A pod reaching `Ready`, image pull included |
| `PodTerminateTimeout` | 90s | A deleted pod leaving the API |
| `DeleteTimeout` | 5m | The whole cleanup for one case |
| `ArtifactTimeout` | 60s | The failure bundle, inside the cleanup budget |
| `EvidenceTimeout` | 30s | Copying the files a case named, on every result, inside the cleanup budget |
| `ExpandTimeout` | 5m | One volume expansion, control plane round trip included |
| `SnapshotProbeTimeout` | 15s | A `VolumeSnapshot` reaching ready |
| `ServerOutageObserveDuration` | 10s | How long claim state is watched while the server is down |
| `FastPoll` | 250ms | Between polls where a second would blur the measurement |
| `nodeInspectTimeout` | 10s | One node's inspection, per node |
| `evidenceReadTimeout` | 10s | One named file's copy, per file |
| `kubeletReadTimeout` | 30s | One kubelet stats read, per node |

Two of these are not round numbers by accident. Test pods carry a 5 second
termination grace period, so a pod still in the API after
`PodTerminateTimeout` is a node that has stopped answering, not a slow unmount;
teardown treats it as the hazard it is rather than waiting it out. And artifact
collection runs on its own clock inside the cleanup budget, so one sick node
cannot spend the whole budget there and leave nothing for cleanup.

Cleanup budgets nest rather than share, and a case's own budget comes from
`caseCtx`. Before adding a wait, add up the worst case: a five minute cleanup
charged to every case eats the `go test -timeout` budget for the package.

## How a failover is measured

Every chaos case runs a workload in a client pod that writes one 4KiB record per
second with `conv=fsync` and logs the outcome and time of every attempt on the
pod's own filesystem, never on the share. That log is copied into the case's
bundle at teardown, pass or fail, since it is the input to all three assertions
below and it dies with the pod. One log gives all three:

- **Recovery**: time from the fault to the first write committed after it,
  against `pkg/slo` for the profile preflight pinned. Never a literal.
- **Errors**: zero, because a hard NFSv4.1 mount is specified to block and retry
  rather than to return an error. An error is a protocol violation, not a slow
  recovery, so it is asserted separately from the timing.
- **Durability**: every write the server acknowledged before the fault must
  still be there afterwards, read from a pod on another node so the check
  crosses the server rather than the writer's own page cache.

Both ends of the timing come from the writer pod's clock, so the measurement
never depends on the workstation and a node agreeing about the time.

## How grace is observed

Grace is the interval after a restart in which the server accepts reclaims of
state that existed before the crash and refuses everything new. It is the
dominant term in every recovery number above, and a grace re-entry loop presents
as a hung client in front of a healthy server, which the triage runbook calls
the most common wrong diagnosis in this architecture.

It is read from the server's own log stream through the Kubernetes API, with the
timestamp the container runtime attached to each line rather than one parsed out
of the server's wording: log formats differ per implementation and change
between versions, runtime timestamps do not. Lines are classified as entry or
exit by a word rule, and `-grace-enter-pattern` with `-grace-exit-pattern`
states the wording for a server the rule does not cover.

Nothing infers grace from the fact that a client stalled, and no case derives a
window from the configured grace value anchored at the fault: grace begins when
the server restarts, which is later than the fault by an unknown amount, so a
derived window ends after the real one and would report a lawful lock grant as a
protocol violation. A case that needs a window and has none reports **blocked**.

The window is stamped by the kubelet on the server's node and a lock attempt by
the client pod that made it, so the two come from different clocks. The window is
narrowed by `slo.ClockSkewGuard` at each end, and only a grant unambiguously
inside it is reported as a violation.

## Flags

Everything discoverable is discovered. These exist because a cluster cannot
answer them:

| Flag | Needed for | Why it cannot be discovered |
|---|---|---|
| `-server-namespace`, `-server-selector` | identifying the NFS server pods | without it the suite falls back to a heuristic (port 2049, or a known server name) and records that it guessed |
| `-lease-seconds`, `-grace-seconds` | every timing assertion | only discoverable when the server exposes them in its pod spec or a mounted ConfigMap |
| `-storage-class` | pinning the class under test | optional: preflight otherwise probes each class by asking it for an RWX volume and mounting it from two nodes |
| `-pvc-size` | claim size | defaults to 1Gi: a backing volume that cannot satisfy the request fails every case, and no case here needs more |
| `-refresh-preflight` | forcing rediscovery | the cached result is reused until it ages out |
| `-tools-image` | client pods and the node agent | needs `dd`, `sha256sum`, `flock`, `stat` and `nsenter`; defaults to `alpine:3.20`, whose busybox carries all five. DATA-07 and DATA-11 probe two things this image may not have, `dd`'s `oflag=direct` and `fallocate`'s `-p`, and report blocked naming this flag rather than filing a tool gap as a protocol gap |
| `-server-process` | CHAOS-01 | derived from the server container's command; a server started through a shell wrapper or an image entrypoint hides it, and the case reports blocked rather than signalling the wrong process |
| `-grace-enter-pattern`, `-grace-exit-pattern` | OBS-03, CHAOS-05, CHAOS-07 | nothing in the Kubernetes API states how a server words grace entry and exit; the built-in rule covers the common wordings, and these state it for a server it does not. Set both or neither: one alone would report every failover as a grace re-entry loop |
| `-root-squash` | SEC-02 | nothing in the Kubernetes API states the export's squash setting; without it the case records what the export does instead of asserting a value nobody stated |

Lease and grace must match one of the two profiles in `pkg/slo`: tuned (20s/30s)
or default (60s/90s). A third value fails preflight rather than silently
invalidating every timing assertion.

The rest are operational rather than descriptive of the deployment, and `-h`
prints all of them with their defaults:

| Flag | Default | What it does |
|---|---|---|
| `-kubeconfig`, `-context` | `$KUBECONFIG` then `~/.kube/config`, current context | which cluster, resolved once by client-go |
| `-artifacts-dir`, `-run-id` | `artifacts`, a UTC timestamp | where the bundle lands and what names the objects; a relative directory is anchored to the repository root |
| `-profile` | either accepted | require a lease/grace profile, `tuned` or `default` |
| `-preflight-max-age`, `-env-file` | 8h, none | how long a cached preflight result stays usable, and a specific record to reuse instead |
| `-delegations` | `auto` | whether delegations are enabled; gates CHAOS-18, which is not written yet |
| `-keep-objects` | off | leave a case's objects behind for triage |
| `-platform`, `-gcloud-project`, `-gcloud-zone`, `-node-power-cmd` | `auto`, empty | how a node would be powered off. Recorded in `environment.json` and read by the capability probe that gates CHAOS-03; the node power operations themselves land in step 10 |

## Three details that bite on real clusters

**StorageClasses that bind on first consumer.** GKE's Filestore classes, and
plenty of others, set `volumeBindingMode: WaitForFirstConsumer`. Such a claim
never binds until a pod referencing it is scheduled, so the suite creates the
pod first and checks the bind afterwards. For the same reason a pinned pod
carries a `kubernetes.io/hostname` node selector rather than `nodeName`:
`nodeName` bypasses the scheduler, and the annotation that triggers binding is
one the scheduler writes.

**Teardown deletes pods gracefully, on purpose.** Force-deleting a pod that
still has the share mounted removes it from the API before kubelet unmounts; the
claim then goes, the export is destroyed, and the node retries RPCs against it
forever on a hard mount. That takes the node out of service. See F-001 in
[`docs/findings.md`](docs/findings.md).

**Lease and grace often are not discoverable.** A server that keeps them in a
config file the pod spec does not reference will fail preflight, and the run
needs `-lease-seconds` and `-grace-seconds`. That is deliberate: a default value
here would silently invalidate every timing assertion.

## Reading a run

Where the suite has got to, and what it returned the last time it was run
against a cluster, are in the test plan's Section 5. Which deployment behaviours
those results exposed, and why each one is what it is, are in
[`docs/findings.md`](docs/findings.md). Neither belongs here: a status paragraph
in a README is wrong one merge after it is written, and the two documents that
own those facts are updated in the same change as the code.

Read a failure with the findings log open, and read a pass with the same
suspicion. Three separate runs have found cases that reported more than they had
measured, and in one of them the over-reported result was a **passing** one that
had been passing for months (F-007, F-014, F-015). A case that cannot run reports
blocked or skipped, and neither is a pass: the vector is simply unexercised, and
the reason is the finding.

Expect a first run on a new cluster to surface flag values that need setting for
the deployment at hand, which is what the specific preflight failure messages
exist to make quick.

> [!TIP]
> On macOS, run long suites under `caffeinate -is`. A workstation that sleeps
> mid-run does not stop the pods, and Go's monotonic clock does not advance
> while it is asleep, so every duration the suite prints about itself comes back
> understated while the cluster-side measurements stay correct. F-017 has the
> run where that happened and what it looked like.

The unit tests cover the parts of the harness that can be wrong on a
workstation: every manifest renders and decodes, the capacity and ownership
parsers are run against the real `stat` and `df` on the machine running the
test rather than against a written-out fixture, the background writer, the
chaos workload and the lock probe are run under a real shell against a real
`flock`, the grace wording rule is tested in both directions because an exit
read as an entry reports a healthy server as looping through grace, the rules
that decide which process a SIGKILL is aimed at are tested directly, because
getting that wrong kills the wrong process on a real cluster rather than
failing a test, and the socket-table parser is run against rows copied from a
real server, because a byte order read backwards produces plausible addresses
belonging to nobody rather than an error. What none of them can tell you is
whether NFS behaves; that needs a cluster.

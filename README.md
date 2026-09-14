# nfs-verification

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-14

End-to-end verification of NFS RWX persistent volumes on Kubernetes.

This repository holds the harness, preflight, the fault injection package, grace
observability, `locktool`, the kubelet stats reader, and thirty-five verification
cases. What is shipped, what is in progress, and the delivery order are tracked
in the test plan ([`docs/01-test-plan.md`](docs/01-test-plan.md)).

## Documentation

Read this file first, then the test plan. Thirty minutes on those two is enough
to read any case in the repository; the design docs are reference material, not
prerequisites.

| Document | What it answers |
|---|---|
| [`docs/01-test-plan.md`](docs/01-test-plan.md) | What gets verified and why: the architecture under test, every case ID, the SLO table, and delivery order with shipped/remaining progress |
| [`docs/findings.md`](docs/findings.md) | What running against a real cluster taught, `F-001` upward. **Read it before touching teardown, deletion, or anything that unmounts** |
| [`AGENTS.md`](AGENTS.md) | How to work here: change size, case conventions, the sources every assertion cites, what to claim when you are done |

One design doc per delivery step, in `docs/`, each stating at its top which
cases it serves and whether it shipped:

| Design doc | Step | Cases | Status |
|---|---|---|---|
| [`03-chaos-operations-design.md`](docs/03-chaos-operations-design.md) | 3 | CHAOS-01, CHAOS-02 | Shipped |
| [`04-grace-and-lock-reclaim-design.md`](docs/04-grace-and-lock-reclaim-design.md) | 4 | CHAOS-05, CHAOS-06 (whole-file locks), CHAOS-07 (OBS-02, OBS-03 consolidated into doc 06) | Shipped |
| [`05-data-path-and-locktool-design.md`](docs/05-data-path-and-locktool-design.md) | 6 | DATA-06 to DATA-13, the byte-range half of DATA-05 and the disjoint-range half of CHAOS-06, plus `locktool` | Shipped, DATA-14 deferred |
| [`06-observability-design.md`](docs/06-observability-design.md) | 7 | OBS-01 through OBS-07 (full observability test group) | In progress: OBS-02, OBS-03, OBS-04, OBS-06 shipped |

Every fact has one home: what a case must verify is the test plan's, what is
built and how to run it is this file's, why a phase is shaped the way it is
belongs to its design doc, and what a real run taught is `findings.md`'s. A copy
anywhere else is how these documents drifted the first time.

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
| `pkg/framework` | Clients, per-case fixture, pods and PVCs from embedded manifests, exec, locks and the lock probe, locktool delivery, the grace observer, the kubelet stats reader, the privileged node agent, artifact collection |
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
make clean                                                # remove artifacts/ and bin/
```

Everything goes through `make`. The targets carry the flags, the timeouts and
the run ID, so a result reported from a target is one anyone else can reproduce.
`FLAGS` passes cluster-specific values through to the binary; `RUN_ID` overrides
the generated run ID. `make unit` needs no cluster, no network and no
kubeconfig, and unit tests must stay that way: a test under `pkg/` that needs a
cluster belongs in `test/e2e` behind a capability check.

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

## Timeouts and budgets

Every wait in the harness is bounded. The bounds below are named constants, the
exported ones in `pkg/framework/wait.go` and the two lowercase ones next to the
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
| `ExpandTimeout` | 5m | One volume expansion, control plane round trip included |
| `SnapshotProbeTimeout` | 15s | A `VolumeSnapshot` reaching ready |
| `ServerOutageObserveDuration` | 10s | How long claim state is watched while the server is down |
| `FastPoll` | 250ms | Between polls where a second would blur the measurement |
| `nodeInspectTimeout` | 10s | One node's inspection, per node |
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
pod's own filesystem, never on the share. One log gives all three assertions:

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

## State of this code

The harness compiles, `go vet` is clean, and the unit tests in `pkg/slo` and
`pkg/framework` pass.

**The whole suite has been run.** Twice on 2026-09-13, and the later of the two
is the one reported here: `make test-e2e` against a three-worker GKE cluster
(Kubernetes v1.37, Container-Optimized OS, kernel 6.12.94+) with the in-cluster
`nfs-server-provisioner` and the `default` profile (lease 60s, grace 90s), in
62 minutes: **26 passed, 5 failed, 4 reported blocked**, out of the thirty-five
cases listed above.

None of the five reds is a defect in the storage server, and none of them is
new. Each one is a finding that already has a number:

| Case | Result | Whose problem |
|---|---|---|
| DATA-02 | fail | Four clients appending to one file landed 150 of 200 records and tore none, with one appender losing its whole contribution and the other three losing none. NFSv4.1 has no append operation, so this is a property of the deployment and goes to the boundary discussion, not to the server owner. It is also intermittent: two runs the same day landed all 200. F-016 |
| OBS-03 | fail | This provisioner never announces grace, so there is no signal to observe. F-008 |
| OBS-06 | fail | The export has no per-volume quota, so both capacity sources describe the backing filesystem rather than the claim. F-009 |
| PROV-04, PROV-11 | fail | The StorageClass advertises `allowVolumeExpansion` and nothing implements it. F-004 |

The earlier sweep of the same day had a sixth red, PROV-07, which was a defect
in this harness rather than in the cluster: a checksum helper returned an empty
string as a digest (F-015). It is fixed, and PROV-07 passes in the run above —
in the same position of the same suite where it failed, which is the comparison
worth having.

The four blocked are CHAOS-01, DATA-12 and DATA-13, which need a process name
this image does not let the harness discover, and CHAOS-07, which needs the
grace window F-008 says this server never announces. Blocked is not a pass:
these vectors are unexercised here.

**A green run against this provisioner is not evidence that grace behaves.**
CHAOS-06's reclaim assertion needs no grace window, which is why it passes while
CHAOS-07 cannot run at all.

Two things the same run settled that earlier versions of this section listed as
open. **DATA-10 has now been run** and passes: 100k directory entries listed
under concurrent deletes, in 238s. **OBS-02 passes**; it had failed on an
earlier run for a reason unrelated to grace.

Two gaps remain in what runs today. **DATA-14 is deferred**, so nothing covers
sustained mixed load at scale; that gap belongs to SCALE-07, and Section 3.2 of
the test plan has the reasoning. And **step 7 is two thirds unwritten**: OBS-05,
OBS-07 and the configuration half of OBS-01 have a design doc and no code
([`docs/06-observability-design.md`](docs/06-observability-design.md)).

Read the failures with the findings log open. Three separate runs have now found
cases that reported more than they had measured — F-007, F-014 and F-015 — and
in the case of F-014 the result being over-reported was a green one, for months.

> [!TIP]
> On macOS, run long suites under `caffeinate -is`. A workstation that sleeps
> mid-run does not stop the pods, and Go's monotonic clock does not advance
> while it is asleep, so every duration the suite prints about itself comes back
> understated while the cluster-side measurements stay correct. F-017 has the
> run where that happened and what it looked like.

Notable findings from running the suite against real clusters are recorded in
[`docs/findings.md`](docs/findings.md).

Expect a first run on a new cluster to surface flag values that need setting
for the deployment at hand, which is what the specific preflight failure messages
exist to make quick.

The unit tests cover the parts of the harness that can be wrong on a
workstation: every manifest renders and decodes, the capacity and ownership
parsers are run against the real `stat` and `df` on the machine running the
test rather than against a written-out fixture, the background writer, the
chaos workload and the lock probe are run under a real shell against a real
`flock`, the grace wording rule is tested in both directions because an exit
read as an entry reports a healthy server as looping through grace, and the
rules that decide which process a SIGKILL is aimed at are tested directly,
because getting that wrong kills the wrong process on a real cluster rather than
failing a test. What none
of them can tell you is whether NFS behaves; that needs a cluster.

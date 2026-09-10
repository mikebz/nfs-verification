# nfs-verification

End-to-end verification of NFS RWX persistent volumes on Kubernetes.

- [`docs/01-test-plan.md`](docs/01-test-plan.md) is the test plan: what gets
  verified and why.
- [`docs/plan.md`](docs/plan.md) is the implementation plan: the test approach, and the
  order the plan gets built in.
- [`docs/findings.md`](docs/findings.md) records what running the suite against a
  real cluster taught us.

This repository currently holds the harness skeleton, preflight, thirteen cases
and the first fault injection. The remaining cases land in the steps listed in
[`docs/plan.md`](docs/plan.md).

## Layout

| Path | Contents |
|---|---|
| `pkg/slo` | Timing and correctness targets, and the two lease/grace profiles |
| `pkg/env` | The environment record written to `artifacts/<run-id>/environment.json` |
| `pkg/framework` | Clients, per-case fixture, pods and PVCs from embedded manifests, exec, locks, the privileged node agent, artifact collection |
| `pkg/framework/manifests` | The YAML the suite applies: the client pod and the node agent DaemonSet |
| `pkg/framework/scripts` | The shell the suite runs inside pods, as scripts rather than as Go strings |
| `pkg/chaos` | The fault operations the CHAOS cases inject |
| `pkg/preflight` | Section 0 checks and all discovery |
| `test/e2e` | The cases; each names its plan ID in the comment above it |
| `cmd/preflight` | `make preflight` |

[`AGENTS.md`](AGENTS.md) is the guide for working in this repository: how to
approach a change, how to write a case, what to claim when you are done.

## Running

```sh
make all                                                  # fmt, vet, unit, build
make check-fmt                                            # verify formatting without modifying
make unit                                                 # harness unit tests, no cluster
make preflight      FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make test-presubmit FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make test-chaos     FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make clean                                                # remove artifacts/
```

Everything goes through `make`. The targets carry the flags, the timeouts and
the run ID, so a result reported from a target is one anyone else can reproduce.
`FLAGS` passes cluster-specific values through to the binary; `RUN_ID` overrides
the generated run ID. `make unit` needs no cluster, no network and no
kubeconfig, and unit tests must stay that way: a test under `pkg/` that needs a
cluster belongs in `test/e2e` behind a capability check.

`test-presubmit` skips the chaos cases and `test-chaos` runs only those; both
select by test name, since chaos cases are named `TestChaos...`. Presubmit
deliberately holds no chaos: chaos is slow, its failures need human triage, and
red in the fast path trains people to ignore red. `make test-e2e` runs
everything.

The suite creates no namespaces. Everything lands in `default`. Objects are
named `nfsv-<case>-<run>-<what>` and labelled with the run and the case, so
teardown deletes exactly that selector, pods first and then claims. The full run
ID stays in the name: truncating it collides across runs and turns triage into
guesswork. The node agent is privileged by design, so a cluster enforcing a
restricted Pod Security level on `default` cannot run the suite as it stands.

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

Every wait in the harness is bounded, and the bounds are named constants in
`pkg/framework/wait.go` rather than literals at call sites. A timeout bug here
does not look like a timeout bug: it looks like a test runner killed by its own
`-timeout` and a pile of leaked claims.

| Constant | Value | What it bounds |
|---|---|---|
| `PollInterval` | 2s | Between polls, everywhere |
| `BindTimeout` | 120s | A claim reaching `Bound`, per Section 0 |
| `PodReadyTimeout` | 5m | A pod reaching `Ready`, image pull included |
| `PodTerminateTimeout` | 90s | A deleted pod leaving the API |
| `DeleteTimeout` | 5m | The whole cleanup for one case |
| `ArtifactTimeout` | 60s | The failure bundle, inside the cleanup budget |
| `nodeInspectTimeout` | 10s | One node's inspection, per node |

Two of these are not round numbers by accident. Test pods carry a 5 second
termination grace period, so a pod still in the API after
`PodTerminateTimeout` is a node that has stopped answering, not a slow unmount;
teardown treats it as the hazard it is rather than waiting it out. And artifact
collection runs on its own clock inside the cleanup budget, so one sick node
cannot spend the whole budget there and leave nothing for cleanup.

Cleanup budgets nest rather than share, and a case's own budget comes from
`caseCtx`. Before adding a wait, add up the worst case: a five minute cleanup
charged to every case eats the `go test -timeout` budget for the package.

## Cases in this repository so far

| ID | Case | Gate |
|---|---|---|
| PROV-01 | Dynamic provision, bind, mount, write, delete, backing volume reclaimed | presubmit |
| PROV-03 | Delete a claim a pod still mounts; it stays Terminating until the mount is gone | presubmit |
| PROV-04 | Volume expansion, or a clean rejection when the class does not advertise it | presubmit |
| DATA-01 | Four pods writing at once, four files, cross-verified checksums | presubmit |
| DATA-02 | Four pods appending to one file through a held-open descriptor | presubmit |
| DATA-03 | Close-to-open across two nodes | presubmit |
| DATA-04 | The negative of DATA-03: what a reader may see before the writer closes | presubmit |
| DATA-05 | flock mutual exclusion across two nodes, clean handover on release | presubmit |
| SEC-01 | uid and gid preservation across pods on two nodes | presubmit |
| SEC-02 | What the export does to a root-owned write, and whether it does it coherently | presubmit |
| OBS-04 | A mount that cannot succeed reaches the operator as a Kubernetes Event | presubmit |
| CHAOS-01 | SIGKILL the server process during an active write | chaos |
| CHAOS-02 | Delete the server pod during an active write, with a lock held across it | chaos |

Several are deliberately careful about what they blame. SEC-01 reports
**blocked**, with the export's own error in the message, when an ordinary uid
has nowhere to write on the share: that is deployment configuration, and filing
it against the storage system wastes a week. PROV-04 records what `df` reports
rather than asserting on it, because an export with no per-volume quota shows
every client the whole backing filesystem and cannot show a capacity change at
all; it still asserts that the control plane grew, that the data survived, and
that the client never restarted. SEC-02 records what the export does to root
unless `-root-squash` says what it was configured to do, and asserts coherence
between clients either way. DATA-02 asserts record integrity outright but
carries a caveat on the record count, because NFSv4.1 has no append operation
and an exact count under concurrent appends is an implementation property; the
failure message routes it to the boundary discussion rather than to the server
owner.

The two chaos cases report **blocked** when the cluster gives them nothing to
injure: no server pods discovered, a server whose process name lives in an image
entrypoint, or a server pod no controller owns, which would not come back. A
case that measures a recovery from a fault that was never injected passes for
the wrong reason, which is worse than a case that does not run.

## How a failover is measured

Both chaos cases run a workload in a client pod that writes one 4KiB record per
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
| `-tools-image` | client pods and the node agent | needs `dd`, `sha256sum`, `flock`, `stat` and `nsenter`; defaults to `alpine:3.20`, whose busybox carries all five |
| `-server-process` | CHAOS-01 | derived from the server container's command; a server started through a shell wrapper or an image entrypoint hides it, and the case reports blocked rather than signalling the wrong process |
| `-root-squash` | SEC-02 | nothing in the Kubernetes API states the export's squash setting; without it the case records what the export does instead of asserting a value nobody stated |

Lease and grace must match one of the two profiles in `pkg/slo`: tuned (20s/30s)
or default (60s/90s). A third value fails preflight rather than silently
invalidating every timing assertion.

## Two details that bite on real clusters

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

PROV-01, PROV-03, PROV-04, DATA-01, DATA-03, DATA-04, DATA-05 and SEC-01 have
been run against GKE clusters during review, not by the author of this code, and
what that first real run produced is recorded in
[`docs/findings.md`](docs/findings.md) rather than summarised here: a node image
whose `umount.nfs` wrapper could never unmount anything (F-003), and a
StorageClass advertising an expansion its provisioner cannot perform (F-004).
Both arrived looking like storage defects and neither was one.

**DATA-02, SEC-02, OBS-04 and both chaos cases have not been run against any
cluster.** Neither has any fault injection. Expect a first run on a new cluster
to surface flag values that need setting for the deployment at hand, which is
what the specific preflight failure messages exist to make quick.

The unit tests cover the parts of the harness that can be wrong on a
workstation: every manifest renders and decodes, the capacity and ownership
parsers are run against the real `stat` and `df` on the machine running the
test rather than against a written-out fixture, the background writer and the
chaos workload are run under a real shell, and the rules that decide which
process a SIGKILL is aimed at are tested directly, because getting that wrong
kills the wrong process on a real cluster rather than failing a test. What none
of them can tell you is whether NFS behaves; that needs a cluster.

# 08: Audit: what the suite validates locally and what it should validate against the server

Author: mikebz@
Created: 2026-09-16
Status: audit, no code changes
Scope: every case in `test/e2e/`, every unit test in `pkg/`, and the readers in
`pkg/framework/` those cases depend on, read against
[`01-test-plan.md`](01-test-plan.md), [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html),
and the Kubernetes storage API.

---

## 1. The question

Local validation is not the problem by itself. A parser that reads `df -P -k`
has to be tested somewhere, and the machine running `go test` is the right
place. The problem is where a local check is the **only** thing standing behind
a claim about the server, and where a case infers the server's behaviour from
something the client would have done anyway.

Three ways that happens in this tree, in order of how much they cost:

1. A claim about the protocol rests on file content alone. Nothing in the suite
   can see an NFS operation, so "the server did X" is always inferred from
   "bytes came back".
2. A number the server is held to is read from what the deployment *declares*
   rather than from what it *does*.
3. A verdict about the server comes out of a classifier whose only fixtures were
   written by hand.

## 2. Two counts worth stating first

**The harness has roughly as much unit test as the suite has case code**: 6,973
lines under `pkg/**/*_test.go` against 6,714 lines in `test/e2e/`. That ratio is
not wrong on its own. What makes it matter is the second count.

**CI runs the first number and never the second.** `.github/workflows/ci.yml`
runs `check-fmt`, `vet`, `unit`, `build` and the `locktool` cross-compile. Every
server-facing assertion in this repository runs only when a person points it at
a cluster by hand. The always-green gate is a statement about the workstation.

**Eight of the twenty-four findings name a `pkg/framework` reader and its unit
test as the thing that changed**: F-006, F-007, F-010, F-011, F-012, F-013,
F-014, F-015, F-024. Every one of them was found by a cluster run and none by
the unit test that now guards it. The unit tests are regression locks written
after the fact, which is a fair thing for them to be, but it means they are not
where a wrong reading of the server gets caught.

---

## 3. Recommendations

Ordered by what they buy, not by effort.

### R1. Nothing in the suite can see an NFS operation. Add `/proc/self/mountstats`.

**Now.** Every DATA and CHAOS assertion reaches the server through file content:
write bytes in one pod, read them from another, compare. The only guard that an
operation reached the server at all is a mount-option check,
`requireServerSideLocking` (`test/e2e/helpers_test.go:93`) reading `/proc/mounts`
through `CheckLockMount` (`pkg/framework/lockmount.go`). A grep for `mountstats`,
`nfsstat`, `/proc/fs/nfsd` and `rpcinfo` across `pkg/`, `cmd/` and `test/`
returns nothing.

**Why it matters.** Some cases pass today on evidence that does not separate the
server from the client:

- **DATA-12** (`test/e2e/data_test.go:1561`) asserts records written with
  `conv=fsync` survive a SIGKILL. A server that ignores `COMMIT` but happens not
  to lose the data passes. The case cannot tell post-COMMIT durability
  ([RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 18.3) from
  luck.
- **DATA-07** (`test/e2e/data_test.go:785`) infers `O_DIRECT` from `dd
  oflag=direct` not erroring. It does not establish the page cache was bypassed
  on either side.
- **DATA-05 / CHAOS-06** establish server-side locking negatively, by the absence
  of `nolock` and `local_lock` in the mount line. Absence of an option is weaker
  than the presence of a `LOCK` on the wire.
- **DATA-08** (`noac`) asserts the option is in `/proc/mounts` and that the read
  saw the data. It does not show attribute revalidation traffic.

**Do.** `/proc/self/mountstats` carries per-mount NFSv4 operation counters
(`OPEN`, `CLOSE`, `LOCK`, `LOCKT`, `LOCKU`, `COMMIT`, `READ`, `WRITE`,
`GETATTR`, `SETATTR`, `DELEGRETURN`), plus per-operation timings, retransmits and
error counts. It is on every Linux client, needs no new binary, and is read
through the node agent's existing `ReadFile` (`pkg/framework/nodeagent.go:150`),
which is how `/proc/mounts` and `/proc/locks` already arrive. Read it before and
after the operation under test and assert the delta.

This is the single highest-value item on the list: it turns roughly eight cases
from "the bytes were right" into "the operation the protocol names actually
happened".

**Update.** [`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md)
Section 2 (verification tools) and Section 4 (the assertion table's Source &
Basis column).

### R2. Lease and grace are read from what the deployment declares, never measured.

**Now.** `DiscoverTiming` (`pkg/framework/server.go:276`) resolves lease and
grace in this order: `-lease-seconds` / `-grace-seconds` flags, then the server
pod's container env, args and command, then a mounted ConfigMap's values. All
three are declarations. `pkg/preflight/preflight.go:123` then pins the run to a
profile from that value, and every bound in `pkg/slo` keys off the profile.

**Why it matters.** A server whose config key is misspelled, whose build ignores
the key, or whose compiled default differs from the ConfigMap it ships with,
yields a profile that matches cleanly and bounds that are wrong by a factor. The
suite would then report SLO results against a lease that is not in force, and
nothing in the run contradicts it. The plan already treats this as load-bearing
("every timing assertion depends on them", Section 0) but the check stops at
discoverability, not correctness.

**Do.** Two measurements already exist and are not fed back:

- **Lease.** DATA-06 (`test/e2e/data_test.go:650`) times how long a
  force-deleted pod's byte-range lock takes to become grantable, and already
  classifies "about one second" as descriptor close versus "about a lease" as
  lease expiry. Compare the lease-expiry case against `p.Lease` and report a
  divergence.
- **Grace.** OBS-03 measures the observed grace window (`GraceWindow.Duration`,
  `pkg/framework/grace.go`). Compare it against the declared grace and report a
  divergence.

Neither has to fail the run on the first mismatch. Recording "declared 60s,
measured 94s" in `environment.json` is enough to stop a whole category of wrong
results being believed.

**Update.** [`01-test-plan.md`](01-test-plan.md) Section 3.8 and
[`03-chaos-operations-design.md`](03-chaos-operations-design.md).

### R3. Grace is read from one channel, and the reference deployment does not use it.

**Now.** `ObserveGrace` reads container log streams only:
`ServerLog` to `containerLog` to `Pods().GetLogs` (`pkg/framework/grace.go:151`
onward). F-022 found the grace lines on `gke-w1`, in the NFS daemon's own log
file inside the export volume, which `kubectl logs` does not carry. The
consequence is recorded in the plan's Section 5.2: OBS-03 fails and CHAOS-07
blocks, on a server that does announce grace.

**Why it matters.** Grace is the dominant term in every recovery number, and
CHAOS-07 is the only case that asserts the protocol's rule that new state is not
granted during grace. It has never run against a real grace window.

**Do.** Two channels, both consistent with the escape hatches already in the
tree, neither of which guesses:

- `-server-log-path`: exec into the server pod and read the daemon's own log
  file, feeding the same classifier. The harness already execs into the server
  pod for `ServerConns` and `serverConfigBlob` (`pkg/preflight/discovery.go:210`).
- `-grace-metric`: read grace or lease state from the metrics endpoint the
  OBS-07 reader already scrapes (`pkg/framework/metrics.go`). F-023 confirms this
  deployment serves 39 families including lease, lock and client-state metrics
  when `Enable_Metrics` is on. Naming the series by flag mirrors
  `-grace-enter-pattern` and keeps `metrics.go`'s rule that nothing here knows a
  metric by name.

**Update.** [`06-observability-design.md`](06-observability-design.md) and
[`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md).

### R4. `blocked` and `skipped` are the same thing to every consumer except a person reading logs.

**Now.** `blocked()` is `t.Skipf` (`test/e2e/helpers_test.go:64`). The README
defines four verdicts; `go test` emits three. There are 58 skip or blocked sites
across `test/e2e/`: 15 in `sec_test.go`, 14 in `obs_test.go`, 11 in
`chaos_test.go`, 7 in `prov_test.go`, 6 in `data_test.go`, 5 in
`helpers_test.go`.

**Why it matters.** The plan already says it: "Blocked and skipped are not
passes: those vectors are unexercised." But nothing enforces that. A run in
which every server-facing case blocked on a missing flag exits zero and looks
like a green run in any tooling that reads exit codes. On the last whole-suite
run, 4 of 35 cases were blocked and the fact lives in a prose paragraph.

**Do.** Write a per-case ledger to `artifacts/<run-id>/verdicts.json`: case ID,
verdict from the four, one line on what was asserted, one line on what was not,
and the flag or capability that would unblock it. The fixture already knows the
case ID (`framework.New(t, "DATA-03")`), and `artifacts.go` already writes
per-run files (`WriteArtifact`, `pkg/framework/artifacts.go:161`). Then gate:
`make test-e2e` fails when a named set of cases, the ones carrying the
server-facing assertions, comes back blocked.

**Update.** README's "Reading a run", [`01-test-plan.md`](01-test-plan.md)
Section 4.3.

### R5. CI enforces the harness and nothing about NFS.

**Now.** `.github/workflows/ci.yml` runs formatting, vet, unit tests, build and
the locktool cross-compile. The README states the position plainly: "What none of
them can tell you is whether NFS behaves; that needs a cluster."

**Do.** Add a CI job that stands up a multi-node `kind` cluster with an
in-cluster NFS server and runs `make test-prov test-data test-sec`. kind supports
several worker nodes, privileged DaemonSets and host namespaces, which is
everything the node agent needs (`pkg/framework/nodeagent.go:129`), and the two
faults in `pkg/chaos` today, pod delete and process SIGKILL, both work there.

What it will not give: node power faults, real network partitions, dual-stack
unless configured, and a real CSI expansion path. Scope it as a gate for the
server-facing cases that can run hermetically, not as a replacement for the GKE
runs. The value is that a change to `sweep.go` or `load.go` would have to keep
passing against a real NFS server before it merges, which is the class of bug
F-007, F-011, F-014 and F-015 all belong to.

### R6. Classifiers that render verdicts about the server need at least one fixture captured from a real run.

**Now.** The tree already has the good pattern in two places, and the README
names both: `peers_test.go` runs against socket-table rows copied from a real
server, and `io_parse_test.go` runs the real `stat` and `df` on the machine
running the test. Neither is an invented fixture.

The ones that decide something about the server on hand-written input:

| Test | Decides | Gap |
|---|---|---|
| `pkg/framework/grace_test.go` | whether a server entered or left grace | wordings are plausible and none is a line from the one deployment that has been run (F-022 has them) |
| `pkg/framework/metrics_test.go` | whether counters survived a restart | bodies are "in the shape a server serves them"; F-023 scraped 39 real families and none was kept |
| `pkg/framework/kubeletstats_test.go` | whether the control plane reports a volume's usage | hand-built summary objects |
| `pkg/framework/lockmount_test.go` | which client holds which range | hand-built `/proc/locks` lines, though F-010's device rule is covered |

**Do.** Not deletion. Keep the invented edge cases, which are what cover the
shapes a real run does not produce, and add one captured fixture per classifier
under `testdata/`, named with the run ID it came from. F-024 is the argument:
`ClassifyMetrics` passed its unit tests and still reported `resumed-continuous`
for a server whose uptime was one second.

**Update.** [`06-observability-design.md`](06-observability-design.md), and the
README's paragraph on what the unit tests cover.

### R7. The Kubernetes storage API is read shallowly on the object-status side.

**Now.** Across `pkg/` and `test/`, the API surface is Pods (24 call sites),
PersistentVolumeClaims (19), PersistentVolumes (13), Nodes (5), StorageClasses
(4), Events (2), ConfigMaps (2), DaemonSets (2), CSIDrivers (1), Services (1).

Not read anywhere:

- **`VolumeAttachment`.** Attach and detach are inferred from
  `Node.Status.VolumesInUse` (`pkg/framework/unmount.go:167`). VolumeAttachment
  is the object the attach-detach controller actually drives, and it is where a
  stuck detach is visible. CHAOS-03 (node loss, step 10) needs it, and the plan's
  floor note on `maxWaitForUnmountDuration` is entirely about that object's
  lifecycle.
- **`PVC.Status.AllocatedResources` and `AllocatedResourceStatuses`.** PROV-04
  (`test/e2e/prov_test.go:250`) and PROV-11 (`:1172`) wait on `status.capacity`
  through `WaitPVCCapacity` (`pkg/framework/pvc.go:204`). A resize the controller
  accepted, recorded and then abandoned is indistinguishable from one never
  accepted. F-012 already fixed the conditions half of this;
  the allocated-resource fields are the other half and are the ones that say
  a resize is in flight.
- **`CSIDriver.spec.fsGroupPolicy`.** SEC-03 (`test/e2e/sec_test.go:373`)
  measures what fsGroup does to the volume and records it. `fsGroupPolicy` is
  where the driver declares what it will do. Reading it turns SEC-03's record
  into a declared-versus-observed comparison, which is exactly the shape F-020
  got wrong once. Same for `.spec.attachRequired`, which `unmount.go:64` already
  fetches for one purpose and could assert for another.
- **`CSINode`.** Which drivers a node carries and `allocatable.count`, which is
  SCALE-01's ceiling before the ramp starts.

**Update.** [`02-provisioning-design.md`](02-provisioning-design.md).

### R8. The 4.1 check happens once, at preflight, on one volume.

**Now.** `assertNFS41` (`pkg/preflight/preflight.go:344`) checks the preflight
probe's own mount and sets `e.NFSVersion = "4.1"` (`:92`). Every case after that
provisions its own claim, and nothing re-reads the negotiated version for those
mounts. The lock cases read `/proc/mounts` per claim already
(`CheckLockMount`), but only for `nolock` and `local_lock`.

**Why it matters.** The whole plan is calibrated to NFSv4.1 semantics and says
so in Section 2.3. A driver that falls back to 4.0 or v3 for a
differently-shaped claim, or under load, leaves DATA and CHAOS asserting 4.1
guarantees against a mount that does not make them, and the run records 4.1
because preflight said so.

**Do.** Extend the per-claim mount reader to record and assert the negotiated
version, and put it in the per-case artifact. Low cost, and it closes the gap
between what `environment.json` claims and what each case actually ran on.

### R9. SEC-08 records the data path rather than observing it. The mechanism to observe it already exists.

**Now.** SEC-08 (`test/e2e/sec_test.go:1454`) takes four readings: mount options
from the node, the server's listening ports, reachability from a pod with no
claim, and NetworkPolicies in the server's namespace. It then writes, honestly:
"no packet capture was taken: it needs tcpdump on a node image that has none".

**Why the reasoning should be revisited.** The repository already solved this
class of problem once. `locktool` is a static Go binary this repository owns,
built by `make locktool` for two architectures and streamed into a pod over
`pods/exec` with a SHA-256 check, precisely because the image could not be relied
on to carry `flock -w` (F-006). The node agent runs privileged with host network
and PID namespaces (`pkg/framework/nodeagent.go:129`).

**Do.** The same mechanism carries a minimal `AF_PACKET` reader into the node
agent. Scope it narrowly: decode the RPC record mark, the NFSv4 `COMPOUND` op
codes, and the status word. Nothing more. That buys two things:

- SEC-08 gets direct evidence instead of an inference: an NFS operation
  recovered in cleartext off the wire.
- CHAOS-07 and OBS-03 get a grace channel that does not depend on log wording at
  all. `NFS4ERR_GRACE` (10013) in a reply is the protocol's own statement that
  the server is in grace, and it is the same on every implementation, which is
  the property the log classifier can never have.

This is the largest item here and the one most worth staging behind R1 and R3.
Note the honest cost: an XDR decoder is a thing to maintain, and getting it wrong
produces confident nonsense. Scoping to op code and status keeps it small.

### R10. DATA-02's caveat is an argument where it could be a measurement.

**Now.** F-016: four clients appending landed 150 of 200 records, none torn, one
appender losing its whole contribution, intermittent across runs. DATA-02
(`test/e2e/data_test.go:451`) reports this with a caveat that NFSv4.1 has no
append operation, so an exact count is an implementation property rather than a
protocol guarantee. The plan (Section 3.2) leaves open whether the case belongs
in the gate at all.

**Do.** R1's mountstats delta answers the question the caveat argues about: if
the lost appender's `WRITE` count matches its record count, its writes reached
the server and landed at an offset another client also wrote, which is the
believed-EOF race stated as a fact. If the count is short, the loss is on the
client. Either answer routes the defect. Today neither is available and the case
can only cite the boundary.

---

## 4. The rule, and the exceptions to it

The rule this audit argues for: **a local test may test what the harness does.
Only a cluster may test what the server does.** Utilities and file parsing are
the common case of the first half, not the whole of it. Four classes sit outside
"utilities and parsing" and still belong on the workstation, because no healthy
server can be asked to demonstrate them.

### 4.1 Exception: guards on destructive actions

The rules that decide what the harness is about to break. `ProcessPattern`
(`pkg/chaos/chaos.go`, tested at `pkg/chaos/chaos_test.go:103`) decides which PID
takes a SIGKILL and refuses generic names like `sh` and `env`. `FirstInjurable`
(`chaos_test.go:157`) refuses to aim a fault at a terminating or Pending pod,
which is F-013 one level up. `checkProbePath` (`peers_test.go:430`) rejects path
escapes. `probeMountOptions` is soft where every other mount in the suite is hard
(`pkg/framework/probemount.go:41`), which is the F-003 and F-005 safety case.

A cluster test of any of these has two outcomes: it does the right thing, proving
nothing, or it does the wrong thing and damages the system under test. The
workstation is not a compromise here, it is the only safe place.

### 4.2 Exception: guards against a case passing without measuring anything

F-007, F-011, F-014 and F-015 are one bug in four places: a reader returned a
zero or an empty string where it should have returned an error, two nothings
compared equal, and a case passed having measured nothing. One of them had been
passing for months.

The guards are local and have to be: a healthy server produces good input by
definition, so a server-anchored test never reaches the path. `parseSum` rejects
an empty digest (`io_parse_test.go:298`, F-015). `shortSweepError` fires on a
sweep that answered for 0 of 14 records (`sweep_test.go:216`, F-011).
`LastRecord` refuses to name one from an empty log (`load_test.go:483`, F-014).
`MovementFloor` refuses a reading that never moved (`volumeusage_test.go:99`).

This is the local test class the findings log says actually catches things, and
it is neither a utility nor a parser.

### 4.3 Exception: preconditions for anchoring on the server at all

`TestNodeAgentManifestRenders` asserts the agent container is privileged
(`manifest_test.go:182`). If that regresses, every node-level assertion degrades
to **blocked** rather than failing, which under R4 is currently invisible. A
local test makes the loss loud at `make unit` instead of quiet on a cluster. The
locktool cross-compile in CI is the same argument: the alternative is finding an
arm64 mistake during a cluster run.

### 4.4 Exception: the suite's own policy, which has no external referent

`pkg/slo/slo_test.go` restates constants in a second form. Close to tautological,
and worth the lines: no server can tell you your own SLO table is wrong, and a
wrong number there silently invalidates every chaos result.

### 4.5 What does not qualify, which is where the rule bites

Four unit tests render a **verdict about the server** from hand-written input.
That is outside "utilities and parsing" and outside all four exceptions above:

| Test | Verdict it renders | What to do |
|---|---|---|
| `grace_test.go` | a server entered or left grace | split: keep the wording rule's edge cases, add a captured fixture, and make OBS-03 able to run (R3) |
| `metrics_test.go`, the `ClassifyMetrics` half | counters survived a restart | F-024 is the proof: it passed its unit tests and still reported `resumed-continuous` for a process whose uptime was one second. Needs a captured scrape (R6) |
| `kubeletstats_test.go` | the control plane reports this volume's usage | hand-built summary objects; capture one (R6) |
| `lockmount_test.go` | which client holds which range | the parse half is parsing and is fine. The device-matching rule (F-010) is a claim about how the NFS client assigns `st_dev`, which is a kernel property and needs a captured fixture |

Split each one: the parse stays local, the verdict becomes either
captured-fixture-backed or server-anchored.

### 4.6 The other direction: what must not be turned into an assertion

A few results are correctly unanchorable, and the rule should not push them into
assertions. SEC-08 records whether the data path is cleartext rather than failing
on it. SEC-02 records which root rule is in force when `-root-squash` was not
stated. OBS-07 reports `resumed-indeterminate` rather than claiming continuity
(F-024). These are not local validation; they are the suite declining to invent
an expectation it was never given, which is the right answer.

### 4.7 One correction to the framing

"Anchor on the server and NFS operations" is slightly too narrow for this suite.
Roughly a third of the shipped cases are about the **Kubernetes storage
contract**, not the NFS protocol: PROV-03's `pvc-protection` finalizer, OBS-04's
Event on a pod whose volume never mounted, PROV-04's requirement that a class not
advertising expansion reject the request, PROV-06's Retain behaviour. None of
them makes an NFS operation claim, and none should.

The plan already splits defect routing on exactly this line (Section 4.3:
provisioning failures to the CSI driver owner, data path failures to the server
owner). So the rule is best stated as **anchor on the system under test**, where
the system is the NFS server and the Kubernetes storage API, and each case names
which of the two it is asking.

## 5. Summary

| # | Recommendation | Primary pointers | Doc to update |
|---|---|---|---|
| R1 | Read `/proc/self/mountstats` and assert operation deltas | `pkg/framework/nodeagent.go`, `test/e2e/data_test.go` | doc 05 §2, §4 |
| R2 | Measure lease and grace, compare against the declared profile | `pkg/framework/server.go:276`, `pkg/preflight/preflight.go:123` | plan §3.8, doc 03 |
| R3 | Add a second and third grace channel: server log file, metrics series | `pkg/framework/grace.go`, `pkg/framework/metrics.go` | doc 06, doc 04 |
| R4 | Emit a four-verdict ledger and gate the run on blocked cases | `test/e2e/helpers_test.go:64`, `pkg/framework/artifacts.go` | README, plan §4.3 |
| R5 | Run the server-facing cases in CI against a kind cluster | `.github/workflows/ci.yml`, `Makefile` | README |
| R6 | One captured fixture per server-verdict classifier | `grace_test.go`, `metrics_test.go`, `kubeletstats_test.go` | doc 06, README |
| R7 | Read VolumeAttachment, allocated-resource status, fsGroupPolicy, CSINode | `pkg/framework/unmount.go:64`, `pkg/framework/pvc.go:204` | doc 02 |
| R8 | Assert the negotiated NFS version per claim, not once at preflight | `pkg/preflight/preflight.go:344`, `pkg/framework/lockmount.go` | plan §0 |
| R9 | A static capture tool into the node agent, op code and status only | `cmd/locktool` as precedent, `test/e2e/sec_test.go:1454` | doc 07 |
| R10 | Settle DATA-02's caveat with wire counters | `test/e2e/data_test.go:451`, F-016 | doc 05 §4 |

R1, R3 and R4 are the ones that change what a green run means. R5 is the one
that keeps it that way.

## 6. Sources

- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html), NFSv4.1: Section 9
  (locking), Section 10 (client caching), Section 18.3 (`COMMIT`), Section 13.1.8
  and the `NFS4ERR_GRACE` error code.
- [`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) and the kernel's
  [NFS client statistics](https://docs.kernel.org/filesystems/nfs/index.html)
  for `/proc/self/mountstats` per-operation counters.
- Kubernetes [storage API](https://kubernetes.io/docs/concepts/storage/persistent-volumes/):
  `VolumeAttachment`, `CSINode`, `CSIDriver` (`fsGroupPolicy`, `attachRequired`),
  and the PVC resize status fields
  ([recovering from expansion failure](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#recovering-from-failure-when-expanding-volumes)).
- This repository: [`findings.md`](findings.md), in particular F-006, F-007,
  F-010, F-011, F-012, F-013, F-014, F-015, F-016, F-020, F-022, F-023, F-024.

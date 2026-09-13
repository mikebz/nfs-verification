# Findings

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-13

Things learned by running the suite against a real cluster that are worth
remembering. Each entry is dated, and says what happened, why, what changed in
the code, and what it implies for the system under test as opposed to the
harness.

**This file is the citation of record.** A case that skips or reports blocked, a
constant that is the value it is, a teardown step that looks like more work than
it should be: if the reason came from a real run, the code comment and the design
doc name the `F-NNN` rather than re-explaining it. A reason that lives only in a
commit message or a pull request comment is lost by the next one.

New entries go at the top, and take the next number.

| # | Found | What it says | Cited by |
|---|---|---|---|
| [F-015](#f-015-a-checksum-that-failed-came-back-as-an-empty-string-and-a-success) | 2026-09-13 | A checksum pipeline ending in `cut` exits zero when `sha256sum` fails, so a helper returned an empty string as a digest | `io.go`'s `sumCmd` and `parseSum`, `io_parse_test.go` |
| [F-014](#f-014-recovery-was-measured-to-a-write-that-committed-before-the-outage) | 2026-09-12 | Time to first I/O after a fault returned a write from before service was lost, reporting a 1m43s failover as 0s | `load.go`'s stall measurement, `slo.LoadStallFloor`, CHAOS-02/05/06/07 |
| [F-013](#f-013-a-terminating-server-pod-counted-as-the-server-being-back) | 2026-09-11 | A gracefully deleted pod stays Running and ready, so the wait for a replacement was satisfied by the pod it was waiting past | `server.go`'s readiness check, `server_test.go` |
| [F-012](#f-012-the-resize-diagnosis-was-unreachable-because-an-unrelated-condition-was-always-there) | 2026-09-11 | Listing every claim condition made the "nothing acted on the request" diagnosis unreachable on any mounted claim | `pvc.go`'s resize description, `pvc_test.go` |
| [F-011](#f-011-a-record-sweep-came-back-empty-from-an-exec-that-reported-success) | 2026-09-12 | A sweep exec returned success with no output at all, and the old message could not tell that from finding nothing | `sweep.go`'s short-answer error, `sweep_test.go` |
| [F-010](#f-010-a-lock-case-accused-the-server-of-losing-a-lock-that-was-plainly-still-held) | 2026-09-12 | A file's identity on an NFS mount is per-node, so one identity cannot match two nodes' lock tables | CHAOS-06's range assertion, `lockmount_test.go` |
| [F-009](#f-009-the-export-has-no-per-volume-quota-so-capacity-monitoring-describes-the-backing-filesystem-and-not-the-claim) | 2026-09-11 | The export has no per-volume quota, so both capacity sources describe the backing filesystem rather than the claim | OBS-06's failure message |
| [F-008](#f-008-nfs-server-provisioner-never-announces-grace-so-the-grace-cases-cannot-run-against-it) | 2026-09-11 | This provisioner never announces grace, so OBS-03 fails and CHAOS-07 reports blocked | CHAOS-07's blocked message, doc 04, doc 06 |
| [F-007](#f-007-two-cases-in-the-data-path-phase-reported-results-they-had-not-measured) | 2026-09-11 | Two cases reported results they had not measured | doc 05 |
| [F-006](#f-006-scriptslock-probesh-passed-flock--w-which-busybox-does-not-have) | 2026-09-11 | `flock -w` does not exist on busybox, so the lock probe never waited | `scripts_test.go` |
| [F-005](#f-005-exponential-mount-propagation-in-gkes-mountnfs-wrapper-wedges-worker-nodes) | 2026-09-11 | A GKE `mount.nfs` wrapper multiplies mounts until the node wedges | preflight records mount propagation |
| [F-004](#f-004-allowvolumeexpansion-is-a-claim-not-a-capability) | 2026-09-10 | `allowVolumeExpansion` is advertised, not implemented, so PROV-04 cannot trust it | `pvc.go`'s expansion helper |
| [F-003](#f-003-a-broken-umountnfs-wrapper-on-gke-wedges-every-terminating-pod) | 2026-09-10 | A broken `umount.nfs` wrapper wedges every terminating pod | teardown's terminate bound |
| [F-002](#f-002-2gb-worker-nodes-cannot-host-the-suite) | 2026-09-10 | 2GB worker nodes cannot host the suite | the node shape a run reports |
| [F-001](#f-001-force-deleting-a-mounted-pod-can-take-a-node-out-of-service) | 2026-09-10 | Force-deleting a mounted pod, then its claim, takes a node out of service | teardown, the force-delete helper, PROV-03, doc 05 |

---

<<<<<<< HEAD
## F-015: A checksum that failed came back as an empty string, and a success

**Found:** 2026-09-13, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, kernel 6.12.94+, StorageClass `nfs`
backed by `nfs-server-provisioner` as a single-replica StatefulSet, profile
`default` (lease 60s, grace 90s). Run `full-e2e-20260912b`, `make test-e2e`,
the whole suite: 25 pass, 6 fail, 4 skip.

**Severity:** a case could compare two checksums, get two empty strings, and
pass having verified nothing.

### What happened

PROV-07 failed with a message that is its own bug report:

```
prov_test.go:800: checksum mismatch: got 27b6c42aeab701165276442cccea836fdfda33a1b64205eb20a9a40be56aeb9f want
```

`want` comes from `WriteFile`, which writes a file and returns its sha256. It
returned an **empty string and no error**. The case had passed on the two
previous runs of the same code.

The case was right to fail, and it failed for the wrong reason: nothing was
wrong with the data as far as anyone can tell. What it caught was its own
helper.

### Why

The script ended `sha256sum "$f" | cut -d' ' -f1`.

A POSIX pipeline reports the status of its **last** command. When `sha256sum`
fails, `cut` reads nothing, prints nothing, and exits zero. So:

- the pipeline exits zero, and `set -e` never fires;
- the exec is a success, so the caller never looks at `Combined()`, and
  `sha256sum`'s complaint on stderr is discarded unread;
- `MustSh` trims empty stdout and returns `("", nil)`;
- `WriteFile` hands that back as a checksum.

Reproducible on a workstation in one line, and now asserted in
`TestSumReportsAFailedChecksum`:

```
$ sh -c "set -e; sha256sum /tmp/not-here | cut -d' ' -f1"; echo "exit $?"
sha256sum: /tmp/not-here: No such file or directory
exit 0
```

The near-miss is the interesting part. `ReadDirect` and `CountNonZeroBytes` in
the same file already carried comments warning about exactly this — *"a POSIX
pipeline reports its last command's status, so piping a failed read into a
counter yields a confident zero"* — and both then ended their own scripts with
`sha256sum | cut`. The hazard was understood one command upstream and missed one
command downstream.

### What changed

- `sumCmd` is the last command of every script in the package that produces a
  checksum: `sha256sum` alone, nothing downstream. Its status is now the
  script's status, so a failure arrives as a failure with stderr attached.
- `parseSum` splits the digest off in Go and refuses anything that is not 64 hex
  characters, naming the pod and the path. An empty answer is an error, not
  data.
- Five call sites: `WriteFile`, `Sha256`, `WriteDirect`, `ReadDirect`, and the
  `locktool` integrity check, which could previously blame the exec stream for a
  `sha256sum` that never ran.
- `TestSumReportsAFailedChecksum` runs the generated command under a real shell
  against a missing file and asserts a non-zero exit. Restoring the `| cut` form
  fails it.

### What it means for the system under test

**Not yet known, and that is the finding's open item.** Something made
`sha256sum` fail on the share at that moment, and the evidence went to a stderr
nobody kept. PROV-07 writes immediately after the server pod has been deleted
and replaced, so a stale handle or a transient error on the freshly recovered
mount is the obvious suspect — and it is only a suspect. A later read of the
same file in the same pod produced a digest, so the file was there.

If this is a real post-failover error on the client, it is a deployment finding
worth having. The reason it is not one today is that the harness threw the
evidence away, which is the same reason F-011 is still unexplained: an exec that
reports success with empty output, where the interesting half was on stderr.
This entry does not claim F-011 has the same cause; it does show that the shape
is producible without anything going wrong at the exec layer at all.

**A pipeline is not a chain of assertions. Only its last command can fail it,
so nothing that matters may be followed by something that does not care.**

---

## F-014: Recovery was measured to a write that committed before the outage

**Found:** 2026-09-12, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, kernel 6.12.94+, StorageClass `nfs`
backed by `nfs-server-provisioner` as a single-replica StatefulSet, profile
`default` (lease 60s, grace 90s). Runs `pr39-chaos-20260912` and
`pr39v2-chaos-20260912`, `make test-chaos`.

**Severity:** every recovery number the chaos cases have ever reported is
suspect, and the SLO comparison that uses it could not fail.

### What happened

CHAOS-05 injects the same fault five times. In one passing run it reported:

| cycle | reported |
|---|---|
| 1 | 1m43s |
| 2 | **0s** |
| 3 | 1m38s |
| 4 | 1m43s |
| 5 | **0s** |

A server pod deletion cannot recover in `0s` on a deployment that takes about
90 seconds to come back, and the same case closed with `longest gap between
attempts was 1m44s`, so the outage plainly happened in every cycle.

CHAOS-06 and CHAOS-07 printed the contradiction on adjacent lines:

```
first committed write 0s after the fault (budget 2m0s, profile default)
longest gap between attempts was 1s
```

A one-second longest gap does not mean a seamless failover. It means the
measurement **finished before the outage started**, and the gap statistic was
then computed from that same premature snapshot of the log.

Both numbers were wrong in the permissive direction: `0s` passes a two-minute
budget, so the assertion could not fail no matter how bad recovery was.

### Why

Recovery was read as `FirstSuccessAfter(faultAt)`, the first committed write
timestamped at or after the fault. Three things conspire:

- The workload writes one record per second and stamps it with `date +%s`, so
  record times have **one-second resolution**.
- `faultAt` comes from the same clock, read by an exec **before** the `DELETE`
  is issued.
- `FirstSuccessAfter` is inclusive, so a write in the same one-second bucket as
  `faultAt` — possibly committed *before* it — satisfies the wait at once.

So the poll returned on its first iteration, with a write that committed while
the server was still up, and recovery came out as zero. Whether a given cycle
reported honestly was a race between the exec round trip and the writer's
one-second cadence, which is why the same run produced both `1m43s` and `0s`.

**Moving the anchor to after the `DELETE` would not have fixed it.** A deleted
pod keeps serving through its termination grace, so a write committed in the
seconds after the call returns is still a pre-outage write. Anchoring is not the
problem; asking the wrong question is.

### What changed

The outage is now found by its **shape** rather than its timestamps. On a hard
NFSv4.1 mount a client that loses its server blocks rather than erroring
([`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html)), so an outage is a
silence in a log that is otherwise one line per second, and that silence is the
only unambiguous evidence of when service was actually lost.

- `LoadReport.StallAfter` returns the first silence longer than a floor whose
  resuming write lands at or after the fault, and recovery is measured to that
  write. `LongestGap` was already reporting this honestly from the same log; the
  recovery path simply was not using it.
- `slo.LoadStallFloor` is the floor, at 10s: well above the one-to-two second
  cadence of a healthy stream, well below the 60s smallest recovery bound any
  profile states, so it separates the two without being near either.
- Cases now log what was measured, not just the result: `recovered 1m43s after
  the fault: the client made no progress for 1m41s, from record 12 to record
  13`. A reader can check the claim against the stream.
- A workload that never stalls is reported as such rather than scored as an
  instant recovery, and `LastRecord` separates that from a client still blocked
  in an outage that never ended. Those are opposite results that previously both
  arrived as silence.
- `FirstSuccessAfter` keeps its narrower meaning and now says in its doc comment
  that it is not the recovery measurement.

The unit test deliberately asserts that the **old** reading still returns the
pre-outage write, so the difference between the two questions is pinned in code
rather than argued in a comment.

### What it means for the system under test

Nothing yet, and that is the point. This is a harness defect: no statement about
this deployment's recovery time should be drawn from any run before this change,
in either direction. The `1m38s`–`1m44s` readings are consistent with a roughly
90-second grace period plus client retry backoff and are plausibly real, but
they were produced by a measurement that demonstrably returns the wrong answer
under a race, so they are not evidence.

The `0s` readings were never evidence of a fast failover. They were the absence
of a measurement.

The general rule: **a measurement that can only be wrong in the permissive
direction will never fail, so nothing about it looks broken.** Every green
recovery assertion in this suite passed for two weeks while this was live. When
a measured quantity has an obvious reading and a correct one, the obvious one
needs a test that pins the difference, and a number printed next to the evidence
that contradicts it is the cheapest way to notice.

---

## F-013: A terminating server pod counted as the server being back

**Found:** 2026-09-11, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, StorageClass `nfs` backed by
`cluster.local/nfs-provisioner-nfs-server-provisioner`, default profile from
flags. CHAOS-05, in run `e2e-full-20260911`, and reproduced twice on 2026-09-12.

**Severity:** high. It fails a case with a message that blames the deployment
for something the harness did, and the cycle it lands on moves between runs, so
it reads as flakiness rather than as a defect.

### What happened

CHAOS-05 deletes the server pod five times and measures each recovery. It fails
partway through:

> cycle 3: the server did not come back as a pod this case can find again: no
> NFS server pods found: pass `-server-namespace` and `-server-selector`,
> otherwise there is nothing for a chaos case to injure

The cycle varies. Three runs against the same cluster on the same flags failed
at cycle 2, cycle 4 and cycle 3, in that order. The message names the two flags
that configure discovery, so the obvious reading is that discovery is
misconfigured. It is not: the earlier cycles in the same run found the pod
without trouble, using the same flags.

### Why

Each cycle ends by waiting for the server to be ready again, and the comment
above that wait says what it is for: so that the pod the next cycle aims at is
the replacement rather than the one still terminating. The wait could not do
that, because `PodReady` looked only at the phase and the container statuses.

A pod that has been gracefully deleted keeps phase `Running` with its containers
ready for the whole of its termination grace period. So the wait was satisfied
immediately, by the very pod it had been written to wait past. The cycle then
moved on, the old pod went to phase `Failed`, `runningOnly` dropped it, the
replacement was not up yet, and the next `ServerTarget` found nothing.

Which cycle this lands on depends on how the grace period lines up with the rest
of the cycle, which is why it moves between runs.

The comment and the code disagreed, and the comment was right.

### What changed

- `PodReady` returns false for a pod carrying a `DeletionTimestamp`.
- `TestPodReadyIgnoresTerminating` covers it, including the states that already
  worked, so the check reads as an addition rather than a replacement.

`WaitServerReplaced` and PROV-07's inline check already paired readiness with a
changed UID or name, so they were protected by accident rather than by the
readiness answer being right. They get the correct answer now too.

`WaitPodReady` in `pod.go` repeats the phase and container logic separately and
has the same gap. It is left alone deliberately: it waits on a pod the harness
has just created, where no case produces a deletion timestamp. It is written
down here because the duplication is what would let this come back.

### What it means for the system under test

Nothing. The server came back every time. What the case reported as the
deployment failing to restore its server was the harness asking whether a server
was ready and being told yes about one that was being deleted.

The general rule: **Running and ready is not the same as not going away.** Any
check that a thing is up, where something has just been deleted, has to exclude
the deleted thing explicitly, because Kubernetes keeps reporting it as healthy
until it is gone.

---

## F-012: The resize diagnosis was unreachable because an unrelated condition was always there

**Found:** 2026-09-11, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, StorageClass `nfs` backed by
`cluster.local/nfs-provisioner-nfs-server-provisioner`, default profile from
flags. PROV-04 and PROV-11, in run `e2e-full-20260911`.

**Severity:** medium. Nothing is asserted wrongly. What is lost is the
explanation, on the two cases whose failure most needs one, and what replaced it
points at the wrong thing.

### What happened

Both expansion cases timed out and said:

> claim `nfsv-prov-04-e2e-full-20260911-prov04` reports 1Gi, want at least 2Gi
> [Unused=False(A pod is currently referencing this PVC)]

PROV-11 said the same. The bracket is meant to hold the resize condition the
driver left behind, or, where there is none, the F-004 diagnosis: that the class
advertises `allowVolumeExpansion`, nothing picked the request up, and the place
to look is whether an external-resizer sidecar exists at all.

That diagnosis never appeared, and the condition that appeared instead has
nothing to do with expansion. A reader chasing `Unused=False` is chasing the
fact that the claim is mounted, which is exactly what the case arranged on
purpose.

### Why

`describeResizeConditions` listed every condition in `status.conditions` and
fell back to the diagnosis only when the list came out empty. This cluster posts
an `Unused` condition on a claim a pod references, so the list is never empty on
a mounted claim, and every claim these two cases expand is mounted by design.
The fallback was unreachable on the only path that reaches it.

Whether `Unused` is upstream Kubernetes or a GKE addition has not been
established, and it does not matter to the fix: the helper must not assume that
every condition on a claim is about the thing it is describing.

### What changed

- `describeResizeConditions` selects the four expansion conditions
  (`Resizing`, `FileSystemResizePending`, `ControllerResizeError`,
  `NodeResizeError`) instead of listing everything.
- Conditions it does not recognise are named, so a reader knows the claim was
  not condition-free, but kept out of the verdict, so they do not read as the
  reason for the timeout.
- `TestDescribeResizeConditions` covers the claim carrying only the unrelated
  condition, using the exact condition the cluster posted. Reverting the filter
  makes it fail and reproduces the original message verbatim.

Verified on the cluster, 2026-09-12, run `pr38-prov-20260912` against `gke-w1`.
Both cases still time out, and both now print the diagnosis:

> no resize condition was ever posted on the claim: nothing acted on the
> request. The StorageClass advertises allowVolumeExpansion, so check whether
> its provisioner supports expansion at all and whether an external-resizer
> sidecar is running alongside the CSI driver. The claim does carry conditions
> that say nothing about expansion: Unused

### What it means for the system under test

Nothing new. F-004 stands unchanged: this provisioner advertises
`allowVolumeExpansion` and does not perform one, and the two timeouts in that
run are that behaviour, not a harness fault. The only thing that was wrong is
that the run did not say so.

The general rule: **a fallback branch guarded by "the list is empty" is a
fallback that something else gets to disable.** Where the interesting answer is
the absence of a signal, select the signal rather than counting everything.

---

## F-011: A record sweep came back empty from an exec that reported success

**Found:** 2026-09-12, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, StorageClass `nfs` backed by
`cluster.local/nfs-provisioner-nfs-server-provisioner`, default profile (60s
lease, 90s grace) from flags. CHAOS-06 and CHAOS-07, in run
`pr36-chaos-20260912`.

**Severity:** high, as a harness defect. It stops the durability assertion from
reaching a verdict, and the message it stopped with pointed at the data rather
than at the harness.

**Mechanism: open.** This entry records the shape and what has been ruled out.
It does not claim a cause.

**Intermittent.** It did not reproduce on the next run against the same cluster
with the same flags, an hour later: `pr37-chaos-20260912` swept 15 of 15 and 5
of 5, all correct, in the same two cases. Two runs is not a rate, and nothing
here says what makes the difference. A run that sweeps cleanly is not evidence
that this is fixed, and no change so far has tried to fix it.

### What happened

Both cases failed at the same place, which is the first assertion after the
fault:

> sweeping committed records from verifier: the sweep answered for 0 of 14
> records, so what it did say cannot stand for the set

CHAOS-07 said the same for 5 records. CHAOS-02, in the same run and through the
same helper, swept 4 records and got 4 correct.

Nothing else in either case had gone wrong yet. CHAOS-06's own
`byte-ranges-before-failover` subtest had passed, the workload reported a
longest gap of 1s and no I/O errors, and the verifier pod was `Running`, ready,
with no restarts and no deletion timestamp.

### Why

Unknown. What is established:

- Zero results **and** zero unparsed lines means stdout held nothing but
  whitespace, because the parser keeps every line it cannot read.
- The exec reported success. `MustSh`, which this used at the time, returns
  stdout only when the stream returned no error, so the shell was not reported
  as having failed and was not reported as having been cut off.
- `verify-records.sh` prints exactly one line per index on every path through
  its loop, including a directory that does not exist. That is now held down by
  `TestVerifyRecordsScriptAnswersForEveryIndex`, which runs the real script
  under a real shell. So "the records were missing" does not produce this; it
  produces fourteen `absent` lines.
- The only way the script itself prints nothing is being handed no indices at
  all, and the caller returns early on an empty set and counted fourteen.
- The sweep code, the script and the parser are untouched by anything that
  landed between the 2026-09-11 full-suite run, where the same two cases swept
  15 and 5 records successfully with identical `0s` recovery and `1s` gap
  readings, and this one.
- It is not a slow or hung mount. A `hard` mount with no server blocks, and
  both cases returned in well under two minutes.

What is left is the exec returning success with an empty stdout for a command
that did run, or for one that never started. Neither has been demonstrated.

The reason this was not diagnosed on the spot is the message. It named a count
and nothing else: no exit status, no stderr, no indication of whether anything
arrived at all. The artifact bundle was no better, because the verdict table is
only written on the success path, so the run that most needed evidence filed
none.

### What changed

- `VerifyRecords` runs the sweep through `Sh` rather than `MustSh`, so the
  short-answer error carries both streams with their lengths and states that the
  exec itself reported no error. The message says that a short answer is the
  sweep failing to run or its output being lost rather than a verdict about the
  data.
- The raw output goes into the bundle as `record-sweep-raw.txt` whenever the
  sweep fails, which is exactly when the verdict table is empty and useless.
- `TestVerifyRecordsScriptAnswersForEveryIndex` holds the script to one line per
  index across a missing directory, an empty one and a mixed set. Without it the
  new message is an assertion nobody checked, and a genuine lost record could be
  filed as a harness problem.
- `TestShortSweepErrorNamesWhatItSaw` holds the message to naming the pod, the
  directory, both counts and both stream lengths.

None of that has yet been exercised by a real short sweep, because the failure
has not recurred. What the next occurrence produces is the evidence this entry
is waiting on: `record-sweep-raw.txt` in the bundle, and a message saying how
many bytes arrived on each stream.

### What it means for the system under test

Nothing, so far, and that is the point of writing it down. No verdict was
reached about the records, so neither case says anything about durability on
this deployment. A reader of that run's log should not take the failure as data
loss, and should not take the pass of the other chaos case as covering it.

The general rule: **a sweep that answered for nothing is not a sweep that found
nothing.** Any check that reads a verdict out of a pod has to be able to tell
those apart, and has to file what the pod actually said.

---

## F-010: A lock case accused the server of losing a lock that was plainly still held

**Found:** 2026-09-12, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, StorageClass `nfs` backed by
`cluster.local/nfs-provisioner-nfs-server-provisioner`, default profile (60s
lease, 90s grace) from flags. CHAOS-06, in the first full-suite run.

**Severity:** high. This is a false accusation against the system under test,
produced by the case whose entire job is to tell reclaim from loss.

### What happened

CHAOS-06 failed with:

> after the failover the client on `gke-w1-default-pool-97537137-x2ev` has no
> held POSIX lock over write [8192..12288) of file `00:a1:524306`

Its own artifact bundle disproves it. `proc-locks-after-failover.txt` holds:

```
# node gke-w1-default-pool-45a6e8ae-dosr
POSIX ADVISORY WRITE pid=98140  00:a1:524306   [0..4095]      held
# node gke-w1-default-pool-97537137-x2ev
POSIX ADVISORY WRITE pid=61722  00:166:524306  [8192..12287]  held
```

Both clients had reclaimed. Both locks are there.

### Why

Same inode, different device: `00:a1` against `00:166`. The inode is the
server's and is the same on every client. The device is not: the Linux NFS
client allocates an anonymous `st_dev` per mount, so the same file has a
different device minor on each node.

The case read one file identity, from a pod on node A, and matched it against
both nodes' lock tables. `ProcLock.Covers` rejects on a device mismatch, which is
correct — inode numbers are unique per filesystem, not per node, and dropping the
device would let an unrelated file stand in as proof. So the cross-node half
could only ever pass by coincidence, when the two minors happened to be equal.

F-007 recorded CHAOS-06 as green. That was the coincidence, not a regression:
this was latent from the moment the assertion was written, and it is the one
claim in F-007 that this entry supersedes.

The comment above the offending line stated the false premise outright — "one
identity, read once from either client: both mount the same file, and the device
and inode are what the node's lock table prints" — while `FileIdentity`'s own doc
comment says it returns a file's device and inode *as a pod sees them*.

### What changed

- CHAOS-06 now reads one identity per node, from a pod on that node.
- `assertClientHoldsRange`'s doc comment states that its `id` must come from a
  pod on the node being interrogated, and why.
- `TestProcLockCoversAcrossNodes` locks the rule in using the two real tables
  above: each node's identity matches its own lock, node A's identity does not
  match node B's, and the inode alone still matches so an unreadable `stat` does
  not become a reported protocol failure.

Verified on the cluster, 2026-09-12, by running the case with and without the
change fifteen minutes apart against `gke-w1` on the same flags. Without it, run
`pr37-chaos-20260912` failed `byte-ranges-after-failover` having matched node
`97537137-x2ev`'s lock table against `00:a1:524306`, which is the other node's
device. With it, run `pr36v2-chaos-20260912` passed, reading `00:a1:524305` for
one node and `00:166:524305` for the other. Different inodes between the two
runs because each run provisions its own file; the device split is the point.

### What it means for the system under test

Nothing, and that is the point. Server-side reclaim worked: both byte-range
locks came back to their original holders, the server refused a third client on
both ranges, and the whole-file half of the case reported all four locks
reclaimed. The only defect was in the question the harness asked.

The general rule, for anything that reads `/proc/locks` later: **a file's
identity on an NFS mount is per-node.** Carrying one across nodes turns a held
lock into a reported loss, with no error and no log line to say so.

---

## F-009: The export has no per-volume quota, so capacity monitoring describes the backing filesystem and not the claim

**Found:** 2026-09-11, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, StorageClass `nfs` backed by
`cluster.local/nfs-provisioner-nfs-server-provisioner`, default profile (60s
lease, 90s grace) from flags. First run of OBS-06.

**Severity:** none for the data path, high for anyone who sets a capacity
threshold on this deployment. Nothing is broken; a number an operator would
reasonably monitor means something other than what its name suggests.

### What happened

OBS-06 got as far as its last assertion and failed there. Everything before it
passed, and that is the part worth reading:

- The kubelet publishes per-volume usage for this claim through the node proxy,
  so the CSI driver does implement volume statistics.
- The two sources agreed exactly, before and after the write: 0 bytes used
  each, then 134,217,728 bytes each.
- Both moved by the full 128 MiB the workload wrote, the kubelet's reading
  arriving 1m33s later, which is its documented aggregation period.

The last check is what failed. The claim is provisioned at 1 GiB and `df`
inside the pod reports a total of 10,464,788,480 bytes, which is the server's
own backing volume.

### Why

This provisioner hands out an export per claim as a subdirectory of one
filesystem, and its XFS quota option is off by default. There is no per-volume
limit, so `statfs` on the mount answers with the filesystem behind it. Both
sources read that same filesystem, which is why they agree so precisely.

### What it means for the system under test

Not a defect in the NFS server, and not a failing harness: it is a limitation of
this deployment, which is the distinction the case exists to draw. An operator
who sets an alert at 80% of the reported capacity is watching the provisioner's
10 GiB volume rather than the 1 GiB their workload was promised. A claim can
reach its own nominal size with the monitored number barely moving, and the
first sign of trouble is the application getting ENOSPC with every dashboard
reading healthy. Turning the quota option on is what changes the answer.

The same run leaves the other half of Section 3.5 open: OBS-02 failed for an
unrelated reason on this cluster and OBS-03 failed as F-008 predicts, neither of
which OBS-06 touches.

### What changed

Two things in the harness, both exposed by this run rather than by the
provisioner:

- **The agreement tolerance is now a fraction of the smaller of the claim's
  provisioned size and the capacity the workload is shown.** It had been a
  fraction of the workload's view alone, which on this deployment is the backing
  filesystem: the tolerance came out at 199 MiB against a 128 MiB write, so the
  two sources could have disagreed by more than the entire workload and still
  been reported as agreeing. On a multi-terabyte pool it would have swallowed
  anything. The assertion that caught the quota was unaffected, but the one
  above it had quietly stopped being able to fail.
- **`TestWriteBytesScriptReportsWhatLanded` skips where `stat` does not take
  `-c`.** The script runs in a pod on busybox, and the test ran it on the
  workstation, so `make unit` failed on macOS for a reason that says nothing
  about the script. It now probes first, the way the capacity parsers already
  probe for `stat -f`.

The case itself is unchanged, and stays red here. That is the point of it: an
assertion is not relaxed because one provisioner cannot meet it, and this one
would pass unchanged on a deployment whose exports carry a quota. A run against
this cluster reports OBS-06 as a failure naming the provisioner's
configuration, with F-009 as the explanation.

### What the second run settled

Re-run the same day on the same cluster with the corrected tolerance, which is
what it was there to check. The tolerance came out at 20.5 MiB, two percent of
the 1 GiB claim rather than of the 10 GiB volume behind it, and both sources
still tracked the 128 MiB write and cleared the 64 MiB movement floor. So the
agreement assertion above the quota check can now fail, and the quota check is
still the only thing failing here.

OBS-02 passed on this run, having failed on the first with a record sweep that
answered nothing. Nothing in this change reaches that case, so the first result
was environmental; it is noted because two runs of the same target disagreeing
is worth knowing when the next one is read.

---

## F-008: `nfs-server-provisioner` never announces grace, so the grace cases cannot run against it

**Found:** 2026-09-11, GKE cluster `gke-w1`, Kubernetes v1.37, StorageClass `nfs`
backed by `cluster.local/nfs-provisioner-nfs-server-provisioner:v4.0.8`. First
run of CHAOS-07 on this deployment.

**Severity:** none for the cluster, high for what can be concluded from a run on
it. A whole class of assertion is unreachable here and the results do not look
empty, they look green.

### What happened

CHAOS-07 ran for 366 seconds and reported blocked: the server emits no grace
entry or exit line that the log stream can classify, so there is no window to
place a lock grant inside or outside of.

The case is right to report blocked rather than fail. What it cannot do is
assert the thing it exists for.

### Why

Grace is the dominant term in every failover measurement in this plan, and the
only way a client-side harness can see it is a signal the server chooses to
emit. This provisioner emits none that the built-in wording rule matches, and
`-grace-enter-pattern` cannot help unless someone first establishes that a line
exists to match.

### What it means for a run on this deployment

Three assertions are out of reach, and two of them are silent about it:

- **CHAOS-07** reports blocked, visibly. Nothing was learned about whether the
  server bars new lock acquisition during grace.
- **OBS-03** is the case that *fails* for this rather than skipping, and it
  should be run here to put the finding on the record where an operator sees it.
- **Every grace-window narrowing elsewhere** falls back to whatever the case
  does without a window. CHAOS-06's reclaim assertion does not need one, which
  is why it passed.

The SLO profile on this run was the default one (60s lease, 90s grace) taken
from flags, not from discovery. That is the documented fallback and it is sound,
but it is worth stating that on this deployment the grace value is *declared*
rather than observed, so a failover measured against it is measured against a
number nobody confirmed.

### What changed

No code. This is a property of the deployment, and both cases already report it
correctly. It is recorded so that a green CHAOS run against this provisioner is
not read as evidence that grace behaves.

---

## F-007: Two cases in the data path phase reported results they had not measured

**Found:** 2026-09-11, GKE cluster `gke-w1` with three workers, first real run of
the step 6 cases from [PR #14](https://github.com/mikebz/nfs-verification/pull/14).

**Severity:** high for the suite, none for the cluster. Both cases were green,
and neither was wrong about anything it claimed. They were wrong about how much
they had claimed.

### What happened

Every case in the phase that could run did: DATA-05 through DATA-09 and DATA-12
and DATA-13 passed, DATA-11 skipped, DATA-14 skipped for want of an image, and
CHAOS-06 passed with both its byte-range subtests. Mount tables on all three
nodes were at baseline afterwards and nothing leaked.

Two results did not say what they appeared to say.

**DATA-12 verified three records and DATA-13 verified four.** Both passed.

**DATA-11 reported SKIP for thirteen seconds of work.** The sparse write, the
zero-filled hole, the byte past it and the logical size had all been asserted
and had all passed before the hole-punch probe stopped the case.

### Why

The durability pair inherited its pre-fault warm-up from the recovery cases,
which wait for three committed writes and then inject. That is the right warm-up
for a case measuring an *interruption*: what matters there is that the workload
was demonstrably running when the fault landed, and the assertion is about the
gap, not about the records.

The durability pair asserts over the records themselves, so the length of the
set is the resolution of the measurement. At one record per second, three
records is a three second window. DATA-13 is the worse of the two: its entire
job is to record how many un-fsynced records came back absent or short, and over
four records on a server whose host page cache survived the SIGKILL it was
always going to report four correct and document nothing. A pass there looks
identical to a pass over a set large enough to mean something.

DATA-11 was one case with two halves gated by different things, and Go reports a
case, not a half. The punch probe correctly found that the Alpine image's
busybox `fallocate` parses `-l` and `-o` only, and correctly reported blocked
rather than filing a tool gap as a protocol gap. But `t.Skipf` marks the whole
case skipped whatever ran before it, so the sparse result went in the bin with
it.

### What changed

The warm-up moved into `awaitRecords`, and the durability pair asks for thirty
records rather than three. Thirty is a minute of load before the fault; nothing
asserts on the number, and it is cheap against a case that already waits out a
server restart. `requireDurabilitySet` fails either case whose set came back
under ten, because a pass over a handful of records is worse than a failure: it
looks the same as a real result.

DATA-11 is now two subtests, `sparse-write-and-read-back` and `hole-punch`, so
the half that can run on a busybox image reports its own result and only the
half that cannot reports blocked.

### What it says about the harness, not the system under test

Nothing about NFS. It says that "the case passed" and "the case measured
something" are different claims, and that the suite had no way to tell them
apart in either of these shapes.

The same pattern had already been caught twice in review on this branch, in
DATA-05 and CHAOS-06, where a mount option blocking one half would have blocked
a case that could still run the other. DATA-11 was the third instance and was
missed because its two halves are gated by an image rather than by a mount
option. A case with halves that can be blocked separately reports them
separately; that is now true of every such case in the phase.

The set-size problem is the more general one, and it has no structural fix here.
A case that asserts over a set it produced has to state how large that set must
be to carry its conclusion, and fail below it. Only the durability pair does
that today.

### What the run settled

Four things the design had left open, recorded here because they are facts about
a real image and a real cluster rather than decisions:

- **busybox `dd` on `alpine:3.20` does carry `oflag=direct`.** DATA-07 passed.
  Section 5.4 of the design holds, and `locktool` does not need a `dio`
  subcommand.
- **busybox `fallocate` does not carry `-p`**, as predicted, so the hole punch
  is unreachable on the default image whatever the mount version is.
- **A lock held by a force-deleted pod came back in 2.27 seconds**, which
  DATA-06 classified as the descriptor-close path rather than lease expiry. On
  this cluster kubelet is prompt, so an application losing a lock to a dying pod
  is back in about a second; losing a *node* is the case that costs a lease, and
  that is CHAOS-03 and SEC-07.
- **The clone volume did not disturb the dynamic claim that owns the export.**
  DATA-08 passed and open question 6 of the design is answered for this driver.

Still unmeasured: DATA-10, which did not run, so whether 100k entries fit on a
directory-backed export is still open; and DATA-14, which needed `-fio-image`.

*Amended 2026-09-11:* DATA-14 was deferred rather than run. It is not a gap
waiting on an image any more, it is a case this suite is not doing yet, and
Section 3.2 of the test plan says why.

---

## F-006: `scripts/lock-probe.sh` passed `flock -w`, which busybox does not have

**Found:** 2026-09-11, by reading the applet sources while writing
[`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md).
Not found by a run: on a workstation and on the default `alpine:3.20` tools
image the probe works, because both carry the util-linux `flock`.

**Severity:** high for the suite, none for the cluster. It makes a merged case
pass while asserting nothing.

### What happened

`lock-probe.sh` ran `flock -w "$wait" -x 9` for each attempt.
`hold-flock.sh`'s own comment two files away already said busybox `flock` has
no timeout flag, and the probe contradicted it.

### Why it matters

busybox `util-linux/flock.c` parses `-s`, `-x`, `-u` and `-n` and nothing else.
On an image carrying the applet, every attempt exits on a usage error before a
lock is ever requested, so every record in the probe log is `ERR`.

CHAOS-07 asserts **on grants**: no grant inside the grace window, and at least
one after it. An all-`ERR` log satisfies the first half for free. The second
half would have failed, which is the only reason this was ever going to be
noticed, and it would have been read as "the server never resumed granting new
state" rather than as a broken probe.

### What changed

The attempt is now a retry loop around `flock -n`, bounded by the same
`wait-seconds` argument. `-n` is carried by busybox and by util-linux alike, so
one loop serves both images, and the `timeout` bound around the attempt is
unchanged.

`TestScriptsUseOnlyPortableFlockOptions` now reads every embedded script and
rejects any `flock` option outside busybox's four. The two existing probe tests
could not catch this: they run against the workstation's `flock`, which accepts
`-w`, so a portability defect has to be asserted against the option set rather
than against behaviour.

### What it says about the harness, not the system under test

Nothing about NFS. It says that "assume nothing beyond busybox" is a rule the
repository states and had no check for. Every case that reports on grants can
pass vacuously if its instrument never gets as far as asking, and an instrument
that fails the same way on every attempt looks exactly like a quiet system.
## F-005: Exponential mount propagation in GKE's `mount.nfs` wrapper wedges worker nodes

**Found:** 2026-09-11, GKE cluster with e2-medium worker nodes, running PROV and DATA test suites.

**Severity:** critical for the cluster, high for the suite. It exhausts kernel mount structures, driving worker nodes into uninterruptible D-state and requiring a hard compute instance reset.

### What happened

During test runs with dynamic provisioning and teardown, worker nodes gradually slowed down and eventually stopped responding to `kubectl exec`, `runc` container management, and kubelet liveness probes. Containerd reported context deadlines on container startup and exit.

Inspection of `/proc/mounts` on an affected node revealed that the mount table had exploded from ~80 lines to **16,510 lines** (1.85 MB), containing **8,192 stacked bind mounts of `/etc`** and **8,191 stacked bind mounts of `/run/systemd/resolve`**.

### Why

GKE wraps `/sbin/mount.nfs` with `/home/kubernetes/bin/mount.nfs` so that in-cluster Kubernetes Service DNS names (`*.cluster.local`) resolve from the host. The wrapper isolates DNS configuration inside a private mount namespace:

```sh
exec unshare --mount --propagation shared -- bash -c '
  ...
  mount --bind /etc /etc
  mount --make-private /etc
  if [[ -d "$RESOLV_DIR" ]]; then
    mount --bind "$RESOLV_DIR" "$RESOLV_DIR"
    mount --make-private "$RESOLV_DIR"
  fi
  mount --bind "$TMP_RESOLV" "$RESOLV_TARGET"
  exec /home/kubernetes/bin/mount.nfs.real "$@"
' ...
```

The bug is the combination of `--propagation shared` with `mount --bind` *before* `mount --make-private`:

1. `unshare --mount --propagation shared` places the new namespace root and `/etc` into the **same shared peer group** as the host namespace.
2. Inside that shared namespace, executing `mount --bind /etc /etc` causes Linux VFS mount propagation to immediately clone the new bind mount across all peers in the group, including the host namespace.
3. The subsequent `mount --make-private /etc` only makes the child namespace's mount private; the cloned mount in the host namespace remains shared.
4. Each subsequent NFS mount starts with $2^N$ mounts on the host, duplicating all existing peer mounts on every execution: $1 \to 2 \to 4 \to 8 \to 16 \dots \to 8192$.
5. By mount 13, the kernel mount table holds over 16,384 mounts. Every subsequent operation iterating mounts (container creation, exec, `cat /proc/mounts`, kubelet housekeeping) acquires `namespace_sem` and spends excessive CPU traversing stacked mounts. Worker threads enter uninterruptible D-state, container runtimes hang, and the node becomes unresponsive.

### The fix applied to the nodes

In `/home/kubernetes/bin/mount.nfs`:

1. **Make directories slave before bind-mounting:**
A slave mount receives propagation from its master (the host) but never propagates changes back. Making `/etc` and `/run` `rslave` before bind-mounting ensures bind mounts remain confined to the private namespace:

```sh
mount --make-rslave /etc 2>/dev/null || true
mount --bind /etc /etc || exit 1
mount --make-private /etc || exit 1

mount --make-rslave /run 2>/dev/null || true
if [[ -d "$RESOLV_DIR" ]]; then
  mount --bind "$RESOLV_DIR" "$RESOLV_DIR" || exit 1
  mount --make-private "$RESOLV_DIR" || exit 1
fi
```
*(Note: Making `/` slave is incorrect, as that would prevent the NFS mount itself under `/var/lib/kubelet` from propagating back to the host).*

2. **Bypass namespace creation for IP-based targets:**
When the target is a raw IP address (e.g. `10.x.x.x:/export`), cluster DNS resolution is not involved. Skipping `unshare` entirely avoids namespace overhead and eliminates propagation hazards:

```sh
if [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+: ]] || [[ "$1" =~ ^\[[0-9a-fA-F:]+\]: ]]; then
  exec /home/kubernetes/bin/mount.nfs.real "$@"
fi
```

### What it means for the system under test

- Like F-003, this is an infrastructure defect in GKE's host mount wrapper, not an NFS protocol bug or harness failure. Any dynamic RWX workload on GKE that provisions and mounts volumes repeatedly will eventually wedge its worker nodes.
- With the fix applied across worker nodes, mount table size remained stable at baseline (~80-84 lines) across hundreds of mounts throughout the full PROV, DATA, SEC, OBS, and CHAOS test suites.

---

## F-004: `allowVolumeExpansion` is a claim, not a capability

**Found:** 2026-09-10, GKE cluster with e2-medium worker nodes, running PROV-04 while reviewing
[PR #3](https://github.com/mikebz/nfs-verification/pull/3).

**Severity:** low for the cluster, medium for the suite. It makes one case fail
for a reason that is not obvious from its failure.

### What happened

The StorageClass under test sets `allowVolumeExpansion: true`, so preflight
recorded `canExpand` and PROV-04 took the expansion path. The resize request was
accepted by the API and then nothing happened. The case failed with:

```
claim capacity did not reach 2Gi: timed out after 5m0s
```

### Why

`allowVolumeExpansion` on a StorageClass is what the class asserts, not what its
provisioner can do. The in-cluster `nfs-server-provisioner` behind that class
performs no expansion, and no external-resizer sidecar was running to act on the
request, so the claim sat with the larger request and the smaller status
forever. No resize condition was ever posted, because nothing was resizing.

### What changed

`WaitPVCCapacity`'s timeout message now distinguishes the three answers. A
resize condition present means expansion is in progress or is waiting on
something (a pod restart, for instance). **No condition at all** means nothing
acted on the request, and the message now says so and names the two things to
check: whether the provisioner supports expansion, and whether an
external-resizer sidecar is running.

### What it says about capability discovery

`Caps.CanExpand` is optimistic by construction, because the only thing the
Kubernetes API offers is the class's own assertion. Every other capability in
the suite is probed; this one is declared, and it is the one that lied. The case
still fails rather than skipping, which is right: a class advertising a
capability its provisioner lacks is a real deployment defect, and a claim
accepted for a resize that never happens is a trap for any workload, not only
this suite. What the case owed was a failure message that says where to look.

---

## F-003: A broken `umount.nfs` wrapper on GKE wedges every terminating pod

**Found:** 2026-09-10, GKE worker nodes, reviewing
[PR #3](https://github.com/mikebz/nfs-verification/pull/3) against a live
cluster.

**Severity:** high, and it is not the harness. Every pod holding an NFS mount
stays `Terminating` forever, on any workload, whether or not this suite is
running.

### What happened

Pods with the share mounted would not go away. Kubelet's volume teardown failed
with:

```
mount.nfs.real: no mount point provided (exit status 32)
```

### Why

GKE wraps `/sbin/mount.nfs` with `/home/kubernetes/bin/mount.nfs`, a script that
arranges a private mount namespace so an in-cluster Service DNS name resolves.
`/sbin/umount.nfs` is a symlink to the same file, because the real binary is
multi-call: it decides whether it is mounting or unmounting from `argv[0]`.

The wrapper ended in:

```sh
exec /home/kubernetes/bin/mount.nfs.real "$@"
```

which sets `argv[0]` to `mount.nfs.real`. The multi-call binary therefore chose
mount mode no matter how it was invoked, so `umount.nfs <mountpoint>` was read
as a mount with no mount point, and failed. Nothing on the unmount path could
ever succeed.

### The fix applied to the nodes

Handle the unmount case before the wrapper's mount logic, preserving `argv[0]`
with `exec -a`:

```sh
if [[ "$(basename "$0")" == *"umount"* ]]; then
  out=$(exec -a "$0" /home/kubernetes/bin/mount.nfs.real "$@" 2>&1)
  status=$?
  if [[ $status -eq 0 || $status -eq 16 ]] || [[ "$out" == *"not mounted"* ]]; then
    exit 0
  fi
  echo "$out" >&2
  exit $status
fi
```

Exit status 16 and "not mounted" are treated as success on purpose: an unmount
of something already gone is the outcome the caller wanted. With this in place
kubelet unmounts finished in under two seconds.

### What it means for the suite

Nothing in this repository changed. The value of the finding is that the
symptom is indistinguishable, from inside a case, from the failures this plan
is actually hunting:

- Pods stuck `Terminating` are what teardown treats as "the node has stopped
  answering" (F-001), so a run against an affected cluster reports leaked
  claims and stuck pods in every case, for a reason that has nothing to do with
  NFS.
- It arrives as a storage-shaped failure and routes to the storage owner, when
  the defect is in a node image's wrapper script.
- **Node reset / reboot recurrence**: Because the wrapper lives in the node root
  filesystem, resetting, rebooting, or replacing a node reinstalls the stock
  image wrapper without the fix. Nodes restarted or added during chaos or
  maintenance must have the wrapper updated.

Anyone seeing `Terminating` pods across the board should check
`/sbin/umount.nfs` on the node before suspecting the server or the driver. The
tell is that unmount fails while everything else about the mount works.

### Open

Preflight could catch this in seconds: the node agent already runs in the host
namespaces, so it could unmount a path that is not mounted and check that the
failure is "not mounted" rather than "no mount point provided". That would turn
a day of attribution into a preflight message. Not built yet, and it would be a
recorded warning rather than a gate, since the wrapper is specific to one node
image and the plan admits no distro-specific checks.

---

## F-002: 2GB worker nodes cannot host the suite

**Found:** 2026-09-10, GKE cluster, `e2-small` node pool (2GB RAM).

**Severity:** medium. It does not produce a wrong result, it produces node
reboots that look like storage failures and cost a day to attribute.

### What happened

On `e2-small` workers, the GKE system daemons alone (fluentbit, gmp-collector,
gke-metrics-agent, filestore-node, pdcsi-node) account for roughly 87% of
requested memory and well over 100% of limits. Adding test pods, their image
pulls and the page cache from a 1MiB write pushed nodes into kernel memory
pressure:

```
virtio_balloon: Out of puff! Can't get 1 pages
systemd-journald: Under memory pressure
```

Kubelet heartbeats then dropped, MIG health checks fired, and nodes rebooted
mid-run.

### Why it matters to the results, not just the runtime

A rebooting node is indistinguishable, from inside a case, from the failure
modes the plan is actually hunting: I/O that stalls, a mount that does not come
back, a lock that is not reclaimed. A suite that cannot tell an undersized node
from a storage defect produces findings nobody can act on.

This is the same class of problem as Appendix C item 4 in the test plan, which
already requires node auto-repair and auto-upgrade to be off: if the platform is
restarting nodes underneath the run, chaos results are invalid.

### What changed

- The node agent now sets resource requests (10m CPU, 32Mi) so it is not
  BestEffort and not the first thing evicted. No limits: it must not be OOM
  killed while a case is reading the node it is inspecting.
- Minimum node size is stated in the README: at least 4GB per worker
  (`e2-medium`), and 8GB (`e2-standard-2`) for anything beyond the presubmit
  cases.

### Open

The node pool requirement belongs alongside the other cluster preconditions in
Section 0 of the test plan, as something preflight could check rather than
something a person has to remember. Allocatable memory per node is readable from
the node status, so a preflight warning is cheap. Not done yet.

---

## F-001: Force-deleting a mounted pod can take a node out of service

**Found:** 2026-09-10, GKE cluster (GKE v1.37.0, Container-Optimized OS,
`e2-small` node pool), reviewing the first three cases on
[PR #1](https://github.com/mikebz/nfs-verification/pull/1).

**Severity:** high. The failure is not a failed test, it is a node that stops
accepting work, and the harness caused it.

### What happened

Teardown deleted the case's pods with `GracePeriodSeconds: 0` and then deleted
the claim.

```go
// pkg/framework/framework.go, before the fix
pods.DeleteCollection(ctx, DeleteNow(), ListOptions(f.Selector()))
// ... immediately followed by
PersistentVolumeClaims(Namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, ...)
```

### Why it wedges the node

1. A force delete removes the Pod object from the API at once. It does not wait
   for kubelet on the node to stop the containers or unmount the volume.
2. The pod is gone from the API, so the wait that follows returns immediately
   and the claim is deleted next.
3. The provisioner destroys the export while the node's kernel still holds an
   active mount of it.
4. The mount is NFSv4.1 and `hard`, so the kernel retries the RPCs
   indefinitely rather than returning an error. The retry is uninterruptible.
5. That blocks kubelet's volume manager, which stops further mounts on that
   node, disturbs PLEG, and hangs anything that reads the mount table,
   including `cat /proc/mounts`.

The ordering is the whole bug. Every step after the force delete behaves
exactly as documented; the force delete simply removed the only thing that was
keeping the export alive until the unmount finished.

### What changed

- Teardown deletes pods gracefully (`metav1.DeleteOptions{}`) and waits for them
  to leave the API before touching any claim. Test pods carry
  `terminationGracePeriodSeconds: 5`, so this costs seconds, not minutes.
- `Framework.DeletePod` is the graceful delete a case should use when it is done
  with a pod.
- `Framework.DeletePodNow` still force-deletes, because some cases need to model
  a client that vanished without unlocking (DATA-06, SEC-07). Its doc comment
  now says plainly that it must never be used for teardown.
- Artifact collection bounds each node's inspection separately
  (`nodeInspectTimeout`), so a node in this state cannot starve the bundle for
  the healthy nodes. This is how the state gets diagnosed next time.

### What it says about the system under test, not the harness

The harness triggered this, but the hazard belongs to the architecture, and the
plan already predicts the shape of it: the server is a singleton in the data
path, and a hard mount blocks rather than failing. Deleting an export out from
under a live mount is therefore not a recoverable client error, it is an
indefinite stall on the client node.

Worth carrying into later work:

- PROV-03 asserts the API-level protection: a claim that a pod still mounts
  stays `Terminating` until the mount is gone. That protection is what stands
  between a normal `kubectl delete pvc` and this failure, so it deserves the
  presubmit gate it has.
- A case for the uglier path is worth adding once the chaos vector lands:
  destroy the export while a node holds the mount, and assert what the node
  does. The honest expected result is an indefinite stall, so the assertion is
  about blast radius (does kubelet keep serving other pods?) and about whether
  the state is observable, not about recovery.
- Node recovery after this state is a reboot in practice. Any run that hits it
  should treat the node as spent.

### Follow-up, 2026-09-10

Teardown has a second path through the same hazard: if a pod does not leave the
API within `PodTerminateTimeout`, the node has stopped answering, and deleting
the claim then is exactly the dangerous act. Teardown now deletes only the
claims no surviving pod mounts, keeps the rest, and fails with the pods, their
nodes and the kept claims named. Leaking a claim is recoverable; wedging a node
is not.

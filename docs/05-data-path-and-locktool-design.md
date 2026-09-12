# 05: Closing out the data path: byte-range locks, protocol edge cases, and durability

Author: mikebz@
Created: 2026-09-11
Updated: 2026-09-12
Status: shipped, delivery step 6. Designed in
[PR #12](https://github.com/mikebz/nfs-verification/pull/12), implemented from
[PR #14](https://github.com/mikebz/nfs-verification/pull/14) onward. DATA-14
deferred.
Serves: DATA-06 to DATA-13, plus the byte-range half of DATA-05 and the
disjoint-range half of CHAOS-06. Requirements in
[`01-test-plan.md`](01-test-plan.md) Section 3.2.
Builds on [`03-chaos-operations-design.md`](03-chaos-operations-design.md) and
[`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md),
whose rules all still hold.

---

## 1. Why this phase exists

Steps 1 to 5 built the harness, all of PROV, and the fault injection and grace
observation that make a failover measurable. What they could not do is express a
byte range, mount one export twice with different caching, or say whether a
specific byte survived a crash. Three gaps:

- **No byte-range locks.** NFSv4.1 carries locking in the protocol itself (RFC
  8881 Section 9), and the Linux client sends a range lock for every lock an
  application takes, `flock` included. The suite could not ask for one, because
  the busybox `flock` applet calls `flock(2)`, which has no range argument. So
  DATA-05 landed half-built, CHAOS-06 landed on whole-file locks, and DATA-06 had
  nothing to acquire.
- **No way to contrast caching.** DATA-04 asserts a reader may see nothing before
  a writer closes; DATA-08 is the other side of it, where attribute caching is
  off and the reader must see it. Mount options belong to the volume, not to the
  pod, so one claim cannot serve both.
- **No verdict finer than present or absent.** The old check answered "is the
  file non-empty", which is not enough for DATA-13, where a truncated record is
  acceptable and a wrong byte is corruption.

## 2. What shipped

- **`locktool`**: about 200 lines of Go over `syscall.FcntlFlock`, built by
  `make locktool` per node architecture, streamed into the ordinary client pod
  over `pods/exec` and verified by checksum. No image, no registry.
- **A `/proc/locks` reader** on the node agent, for the client's own view of what
  it believes it holds, with ranges.
- **A per-lock-kind mount-option gate**: `nolock` and `local_lock=all|flock|posix`
  block the halves they make local.
- **A clone volume**: a static PV pointing at an export a dynamic claim already
  owns, with different mount options, so one export is mounted twice.
- **A force-delete helper** that waits for the node to release the mount, and a
  teardown guard that keeps a claim whose unmount was never proven.
- **A content-verifying record sweep** returning four verdicts, which replaced
  the existence check everywhere, upgrading CHAOS-01, CHAOS-02 and PROV-11 by the
  same change.
- **DATA-06 to DATA-13**, the byte-range half of DATA-05, and the disjoint-range
  half of CHAOS-06.
- Directory population, deletion and census helpers, and probes for `O_DIRECT`,
  hole punching and free inodes.

DATA-14 was built as designed and then removed. Section 7 says why.

## 3. What these cases assert

Shared conventions, including probing a tool before using it and reporting
blocked when it is absent, are in [`01-test-plan.md`](01-test-plan.md) Section
4.1. What is specific to the data path:

| # | Assertion | Where it comes from, and how it is checked |
|---|---|---|
| 1 | A pod dying and a node dying release locks differently, and a case says which it injected | One lease per client per server, shared by every mount and pod on that node ([kernel client-identifier](https://docs.kernel.org/filesystems/nfs/client-identifier.html)). A pod's death closes descriptors in about a second; a node's death drops every pod's locks together when the lease expires. DATA-06 injects the first and bounds it by one lease; node loss is CHAOS-03 and SEC-07 |
| 2 | Locks are asserted only on a mount that sends them to the server, per lock type | `nolock` and `local_lock=all\|flock\|posix` keep locks on the node, so cross-node exclusion does not exist to be tested and a failure would name the server for a mount option's fault. The lock cases read `/proc/mounts` first and report blocked naming the option |
| 3 | A lock the harness cannot express is never silently replaced by a weaker one | A whole-file lock stands only where the case says whole file |
| 4 | A mount option a case depends on is read back from the node before anything is asserted on it | Otherwise a driver that dropped `noac` turns DATA-08 into a slower DATA-04 that passes when the timing is kind |
| 5 | A force delete does not return until the node released the mount, and teardown refuses a claim whose unmount was never proven | This is [F-001](findings.md)'s exact ordering. A force-deleted pod is not in the API, so the usual "is a pod still using this claim" check sees nothing; the fixture marks the claim unproven and only an observed unmount clears it |
| 6 | Data never committed may be absent, and that is never a failure | RFC 8881 Section 18.3 promises durability after `COMMIT` and nothing before it |
| 7 | A short record is acceptable exactly where no fsync was issued; a wrong byte never is | Without an fsync the client is free to have flushed a prefix, and a prefix is lawful. A wrong byte inside it is corruption under any reading |
| 8 | An operation NFSv4.1 does not define is recorded as unsupported, not failed | Hole punching is NFSv4.2 (RFC 7862), and preflight pins `vers=4.1` |
| 9 | A listing racing deletes may miss an entry; it may never return a name that was never created | The defect this comes from is a use-after-free in directory chunk reuse during READDIR, which surfaces as an invented name, not as a count being off |
| 10 | Every file a case verifies is verified by content | Existence is not a check. [F-007](findings.md) is what happens when it is treated as one |

## 4. What the tools produce

The shapes are in the code: `cmd/locktool/main.go` for the three subcommands and
their output, `pkg/framework/locktool.go` for how they are driven,
`pkg/framework/sweep.go` for the record verdicts, `pkg/framework/dircensus.go`
for the directory census, and `pkg/slo/slo.go` for every bound. What matters
about them:

- **`locktool hold` never blocks in `F_SETLKW`.** A blocking acquire cannot
  notice its run-file being removed, and on a hard mount the process may be
  unkillable in `D` state, which leaves the pod `Terminating` and turns teardown
  into [F-001](findings.md)'s path. It polls instead.
- **Nothing prints a holder's pid.** The protocol does not carry one: a denied
  `LOCK` or `LOCKT` gives the conflicting offset, length and type plus an opaque
  lock owner, and nothing that identifies a process on another node. Which pod
  holds which range is something the harness knows because it put it there.
- **`getlk` exists as well as `try`**, because `F_SETLK` answers "who holds
  this" by acquiring, which changes what every later attempt observes. CHAOS-06
  asserts "still held by the original holder", and a probe that acquires cannot
  say that.
- **The sweep returns four verdicts, not a boolean.** `correct`, `absent`,
  `short` and `wrong` are exhaustive over observed length, and a record longer
  than it should be is `wrong` whatever its prefix says, because that is a byte
  nobody wrote. DATA-12 fails on anything but `correct`; DATA-13 fails only on
  `wrong`. That asymmetry is the whole content of the pair.
- **The sweep runs in one exec for the whole set.** A round trip per record
  would take longer than the outage being measured, and would run while the
  mount is still recovering.
- **The census asserts on names, and records counts.** A listing racing deletes
  is supposed to have a count that is off.

## 5. What a byte-range lock is

This phase rests on this, and it is not obvious from the outside.

**The protocol locks ranges natively.** RFC 8881 Section 9 carries `LOCK`,
`LOCKT` and `LOCKU`, each taking an offset and a length. NFSv3 needed a side
protocol for this, the Network Lock Manager, with its own state and its own
failure modes; folding locking into NFS is one of the things v4 is for. A
byte-range lock is the ordinary case and whole-file is the degenerate one.

**The Linux client sends one for every lock.** `fs/nfs/file.c` says it plainly:
`flock` is simulated using POSIX locks on the server. So `flock -x` from a pod is
already a `LOCK` over the whole range on the wire, which is why step 4 could land
CHAOS-06 on whole-file locks honestly.

**Four ways to ask, one wire operation:**

| Application call | `/proc/locks` type | Range | Released when |
|---|---|---|---|
| `flock(2)` | `FLOCK` | whole file | the last descriptor of that open file description closes |
| `fcntl(F_SETLK/F_SETLKW)` | `POSIX` | yes | **any** descriptor to that file is closed by the process, or it exits |
| `fcntl(F_OFD_SETLK)` | `OFDLCK` | yes | the descriptor that took it closes |
| `fcntl(F_GETLK)` | query | yes | n/a |

Three consequences the cases are built on:

- **Advisory, always.** A process that never asks writes anyway. No case here
  asserts otherwise.
- **The POSIX close rule is a foot-gun.** Opening the same file twice and closing
  either descriptor drops *all* that process's POSIX locks on the file, which is
  why `locktool hold` opens once and keeps that descriptor.
- **A lock that cannot be reclaimed becomes an I/O error, not a silent share.**
  When reclaim fails after a restart, the client marks the lock lost and later
  I/O through it returns `EIO` (`fs/nfs/nfs4state.c`). The application finds out.
  Two clients both believing they hold one range would be a far worse defect, and
  CHAOS-06 is what would see it.

**And the mount option that makes all of it disappear.** `nolock` and
`local_lock=all|flock|posix` keep locks on the node and never send them to the
server (`fs/nfs/fs_context.c`). There, two pods on two nodes both get the lock,
every time, correctly, by configuration. A lock case that ran there would fail
and the failure would point at the server. Hence rule 2, which costs one
`/proc/mounts` read and saves a week of misattribution.

## 6. Decisions

**A binary, because nothing off the shelf does this.** `flock(1)` is whole-file
(and busybox's has no `-w`); `lslocks` is read-only and not in busybox; there is
no `fcntl(1)`; `python3 -c` with `fcntl.lockf` needs Python in the tools image,
against the rule that assumes nothing beyond busybox; Connectathon `tlock`,
`fstests` and NFStest need a compile or an install heavier than the 200 lines
they replace; `fio --file_lock` is whole-file and coordinates fio's own jobs.

**Delivered over `pods/exec`, not as an image and not by `kubectl cp`.** A binary
this repository owns, gated on a registry an operator has to populate, means
DATA-06 never runs anywhere, and a case skipped everywhere does not exist. `kubectl cp`
was rejected on four compounding points: it creates a `kubectl` dependency the
harness does not have; it resolves the kubeconfig and context a second time, so a
divergence lands the binary in one cluster while the case runs in another; it
requires `tar` in the container, which `cat > file` does not; and it is more code
with a worse error than stdin on the existing exec helper. Three guards: a
sha256 comparison after the copy, blocked on a node architecture with no built
binary, and `bin/` git-ignored. It lands on the container filesystem, never on
the share, for the same reason the workload log does.

**Three subcommands and nothing that is not a lock.** No `dio`, no `punch`, no
`populate`: each is met by the image's own tools, probed. A binary named for
locks that also does direct I/O is a harness-specific busybox, and the next case
adds a fourth thing to it. The cost is that DATA-07 reports blocked rather than
falling back when an image lacks `oflag=direct`.

**DATA-06 bounds the release by one lease and classifies which mechanism
produced it** (assertion 1). Asserting expiry would fail on a healthy cluster,
and asserting promptness would fail wherever kubelet was slow for reasons that
have nothing to do with NFS. The plan's bound holds under both, and the recorded
classification is what makes the result readable: a second means one
application's locks moved, a lease means every pod on that node lost theirs, and
an operator is looking at two different problems. The re-acquirer sits on another
node so the request crosses the server.

**The force delete waits for the unmount rather than trusting the API**
(assertion 5). This is the "uglier path" [F-001](findings.md)'s follow-up asks
for, approached from the safe side: the same two API calls, with an observation
between them. Leaking a claim is recoverable; wedging a node is not.

**DATA-08 reads the option back before asserting on visibility.** Without that,
a driver that dropped `noac` turns DATA-08 into a slower DATA-04 that passes when
the timing is kind. Extracting server and path from the bound PV is the fragile
part: `spec.nfs` gives them directly, `spec.csi` puts them in `volumeAttributes`
under keys the driver chooses, and neither readable is **blocked**, naming the
fields. The extraction is a pure function over a PV object with unit tests.

**DATA-09 has two halves that assert different things.** Silly rename is a client
behaviour: the Linux client renames an unlinked open file to `.nfsXXXXXXXX` and
removes it when the last descriptor closes, and that can only happen when the
unlinking client is the one holding it open, which by the lease rule above means
the same node. Cross-node, the server removes the file and the holder's next
operation may get `ESTALE`. Both are correct. A case that ran the cross-node
shape and asserted the same-node expectation would file a conformant client as a
defect. What fails is a `.nfs*` leftover after the holder is gone, or a
descriptor that reads the *new* file's content, which would mean a file handle
was reused.

**DATA-10 asserts on names, records counts.** The defect it derives from is a
use-after-free in directory chunk reuse during READDIR, which surfaces as a name
that is not in the directory, not as a count being off: a listing racing deletes
is supposed to have a count that is off. Two practicalities: the listing uses
`find -mindepth 1 -maxdepth 1` rather than `ls`, because busybox `ls` sorts and
sorting 100k entries in a pod with no memory limit finds the node's OOM killer,
and because `find` without `-mindepth 1` emits the directory itself, which a
classifier would flag as a name nobody created. Population is inode-hungry, so
the case checks free inodes and space first and reports blocked rather than
failing on `ENOSPC` halfway and pointing at the server.

**DATA-11 asserts sparse, records the punch, and keeps the two refusals apart.**
Hole punching is unavailable twice over: `ALLOCATE`, `DEALLOCATE` and `READ_PLUS`
arrived with NFSv4.2 (RFC 7862) and preflight pins `vers=4.1`, and the busybox
`fallocate` applet parses `-l` and `-o` only. So an image whose `fallocate` has no
`-p` is **blocked** naming `-tools-image` (rule 8), and only a `-p` that parses
and then returns `EOPNOTSUPP` is recorded as the protocol saying no (rule 9).
Without that split, every run would file "NFSv4.1 does not support hole punching"
on evidence that is really "this image's applet has no `-p`". The case fails only
if a punch reports success without zeroing, which is the assertion that survives
the day preflight accepts 4.2.

**DATA-12 and DATA-13 are one workload with two verdict tables.** They differ in
one flag, `conv=fsync`, and in which verdicts fail. Three things refused: DATA-13
does not assert that data is lost, because a SIGKILL of the server process does
not drop the host page cache underneath it and unstable writes often survive;
DATA-12 does not re-measure recovery time, because CHAOS-01 owns that number and
two cases reporting it is two numbers to reconcile; and a short record is not
called corruption, because without an fsync a flushed prefix is lawful.

**Every case here is a `TestData` case, including the two that kill the server.**
[PR #11](https://github.com/mikebz/nfs-verification/pull/11) sorted the suite
strictly by category, which supersedes the earlier rule that the chaos prefix
marks any case injuring the server. The precedent was already in the tree:
PROV-07, PROV-08, OBS-02 and OBS-03 all inject faults under their own category.
The consequence deserves saying out loud: **`make test-data` injects faults.**

**DATA-05 and CHAOS-06 were extended last**, after `locktool` had run against a
real cluster under DATA-06. DATA-05 gains two pods on two nodes holding disjoint
ranges of one file, each refused on the other's range. CHAOS-06 gains that shape
held across a failover, read from both ends: `F_GETLK` through the server and
`/proc/locks` on each holder's node, because a client that believes it holds a
range the server has forgotten is visible only as the disagreement between them.
Extending a passing chaos case with a tool nobody has run is how a green case
becomes a flaky one.

## 7. DATA-14, deferred

DATA-14 (twenty pods, 70/30 mixed load, 4KiB to 1GiB files, one hour, `fio` with
`verify=crc32c`) was built as designed and then removed, along with the
`-fio-image` flag, `pkg/framework/fio.go`, `scripts/fio-soak.sh` and the
`make test-data-soak` target.

The reason is not that the case is wrong. It is that this suite's soak is
SCALE-07, and twenty pods under sustained mixed load for an hour is a scale
question wearing a data path number; it also needs an image nobody has supplied,
asks for tens of gigabytes, and fits inside no category budget. A target named
for a runtime reads as a category, and soak is not one of this suite's
categories. Test plan Section 3.2 carries the full reasoning and what the
deferral costs: nothing that runs today covers sustained mixed load at scale, and
that gap is SCALE-07's to close.

The design that was reversed, for whoever picks the case up: one job per pod over
a directory it owns alone, because two pods writing one file with verification on
report mismatches that are the harness's fault; `verify_fatal=1`, so a mismatch
stops at the offending offset rather than becoming a count with no location; and
capacity checked before the run rather than discovered during it, because an hour
of soak that dies on `ENOSPC` at minute fifty is an hour spent to learn nothing.

## 8. Configuration

`-tools-image` is the only flag this phase leans on: DATA-07 and DATA-11 probe
what its `dd` and `fallocate` support. `make locktool` is the one new target, and
`make all` calls it. No flag for the locktool path, its architecture, or where it
lands in the pod, because the cluster answers all three.

Three answers are kept apart on bad config: a mount carrying `nolock` is
**blocked** naming the option; an image whose `dd` has no `oflag=direct` is
**blocked**, because the case could run on a different image; a PV whose server
and path cannot be read is **blocked**, naming the missing fields. None is a
pass.

Not configurable: the record size and pattern, the directory entry count, and the
lock ranges. Each is a property of the measurement and each is stated in the
plan.

## 9. What changed after this was written

- **DATA-14 was deferred**, with the flag, the helper, the script and the target
  removed. Section 7.
- **DATA-11's requirement moved into the test plan.** Narrowing the punch from an
  assertion to a record changes what is verified, so Section 3.2's row was
  amended rather than the narrowing living only here.
- **The run happened.** On 2026-09-11, against a three-worker GKE cluster on
  Kubernetes v1.37 with the in-cluster `nfs-server-provisioner`, DATA-05 to
  DATA-09 and DATA-11 to DATA-13 passed along with CHAOS-06; DATA-11's punch half
  reported blocked, which is the documented answer on a busybox image. **DATA-10
  has not been run**, so whether a directory-backed export holds 100k entries is
  still unmeasured.
- **F-006 and F-007** came out of this phase: `scripts/lock-probe.sh` passed
  `flock -w` to an applet that has no `-w`, and two cases reported results they
  had not measured. Both are fixed and recorded in [`findings.md`](findings.md).

Still open: whether `locktool` should grow a `dio` subcommand if real images turn
out not to carry `oflag=direct`, and whether any server reclaims a sub-file range
differently from a whole-file one.

## 10. Sources

- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 9 for file
  locking and share reservations, Section 8.4.2 for reclaim during grace, Section
  18.3 for `COMMIT`.
- [RFC 7862](https://www.rfc-editor.org/rfc/rfc7862.html) for the NFSv4.2
  sparse-file operations DATA-11 cannot reach on a 4.1 mount.
- [`fcntl(2)`](https://man7.org/linux/man-pages/man2/fcntl.2.html),
  [`flock(2)`](https://man7.org/linux/man-pages/man2/flock.2.html) and
  [`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) for the
  application view and for `local_lock`, `nolock` and `noac`.
- The kernel's own
  [client-identifier](https://docs.kernel.org/filesystems/nfs/client-identifier.html),
  plus `fs/nfs/file.c`, `fs/nfs/fs_context.c` and `fs/nfs/nfs4state.c`, for the
  one-lease rule, `flock` simulation, the local-lock options and lost-lock `EIO`.
- The busybox `coreutils/dd.c`, `util-linux/flock.c` and `util-linux/fallocate.c`
  applets for what a stock image can do.
- Kubernetes [PersistentVolume](https://kubernetes.io/docs/concepts/storage/persistent-volumes/)
  and the [CSI spec](https://github.com/container-storage-interface/spec/blob/master/spec.md)
  for the clone PV, `volumeAttributes` and volume handles, and `NodeStatus`
  `volumesInUse` for the unmount observation.

# 04: Closing out the data path: byte-range locks, protocol edge cases, and durability

Author: Claude Code
Created: 2026-09-11
Updated: 2026-09-11

Phase boundary: the cases that assert what the protocol does to a *file* rather
than what a failover does to a *server*. Ends when DATA-06 through DATA-14 run
against a real cluster and report a byte-range lock result, a `noac` visibility
result, a sparse-file result, and a durability verdict per record. Cadence: four
pull requests, following [`plan.md`](plan.md) step 6, on top of step 5. This is
the next phase after [`03-grace-and-lock-reclaim-design.md`](03-grace-and-lock-reclaim-design.md)
and it does not amend it.

---

## 1. Problem and outcomes

Steps 1 through 5 built the harness, all of PROV, and the fault injection and
grace observation that make a failover measurable. What they cannot do is
express a byte range, mount one export twice with different caching, or say
whether a specific byte survived a crash. Section 3.2 of the test plan is nine
cases wide and five of them are unreachable with the tools on a busybox image.

Three gaps, concretely:

- **No byte-range locks.** NFSv4.1 carries them in the protocol itself, and the
  Linux client sends one for every lock an application takes, `flock` included
  (section 5.1). The suite cannot ask for one, because the `flock` applet on a
  busybox image calls `flock(2)`, which has no range argument. So DATA-05
  landed half-built, CHAOS-06 landed on whole-file locks with the sub-file
  dimension deferred here, and DATA-06 has nothing to acquire.
- **No way to contrast caching.** DATA-04 asserts that a reader may see nothing
  before a writer closes. DATA-08 is the other side of that: with attribute
  caching off, the reader must see it. The two must run against one export, and
  mount options belong to the volume, not to the pod, so one claim cannot serve
  both.
- **No verdict finer than present or absent.** `missing-records.sh` answers
  "is the file non-empty". That is enough for a failover case that asserts
  nothing was lost. It is not enough for DATA-13, where a truncated record is an
  acceptable outcome and a wrong byte at a correct offset is corruption.

Observable outcomes that define done:

- A case can take a lock on a byte range, from a pod, on a named node, and say
  whether it was granted, and by whom it is held if it was not.
- A case can state that two clients hold disjoint ranges of one file at the same
  time, and that a third is refused on either.
- A case can state that a lock held by a pod that vanished became available
  again, how long that took, and which mechanism released it.
- A case can state that a reader with attribute caching off saw a writer's bytes
  before the writer closed, having first proved that the reader's mount actually
  carries `noac`.
- A case can state, per record, whether it is present and correct, absent,
  a correct prefix, or wrong, and can fail on the last of those alone.
- A case can run 100k directory entries through readdir while another pod
  deletes them, and say whether anything the listing returned was never created.

Requirements: Section 3.2 of [`01-test-plan.md`](01-test-plan.md) for DATA-06
through DATA-14, Section 3.3 for the CHAOS-06 extension, Section 4.2 for the
per-category targets and their budgets, Section 2.3 for the protocol claim these assertions are calibrated
against. Delivery order and the harness inventory: [`plan.md`](plan.md) step 6
and section 3e.

Cases served by this phase: **DATA-06** (lock held by a force-deleted pod),
**DATA-07** (`O_DIRECT` from two pods), **DATA-08** (`noac` visibility),
**DATA-09** (rename, unlink and re-create under an open descriptor),
**DATA-10** (100k-entry readdir under concurrent deletes), **DATA-11** (sparse
file), **DATA-12** (fsync durability), **DATA-13** (negative durability),
**DATA-14** (mixed soak). Two cases already in the repository are extended:
**DATA-05** gains its byte-range half, **CHAOS-06** gains disjoint ranges.

---

## 2. Human-readable rules

Each line is testable, and a reviewer can accept or reject the phase from this
section without reading code.

1. A pod dying and a node dying are different failures. A case says which one it
   injected and asserts only what that one releases. The first drops one pod's
   locks when its descriptors close; the second drops the locks of every pod on
   that node when the node's lease expires.
2. Locks are asserted only on a mount that sends them to the server, per lock
   type: `nolock` and `local_lock=all` block every lock case, `local_lock=flock`
   blocks the `flock` half and `local_lock=posix` the byte-range half.
   `local_lock=none` is the default and blocks nothing. On a blocked mount the
   lock never reaches the server, so cross-node exclusion does not exist to be
   tested and a failure would name the server for a mount option's fault.
3. A lock the harness cannot express is never silently replaced by a weaker one.
   A whole-file lock stands only where the case says whole file.
4. A mount option a case depends on is read back from `/proc/mounts` on the node
   that mounted it, before anything is asserted on it. An option the driver
   dropped must fail the case, not pass it vacuously.
5. A case that force-deletes a pod does not return until that node has released
   the mount, and teardown refuses to delete a claim whose unmount was never
   proven. Neither is best effort: an unproven unmount keeps the claim and fails
   loudly, because a leaked claim is recoverable and a wedged node is not.
6. Data that was never committed may be absent. That is never a failure.
7. A record that is short is acceptable exactly where no fsync was issued. A
   byte that differs from what was written, at an offset that was written, is a
   failure under every reading of the protocol.
8. A tool the image may not carry is probed before it is used. Its absence
   reports blocked and names the flag that fixes it. It never reads as a pass
   and never as a failure.
9. An operation NFSv4.1 does not define is recorded as unsupported, not failed.
10. A directory listing running against concurrent deletes may miss an entry. It
    may never return a name that was never created.
11. Every file a case verifies is verified by content. Existence is not a check.
12. Everything from phases 2 and 3 still holds. This phase adds to that
    contract, it does not amend it.

---

## 3. Scope

**In scope for this phase**

- `locktool`: a static Go binary in this repository for `fcntl` byte-range
  locks, built by `make` and copied into an existing pod. No image, no registry.
  Section 5.2 says why nothing off the shelf does this.
- A `/proc/locks` reader on the node agent, for the client's own view of what it
  believes it holds, with ranges. No tool required; it is a kernel file.
- A mount-option check on the lock cases, per lock type: `nolock` and
  `local_lock=all|flock|posix` block the halves they make local.
- The byte-range half of DATA-05 and the disjoint-range extension of CHAOS-06,
  both of which [`03-grace-and-lock-reclaim-design.md`](03-grace-and-lock-reclaim-design.md)
  section 5.5 deferred to this phase by name.
- DATA-06 through DATA-13. DATA-14 was in scope when this was written and is
  now **deferred**: see the note at the head of section 5.13.
- A clone volume: a static PV pointing at an export a dynamic claim already
  owns, with different mount options, so one export can be mounted twice.
- Directory population and census helpers for DATA-10.
- A content-verifying record sweep, replacing the existence check for the
  durability cases and upgrading CHAOS-01 and CHAOS-02 by the same change.

**Out of scope (explicit non-goals)**

- Any new container image. Section 5.3.
- Renaming or re-targeting any case outside DATA. [PR #11](https://github.com/mikebz/nfs-verification/pull/11) sorted the whole suite
  by category; this phase adds to that and changes none of it. It does add one
  target, `make test-data-soak`, for DATA-14's hour, in the PR that lands
  DATA-14: section 5.14.
- NFSv4.2. Preflight pins the mount at `vers=4.1` and fails otherwise, so
  `ALLOCATE`, `DEALLOCATE` and `READ_PLUS` are out of reach. DATA-11 is written
  so that the day preflight accepts 4.2, the punch-hole half becomes an
  assertion without the case being rewritten.
- Open-file-description locks (`F_OFD_SETLK`) and NFSv4 share reservations.
  Section 5.1 says what they are and why nothing here needs them yet.
- Delegation recall (CHAOS-18), the remaining OBS, SEC, SCALE and CHAOS cases.
- Fixing `scripts/lock-probe.sh`, which passes `flock -w` to an applet that has
  no `-w`. It is a live defect in a merged case, it is one line, and it is its
  own PR with its own finding. Section 11, risk 1.

**Depends on**

- The node agent, for `/proc/mounts` and `/proc/locks` on a named node.
- `pkg/chaos` from phase 2, for DATA-12 and DATA-13.
- The `OK|ERR <index> <epoch>` log format from phase 2 and the run-file and
  state-file convention from `scripts/`, both reused unchanged.
- `env.Environment.NFSVersion` from preflight, for DATA-11.

---

## 4. Data contract

### 4.1 The locktool invocation

One binary, three subcommands, one line of output each. Every field is a fixed
token or an integer, so the parser is a `strings.Fields` and a switch.

| Subcommand | Arguments | Blocks | Prints |
|---|---|---|---|
| `hold` | path, start, len, mode, run-file, state-file | no: retries `F_SETLK` once a second while the run-file exists | `launched` on the first pass |
| `try` | path, start, len, mode | no | `GRANTED` or `REFUSED type=<r\|w> start=<n> len=<n>` |
| `getlk` | path, start, len, mode | no | `FREE` or `HELD type=<r\|w> start=<n> len=<n>` |

**`hold` never blocks in `F_SETLKW`.** A blocking acquire cannot notice its
run-file being removed, so a holder that is refused, or whose peer never
releases, sits in the kernel with an empty state file: `Release` cannot stop it,
and on a hard mount the process may be unkillable in `D` state, which leaves the
pod `Terminating` and turns teardown into the F-001-adjacent path it is supposed
to avoid. So `hold` polls `F_SETLK` at the probe rate instead, checking the
run-file between attempts, and reports `waiting` in its state file until it is
granted. That also gives the caller a fourth state to poll, rather than
inferring a blocked acquire from an empty file. `hold-flock.sh` has the same
weakness today with busybox `flock`, which has no timeout flag; that is not this
phase's to fix, but locktool is ours and starts without it.

**No `pid` field.** An earlier draft printed the holder's pid. The protocol
does not carry one: the denied response to `LOCK` and `LOCKT` gives the
conflicting offset, length and type plus an opaque lock owner (a client id and
an owner blob), and nothing that identifies a process on another node. Whatever
`l_pid` holds after a cross-client `F_GETLK` cannot name a remote process, so
printing it would be inviting a reader to trust a number that means nothing. The
range and the type are real and are what the assertions use; which pod on which
node is expected to hold a range is something the harness knows because it put
it there, so the case carries that and the failure message names it.

Fields:

| Field | Type | Meaning |
|---|---|---|
| path | string | file on the share; created if absent, as the lock probe already does |
| start | int64 | `l_start`, absolute, `SEEK_SET` |
| len | int64 | `l_len`; 0 means "to end of file", which is how a whole-file `fcntl` lock is expressed |
| mode | `read` or `write` | `F_RDLCK` or `F_WRLCK` |
| run-file, state-file | path on the pod's own filesystem, never the share | same convention as `hold-flock.sh` |

Exit codes: 0 for granted or free, 1 for refused or held, 2 for a usage or I/O
error. Refused and error are separated because a refusal is the expected result
in half these cases and an error never is.

`hold` writes `waiting`, `held`, `released` or `failed` into the state file, so
`framework.LockHolder` covers `flock` and `fcntl` holders with one shape and one
polling loop.

**Why `getlk` as well as `try`.** `F_GETLK` asks the server who holds the range
without taking anything. `F_SETLK` answers the same question by acquiring, which
changes the state every later attempt observes. CHAOS-06 asserts "still held by
the original holder" from the other end, and a probe that acquires cannot say
that.

### 4.2 The client's own lock table

`/proc/locks` on a node, read through the node agent. Not an invocation, a
kernel file, present on every Linux node with no tool installed. One line per
lock the kernel knows about:

| Field | Example | Meaning |
|---|---|---|
| type | `POSIX`, `FLOCK`, `OFDLCK` | which API took it, which is exactly the distinction section 5.1 turns on |
| enforcement | `ADVISORY` | always advisory here; mandatory locking is gone from Linux and never crossed NFS |
| mode | `READ`, `WRITE` | shared or exclusive |
| pid | integer | the holder, in the node's PID namespace |
| start, end | integer or `EOF` | the byte range |

This is the *client's* belief, not the server's, and the difference is the
point. A lock the client thinks it holds and the server has forgotten is the
failure CHAOS-06 exists for, and it is visible as a `/proc/locks` line on the
client with no matching refusal at the server. Parsed by a pure function with a
unit test over recorded output, as `ParseMounts` already is.

### 4.3 The record set on the share

The durability sweep replaces "is the file non-empty" with a verdict per record.
A record is `<dir>/rec-<index>`, `RecordBytes` long, filled with a deterministic
byte pattern derived from the index, so any byte's expected value is computable
from its offset without a copy of the original.

| Verdict | Meaning | DATA-12 | DATA-13 |
|---|---|---|---|
| `correct` | full length, every byte as written | pass | pass |
| `absent` | no file, or zero length | **fail** | pass, recorded |
| `short` | a correct prefix, less than full length | **fail** | pass, recorded |
| `wrong` | any byte differs at an offset that was written, **or the file is longer than `RecordBytes`** | **fail** | **fail** |

The four verdicts are exhaustive over observed length, which an earlier draft's
were not: below `RecordBytes` is `absent` or `short`, exactly `RecordBytes` is
`correct` or `wrong`, and above `RecordBytes` is `wrong` whatever the prefix
says. A record that grew is a byte nobody wrote, and rule 6 admits no reading
under which that is acceptable.

DATA-12 fails on anything but `correct`, because the write was fsynced and the
server acknowledged it. DATA-13 fails only on `wrong`. That asymmetry is the
whole content of the pair, and it is why the sweep returns four verdicts rather
than a boolean.

The sweep runs in one exec for the whole set. A round trip per record would take
longer than the outage being measured, and would be running while the mount is
still recovering. It reports counts plus the index and offset of the first
`wrong` byte, which is what a filed defect needs.

### 4.4 The directory census

DATA-10 populates `<dir>` with entries named `e-<index>`, index from 1 to
`DirEntries`. The population is split across shells inside one pod so that
creation is not a serial round trip per file. The case records:

| Field | Type | Meaning |
|---|---|---|
| created | int | entries the populate step reports it created |
| listed | int | entries one listing returned |
| deleted | int | entries the deleting pod reports it removed |
| unknown | []string | names the listing returned that do not match `e-<index>` for an index in range |

`unknown` is the assertion. `listed` is a record: a listing racing a delete is
allowed to miss entries, and asserting a count would fail on lawful behavior.
A name that was never created is a directory chunk read after it was reused,
which is the defect this case exists for.

### 4.5 The fio job contract

DATA-14 runs one fio job per pod against a directory that pod owns alone.
Verification is fio's own: `verify=crc32c` with `verify_fatal=1`, so a mismatch
ends that job with a non-zero exit rather than being buried in a summary.

The harness consumes only what it asserts on, from `--output-format=json`:
`jobs[].error`, and the verify error counters. Throughput numbers are recorded
in the bundle and asserted on by nobody: DATA-14's expected result is zero
checksum mismatches, and a performance bound here would be a SCALE case wearing
a DATA number.

### 4.6 Ownership and evolution

Producer of all of the above is the harness; the only consumer is the case that
produced it plus the artifact bundle. Nothing is a system of record, and nothing
survives a run. The share is truth for file content; the pod's own filesystem is
truth for what the harness did and when, exactly as in phase 2.

Likely next additions, and what they cost:

- `F_OFD_SETLK` as a fourth mode, if a case ever needs the release semantics in
  the last row of section 5.1's table. Additive: a new mode token, no change to
  the output shape.
- A per-record timestamp in the sweep, if a durability failure ever needs to be
  placed against the fault timeline. Additive: a fifth field.
- A `locktool` subcommand for `O_DIRECT`, if section 5.7's probe shows the tools
  image cannot do it. That widens the binary past its name and is the reason it
  is an alternative rather than the plan.

What would force a breaking change: putting the locktool output into the
`OK|ERR <index> <epoch>` log format shared with the workload and the lock probe.
It nearly fits, and it would couple three parsers to one shape so that adding a
field to one breaks the other two. They stay separate.

---

## 5. Design and decisions

### 5.1 What a byte-range lock is, and what an application sees

[decided: byte ranges are the protocol's native lock; whole-file locking is a special case of it, not a different mechanism]

This section exists because the phase rests on it and because the answer is not
obvious from the outside.

**NFSv4.1 locks byte ranges, natively.** RFC 8881 carries file locking in the
protocol itself, under "File Locking and Share Reservations", with `LOCK`,
`LOCKT` and `LOCKU` operations that each take an offset and a length; a length
of all-ones means "to the end of the file". This is not an extension and not a
side protocol. NFSv3 needed one, the Network Lock Manager, running alongside
NFS with its own state and its own failure modes; folding locking into the
protocol is one of the things NFSv4 is for. So a byte-range lock is not an
exotic thing this phase is reaching for: it is the ordinary thing, and
whole-file is the degenerate case of it.

**The Linux client sends one for every lock, including `flock`.** From
`fs/nfs/file.c`, on `nfs_flock`:

> We're simulating flock() locks using posix locks on the server

So `flock -x` from a pod already becomes a byte-range `LOCK` over the whole
range on the wire. That is why phase 3 could land CHAOS-06 on whole-file locks
and still exercise reclaim honestly: same operation, same reclaim path,
different range. What it could not exercise is two clients holding *different*
parts of one file, which is the only thing ranges add and the only thing this
phase is after.

**What the consuming application sees.** Four ways to ask, one wire operation:

| What the app calls | Type in `/proc/locks` | On the wire | Range | Released when | Who actually uses it |
|---|---|---|---|---|---|
| `flock(2)`, `flock -x` | `FLOCK` | `LOCK` over the whole range | whole file | the last descriptor of that open file description closes | shell scripts, leader election, lock files |
| `fcntl(F_SETLK / F_SETLKW)` | `POSIX` | `LOCK` at offset and length | yes | **any** descriptor to that file is closed by the process, or the process exits | databases; SQLite locks a region of the database file |
| `fcntl(F_OFD_SETLK)` | `OFDLCK` | `LOCK` at offset and length | yes | the descriptor that took it closes | newer libraries avoiding the POSIX row's rule |
| `fcntl(F_GETLK)` | n/a, a query | `LOCKT` | yes | n/a | "who holds this", without taking it |

Three consequences an application owner can feel, and all three are why these
cases exist:

- **Advisory, always.** A process that never asks for a lock writes anyway.
  Mutual exclusion here is a property of cooperating applications, not of the
  filesystem. No case in this suite asserts otherwise.
- **The POSIX close rule is a foot-gun.** A process that opens the same file
  twice and closes either descriptor loses *all* its POSIX locks on that file.
  That surprises people, it is the reason OFD locks were added, and it is why
  locktool opens once and holds that one descriptor (section 5.2).
- **A lock that cannot be reclaimed becomes an I/O error, not a silent share.**
  When reclaim fails after a server restart, the Linux client sets
  `NFS_LOCK_LOST` on the lock owner, and subsequent I/O through that lock
  returns `-EIO` (`fs/nfs/nfs4state.c`). This is the honest failure the plan
  wants: the application finds out. A case that saw two clients both believing
  they hold the same range would be looking at a much worse defect, and
  CHAOS-06 is what would see it.

**And the mount option that makes all of it disappear.** `nolock`, and
`local_lock=all|flock|posix`, tell the client to keep locks on the node and
never send them to the server (`fs/nfs/fs_context.c`). On such a mount two pods
on two nodes both get the lock, every time, correctly, by configuration. A lock
case that ran there would fail and the failure would point at the server. So
rule 2: the lock cases read the mount options first and report **blocked**,
naming the option, if locking is local. This is cheap, it is the same
`/proc/mounts` read DATA-08 already needs, and it converts a week of
misattribution into a line of output.

### 5.2 Why a binary, and what already exists

[decided: nothing off the shelf takes an fcntl byte-range lock from a command line, so a small one is written here]

The preference is to use a tool that already exists. There is not one. What was
checked:

| Candidate | What it does | Why it does not serve |
|---|---|---|
| `flock(1)`, util-linux or busybox | `flock(2)` | whole file only; the system call has no range argument. busybox also has no `-w` |
| `lslocks`, util-linux | lists `/proc/locks` | read-only, and not in busybox. Its input is used directly instead, section 4.2 |
| `fcntl(1)` | would be the obvious tool | does not exist. util-linux ships no byte-range lock command |
| `python3 -c` with `fcntl.lockf` | real POSIX record locking, no binary to build | needs Python in the tools image. The repository's rule is to assume nothing beyond busybox, so every range case would skip on the default image |
| Connectathon `tlock`, `fstests`, NFStest | purpose-built NFS locking tests | source to compile or a Python suite to install; both are a heavier dependency than the 200 lines they would replace, and neither is packaged on a stock image |
| `fio --file_lock` | job coordination | whole file, and it locks to serialize fio's own jobs, not to assert anything |

So: about 200 lines of Go over `syscall.FcntlFlock` with `F_SETLK` and
`F_GETLK`. `F_SETLKW` is deliberately unused; section 4.1 says why a blocking
acquire is the wrong primitive for a process the harness has to be able to
stop. No cgo, therefore static by default. POSIX record locks are
per-process rather than per-thread, so the Go runtime's threads are not a
hazard; the hazard is the close rule in section 5.1, which is why `hold` opens
the file once and keeps that descriptor rather than reopening.

### 5.3 How locktool reaches a pod, without an image

[decided: built by `make locktool` per node architecture, streamed into the existing pod over pods/exec]

No container image is built, published or pulled. The alternative shape is an
image, which is what `fio` does, and it is right for `fio` because `fio` is not
ours to build. It is wrong here: a binary this repository owns, gated on a
registry the operator has to populate, means DATA-06 and the byte-range half of
DATA-05 never run anywhere. A case that is skipped everywhere is a case that
does not exist.

So: `make locktool` cross-compiles with `CGO_ENABLED=0` into
`bin/locktool-linux-<arch>` for `amd64` and `arm64`, about 1.5 MB each. At
runtime the harness reads the target node's architecture from
`node.Status.NodeInfo.Architecture`, streams the matching file into the ordinary
client pod over `pods/exec` with stdin attached to `cat > /tmp/locktool`, and
chmods it. The exec stream is binary-clean, so no encoding step is needed. The
one harness addition is stdin on `Client.Exec`, which it does not wire today:
`PodExecOptions.Stdin` and `remotecommand.StreamOptions.Stdin` are both already
in the vendored API, so it is a handful of lines on a path every case already
uses.

**Why not `kubectl cp`.** It is the obvious answer and it is the wrong one
here, for four reasons that compound:

- **The suite has no `kubectl` dependency, and this would create one.** The
  harness is a client-go program authenticating with a kubeconfig; `os/exec`
  appears only in unit tests, deliberately, to run generated shell under a real
  shell. Shelling out would put a binary on the operator's PATH in the README's
  requirements for one file copy.
- **Two independent resolutions of the same cluster.** `-kubeconfig` and
  `-context` are resolved by client-go today. `kubectl` would resolve them
  again, from its own flags and environment. A multi-path `KUBECONFIG` or a
  different current-context makes those two answers diverge, and the failure is
  that the binary lands in one cluster while the case runs in another. Nothing
  about that failure looks like what it is.
- **`kubectl cp` needs `tar` in the container.** It works by streaming a tar
  archive into `tar -xmf -` inside the target, and its own documentation says
  "Requires that the 'tar' binary is present in your container image. If 'tar'
  is not present, 'kubectl cp' will fail." busybox has `tar`, so this is not
  fatal, but it is a dependency bought for nothing: `cat > file` needs no tool
  at all, which is the same standard every other script in this repository is
  held to.
- **It is not less code, and it is worse code.** Against ten lines of stdin on
  an existing helper: locate the binary, thread three flags, quote the paths,
  and parse a combined-output blob instead of the wrapped error naming the
  namespace and pod that `MustSh` already produces. Bounding it on the case's
  context is weaker too, and every wait in this harness is bounded by a named
  constant.

Importing `k8s.io/kubectl/pkg/cmd/cp` as a library is the third shape, and it
is rejected for a plainer reason: it pulls the cobra command surface in behind
it, and that package exists to back a command, not to be called.

Three guards, because a silently wrong binary is worse than a missing one:

- The harness computes the sha256 of the local file and compares it against
  `sha256sum` in the pod after the copy. A truncated stream fails here rather
  than as a mysterious exec error later.
- A node whose architecture has no built binary reports **blocked**, naming the
  architecture and `make locktool`.
- `bin/` is git-ignored. The binary is never committed; a repository that ships
  compiled artifacts cannot be reviewed.

It lands at `/tmp/locktool`, on the container filesystem, never on the share.
Putting the tool that tests the filesystem *on* the filesystem under test is the
same mistake as logging progress there, which phase 2 already ruled out.

### 5.4 locktool's surface

[decided: three subcommands, and nothing that is not a lock]

`hold`, `try`, `getlk`. No `dio`, no `punch`, no `populate`. Each of those is a
real need in this phase and each is met by the image's own tools, probed. A
binary named for locks that also does direct I/O and hole punching is a
harness-specific busybox, and the next case adds a fourth thing to it.

The cost of this decision is section 5.7: if the tools image turns out not to
carry `oflag=direct`, DATA-07 reports blocked rather than falling back to
locktool. That is the failure mode this repository prefers, and section 11
carries it as the open item it is.

### 5.5 DATA-06: which client vanished, and why an operator cares

[decided: assert one lease as an upper bound, and record which mechanism released the lock]

The test plan's expected result is "lock released within one lease period; new
acquirer succeeds". Read literally against Kubernetes, that sentence has the
wrong actor in it, and the case has to be written against the real one.

The client is the node, not the pod. From the kernel's own documentation
(`Documentation/filesystems/nfs/client-identifier.rst`):

> The Linux NFSv4 client establishes a single lease on each NFSv4 server it
> accesses. NFSv4 mounts from a Linux NFSv4 client of a particular server then
> share that lease.

One lease per node per server, shared by every mount and every pod on it. So:

- **A pod dies.** Kubelet SIGKILLs the container, its descriptors close, the
  client sends `LOCKU`, and the lock is gone in about a second. No lease
  expires, because the node never stopped renewing. Only that pod's locks move.
- **A node dies.** Nothing closes anything. The locks of *every pod on that
  node* stay held until the lease expires, and then all of them drop together.

That difference is what an operator is actually looking at when an application
reports a lock it cannot take, and it is why the distinction is worth a rule
rather than a footnote. The two answers are a second versus a lease, and one pod
versus every pod on a node. If the answer is "one application, back in a
second", nothing is wrong. If it is "every pod on node X, for a minute", the
node is the story. DATA-06 is the case that establishes the first number so the
second one means something; the node-loss side is CHAOS-03 and SEC-07.

The case therefore:

- asserts the range is re-acquirable, from a pod on another node, within one
  lease period (a new row in `pkg/slo`, not a literal in the case);
- records the observed interval and classifies it: a release inside a couple of
  poll intervals is the descriptor-close path, a release near the lease is
  expiry, and the difference says whether the node ever noticed the pod died.

A case that asserted expiry would fail on a healthy cluster. A case that
asserted promptness would fail wherever kubelet was slow to kill, for reasons
that have nothing to do with NFS. The bound the plan states holds under both,
and the classification is what makes the result readable.

The re-acquirer is on another node on purpose: that request crosses the server,
which is the only place the two clients can be made to agree.

### 5.6 Force delete, and what the Kubernetes API can and cannot see

[decided: use the API where the driver attaches, and the node where it does not; never skip the wait]

DATA-06 force-deletes a pod. Teardown then deletes the case's claims. That is
the exact ordering of F-001: the pod leaves the API before kubelet has
unmounted, the wait that follows returns immediately because the object is gone,
the claim is deleted, the provisioner destroys the export, and a node is left
retrying RPCs against an export that no longer exists, uninterruptibly. So the
case must wait for the unmount, and the question is where it can watch it.

**The API answers, but only sometimes.** `node.status.volumesInUse` is the
record of what kubelet still has mounted, and it is authoritative when it
applies. Its own definition is narrower than it first reads:

> List of attachable volumes in use (mounted) by the node.

Attachable. A CSI driver declares that with `attachRequired` on its `CSIDriver`
object, and when it is false, "the attach operation will be skipped" and no
`VolumeAttachment` is created. NFS CSI drivers generally set it false: there is
nothing to attach, the mount is the whole operation. On such a driver
`volumesInUse` says nothing about the case's volume, and an empty list is
indistinguishable from a finished unmount.

Note also that after a force delete there is no Pod object left, so kubelet's
unmount progress and its errors have nowhere to land as Events either.

So the wait reads the `CSIDriver` object once and takes one of two paths:

- `attachRequired: true`: watch `node.status.volumesInUse`. The entries are
  kubelet `UniqueVolumeName`s, **not** raw CSI volume handles: for a CSI volume
  the plugin builds `<driver>^<handle>` (`volNameSep = "^"` in
  `pkg/volume/csi/csi_plugin.go`) and kubelet prefixes the plugin name, giving
  `kubernetes.io/csi/<driver>^<handle>`. Comparing `pv.Spec.CSI.VolumeHandle`
  against that list matches nothing and would report the volume gone on the
  first poll, which is F-001 with extra steps. The wait constructs the unique
  name from the driver and handle and compares that, and a name it cannot
  construct is treated as "not proven gone", never as gone.
- `attachRequired: false`, or no CSIDriver object (an in-tree `spec.nfs` PV):
  read `/proc/mounts` on the node through the node agent until the mount is
  gone.

**Both paths are always available.** An earlier draft said the case skips
without the node agent. It cannot: preflight creates the privileged DaemonSet
and returns an error if it cannot schedule ("Its absence is a hard failure, not
a silent skip, because it is the only way to see the node",
`pkg/preflight/preflight.go`), and nothing runs until preflight passes. So
`Caps.NodeAgent` is true whenever a case executes at all, the skip was dead
text, and worse it advertised a way to weaken the F-001 rule that can never be
reached. The API branch is therefore an optimisation, not a fallback: where the
driver attaches, the wait is a Node `GET` instead of an exec into a privileged
pod.

**Registering the wait is not enough; teardown needs its own guard.** `Defer`
callbacks return nothing, and `DeleteCaseObjects` decides what to keep from
`claimsHeldBy`, which reads the pods still in the API. A force-deleted pod is
not in the API, so a wait that timed out leaves nothing behind to stop the claim
being deleted under a live mount. The fixture therefore carries an explicit
record: a case that force-deletes a pod marks that claim unproven, the wait
clears the mark only on an observed unmount, and `DeleteCaseObjects` keeps every
still-marked claim and fails naming it, exactly as it already does for a pod
that outlived `PodTerminateTimeout`. Leaking a claim is recoverable; wedging a
node is not, and that trade is already the rule in teardown.

The wait is bounded, and expiry fails the case loudly, naming the node and the
volume. A node that has not unmounted is the state F-001 says to treat as
dangerous rather than wait out. `DeletePodNow` stays as the primitive; cases
call the helper that force-deletes, waits, and clears the mark.

This is the "uglier path" F-001's follow-up section asks for, approached from
the safe side: the same sequence, with an observation between the two steps.

### 5.7 DATA-07: `O_DIRECT` comes from the image, and is probed

[decided: probe `oflag=direct` on the share at case start; blocked if absent]

busybox `dd` does support `iflag=direct` and `oflag=direct`, but both live
behind the `FEATURE_DD_IBS_OBS` build option, so whether a given image has them
is a property of that image's build and not something to assume. The Linux NFS
client imposes no memory-alignment requirement for direct I/O the way a block
device does, so a busybox `dd` buffer is usable if the flag parses at all.

The case therefore opens with a 4KiB `oflag=direct` write to the share. If `dd`
rejects the flag, the case reports blocked, naming `-tools-image`. If it
succeeds, the case proceeds: two pods on two nodes, both `O_DIRECT`, disjoint
offsets in one file, then each reads back the other's region with
`iflag=direct`, and the content is verified by pattern. Direct I/O bypasses the
page cache on both ends, so a read that returns the other pod's bytes crossed
the server, and one that returns zeros did not.

Alt: a `dio` subcommand on locktool would remove the dependency and the skip.
Rejected for now under section 5.4, revisited in section 11 once a real image
has been probed.

### 5.8 DATA-08: one export, two mounts, and the option read back

[decided: a static clone PV over the same export, with `noac`, verified in /proc/mounts before use]

Mount options belong to the PV, and both pods must see one file, so the two
mounts have to be two volumes over one export. The claim the case provisions
dynamically gives the export; a static PV is created pointing at the same
server and path with `vers=4.1,hard,noac`, and a second claim binds to it by
name with an empty storage class, the way the broken-mount volume in OBS-04
already does.

Extracting the server and path from the bound PV is the fragile part, and it is
fragile in a way a unit test can hold:

- `spec.nfs` gives `server` and `path` directly. That is what the in-cluster
  provisioner behind the first real run produces.
- `spec.csi` puts them in `volumeAttributes`, under keys the driver chooses.
  The harness knows the common ones and nothing more.
- Neither readable means **blocked**, naming the PV and the fields it could not
  read. It does not guess.

The extraction is a pure function over a PV object, unit-tested against recorded
PV shapes, per the repository's rule that anything failing expensively on a
cluster should fail cheaply on a workstation first.

Then the rule that makes the case worth having: before asserting anything about
visibility, read `/proc/mounts` on the reader's node through the node agent and
confirm the mount carries `noac`. A driver or a node that dropped the option
would otherwise turn DATA-08 into a second, slower DATA-04 that passes when the
timing is kind. With the check, a dropped option fails the case and names the
option and the node.

Teardown ordering is safe by the API rather than by luck: the clone PV is
registered for deletion on the fixture, and `pv-protection` holds it until its
claim is gone, which happens after the pods are gone. The dynamic claim that
owns the export is deleted by the same collection delete, after the same wait.

### 5.9 DATA-09: silly rename is a property of one client

[decided: two halves, same node and cross node, asserting different things]

The Linux client implements unlink-of-an-open-file by renaming it to
`.nfsXXXXXXXX` and deleting it when the last descriptor closes. That is a client
behavior, and it can only happen when the client doing the unlinking is the
client holding the file open. Section 5.5 says who that is: the node. When
another node unlinks it, the server removes it, and the holder's next operation
gets `ESTALE`. Both outcomes are correct. A case that ran the cross-node shape
and asserted the same-node expectation would file an NFS-conformant client as a
defect.

So DATA-09 has two halves, named as such in the outline:

- **Same node.** Pod A holds a descriptor open; pod B on the same node unlinks
  the file. The directory must show a `.nfs*` entry, A's descriptor must stay
  usable (a read returns what was written), and the silly entry must disappear
  once A closes. A leftover `.nfs*` file after the holder is gone is a leak and
  is a failure.
- **Cross node.** Pod A on one node holds it open; pod B on another unlinks and
  re-creates the name with different content. A's descriptor refers to the old
  file or fails with `ESTALE`; both are recorded, neither fails. What fails is
  A reading the *new* file's content through the old descriptor, which would
  mean a file handle was reused.

The rename half runs in both shapes: a rename is not an unlink, the descriptor
stays valid through it on any client, and a descriptor that follows the name
rather than the file is a failure in either shape.

### 5.10 DATA-10: assert on names, record counts

[decided: `unknown` names fail the case; a listing that misses entries does not]

The defect this case is derived from is a use-after-free on directory chunk
reuse during READDIR under cache pressure. From a client, that surfaces as a
listing returning a name that is not in the directory: a fragment of a reused
page decoded as an entry. It does not surface as a count being off, because a
listing racing deletes is *supposed* to have a count that is off.

So the assertions are: the listing completes with a zero exit, every name it
returns is one the populate step created, the server pod's restart count is
unchanged, and the client nodes' `dmesg` carries no oops or `nfs: server ...`
error over the window. The counts go in the bundle.

Two practical points. The listing streams: `find <dir> -mindepth 1 -maxdepth 1`
and compares basenames, not `ls`,
because busybox `ls` sorts, and sorting 100k entries in a pod with no memory
limit set is a way to discover the node's OOM killer. `-mindepth 1` is not
decoration: `find <dir> -maxdepth 1` emits `<dir>` itself first, and a
classifier fed that path would flag the directory as a name nobody created and
fail DATA-10 on every run. And population is
inode-hungry: the case checks free inodes and space before it starts and reports
**blocked** if the export cannot hold the set, rather than failing on `ENOSPC`
halfway through and pointing at the server.

### 5.11 DATA-11: sparse yes, hole punch no, and say which

[decided: assert sparse write and read-back; record the punch as unsupported]

Hole punching on this suite's mounts is unavailable twice over, and both are
worth stating because a reader will otherwise assume the case is weak:

- NFSv4.1 has no operation for it. `ALLOCATE`, `DEALLOCATE` and `READ_PLUS`
  arrived with NFSv4.2 in RFC 7862. Preflight pins `vers=4.1` and fails on
  anything else, so `fallocate -p` on these mounts returns `EOPNOTSUPP` from
  the client before any request reaches the server.
- The busybox `fallocate` applet parses `-l` and `-o` only. `-p` appears in its
  own usage comment and not in its option string, so on a stock image the flag
  is rejected by the tool regardless of the mount.

What the case asserts on 4.1: a file written sparsely (`dd` with `seek`) reads
back zeros in the hole, the byte written past the hole is at the right offset,
`stat` reports the full logical size, and allocated blocks are recorded rather
than asserted, since whether the backing filesystem stores the hole sparsely is
not an NFS property.

What it does with the punch: probes the tool first, then attempts it, and the
two refusals are not the same answer. If the image's `fallocate` does not parse
`-p` the case reports **blocked**, naming `-tools-image`, because rule 8 owns a
missing tool and nothing about the protocol has been learned. Only a `-p` that
parses and then returns `EOPNOTSUPP` is recorded as the protocol saying no,
under rule 9. On a busybox image the first branch is the one that fires, so
without this split every run would file "NFSv4.1 does not support hole
punching" on evidence that is really "this image's applet has no `-p`".

It fails only if the punch *reports success* and the region is not zero
afterwards, or the reported size changes. That is the assertion that survives
the day preflight accepts 4.2, and on that day the case starts asserting the
punch without being rewritten. The 4.2 branch is unreachable until then, and it
is written anyway rather than left out, because the alternative is a case that
has to be rediscovered and rewritten.

**This narrows a requirement, so the requirement moves.** Section 3.2 lists
DATA-11 as "sparse file write, hole punch, read back", and a design document is
not the place to quietly drop the middle third of that. The test plan's DATA-11
row is amended in this PR to say that the punch is recorded rather than asserted
on a 4.1 mount, and what the case still fails on. Requirements live in the test
plan; if the maintainer would rather keep the row as it is and have DATA-11
report blocked until preflight accepts 4.2, that is the other legitimate answer
and it is a one-line revert of that row.

### 5.12 DATA-12 and DATA-13: one workload, two verdict tables

[decided: the sweep returns four verdicts; DATA-12 fails on three of them, DATA-13 on one]

Both cases are: run the record workload, kill the server under it, bring it
back, sweep from a pod on another node. They differ in one flag on the workload
(whether each record is written with `conv=fsync`) and in which verdicts fail.
Section 4.3 has the table.

Three things this design refuses to do:

- **Assert that DATA-13 loses data.** It very often will not. A SIGKILL of the
  server *process* does not drop the host page cache underneath it, so unstable
  writes the server had not yet committed to disk may well still be there when
  it restarts. The plan says "may be absent, asserted as acceptable,
  documented", and documented is the whole job: the case records how many
  records were absent or short and moves on.
- **Reuse CHAOS-01's measurement.** DATA-12 asserts durability and nothing else.
  It does not re-measure recovery time; CHAOS-01 owns that number and two cases
  reporting it is two numbers to reconcile when they disagree.
- **Call a short record corruption.** Without an fsync the client is free to
  have flushed a prefix. A prefix is lawful. A wrong byte inside the prefix is
  not, and that is the line the sweep draws.

The content-verifying sweep replaces the existence check everywhere, so CHAOS-01
and CHAOS-02 get stronger by the same change: "every committed write is still
there" becomes "every committed write is still there and still says what it
said".

### 5.13 DATA-14: fio, partitioned by pod, capacity checked first

[superseded 2026-09-11: DATA-14 is deferred and not implemented]

**This section records a decision that was made and then reversed.** DATA-14 was
built as designed below and then removed, because this project is not doing soak
testing yet: the suite's soak is SCALE-07 in Section 3.4 of the test plan, and
twenty pods under sustained mixed load for an hour is a scale question wearing a
data path number. Section 3.2 of the test plan carries the full reasoning, what
the deferral costs, and the three other arguments against doing it here.

The design below stands as written for whoever picks the case up, in this
section or in SCALE-07's. What went with the removal: the `-fio-image` flag,
`pkg/framework/fio.go`, `scripts/fio-soak.sh` and the `make test-data-soak`
target.

[decided, then reversed: one job per pod over its own directory, `verify=crc32c`, `verify_fatal=1`]

Twenty pods, 70/30 read/write, file sizes from 4KiB to 1GiB, one hour, zero
checksum mismatches. Three decisions inside that:

- **Each pod owns a directory.** Two pods writing one file with verification on
  would report mismatches that are the harness's fault, exactly as DATA-01
  established. Cross-pod interference is DATA-01's and SCALE-06's job.
- **`verify_fatal=1`.** A mismatch stops that job at the mismatch, so the
  offending offset is in the output. Without it fio continues and the report is
  a count with no location.
- **Capacity is checked before the run, not discovered during it.** Twenty jobs
  at these sizes need tens of gigabytes. The case sizes its claim from the job
  set, compares it against what `df` reports inside a pod, and reports
  **blocked** if it does not fit. An hour of soak that dies on `ENOSPC` at
  minute fifty is an hour spent to learn nothing.

`fio` comes from `-fio-image` and the case skips without it. That is right here
and wrong for locktool (section 5.3) for one reason: `fio` is not ours, its
image is a packaging decision of whoever runs the suite, and a soak case that
skips costs nothing on the days nobody has an image to run it with.

### 5.14 Naming: every case here is a `TestData` case

[decided: follow the category scheme introduced by the suite-wide reorganization, including for the two cases that kill the server]

Two earlier drafts of this section are now wrong, and the history is worth one
paragraph because it is the second time this has moved.

The first draft proposed a nightly gate for the plan's gate-N cases. The second
dropped it: nothing in this repository runs on a schedule, so a `TestNightly`
prefix would name a cadence nothing keeps, and the prefixes then in use named
what a case *did* (`TestChaos` for injuring the server, `TestSoak` for running
long). [PR #11](https://github.com/mikebz/nfs-verification/pull/11) has since replaced that scheme outright. There is no gate column
in the test plan any more, `test-presubmit` and `test-soak` are gone, and
Section 4.2 is now "Execution by Category": one prefix and one target per plan
section, `TestProv`, `TestData`, `TestChaos`, `TestObs`, `TestSec`.

So every case in this phase is a DATA case and carries `TestData`:

| Case | Name | Target |
|---|---|---|
| DATA-06 to DATA-14 | `TestData...` | `make test-data` |
| the DATA-05 byte-range half | `TestDataLocksAcrossNodes`, extended | `make test-data` |
| the CHAOS-06 range extension | `TestChaosLockReclaimAcrossFailover`, extended | `make test-chaos` |

**Including DATA-12 and DATA-13, which kill the server.** Under the previous
scheme they would have been `TestChaos`, because that prefix marked a case that
injures the server whatever section it came from; that is
[`03-grace-and-lock-reclaim-design.md`](03-grace-and-lock-reclaim-design.md)
section 5.8, and [PR #11](https://github.com/mikebz/nfs-verification/pull/11) supersedes it. The precedent is already in the tree and
it is unambiguous: PROV-07 and PROV-08 take the server down and are
`TestProvProvisionServerDown` and `TestProvDeleteClaimServerDown`; OBS-02 and
OBS-03 inject a failover and are `TestObsFailoverIsObservable` and
`TestObsGracePeriodIsObservable`. Category wins over what the case does. Doc 03
is not edited; this records the supersession, as the rule for prior numbered
design docs requires.

One consequence deserves saying out loud rather than being discovered: after
this phase **`make test-data` injects faults**. It kills the NFS server twice,
through `pkg/chaos`, and it is no longer a read-mostly target. `make test-prov`
and `make test-obs` are already in that position, so this is where the
repository is rather than something this phase introduces, but a reader of the
Makefile should not have to infer it.

**DATA-14 got its own target, and then the case was deferred.** What follows is
why the target existed; with DATA-14 gone there is no `make test-data-soak` and
`make test-data` holds every DATA case again. The reasoning is kept because it
is the argument anyone reviving the case has to answer.


`make test-data` is a single `go test` over every `^TestData` case with
`-timeout=45m`, and Section 4.2 budgets it at under 45 minutes on two nodes.
After this phase that one invocation would hold fourteen cases: DATA-01 to
DATA-05 already in the tree, the eight added here, and DATA-14, which is an hour
by itself across 20 pods. The target cannot execute its own documented contents.
That has to be settled before implementation rather than left as a note.

Three ways out:

1. Raise `test-data` to `-timeout=120m` and restate the budget. Costs the
   category the "under 45 minutes" property that makes a category target worth
   running at all, and charges every DATA run for a case almost nobody wants.
2. Give the soak its own target: `make test-data-soak` running
   `-run '^TestDataMixedSoak' -timeout=120m`, and `test-data` gains
   `-skip 'MixedSoak'`. One prefix, one file, one category, and the 45 minute
   budget stays true.
3. Shorten DATA-14 in the test plan. A legitimate answer, since requirements
   live there, but it changes what is verified to fit a Makefile, which is the
   wrong direction to reason in.

**Decided: 2, then overtaken by a fourth answer nobody had listed: not doing the
case at all.** Option 2 shipped and was then removed with DATA-14. The objection
that settled it is one none of the three options addressed: a target named for a
runtime reads as a category, and soak is not one of this suite's categories. The cost distinction it reintroduces is inside a
category rather than across them, which is what
[PR #11](https://github.com/mikebz/nfs-verification/pull/11) removed. This is a
change to the Makefile and to Section 4.2, and it lands in the PR that lands
DATA-14, not before: until then `test-data` holds thirteen cases that fit.

### 5.15 What DATA-05 and CHAOS-06 gain

[decided: extend both in the last PR of the phase, once locktool has run on a real cluster]

Phase 3 landed CHAOS-06 on whole-file locks and said why: a whole-file lock
reaches the server as a lock over the whole range (section 5.1 has the kernel's
own words for it) and is reclaimed by the same mechanism, so reclaim,
exclusivity and the grace bar were all assertable without ranges. What was not
assertable is that two clients can hold *different* parts of one file, which is
the thing a byte-range lock is for.

- **DATA-05** gains: two pods on two nodes take disjoint ranges of one file and
  both are granted; each is then refused on the other's range; a range that
  overlaps is refused. The existing `flock` half stays as it is.
- **CHAOS-06** gains: the disjoint-range shape held across a failover. After it,
  each holder still holds its own range by `F_GETLK` from the other side, a
  third client is refused on both, and `/proc/locks` on each holder's node
  agrees with what the server says. A client that thinks it holds a range the
  server has forgotten is the failure in section 5.1's third consequence, and
  reading both sides is the only way to see it.

Both land last, after locktool has been exercised by DATA-06 against a real
cluster. Extending a passing chaos case with a tool nobody has run yet is how a
green case becomes a flaky one.

---

## 6. Delivery phases

**MVP**: locktool, its delivery path, and DATA-06. The smallest slice that can
merge and run: a binary, a `make` target, exec stdin, the copy-and-verify path,
one lock holder shape covering `fcntl`, the mount-option gate, the
force-delete-and-await-unmount helper with both its observation paths, one new
`pkg/slo` row, and one case that uses all of it.

Validated outside a test harness by running the case against a real cluster and
reading three things out of it: a byte-range lock granted on a named node, the
interval between the force delete and the re-acquisition, and the classification
of which mechanism released it. A run that reports blocked is not a validation,
and the reason it reported blocked is the finding.

Later PRs, intent only:

1. **Protocol edge cases**: DATA-07, DATA-08, DATA-09, DATA-10, DATA-11, with
   the clone volume, the NFS-source extraction and its unit tests, the directory
   helpers, and the tool probes.
2. **Durability**: DATA-12 and DATA-13, with the content-verifying sweep
   replacing the existence check, which also upgrades CHAOS-01 and CHAOS-02.
3. **Soak and ranges**: DATA-14 behind `-fio-image`, then the DATA-05 and
   CHAOS-06 range extensions and the `/proc/locks` reader they use.

Later PRs sharpen once the MVP ships. In particular, which of the tool probes
actually fire on a real image decides whether section 5.4 holds or whether
locktool grows a `dio` subcommand.

---

## 7. Configuration

Source: command line flags, passed through the make targets, as in phases 2
and 3.

| Flag | Default | Needed for |
|---|---|---|
| `-tools-image` | unchanged | DATA-07 and DATA-11 probe what this image's `dd` and `fallocate` support |
| everything from phases 2 and 3 | unchanged | the fault, the target, the profile, the grace wording |

One new make target, `make locktool`, which `make all` calls. (`-fio-image` and
`make test-data-soak` shipped and were removed with DATA-14.) No flag for the
locktool path, its architecture, or where it lands in the pod: the path is
derived from the repository root, which `FinalizeFlags` already resolves for the
artifacts directory; the architecture is read from the node; the destination is
fixed. A flag for any of them would be a flag for a value the cluster can
answer.

Validation and behavior on bad config: fail loud, and distinguish the three
answers. A missing `-fio-image` is a skip, because the case cannot run. An image
whose `dd` has no `oflag=direct` is **blocked**, because the case could run on a
different image. A mount carrying `nolock` is **blocked**, naming the option. A
PV whose server and path cannot be read is **blocked**, and says which fields
were missing. None of these is a pass.

Intentionally not configurable: the record size and pattern, the directory entry
count, the soak duration, the number of fio jobs, and the lock ranges. Each is a
property of the measurement, and each is stated in the plan. See section 11 for
the soak duration, which is the one of these a real run may argue with.

Credentials policy: unchanged, none. The suite authenticates with a kubeconfig
and holds nothing else.

---

## 8. Deployment decisions

Runtime unit: unchanged. `go test` on a workstation, driving pods.

One new build artifact, and no new image anywhere. `make locktool` produces
`bin/locktool-linux-amd64` and `bin/locktool-linux-arm64` with
`CGO_ENABLED=0`, so they are static and run on any Linux image including a
busybox one. `make all` gains it, so a contributor who never touches a cluster
still compiles it, and `bin/` is git-ignored.

Lifecycle inside a case:

- The binary is copied into a pod on first use and verified by checksum. Once
  per pod, not once per call.
- Every lock holder and every background loop is registered for stop on the
  fixture before the case can fail, as phase 3 established, so a failing case
  does not leave a lock held on the share by a pod that outlives it.
- The force-delete helper's unmount wait is registered the same way: a case that
  fails between the force delete and the wait must still not reach teardown with
  a live mount behind it.

No new privilege. locktool runs in the ordinary client pod as the pod's own
identity; the node agent, which is privileged and already exists, is read-only
here (`/proc/mounts`, `/proc/locks` and `dmesg`). Where the CSI driver attaches,
section 5.6's wait needs no node access at all.

Cost visibility: DATA-14 is the first case that asks for tens of gigabytes and
an hour. It is skipped without an image, and it reports blocked rather than
provisioning storage it cannot fill.

---

## 9. Observability

The artifact bundle gains five things, written before teardown as everything
else is:

| Artifact | Written by | What it answers |
|---|---|---|
| the verdict table | DATA-12, DATA-13, CHAOS-01, CHAOS-02 | which records were correct, absent, short or wrong, and the first wrong offset |
| the mount line | DATA-08, and every lock case | whether the reader's mount carried `noac`, and whether locking was local, in the driver's own words |
| `/proc/locks` per node | DATA-05, DATA-06, CHAOS-06 | what each client believes it holds, with type and range, next to what the server says |
| the directory census | DATA-10 | created, listed, deleted, and every unknown name verbatim |
| the fio job output | DATA-14 | per-job error and verify counters, and the throughput nobody asserts on |

Log fields on the case's own output stay as they are: the case ID, the node, the
pod, the profile. Two additions, both because a failure without them is not
triageable:

- DATA-06 logs the observed release interval and its classification, not just a
  pass. A run where every release is at the lease boundary is a different
  cluster from one where every release is prompt, and the pass looks identical.
- Every blocked report names the probe that failed and the flag or mount option
  that would fix it. "blocked" with no reason is the failure mode this phase
  adds the most opportunities for.

Export path: unchanged, `artifacts/<run-id>/<CASE-ID>/`.

---

## 10. Alternatives considered

| Option | What it is | Why not chosen |
|---|---|---|
| An existing byte-range lock tool | Use what Linux ships | Nothing ships one. Section 5.2 has the candidates that were checked and what each is missing |
| Python's `fcntl.lockf` | Real record locking from a one-line snippet, no binary | Needs Python in the tools image; the repository assumes nothing beyond busybox, so every range case would skip by default |
| locktool as a published image | Build and push an image, name it with a flag, skip without it | A binary this repository owns, gated on a registry the operator must populate, means the byte-range cases never run anywhere |
| `kubectl cp` | Shell out to copy the binary in | Adds a PATH dependency and a second resolution of kubeconfig and context that can disagree with client-go's; needs `tar` in the container; more code than stdin on the exec helper the suite already has. Section 5.3 |
| `k8s.io/kubectl/pkg/cmd/cp` as a library | Import the copy logic instead of shelling out | Pulls the cobra command surface in; the package backs a command rather than a caller |
| locktool embedded with `go:embed` | Commit the compiled binaries and embed them in the test binary | Committed binaries cannot be reviewed, and the repository would carry two per release |
| Compile locktool inside the pod | Ship the source and build it there | No toolchain on a busybox image, and adding one makes the image the thing under test |
| A `TestNightly` gate | A third prefix for the plan's gate-N cases | Named a schedule nothing keeps, and [PR #11](https://github.com/mikebz/nfs-verification/pull/11) has since removed the gate column entirely. Section 5.14 |
| `TestChaos` for DATA-12 and DATA-13 | Name the two fault-injecting cases for what they do rather than their section | [PR #11](https://github.com/mikebz/nfs-verification/pull/11) sorts strictly by category, and PROV-07 and OBS-02 already inject faults under their own prefixes. Section 5.14 |
| `volumesInUse` alone for the unmount wait | Watch the Node object, skip the node agent | Only covers attachable volumes, and NFS CSI drivers set `attachRequired: false`. Section 5.6 uses it where it applies and falls back where it does not |
| A second StorageClass with `noac` | Clone the discovered class with an extra mount option and provision from it | Gives a different export. DATA-08 needs two mounts of the *same* file |
| Per-pod mount options | Set `noac` on the reader pod | Mount options belong to the PV. Kubernetes has no per-pod override |
| A `dio` subcommand on locktool | Do DATA-07's direct I/O from our own binary | Widens a lock tool into a general one; held as the fallback if a real image's `dd` lacks the flag |
| Asserting DATA-13 loses data | Fail if the un-fsynced records survived | A SIGKILL of the process does not drop the host page cache, so surviving is normal and the assertion would fail on healthy systems |
| Asserting DATA-10's entry count | Compare listed against created minus deleted | A listing racing deletes is allowed to miss entries; the count would fail on lawful behavior |

---

## 11. Open questions and risks

1. **`scripts/lock-probe.sh` passes `flock -w`, which busybox `flock` does not
   have.** Its options are `-s`, `-x`, `-u` and `-n`; `hold-flock.sh`'s own
   comment says as much, and the probe contradicts it. If the tools image
   carries the busybox applet, every probe attempt fails on a usage error, every
   record in the probe log is `ERR`, and CHAOS-07 passes vacuously, since it
   asserts on grants. **Decided by** running the probe against the tools image
   in use and reading the log for a single `OK`. This is a live defect in a
   merged case, one line to fix (`-n` in a retry loop, which is what the bound
   already provides), and it lands as its own PR with a finding before this
   phase starts. locktool later gives the probe a real timed acquire.
2. ~~**Whether the tools image's `dd` carries `direct`.**~~ **Answered**
   2026-09-11 on GKE: `alpine:3.20`'s busybox carries `oflag=direct` and DATA-07
   passed. Section 5.4 holds and locktool does not grow a `dio` subcommand.
   `fallocate -p` is absent on the same image, as predicted, so DATA-11's punch
   half reports blocked there. See F-007 in [`findings.md`](findings.md).
3. **Whether `make test-data` still fits 45 minutes with thirteen cases in it.**
   Section 5.14 takes DATA-14's hour out to its own target, which is the part
   that made the budget impossible. What remains is a real question: DATA-05
   plus the eight added here, with DATA-10 populating 100k entries, against a
   budget written when the category held five quick cases. **Decided by** timing
   the target once the protocol edge cases land, which is the second PR of four.
   If it is over, the honest answers are to move DATA-10 alongside DATA-14 or to
   restate the budget on measured evidence, not to leave a target that overruns
   its own contract.
4. **Whether 100k entries fit.** The default claim is 1Gi and the export may be
   directory-backed with no per-volume quota, so the real limits are the backing
   filesystem's free inodes. **Decided by** the precheck in section 5.10 on a
   real cluster. If the honest answer is that 100k is out of reach there, the
   number moves in the test plan, not in the case.
5. **DATA-14's one hour is a constant.** Fixed values stay fixed until something
   needs them to vary, so there is no duration flag, which means exercising the
   fio path costs an hour. **Decided by** the first attempt to debug it: if that
   hour is spent twice, the flag has earned its place and gets a README row.
6. ~~**The clone PV's reclaim policy.**~~ **Answered for one driver**,
   2026-09-11 on GKE: DATA-08 passed and the clone did not disturb the dynamic
   claim that owns the export. It stays worth re-checking on a driver that
   reconciles static PVs, since the hazard is a property of the driver rather
   than of the clone.
7. **Sizing risk on the nodes.** F-002 is 2GB workers rebooting under a 1MiB
   write and a page cache. DATA-10 and DATA-14 are heavier than anything the
   suite has run. A case that reboots a node reports a storage defect that is a
   node pool defect. Mitigation is the README's existing minimum of 8GB per
   worker for anything past the quick cases, and the fact that neither runs
   unless someone asks for its target; there is no mitigation inside the case.
8. **A case that asserts over a set it produced has to state how large that set
   must be.** The durability pair inherited a three-record warm-up from the
   recovery cases and passed having verified three and four records. Fixed for
   that pair with `durabilityRecords` and `requireDurabilitySet`, and recorded
   as F-007 in [`findings.md`](findings.md). **Open** for everything else: no
   other case in the suite states a floor on the set it measures, and the ones
   that produce a set (DATA-10's directory, DATA-14's files) should be looked at
   before anyone trusts a small pass from them.
9. **Nothing now records that a case is expensive.** [PR #11](https://github.com/mikebz/nfs-verification/pull/11) removed the gate
   column, so DATA-10's 100k entries and DATA-14's hour read in the plan exactly
   like a case that takes twenty seconds. **Decided by** whether anyone is
   surprised by a category target's runtime. If they are, the answer is a cost
   column in Section 4.2 saying what a case costs, which is a fact about the
   case, rather than a gate letter saying when to run it, which is a policy
   nothing enforces.

---

## 12. Verification

One check per rule in section 2, observable from a run.

| Rule | Check |
|---|---|
| 1. Pod death and node death differ | DATA-06 injects a pod death and logs the release interval with its classification. A prompt release is the expected record on a healthy node; the case passes either way within the bound, and the node-loss shape is not asserted here |
| 2. Locks need a non-local mount | DATA-05 and DATA-06 report blocked, naming the option, on a mount carrying `nolock`, `local_lock=all`, or the value that makes their own lock type local. Unit test: `nolock` and `local_lock=all` block both halves, `local_lock=flock` blocks only the `flock` half, `local_lock=posix` only the byte-range half, and **`local_lock=none` blocks neither**, since it is the default and means locks do reach the server |
| 3. No silent weakening | `locktool try` on an overlapping range is refused while a disjoint range on the same file is granted, in DATA-05's new half. A whole-file fallback would grant neither or both. The refusal is read from its range and type; no assertion reads a pid, because the protocol carries none |
| 4. Options are read back | DATA-08 fails, naming the node and the option, when `/proc/mounts` on the reader's node does not carry `noac` |
| 5. Force delete waits for the unmount, and teardown will not delete an unproven claim | DATA-06's mount is gone before the case returns, by `volumesInUse` on an attaching driver and by `/proc/mounts` otherwise. Unit tests: the unique-name builder produces `kubernetes.io/csi/<driver>^<handle>` and a raw handle never matches an entry; the wait treats a mount still present at the deadline as an error, not as done; and `DeleteCaseObjects` keeps a claim still marked unproven and names it, as it already does for a pod that outlived `PodTerminateTimeout` |
| 6. Uncommitted absence is not failure | DATA-13 passes with `absent` and `short` counts greater than zero, and the counts appear in the bundle |
| 7. A wrong byte always fails | Unit test on the sweep parser: `correct`, `absent`, `short` and `wrong` verdicts from recorded tool output, including a record **longer** than `RecordBytes` classified `wrong`, with only `wrong` failing DATA-13 and three of four failing DATA-12 |
| 8. Missing tools report blocked | DATA-07 against an image without `oflag=direct` reports blocked naming `-tools-image`, not a pass and not a failure. Same for DATA-14 without `-fio-image`, which skips |
| 9. Undefined operations are recorded, missing tools are not | DATA-11 on a `vers=4.1` mount passes on the sparse assertions and records the punch refusal **only when the applet parsed `-p`**; an image whose `fallocate` has no `-p` reports blocked naming `-tools-image`, so a tool gap is never filed as a protocol gap |
| 10. Listings may miss, never invent | DATA-10 passes with `listed` below `created`, and fails on any name outside `e-<index>`. Unit tests: the census classifier flags a `.nfs*` leftover and a truncated name as unknown, and the listing under `-mindepth 1` never presents the directory itself for classification |
| 11. Verified by content | The existence check is gone from the durability path; CHAOS-01 and CHAOS-02 assert content after this phase, which is visible as a verdict table in their bundles |
| 12. Phases 2 and 3 still hold | `make test-chaos` passes unchanged before the DATA-05 and CHAOS-06 extensions land, and again after |

Sources for the protocol and platform claims above: RFC 8881 for NFSv4.1 file
locking, RFC 7862 for the NFSv4.2 sparse-file operations, the Linux kernel's
`fs/nfs/file.c`, `fs/nfs/fs_context.c`, `fs/nfs/nfs4state.c` and
`Documentation/filesystems/nfs/client-identifier.rst` for client behavior, the
busybox `coreutils/dd.c`, `util-linux/flock.c` and `util-linux/fallocate.c`
applets for what a stock image can do, the Kubernetes `NodeStatus` and
`CSIDriverSpec` API definitions for section 5.6, and `kubectl`'s own
`pkg/cmd/cp` for the `tar` requirement in section 5.3.

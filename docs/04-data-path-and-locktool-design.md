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

- **No `fcntl` locks.** The `flock` applet on a busybox image calls `flock(2)`,
  which locks a whole file and nothing less. There is no byte-range lock in the
  suite today, so DATA-05 landed half-built, CHAOS-06 landed on whole-file locks
  with the sub-file dimension deferred here, and DATA-06 has nothing to acquire.
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
gate budgets, Section 2.3 for the protocol claim these assertions are calibrated
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

1. The NFS client is the node, not the pod. A case about a client that vanished
   says which one vanished, and asserts only what the loss of that one implies.
2. A lock the harness cannot express is never silently replaced by a weaker one.
   A whole-file lock stands only where the case says whole file.
3. A mount option a case depends on is read back from `/proc/mounts` on the node
   that mounted it, before anything is asserted on it. An option the driver
   dropped must fail the case, not pass it vacuously.
4. A case that force-deletes a pod does not return until that node's mount is
   gone. Deleting a claim while a node still mounts the export is F-001.
5. Data that was never committed may be absent. That is never a failure.
6. A record that is short is acceptable exactly where no fsync was issued. A
   byte that differs from what was written, at an offset that was written, is a
   failure under every reading of the protocol.
7. A tool the image may not carry is probed before it is used. Its absence
   reports blocked and names the flag that fixes it. It never reads as a pass
   and never as a failure.
8. An operation NFSv4.1 does not define is recorded as unsupported, not failed.
9. A directory listing running against concurrent deletes may miss an entry. It
   may never return a name that was never created.
10. Every file a case verifies is verified by content. Existence is not a check.
11. No load case has two clients writing the same bytes. Every one partitions by
    file, as DATA-01 does.
12. Everything from phases 2 and 3 still holds. This phase adds to that
    contract, it does not amend it.

---

## 3. Scope

**In scope for this phase**

- `locktool`: a static Go binary in this repository for `fcntl` byte-range
  locks, plus the delivery path that gets it into a pod without a registry.
- The byte-range half of DATA-05 and the disjoint-range extension of CHAOS-06,
  both of which [`03-grace-and-lock-reclaim-design.md`](03-grace-and-lock-reclaim-design.md)
  section 5.5 deferred to this phase by name.
- DATA-06 through DATA-14.
- A clone volume: a static PV pointing at an export a dynamic claim already
  owns, with different mount options, so one export can be mounted twice.
- Directory population and census helpers for DATA-10.
- A content-verifying record sweep, replacing the existence check for the
  durability cases and upgrading CHAOS-01 and CHAOS-02 by the same change.
- `fio` orchestration behind `-fio-image`, for DATA-14 only.
- A nightly gate, because this phase is the first to add gate-N cases whose cost
  the 15 minute presubmit budget cannot absorb.

**Out of scope (explicit non-goals)**

- NFSv4.2. Preflight pins the mount at `vers=4.1` and fails otherwise, so
  `ALLOCATE`, `DEALLOCATE` and `READ_PLUS` are out of reach. DATA-11 is written
  so that the day preflight accepts 4.2, the punch-hole half becomes an
  assertion without the case being rewritten.
- Publishing a `locktool` container image. Section 5.2.
- Delegation recall (CHAOS-18), the remaining OBS, SEC, SCALE and CHAOS cases.
- Re-gating the gate-N cases already in the repository (PROV-02, PROV-05 to
  PROV-08, PROV-10). They sit in the presubmit gate today. Moving them is a
  rename, and a rename does not land with a behavior change; it is a follow-up
  PR. Section 5.13.
- Fixing `scripts/lock-probe.sh`, which passes `flock -w` to an applet that has
  no `-w`. It is a live defect in an existing case, it is one line, and it is
  its own PR with its own finding. Section 11, risk 1.

**Depends on**

- The node agent, for reading `/proc/mounts` on a named node. DATA-06 and
  DATA-08 are skipped, not failed, where it is unavailable.
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
| `hold` | path, start, len, mode, run-file, state-file | until granted, then until the run-file is removed | `launched` on the first pass |
| `try` | path, start, len, mode | no | `GRANTED` or `REFUSED pid=<n> type=<r\|w> start=<n> len=<n>` |
| `getlk` | path, start, len, mode | no | `FREE` or `HELD pid=<n> type=<r\|w> start=<n> len=<n>` |

Fields:

| Field | Type | Meaning |
|---|---|---|
| path | string | file on the share; created if absent, as the lock probe already does |
| start | int64 | `l_start`, absolute, `SEEK_SET` |
| len | int64 | `l_len`; 0 means "to end of file", which is what makes a whole-file `fcntl` lock expressible |
| mode | `read` or `write` | `F_RDLCK` or `F_WRLCK` |
| run-file, state-file | path on the pod's own filesystem, never the share | same convention as `hold-flock.sh` |

Exit codes: 0 for granted or free, 1 for refused or held, 2 for a usage or I/O
error. Refused and error are separated because a refusal is the expected result
in half these cases and an error never is.

`hold` writes `held`, `released` or `failed` into the state file, so
`framework.LockHolder` covers `flock` and `fcntl` holders with one shape and one
polling loop.

**Why `getlk` as well as `try`.** `F_GETLK` asks the server who holds the range
without taking anything. `F_SETLK` answers the same question by acquiring, which
changes the state every later attempt observes. CHAOS-06 asserts "still held by
the original holder" from the other end, and a probe that acquires cannot say
that.

### 4.2 The record set on the share

The durability sweep replaces "is the file non-empty" with a verdict per record.
A record is `<dir>/rec-<index>`, `RecordBytes` long, filled with a deterministic
byte pattern derived from the index, so any byte's expected value is computable
from its offset without a copy of the original.

| Verdict | Meaning | DATA-12 | DATA-13 |
|---|---|---|---|
| `correct` | full length, every byte as written | pass | pass |
| `absent` | no file, or zero length | **fail** | pass, recorded |
| `short` | a correct prefix, less than full length | **fail** | pass, recorded |
| `wrong` | any byte differs at an offset that was written | **fail** | **fail** |

DATA-12 fails on anything but `correct`, because the write was fsynced and the
server acknowledged it. DATA-13 fails only on `wrong`. That asymmetry is the
whole content of the pair, and it is why the sweep returns four verdicts rather
than a boolean.

The sweep runs in one exec for the whole set. A round trip per record would take
longer than the outage being measured, and would be running while the mount is
still recovering. It reports counts plus the index and offset of the first
`wrong` byte, which is what a filed defect needs.

### 4.3 The directory census

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

### 4.4 The fio job contract

DATA-14 runs one fio job per pod against a directory that pod owns alone.
Verification is fio's own: `verify=crc32c` with `verify_fatal=1`, so a mismatch
ends that job with a non-zero exit rather than being buried in a summary.

The harness consumes only what it asserts on, from `--output-format=json`:
`jobs[].error`, and the verify error counters. Throughput numbers are recorded
in the bundle and asserted on by nobody: DATA-14's expected result is zero
checksum mismatches, and a performance bound here would be a SCALE case wearing
a DATA number.

### 4.5 Ownership and evolution

Producer of all of the above is the harness; the only consumer is the case that
produced it plus the artifact bundle. Nothing is a system of record, and nothing
survives a run. The share is truth for file content; the pod's own filesystem is
truth for what the harness did and when, exactly as in phase 2.

Likely next additions, and what they cost:

- A third lock type once someone wants `F_OFD_SETLK`. Additive: a new mode
  token, no change to the output shape.
- A per-record timestamp in the sweep, if a durability failure ever needs to be
  placed against the fault timeline. Additive: a fifth field.
- A `locktool` subcommand for `O_DIRECT`, if section 5.6's probe shows the tools
  image cannot do it. That widens the binary past its name and is the reason it
  is an alternative rather than the plan.

What would force a breaking change: putting the locktool output into the
`OK|ERR <index> <epoch>` log format shared with the workload and the lock probe.
It nearly fits, and it would couple three parsers to one shape so that adding a
field to one breaks the other two. They stay separate.

---

## 5. Design and decisions

### 5.1 Why a binary at all

[decided: `fcntl` byte-range locks need one, and nothing on a stock image has them]

The busybox `flock` applet calls `flock(2)` and takes `-s`, `-x`, `-u` and `-n`.
`flock(2)` locks an open file description whole. There is no range argument
because the system call has no range. The util-linux `flock(1)` is the same
shape. Byte ranges are `fcntl(F_SETLK)` with a `struct flock`, and the only way
to reach that from a pod on an image that carries nothing but busybox is to put
a binary there.

Go's standard library exposes it directly (`syscall.FcntlFlock` with
`F_SETLK`, `F_SETLKW` and `F_GETLK`), so the binary needs no cgo and links
static by default. POSIX record locks are per-process rather than per-thread, so
the Go runtime's threads are not a hazard; the one hazard is that closing *any*
descriptor to the file drops the process's locks on it, which is why `hold`
opens once and holds that descriptor rather than reopening.

### 5.2 How locktool reaches a pod

[decided: built by `make locktool` per node architecture, streamed into the pod over pods/exec]

The alternative shape is an image: build it, push it to a registry the cluster
can pull from, name it with a flag, skip the cases without it. That is what
`fio` does, and it is right for `fio` because `fio` is not ours to build. It is
wrong here. A binary this repository owns, behind a registry the operator has to
populate, means DATA-06 and the byte-range half of DATA-05 never run anywhere.
A case that is skipped everywhere is a case that does not exist.

So: `make locktool` cross-compiles with `CGO_ENABLED=0` into
`bin/locktool-linux-<arch>` for `amd64` and `arm64`, about 1.5 MB each. At
runtime the harness reads the target node's architecture from
`node.Status.NodeInfo.Architecture`, streams the matching file into the pod over
`pods/exec` with stdin attached to `cat > /tmp/locktool`, and chmods it. This is
what `kubectl cp` does, so the path is well travelled: the exec stream is
binary-clean, and no encoding step is needed.

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

### 5.3 locktool's surface

[decided: three subcommands, and nothing that is not a lock]

`hold`, `try`, `getlk`. No `dio`, no `punch`, no `populate`. Each of those is a
real need in this phase and each is met by the image's own tools, probed. A
binary named for locks that also does direct I/O and hole punching is a
harness-specific busybox, and the next case adds a fourth thing to it.

The cost of this decision is section 5.6: if the tools image turns out not to
carry `oflag=direct`, DATA-07 reports blocked rather than falling back to
locktool. That is the failure mode this repository prefers, and section 11
carries it as the open item it is.

### 5.4 DATA-06: who the client is, and what one lease bounds

[decided: assert one lease as an upper bound, and record which mechanism released the lock]

The test plan's expected result is "lock released within one lease period; new
acquirer succeeds". Read literally against Kubernetes, that sentence has a
mistaken actor in it, and the case has to be written against the real one.

The NFS client is the node's kernel, not the pod. One `clientid` per server per
node, renewed by the node for as long as any mount of that server exists. When a
pod is force-deleted, kubelet SIGKILLs the container, the process dies, its
descriptors close, and the client sends `LOCKU` and `CLOSE` for the state that
process held. The lock is released in about a second and no lease expires,
because the *node* never stopped renewing. Lease expiry is the release path for
a node that stopped answering, which is CHAOS-03 and SEC-07, not this case.

So one lease period is the right bound and the wrong expectation. The case:

- asserts the range is re-acquirable, from a pod on another node, within one
  lease period (a new row in `pkg/slo`, not a literal in the case);
- records the observed interval and classifies it: a release inside a couple of
  poll intervals is the descriptor-close path, a release near the lease is
  expiry, and the difference says whether the node ever noticed the pod died.

A case that asserted expiry would fail on a healthy cluster. A case that
asserted promptness would fail on a cluster where kubelet was slow to kill, for
reasons that have nothing to do with NFS. The bound the plan states is the one
that holds under both, and the classification is what makes the result readable.

The re-acquirer is on another node on purpose: that request crosses the server,
which is the only place the two clients can be made to agree.

### 5.5 Force delete and F-001

[decided: the force-delete helper does not return until the node's mount is gone]

DATA-06 force-deletes a pod. Teardown then deletes the case's claims. That is
the exact ordering of F-001: the pod leaves the API before kubelet has
unmounted, the wait that follows returns immediately because the object is gone,
the claim is deleted, the provisioner destroys the export, and a node is left
retrying RPCs against an export that no longer exists, uninterruptibly.

`DeletePodNow` exists for cases that model a client that vanished, and its doc
comment already says it must never be used for teardown. That is necessary and
not sufficient: DATA-06 is not teardown, but teardown runs right after it.

So the phase adds a helper that force-deletes and then waits, through the node
agent, for the node's `/proc/mounts` to stop carrying a mount of that volume.
Cases use that helper; `DeletePodNow` stays as the primitive underneath it.
Consequences, stated so a reviewer can hold the phase to them:

- DATA-06 requires `Caps.NodeAgent` and **skips** without it. Force-deleting a
  mounted pod with no way to observe the unmount is the hazard, not the case.
- The wait is bounded and its expiry is a failure of the case, loudly, naming
  the node and the mount. A node that has not unmounted is the state F-001 says
  to treat as dangerous rather than wait out.

This is the "uglier path" F-001's follow-up section asks for, approached from
the safe side: the same sequence, with an observation between the two steps.

### 5.6 DATA-07: `O_DIRECT` comes from the image, and is probed

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
Rejected for now under section 5.3, revisited in section 11 once a real image
has been probed.

### 5.7 DATA-08: one export, two mounts, and the option read back

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

### 5.8 DATA-09: silly rename is a property of one client

[decided: two halves, same node and cross node, asserting different things]

The Linux client implements unlink-of-an-open-file by renaming it to
`.nfsXXXXXXXX` and deleting it when the last descriptor closes. That is a client
behavior, and it can only happen when the client doing the unlinking is the
client holding the file open. When another node unlinks it, the server removes
it, and the holder's next operation gets `ESTALE`. Both outcomes are correct.
A case that ran the cross-node shape and asserted the same-node expectation
would file an NFS-conformant client as a defect.

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

### 5.9 DATA-10: assert on names, record counts

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

Two practical points. The listing streams: `find <dir> -maxdepth 1`, not `ls`,
because busybox `ls` sorts, and sorting 100k entries in a pod with no memory
limit set is a way to discover the node's OOM killer. And population is
inode-hungry: the case checks free inodes and space before it starts and reports
**blocked** if the export cannot hold the set, rather than failing on `ENOSPC`
halfway through and pointing at the server.

### 5.10 DATA-11: sparse yes, hole punch no, and say which

[decided: assert sparse write and read-back; record the punch as unsupported]

Hole punching on this suite's mounts is unavailable twice over, and both are
worth stating because a reader will otherwise assume the case is weak:

- NFSv4.1 has no operation for it. `ALLOCATE`, `DEALLOCATE` and `READ_PLUS`
  arrived in NFSv4.2 (RFC 7862 section 4). Preflight pins `vers=4.1` and fails
  on anything else, so `fallocate -p` on these mounts returns `EOPNOTSUPP` from
  the client before any request reaches the server.
- The busybox `fallocate` applet parses `-l` and `-o` only. `-p` appears in its
  own usage comment and not in its option string, so on a stock image the flag
  is rejected by the tool regardless of the mount.

What the case asserts on 4.1: a file written sparsely (`dd` with `seek`) reads
back zeros in the hole, the byte written past the hole is at the right offset,
`stat` reports the full logical size, and allocated blocks are recorded rather
than asserted, since whether the backing filesystem stores the hole sparsely is
not an NFS property.

What it does with the punch: attempts it, and records the refusal with the
reason. It fails only if the punch *reports success* and the region is not zero
afterwards, or the reported size changes. That is the assertion that survives
the day preflight accepts 4.2, and on that day the case starts asserting the
punch without being rewritten.

### 5.11 DATA-12 and DATA-13: one workload, two verdict tables

[decided: the sweep returns four verdicts; DATA-12 fails on three of them, DATA-13 on one]

Both cases are: run the record workload, kill the server under it, bring it
back, sweep from a pod on another node. They differ in one flag on the workload
(whether each record is written with `conv=fsync`) and in which verdicts fail.
Section 4.2 has the table.

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

### 5.12 DATA-14: fio, partitioned by pod, capacity checked first

[decided: one job per pod over its own directory, `verify=crc32c`, `verify_fatal=1`]

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
and wrong for locktool (section 5.2) for one reason: `fio` is not ours, its
image is a packaging decision of whoever runs the suite, and a soak case that is
skipped in the fast path costs nothing on the days nobody runs the soak.

### 5.13 The nightly gate

[decided: a `TestNightly` prefix and a `make test-nightly`, and the existing gate-N cases move separately]

The suite has three gates in a `go test` pattern: chaos, soak, and everything
else. The plan has four; gate N has had nowhere to go, so every nightly case so
far sits in the fast gate. That was free while nightly meant a snapshot round
trip. It stops being free here: DATA-10 alone populates 100k entries, and the
presubmit budget is 15 minutes on two nodes.

So the prefix arrives with the cases that need it, which is the repository's
rule about not building gates ahead of a case that uses them. DATA-06 through
DATA-11 are `TestNightly...`, DATA-12 and DATA-13 are `TestChaos...` because
they injure the server and that prefix outranks the others, DATA-14 is
`TestSoak...`. `test-presubmit` skips all three prefixes.

The gate-N cases already in the repository keep their names in this phase.
Renaming them is mechanical, it touches files this phase otherwise does not, and
a rename does not travel with a behavior change. It is a follow-up PR whose
whole diff is names and two table rows.

### 5.14 What DATA-05 and CHAOS-06 gain

[decided: extend both in the last PR of the phase, once locktool has run on a real cluster]

Phase 3 landed CHAOS-06 on whole-file locks and said why: a whole-file lock
reaches an NFSv4 server as a lock over the whole range and is reclaimed by the
same mechanism, so reclaim, exclusivity and the grace bar were all assertable
without ranges. What was not assertable is that two clients can hold *different*
parts of one file, which is the thing a byte-range lock is for.

- **DATA-05** gains: two pods on two nodes take disjoint ranges of one file and
  both are granted; each is then refused on the other's range; a range that
  overlaps is refused. The existing `flock` half stays as it is.
- **CHAOS-06** gains: the disjoint-range shape held across a failover. After it,
  each holder still holds its own range by `F_GETLK` from the other side, and a
  third client is refused on both.

Both land last, after locktool has been exercised by DATA-06 against a real
cluster. Extending a passing chaos case with a tool nobody has run yet is how a
green case becomes a flaky one.

---

## 6. Delivery phases

**MVP**: locktool, its delivery path, and DATA-06. The smallest slice that can
merge and run: a binary, a `make` target, the copy-and-verify path, one lock
holder shape covering `fcntl`, the force-delete-and-await-unmount helper, one
new `pkg/slo` row, and one case that uses all of it.

Validated outside a test harness by running the nightly gate against a real
cluster and reading three things out of it: a byte-range lock granted on a named
node, the interval between the force delete and the re-acquisition, and the
classification of which mechanism released it. A run that reports blocked is not
a validation, and the reason it reported blocked is the finding.

Later PRs, intent only:

1. **Protocol edge cases**: DATA-07, DATA-08, DATA-09, DATA-10, DATA-11, with
   the clone volume, the NFS-source extraction and its unit tests, the directory
   helpers, and the tool probes.
2. **Durability**: DATA-12 and DATA-13, with the content-verifying sweep
   replacing the existence check, which also upgrades CHAOS-01 and CHAOS-02.
3. **Soak and ranges**: DATA-14 behind `-fio-image`, then the DATA-05 and
   CHAOS-06 range extensions.

Later PRs sharpen once the MVP ships. In particular, which of the tool probes
actually fire on a real image decides whether section 5.3 holds or whether
locktool grows a `dio` subcommand.

---

## 7. Configuration

Source: command line flags, passed through the make targets, as in phases 2
and 3.

| Flag | Default | Needed for |
|---|---|---|
| `-fio-image` | empty | DATA-14 only. Empty skips it. No default image: a suite that pulls an image nobody named is a supply chain the operator did not agree to |
| `-tools-image` | unchanged | DATA-07 and DATA-11 probe what this image's `dd` and `fallocate` support |
| everything from phases 2 and 3 | unchanged | the fault, the target, the profile, the grace wording |

Validation and behavior on bad config: fail loud, and distinguish the three
answers. A missing `-fio-image` is a skip, because the case cannot run. An image
whose `dd` has no `oflag=direct` is **blocked**, because the case could run on
a different image. A PV whose server and path cannot be read is **blocked**, and
says which fields were missing. None of the three is a pass.

No flag for the locktool path, its architecture, or where it lands in the pod.
The path is derived from the repository root, which `FinalizeFlags` already
resolves for the artifacts directory; the architecture is read from the node;
the destination is fixed. A flag for any of them would be a flag for a value the
cluster can answer.

Intentionally not configurable: the record size and pattern, the directory entry
count, the soak duration, the number of fio jobs, and the lock ranges. Each is a
property of the measurement, and each is stated in the plan. See section 11 for
the soak duration, which is the one of these a real run may argue with.

Credentials policy: unchanged, none. The suite authenticates with a kubeconfig
and holds nothing else.

---

## 8. Deployment decisions

Runtime unit: unchanged. `go test` on a workstation, driving pods.

One new build artifact. `make locktool` produces `bin/locktool-linux-amd64` and
`bin/locktool-linux-arm64` with `CGO_ENABLED=0`, so they are static and run on
any Linux image including a distroless or a busybox one. `make all` gains it, so
a contributor who never touches a cluster still compiles it, and `bin/` is
git-ignored.

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
here (`/proc/mounts` and `dmesg`).

Cost visibility: DATA-14 is the first case that asks for tens of gigabytes and
an hour. It is weekly, it is skipped without an image, and it reports blocked
rather than provisioning storage it cannot fill.

---

## 9. Observability

The artifact bundle gains four things, written before teardown as everything
else is:

| Artifact | Written by | What it answers |
|---|---|---|
| the verdict table | DATA-12, DATA-13, CHAOS-01, CHAOS-02 | which records were correct, absent, short or wrong, and the first wrong offset |
| the mount line | DATA-08 | whether the reader's mount actually carried `noac`, in the driver's own words |
| the directory census | DATA-10 | created, listed, deleted, and every unknown name verbatim |
| the fio job output | DATA-14 | per-job error and verify counters, and the throughput nobody asserts on |

Log fields on the case's own output stay as they are: the case ID, the node, the
pod, the profile. Two additions, both because a failure without them is not
triageable:

- DATA-06 logs the observed release interval and its classification, not just a
  pass. A run where every release is at the lease boundary is a different
  cluster from one where every release is prompt, and the pass looks identical.
- Every blocked report names the probe that failed and the flag that would fix
  it. "blocked" with no reason is the failure mode this phase adds the most
  opportunities for.

Export path: unchanged, `artifacts/<run-id>/<CASE-ID>/`.

---

## 10. Alternatives considered

| Option | What it is | Why not chosen |
|---|---|---|
| locktool as a published image | Build and push an image, name it with a flag, skip without it | A binary this repository owns, gated on a registry the operator must populate, means the byte-range cases never run anywhere |
| locktool embedded with `go:embed` | Commit the compiled binaries and embed them in the test binary | Committed binaries cannot be reviewed, and the repository would carry two per release |
| Compile locktool inside the pod | Ship the source and build it there | No toolchain on a busybox image, and adding one makes the image the thing under test |
| A second StorageClass with `noac` | Clone the discovered class with an extra mount option and provision from it | Gives a different export. DATA-08 needs two mounts of the *same* file |
| Per-pod mount options | Set `noac` on the reader pod | Mount options belong to the PV. Kubernetes has no per-pod override |
| A `dio` subcommand on locktool | Do DATA-07's direct I/O from our own binary | Widens a lock tool into a general one; held as the fallback if a real image's `dd` lacks the flag |
| Asserting DATA-13 loses data | Fail if the un-fsynced records survived | A SIGKILL of the process does not drop the host page cache, so surviving is normal and the assertion would fail on healthy systems |
| Asserting DATA-10's entry count | Compare listed against created minus deleted | A listing racing deletes is allowed to miss entries; the count would fail on lawful behavior |
| Folding the new gate-N cases into presubmit | Keep two prefixes, let nightly cases run in the fast gate | DATA-10 alone does not fit a 15 minute budget, and red in the fast path trains people to ignore red |

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
2. **Whether the tools image's `dd` carries `direct`.** It is behind
   `FEATURE_DD_IBS_OBS` in busybox. **Decided by** the probe in section 5.6 on
   the first real run. If it is absent, section 5.3 is revisited and locktool
   grows a `dio` subcommand rather than DATA-07 being permanently blocked.
3. **Whether 100k entries fit.** The default claim is 1Gi and the export may be
   directory-backed with no per-volume quota, so the real limits are the backing
   filesystem's free inodes. **Decided by** the precheck in section 5.9 on a
   real cluster. If the honest answer is that 100k is out of reach there, the
   number moves in the test plan, not in the case.
4. **DATA-14's one hour is a constant.** Fixed values stay fixed until something
   needs them to vary, so there is no duration flag, which means exercising the
   fio path costs an hour. **Decided by** the first attempt to debug it: if that
   hour is spent twice, the flag has earned its place and gets a README row.
5. **The clone PV's reclaim policy.** It is `Retain` and the fixture deletes the
   object, because nothing provisioned it. If a driver treats an unknown static
   PV over a managed export as something to reconcile, the clone could disturb
   the dynamic claim that owns the export. **Decided by** watching the dynamic
   PV's phase across DATA-08 on a real cluster. Unresolved, and it is the reason
   DATA-08 is not in the MVP.
6. **Sizing risk on the nodes.** F-002 is 2GB workers rebooting under a 1MiB
   write and a page cache. DATA-10 and DATA-14 are heavier than anything the
   suite has run. A case that reboots a node reports a storage defect that is a
   node pool defect. Mitigation is the README's existing minimum of 8GB per
   worker for anything past presubmit, and the fact that both cases are outside
   the fast gate; there is no mitigation inside the case.

---

## 12. Verification

One check per rule in section 2, observable from a run.

| Rule | Check |
|---|---|
| 1. The client is the node | DATA-06 logs the release interval and its classification; a prompt release on a healthy node is the expected record, and the case passes either way within the bound |
| 2. No silent weakening | `locktool try` on an overlapping range is refused while a disjoint range on the same file is granted, in DATA-05's new half. A whole-file fallback would grant neither or both |
| 3. Options are read back | DATA-08 fails, naming the node and the option, when `/proc/mounts` on the reader's node does not carry `noac`. Unit test: the option check rejects an option list that lacks it |
| 4. Force delete waits for the unmount | DATA-06 skips without the node agent; with it, the mount is absent from the node before the case returns. Unit test: the wait treats a mount still present at the deadline as an error, not as done |
| 5. Uncommitted absence is not failure | DATA-13 passes with `absent` and `short` counts greater than zero, and the counts appear in the bundle |
| 6. A wrong byte always fails | Unit test on the sweep parser: `correct`, `absent`, `short` and `wrong` verdicts from recorded tool output, with only `wrong` failing DATA-13 and three of four failing DATA-12 |
| 7. Missing tools report blocked | DATA-07 against an image without `oflag=direct` reports blocked naming `-tools-image`, not a pass and not a failure. Same for DATA-14 without `-fio-image`, which skips |
| 8. Undefined operations are recorded | DATA-11 on a `vers=4.1` mount records the punch refusal and passes on the sparse assertions |
| 9. Listings may miss, never invent | DATA-10 passes with `listed` below `created`, and fails on any name outside `e-<index>`. Unit test: the census classifier flags a `.nfs*` leftover and a truncated name as unknown |
| 10. Verified by content | The existence check is gone from the durability path; CHAOS-01 and CHAOS-02 assert content after this phase, which is visible as a verdict table in their bundles |
| 11. Load partitions by file | DATA-14's job file names one directory per pod; a shared directory would be visible in the job output the bundle carries |
| 12. Phases 2 and 3 still hold | The chaos gate passes unchanged before the DATA-05 and CHAOS-06 extensions land, and again after |

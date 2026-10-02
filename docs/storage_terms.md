# Storage terms

Author: mikebz@
Created: 2026-10-01
Updated: 2026-10-02

The one place a term used across this repository is defined. Every other
document links here rather than restating a definition, so that a correction
lands once and the documents cannot drift apart on what a word means.

Each entry says what the word means here, where that meaning comes from, and,
where a word is read two ways, which reading the documents intend. Sources
follow the order of authority in [`AGENTS.md`](../AGENTS.md): the protocol,
then the Linux client and server, then Kubernetes, then the deployment in front
of us. A statement that holds for one implementation says so.

To add a term: define it here, with its source, and link it from its first use
in each document that uses it. A term that another document defines moves here,
and that document links it instead.

---

## Protocol and client

### Lease

How long a server keeps a client's state without hearing from it. Each client
ID has a single lease with each server, and every successful `SEQUENCE`
renews it, so any request on a session renews the lease and an idle client
sends a `SEQUENCE` on its own ([RFC 8881 Section 8.3][rfc-8.3]). When a lease
expires, the server may discard that client's opens and locks.

The Linux client holds one lease per server, shared by every mount on the node
and so by every pod on the node ([client-identifier][kernel-clid]). When nothing
else has renewed it, the client renews about two thirds of a lease after the
last renewal (`nfs4_schedule_state_renewal` in
[`fs/nfs/nfs4renewd.c`][linux-renewd]).

What follows from that here:

- The lease belongs to the node, not the pod. See [client
  identity](#client-identity).
- `slo.LockReleaseBound` is one lease. A lock whose holder vanished without
  closing it is released when the holder's lease expires, and no later. A pod
  that dies closes its descriptors and the lock goes in about a second; a node
  that dies closes nothing, and its locks wait out the lease. The bound holds
  under both, which is why DATA-06 asserts it rather than either timing. The
  full reasoning is the comment on `LockReleaseBound` in
  [`pkg/slo/slo.go`](../pkg/slo/slo.go).
- The lease length is half of a [profile](#profile).

### Grace period

The interval after a server restart in which the server accepts reclaims of
state that existed before the restart and refuses new state, so that no client
is granted a lock that another client is about to reclaim. During it the server
refuses new `OPEN` and `LOCK` requests, and any `READ` or `WRITE` it cannot
prove safe, with `NFS4ERR_GRACE` ([RFC 8881 Section 8.4.2.1][rfc-8.4.2.1]).

One failover, in order:

| # | Event | Server | What a client pod sees |
|---|---|---|---|
| 1 | Fault | The serving process is killed, or its pod deleted | I/O in flight stops |
| 2 | Server down | Nothing answers | I/O [blocks](#blocked-and-blocks), because the mount is [hard](#hard-mount) |
| 3 | Restart | A new process starts with no state in memory, and reads the [recovery store](#recovery-store) | Still blocked |
| 4 | Grace starts | Ganesha writes `NFS Server Now IN GRACE, duration 90` to its own log file ([F-022](findings.md), [F-028](findings.md)) | Still blocked. The client learns of the restart when a request fails, then sets up a new client ID and session |
| 5 | Reclaims | [Reclaims](#reclaim) are granted; new state is refused with `NFS4ERR_GRACE` | New opens and locks wait in the kernel |
| 6 | `RECLAIM_COMPLETE` | Each client sends one once it has reclaimed everything | No change |
| 7 | Grace ends | On its timer, or [early](#reclaim_complete-and-early-end-of-grace) | The waiting requests are granted |
| 8 | Normal service | New state is granted; reclaims are refused with `NFS4ERR_NO_GRACE` | I/O resumes. A client that reclaims now loses its locks ([F-028](findings.md)) |

**Measured recovery is restart plus grace.** The README's [How a failover is
measured](../README.md#how-a-failover-is-measured) times recovery from step 1 to
the first write committed after the silence. The chaos workload writes each
record to a new file (`pkg/framework/scripts/write-load.sh`), so each write
needs a new `OPEN`, which is new state and waits for step 7. Neither reference
deployment has ever ended grace early, so every recovery measured on them so far
is the restart time, plus the whole configured grace period
([F-029](findings.md), which has the numbers), plus however long the client
waits after grace ends before it tries again: 1 to 28 seconds across one
CHAOS-05 run ([F-031](findings.md)). This is why recovery targets are
set per [profile](#profile) in test plan Section 3.8.

How the suite observes grace, from the server's log stream and the log files
its serving process writes, is the README's
[How grace is observed](../README.md#how-grace-is-observed).

### Reclaim

A client re-establishing, after a server restart, an open or a lock it held
before it: a `LOCK` with `reclaim` set, or an `OPEN` with claim type
`CLAIM_PREVIOUS` ([RFC 8881 Section 8.4.2.1][rfc-8.4.2.1]).

- **During grace**, reclaims are granted, and new state is refused with
  `NFS4ERR_GRACE`.
- **After grace**, or after the client's own `RECLAIM_COMPLETE`, a reclaim is
  refused with `NFS4ERR_NO_GRACE` ([RFC 8881 Section
  15.1.9.3][rfc-15.1.9.3]), and the lock is free for another client to take.
  That is lawful, and it is what [F-028](findings.md) met: a client whose
  reclaim arrived after grace ended lost both its locks.

The Linux client retries `NFS4ERR_GRACE` inside the kernel rather than returning
it to the application (`nfs4_handle_exception` in
[`fs/nfs/nfs4proc.c`][linux-nfs4proc]; RFC 8881 Section 8.4.2.1 asks clients to
retry). So a lock attempted during grace is seen from the pod as a call that
[blocks](#blocked-and-blocks) for a long time and then succeeds, not as a run of
refusals. That is why CHAOS-07 asserts on grants rather than on refusals.

When reclaims are refused, the client logs `NFS: <server>: lost N locks` to the
node's kernel log (`nfs4_do_reclaim` in
[`fs/nfs/nfs4state.c`][linux-nfs4state]). The count is per node and server,
shared by every pod on the node, names no lock, and counts a refusal for any
reason, including a reclaim that conflicts with a lock already granted to
someone else. It proves that the node had reclaims refused, not which locks or
why ([F-028](findings.md)).

Not the Kubernetes reclaim policy, `Delete` or `Retain`, which decides what
happens to a PV after its claim is deleted (PROV-01, PROV-06; [persistent
volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#reclaiming)).

### RECLAIM_COMPLETE and early end of grace

`RECLAIM_COMPLETE` is how a client tells the server it has reclaimed everything
it will. A client with a new client ID must send one before its first
non-reclaim lock, even with nothing to reclaim; after it, that client's reclaims
are refused with `NFS4ERR_NO_GRACE` ([RFC 8881 Section 18.51][rfc-18.51]).

The protocol lets grace last until every client that may have held state has
sent `RECLAIM_COMPLETE`, lets the server end it sooner, and says it SHOULD NOT
end before one lease period has passed; after a restart, grace must be at least
as long as the previous instance's lease ([RFC 8881 Section
8.4.2.1][rfc-8.4.2.1]). Beyond that, **the length of grace and whether it ends
early are implementation choices**:

- **NFS-Ganesha** runs grace for `Grace_Period` seconds. Both reference
  deployments set 90. [F-029](findings.md) found neither ending early, for two
  reasons: `gke-w1`'s 4.0.8 holds grace for its full length even when every
  client it knows has reclaimed, and `gke-w2`'s 15.3 would end early but waits
  on a record for a client that no longer exists.
- **Linux knfsd** runs grace for its configured grace time (90s by default).
  With client tracking through `nfsdcld`, it skips grace when no client could
  reclaim, and ends grace as soon as every client in its reclaim table has sent
  `RECLAIM_COMPLETE` (`inc_reclaim_complete`). When the timer runs out while
  clients are still reclaiming, it extends grace, up to two lease periods after
  the server started (`clients_still_reclaiming`). All three are in
  [`fs/nfsd/nfs4state.c`][linux-nfsd-state].

**Why grace exit is bounded at two lease periods.** Test plan Section 3.8 holds
grace exit to 2 × lease, as `slo.GraceExitBound`, and OBS-03 fails above it.
The bound is kept, for these reasons:

- It is not a protocol bound. RFC 8881 sets a floor on grace (one lease) and no
  ceiling.
- It is the ceiling knfsd puts on its own extension. A bound at the configured
  grace would fail a lawful knfsd extension for slow reclaimers.
- Both profiles pin grace at 1.5 × lease (30s on 20s, 90s on 60s), so a server
  that honours its configured grace ends half a lease inside the bound.

What it does not catch: a server that ignores its configured grace but stays
under two leases passes. OBS-03 logs the observed window next to the configured
grace, so that case is visible in the log but not asserted.

### Recovery store

**This is the one name for it.** It is what the server keeps on disk so that,
after a restart, it knows which clients may reclaim. RFC 8881 requires the
server to keep "in stable storage a list of clients that may have" locks, so
that it can tell when all of them have finished reclaiming ([Section
8.4.2.1][rfc-8.4.2.1]). The documents have called it the recovery state path,
recovery backend, recovery state store, recovery state directory and stable
storage; each of those means this, except where "stable storage" means committed
file data, as in [`COMMIT`](#close-to-open).

- **Where it is** depends on the implementation. Ganesha on both reference
  deployments keeps it in `/export/v4recov/` on the export's own claim, one
  directory per client, so it outlives the server pod ([F-029](findings.md)).
  knfsd keeps it through the `nfsdcld` daemon, or in the older
  `/var/lib/nfs/v4recovery` directory
  ([`fs/nfsd/nfs4recover.c`][linux-nfsd-recover]).
- **What preflight records** as `recoveryStateBackend` in `environment.json`
  is the kinds of volume the server pod mounts (claim, `emptyDir`, `hostPath`),
  not which of them holds the store. It is recorded for triage, never asserted
  (test plan Section 0).
- **A stale record keeps grace from ending early.** A server that ends grace
  once every client in its store has sent `RECLAIM_COMPLETE` waits forever for
  a client that will never return. Nothing prunes a record for a node that was
  replaced, so on `gke-w2` every grace period runs to its timer
  ([F-029](findings.md)).
- CHAOS-17 is the case about losing or corrupting it. It is not written, and
  needs `-recovery-state-path`, a flag that does not exist yet, because there
  is no portable way to find the store.

### Client identity

**The NFS client is the node, not the pod.** The mount is made by the node's
kernel in the host network namespace, so everything the server can see, the
source address, the reserved source port and the NFSv4 client ID, belongs to
the node ([F-019](findings.md)). The Linux client builds its client ID from
`Linux NFS` and the node name, and holds one [lease](#lease) per server for
every mount on the node ([client-identifier][kernel-clid]). The client records
in `gke-w2`'s recovery store show exactly that: each is named `Linux NFSv4.1`
and a node name ([F-029](findings.md)).

So two pods on one node are one client to the server: one lease, one set of
state, one cache. Cases written around this:

- DATA-06: a lock held by a force-deleted pod is released when its descriptors
  close or its node's lease expires.
- SEC-04: one node losing its mount must not discard another node's state,
  which only holds if two nodes are two clients.
- SEC-07: two pods sharing one identity after a restart.
- [F-016](findings.md) and [#16](https://github.com/mikebz/nfs-verification/issues/16):
  every DATA-02 appender that lost its records shared its node with another pod
  on the share, and the reader shares a node, and so a client cache, with an
  appender.

See [client and client pod](#client-and-client-pod) for how the documents use
the word.

### Hard mount

A mount on which a request that gets no answer is retried indefinitely, instead
of failing after `retrans` retransmissions and returning an error to the
application ([`nfs(5)`][nfs5], `soft` and `hard`). Preflight requires it, and
it is why a chaos case asserts zero I/O errors: on a hard mount, a server
outage is a wait, and an error is a violation of the mount's contract rather
than a slow recovery.

The same property is the hazard in [F-001](findings.md). If the export goes away
while a node still mounts it, the node's I/O retries forever, uninterruptibly,
and the node is out of service until it is rebooted. That is why teardown waits
for pods to leave the API before touching a claim.

### Close-to-open

The consistency the NFS client gives between two clients on different nodes:
what one client writes and closes, another sees if it opens the file afterwards,
and nothing stronger. [RFC 8881 Section 10.3.1][rfc-10.3.1] states the two
rules behind it: a client flushes modified data, committed on the server, before
`CLOSE`, and revalidates its cache against the server's change attribute after
`OPEN`. The RFC requires the revalidation only for an `OPEN` that denies writes
to others. The Linux client checks with the server on every open, whatever it
has cached, flushes on every close, and calls the pair close-to-open cache
consistency; the `nocto` mount option turns it off ([`nfs(5)`][nfs5], "Data and
metadata coherence"). What DATA-03 asserts is therefore the Linux client's
behaviour, on top of the protocol's.

Committed means acknowledged by `COMMIT` ([RFC 8881 Section 18.3][rfc-18.3]),
which is what `fsync` sends and what post-fsync durability means in DATA-12.

Cases:

- DATA-03 asserts the guarantee: B sees what A wrote and closed.
- DATA-04 asserts its absence while A holds the file open: B may see stale data,
  and that is not a failure.
- DATA-08 mounts with `noac`, which turns off attribute caching, and asserts
  visibility without a close, to show the difference is the cache.
- [F-016](findings.md): appenders that share a node share a cache, so
  close-to-open says nothing about what they see of each other.

---

## Terms the test plan uses

### GDC

Google Distributed Cloud, Google's Kubernetes platform for on-premises and
edge hardware ([Google Distributed
Cloud](https://cloud.google.com/distributed-cloud)). Test plan Section 1 names
it for one supported topology: three nodes that each serve the Kubernetes API
and the workloads. The suite does not depend on it; it counts nodes it can
schedule on ([schedulable node](#schedulable-node-and-worker)).

### CNode, DNode

The plan does not expand them. Read here as **CNode**, a node that runs the NFS
server, and **DNode**, a node that holds the storage behind it; "colocated", in
Section 1 and CHAOS-14, means both roles on the nodes that also run the
workloads, so a client and its server can share a node's memory. The same
names are VAST Data's, for nodes that run its protocol servers and nodes that
hold its drives, with colocation as one packaging ([VAST
Data](https://www.vastdata.com/blog/mixing-cnodes-eboxes-power-future-ai-infrastructure)).
**Open:** whether the plan took the terms from VAST or meant them generically.

### SDS

Software-defined storage: the layer that provides the backing block volume under
the NFS server, and replicates it where it does (test plan Section 2.1). The
plan treats it as a black box; CHAOS-13 is the one case aimed at it.

### Archetypes A to D

A classification of how an RWX volume is built, from which the plan picks one
(Section 2.2). Only A is defined in the plan, and the rest of the taxonomy is
not in the repository:

- **A**, the architecture under test: a userspace NFS server exports a
  filesystem on a block volume attached to one node.
- **B** contributes one trait the plan adopts: the server is packaged and
  versioned apart from the CSI driver, which is what enables the SKEW cases.
- **C and D** are what the plan excludes: architectures with a distributed lock
  manager, a cluster filesystem, or several servers that clients could see
  disagree.

**Open:** what B, C and D denote in full, and which is which of C and D.

### Fast gate, presubmit gate

No gate exists in the repository. The words mean the set of cases a change must
pass before it merges, and nothing runs cases before a merge today: CI runs
`make check-fmt`, `vet`, `unit`, `build` and the `locktool` cross-compile, and
no case. Where the plan (DATA-02) or a finding ([F-001](findings.md),
[F-002](findings.md)) uses the words, read them as "the cases every change is
expected to pass". DATA-02's open question, whether its exact count belongs
there, stays open under that reading.

---

## Harness vocabulary

### Passed, failed, blocked, skipped

The four results a case reports are defined once, in the README, at the end of
its [Documentation](../README.md#documentation) section. What is not said there
is how each looks in `go test` output:

- **Passed** and **failed** are `--- PASS` and `--- FAIL`.
- **Blocked** and **skipped** both print `--- SKIP`, because both are
  `t.Skip`. A blocked case's message starts with `blocked: `, written either
  directly as `t.Skipf("blocked: …")` or through the `blocked` helper in
  `test/e2e/helpers_test.go`. A case skipped by capability starts with
  `capability unavailable: `, from `requireCap`. Read the message, not the
  status, to tell them apart; neither is a pass.

### Blocked and blocks

Two unrelated meanings:

- **Blocked**, the [result](#passed-failed-blocked-skipped): a case that could
  not run here, for a reason the cluster or the image gives it. "CHAOS-07
  reports blocked", "blocked on cluster configuration".
- **Blocks**, what a call does on a [hard mount](#hard-mount): it waits in the
  kernel until the server answers. "Clients block, then recover", "an attempt
  that blocks is not an attempt that failed".

A result is always about a case. Blocking is always about a call, a client or
I/O.

### Recorded and asserted

- **Asserted**: the observation decides the case's result.
- **Recorded**: the observation is written down, in the case log,
  `environment.json` or the evidence, and cannot change the result.

"Recorded, not asserted" says a case looked, kept what it saw, and deliberately
does not judge it, usually because nothing states what the value should be.
DATA-11's hole punch on a 4.1 mount, SEC-08's confidentiality, preflight's
mount propagation and recovery store, and PROV-10's export name are all
recorded. A recorded value can still be a finding. It is never a pass.

### Client and client pod

- **Client pod**: a pod the suite runs I/O in. Cases name them by role:
  writer, verifier, probe, holder.
- **Client**, or NFS client: the kernel NFS client on a node, which is what the
  server sees ([client identity](#client-identity)).

The plan and the chaos cases often say "client" for a client pod. That is safe
only while the pods are on different nodes, which the cases that say it arrange:
CHAOS-06's "two clients" are two pods on two nodes, and so two NFS clients as
well. Where two pods share a node, the difference decides the result (DATA-06,
SEC-04, SEC-07, [F-016](findings.md)).

### Record

Four meanings:

- **Workload record**: one unit of what a workload writes to the share. The
  chaos workload writes each as its own 4KiB file `rec-<index>` and logs one
  `OK` or `ERR` line per attempt to the **record stream**, on the pod's own
  filesystem (`pkg/framework/scripts/write-load.sh`). DATA-02's records are
  fixed-format lines appended to one shared file.
- **Environment record**: `artifacts/<run-id>/environment.json`, what preflight
  discovered about the cluster (`pkg/env`).
- **Client record**: a client's entry in the [recovery store](#recovery-store).
- **Recorded**: see [recorded and asserted](#recorded-and-asserted).

### Failover

Any event after which clients are served by a different server process than
before, whatever the cause and wherever the new one runs. It includes a
restart in place, where the supervisor respawns the killed process in the same
container (CHAOS-01, DATA-12, DATA-13), and a pod delete, where a controller
starts a new pod (CHAOS-02, CHAOS-05, CHAOS-06, CHAOS-07). How the deployment
fails over is a black box (test plan Section 1); every failover case asserts
only what a client sees. A failover is what starts a [grace
period](#grace-period).

### Serving process and supervisor

- **Serving process**: the process holding the listening socket on port 2049,
  the one process that identifies an NFS server without reference to any
  implementation. On both reference deployments it is `ganesha.nfsd`.
- **Supervisor**: the container's PID 1, which starts the serving process and
  respawns it when it dies. On both reference deployments it is
  `nfs-provisioner`.

A kill aimed at the supervisor measures a container restart, not an NFS
failover, and the container's restart count and termination state never see
the serving process die, because the supervisor survives it
([F-026](findings.md), [F-027](findings.md)). [#25], [#97] and [#98] each came
from reading a container-level signal as though it described the serving
process.

[#25]: https://github.com/mikebz/nfs-verification/issues/25
[#97]: https://github.com/mikebz/nfs-verification/issues/97
[#98]: https://github.com/mikebz/nfs-verification/issues/98

### Step, phase, category, section, test group, delivery group

Overlapping names for pieces of the plan and of the work. In new text, use
**category** for a set of cases, **step** for a unit of delivery, and
**Section** for a part of the test plan. The others are read as:

| Word | Means | Defined by |
|---|---|---|
| Category | A case ID prefix (`PROV`, `DATA`, `CHAOS`, `SCALE`, `OBS`, `SEC`, `SKEW`), with its test file, `make test-<category>` target and `Test<Category>` prefix | Test plan Sections 3 and 4.2 |
| Section | A numbered part of the test plan. Sections 3.1 to 3.7 each hold one category's case table | The test plan |
| Step | One delivery step: one pull request, or a short run of them, numbered 1 to 11 with 2b, independently of the documents | Test plan Section 5.3 |
| Test group | A category, as the design docs call it | Design doc headers |
| Delivery group | The cases one design doc serves: usually one category, but doc 04 served cases from two | The README's design doc table |
| Phase | The work one design doc describes, which is one or more steps. "This phase" in a design doc means that doc's steps | Design docs, `AGENTS.md` |

"Phase" also has its Kubernetes meaning, an object's `status.phase`: a claim
`Pending` or `Bound`, a PV `Released`. Where it is followed by a phase name, it
means that.

### Schedulable node and worker

A **schedulable node** is one a test pod can land on: Ready, not cordoned, and
carrying no taint a test pod would have to tolerate. It is the only kind of node
the suite counts, and it never reads a role label (test plan Section 4.1).
**Worker**, in docs 02, 03, 05 and 07, predates that rule and means schedulable
node. It carries no role meaning anywhere in the suite, and a control-plane node
that accepts workloads counts.

### Fan-out

How many NFS server pods serve the cluster's RWX volumes, and which exports each
serves (test plan Section 0). An export set is the exports one server pod serves,
and the server is a single point of failure for its set: with a fan-out of one,
one server failure takes every RWX volume in the cluster (Section 2.3).
Preflight records it. The reference provisioner refuses to provision when its
Service has more than one endpoint, so its fan-out is one (Section 4.1).
SCALE-06 is enabled only above one.

### Bundle and evidence

- **Bundle**: `artifacts/<run-id>/<CASE-ID>/` for a failed case: pod logs,
  Kubernetes Events, `/proc/mounts` and dmesg from every involved node, and the
  fault timeline, indexed by `artifacts.txt`. Written before teardown, on a
  failure only.
- **Evidence**: the files a case names as what it argues from, the workload's
  record stream and the file on the share a data case is about. Copied into the
  same directory before teardown **on a pass as well as a failure**, capped per
  file, and indexed by `evidence.txt`.

Both are described in the README, after [Running](../README.md#running).

### Profile

One of the two lease and grace configurations the suite accepts, defined in
[`pkg/slo/slo.go`](../pkg/slo/slo.go) and in test plan Section 3.8: `tuned`
(20s lease, 30s grace) and `default` (60s lease, 90s grace). Preflight fails on
any other pair, because every timing bound is derived from the pinned profile.
Write `default` in code font: "the `default` profile" is this configuration, and
"the default profile" reads as whichever one is used when nothing is chosen.
Not a [Pod Security
Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
profile, which is what SEC-09 and doc 07 mean by "restricted profile".

---

## Sources

- [RFC 8881][rfc-8881], NFSv4.1: [Section 8.3][rfc-8.3] (lease renewal),
  [Section 8.4.2.1][rfc-8.4.2.1] (state reclaim and the grace period),
  [Section 10.3.1][rfc-10.3.1] (data caching and `OPEN`s),
  [Section 15.1.9.3][rfc-15.1.9.3] (`NFS4ERR_NO_GRACE`),
  [Section 18.3][rfc-18.3] (`COMMIT`), [Section 18.51][rfc-18.51]
  (`RECLAIM_COMPLETE`).
- [`nfs(5)`][nfs5] for `hard` and close-to-open cache consistency.
- Kernel [client-identifier][kernel-clid] for the client ID and the one lease
  per server.
- Linux source, read on 2026-09-30: [`fs/nfs/nfs4proc.c`][linux-nfs4proc],
  [`fs/nfs/nfs4state.c`][linux-nfs4state] and
  [`fs/nfs/nfs4renewd.c`][linux-renewd] for the client,
  [`fs/nfsd/nfs4state.c`][linux-nfsd-state] for knfsd's grace and
  [`fs/nfsd/nfs4recover.c`][linux-nfsd-recover] for its recovery store.
- [`findings.md`](findings.md) for what the reference deployments do: F-001,
  F-016, F-019, F-026 to F-029, F-031.

[rfc-8881]: https://www.rfc-editor.org/rfc/rfc8881.html
[rfc-8.3]: https://www.rfc-editor.org/rfc/rfc8881.html#section-8.3
[rfc-8.4.2.1]: https://www.rfc-editor.org/rfc/rfc8881.html#section-8.4.2.1
[rfc-10.3.1]: https://www.rfc-editor.org/rfc/rfc8881.html#section-10.3.1
[rfc-15.1.9.3]: https://www.rfc-editor.org/rfc/rfc8881.html#section-15.1.9.3
[rfc-18.3]: https://www.rfc-editor.org/rfc/rfc8881.html#section-18.3
[rfc-18.51]: https://www.rfc-editor.org/rfc/rfc8881.html#section-18.51
[nfs5]: https://man7.org/linux/man-pages/man5/nfs.5.html
[kernel-clid]: https://docs.kernel.org/filesystems/nfs/client-identifier.html
[linux-nfs4proc]: https://github.com/torvalds/linux/blob/master/fs/nfs/nfs4proc.c
[linux-nfs4state]: https://github.com/torvalds/linux/blob/master/fs/nfs/nfs4state.c
[linux-renewd]: https://github.com/torvalds/linux/blob/master/fs/nfs/nfs4renewd.c
[linux-nfsd-state]: https://github.com/torvalds/linux/blob/master/fs/nfsd/nfs4state.c
[linux-nfsd-recover]: https://github.com/torvalds/linux/blob/master/fs/nfsd/nfs4recover.c

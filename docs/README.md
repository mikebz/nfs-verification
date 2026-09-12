# Documentation map

Author: mikebz@
Created: 2026-09-11
Updated: 2026-09-12

This suite verifies NFS RWX persistent volumes on Kubernetes by driving a real
cluster and asserting what a client can observe. These documents say what is
verified, in what order it was built, what was decided along the way, and what
real runs taught.

## Every document carries its dates

Each file here, plus [`../README.md`](../README.md) and
[`../AGENTS.md`](../AGENTS.md), opens with a header block:

```
Author: mikebz@
Created: <the date the document first landed in the repository>
Updated: <the date of the last change to what it says>
```

Both dates are UTC, and both matter for different reasons. **Created** says how
old the thinking is: a design written before the first real run was written
without evidence that has since arrived. **Updated** says whether anyone has
reconciled it with the code since. A design doc adds `Status:` and `Serves:` so
that a reader who gets no further than the header still knows whether it
describes code that exists.

A typo fix does not move `Updated`; a change to what the document claims does.
Both dates are checkable against history:

```sh
TZ=UTC git log --diff-filter=A --format=%ad --date=format-local:%F -1 -- docs/<file>   # created
TZ=UTC git log               --format=%ad --date=format-local:%F -1 -- docs/<file>   # last change
```

## Read in this order

| # | Document | What it answers | Read it when |
|---|---|---|---|
| 0 | [`../README.md`](../README.md) | What the suite is, how to build and run it, every flag | First, always |
| 1 | [`01-test-plan.md`](01-test-plan.md) | What gets verified and why: the architecture under test, every case ID, the SLO table | Before writing or reading any case |
| 2 | [`02-implementation-plan.md`](02-implementation-plan.md) | How it gets built: harness approach, delivery order, what is done and what is next | Before starting work |
| 3 | [`findings.md`](findings.md) | What running against a real cluster taught, `F-001` upward | Before touching teardown, deletion, or anything that unmounts |
| 4 | Design docs 03 to 06, below | Why one phase is shaped the way it is | Only when you touch that area |
| 5 | [`../AGENTS.md`](../AGENTS.md) | How to work here: change size, case conventions, review, what to claim | Before opening a pull request |

Thirty minutes on documents 1 and 2 is enough to read any case in the
repository. The design docs are reference material, not prerequisites.

## Design docs

Each one covers a delivery step from [`02-implementation-plan.md`](02-implementation-plan.md)
and states, at the top, which cases it serves and whether it shipped.

| Doc | Step | Cases | Status |
|---|---|---|---|
| [`03-chaos-operations-design.md`](03-chaos-operations-design.md) | 3 | CHAOS-01, CHAOS-02 | Shipped |
| [`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md) | 4 | CHAOS-05, CHAOS-06, CHAOS-07, OBS-02, OBS-03 | Shipped |
| [`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md) | 6 | DATA-06 to DATA-13, plus `locktool` | Shipped, DATA-14 deferred |
| [`06-observability-design.md`](06-observability-design.md) | 7 | OBS-01 (half), OBS-05, OBS-06, OBS-07 | Designed, not implemented |

A design doc is required for any change estimated at 1000 lines or more, and it
lands as its own pull request before the code ([`../AGENTS.md`](../AGENTS.md),
*Approach to a change*).

## Where each fact lives

Every fact has one home. A copy of it somewhere else is how these documents
drifted the first time.

| Fact | Home |
|---|---|
| What a case must verify | [`01-test-plan.md`](01-test-plan.md), Section 3 |
| Timing and correctness targets | [`01-test-plan.md`](01-test-plan.md) Section 3.8, values in `pkg/slo` |
| Delivery order and step status | [`02-implementation-plan.md`](02-implementation-plan.md), Section 2 |
| Cases that exist today, flags, `make` targets | [`../README.md`](../README.md) |
| Why a phase is built the way it is | That phase's design doc |
| What a real run taught | [`findings.md`](findings.md) |
| How to work in the repository | [`../AGENTS.md`](../AGENTS.md) |

## Vocabulary

- **Case ID**: `CATEGORY-NN`, for example `CHAOS-06`. Defined in the test plan,
  named in the comment above the test function that implements it.
- **Step**: one entry in the delivery order, one pull request or a small run of
  them. Steps are numbered independently of the documents; step 3 is designed in
  doc 03 only by coincidence of ordering, and each design doc says which step it
  covers.
- **Passed**: the assertion held.
- **Failed**: the deployment did not do what the protocol, the Kubernetes API or
  the CSI spec requires. A finding about the deployment.
- **Blocked**: the case could not run, because the cluster or the tools image
  gave it nothing to assert on. Never reported as a pass, and the reason is
  itself the finding.
- **Skipped**: the cluster lacks a capability the case needs, discovered at
  preflight. Never used to hide a deployment that publishes nothing.

## Sources the assertions rest on

Every assertion in this suite traces to one of these. Cite the specific one in
the case comment, per [`../AGENTS.md`](../AGENTS.md), *The test is the test*.

**NFS protocol**

- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html), NFSv4.1. The
  guarantee under test. Section 8, State Management, and Section 8.4.2, Server
  Failure and Recovery, for leases, grace and reclaim; Section 9, File Locking
  and Share Reservations, for `LOCK`, `LOCKT`, `LOCKU` and byte ranges; Section
  10, Client-Side Caching, for what a client may cache; Section 18.3 for
  `COMMIT`, which is what post-fsync durability means here.
- [RFC 7530](https://www.rfc-editor.org/rfc/rfc7530.html), NFSv4.0, where v4.0
  behaviour is being contrasted.
- [RFC 7862](https://www.rfc-editor.org/rfc/rfc7862.html), NFSv4.2, for
  `ALLOCATE`, `DEALLOCATE` and `READ_PLUS`. Out of reach on the `vers=4.1` mount
  preflight pins, which is why DATA-11 records a hole punch rather than
  asserting one.

**Linux NFS client**, which is not the protocol and where the difference belongs
in the comment

- [`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) for mount
  options: `hard`, `ac` and `noac`, `local_lock`, `nolock`, and the close-to-open
  cache consistency the client actually implements.
- [Kernel NFS documentation](https://docs.kernel.org/filesystems/nfs/), in
  particular
  [client-identifier](https://docs.kernel.org/filesystems/nfs/client-identifier.html):
  one lease per client per server, shared by every mount and every pod on that
  node. DATA-06 is written around this.
- [`fcntl(2)`](https://man7.org/linux/man-pages/man2/fcntl.2.html) and
  [`flock(2)`](https://man7.org/linux/man-pages/man2/flock.2.html) for what an
  application sees, and why `locktool` exists.

**Kubernetes**

- [Persistent volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/),
  for reclaim policies, [storage object in use
  protection](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#storage-object-in-use-protection)
  (PROV-03) and [expansion](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#expanding-persistent-volumes-claims)
  (PROV-04, PROV-11).
- [Storage classes](https://kubernetes.io/docs/concepts/storage/storage-classes/),
  for `volumeBindingMode: WaitForFirstConsumer`, which decides the order in
  which every case creates its pod and its claim.
- [Volume snapshots](https://kubernetes.io/docs/concepts/storage/volume-snapshots/)
  (PROV-05).
- [Taints and tolerations](https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/)
  for the default `tolerationSeconds: 300` on `not-ready` and `unreachable`, and
  the non-graceful node shutdown section of
  [Nodes](https://kubernetes.io/docs/concepts/architecture/nodes/) for the
  `out-of-service` taint. These are the two platform defaults behind the floor
  note in test plan Section 3.8, and the reason CHAOS-03 reports blocked on a
  stock cluster.
- [Node metrics data](https://kubernetes.io/docs/reference/instrumentation/node-metrics/),
  the kubelet Summary API that OBS-05 and OBS-06 read.

**CSI**

- [The CSI specification](https://github.com/container-storage-interface/spec/blob/master/spec.md).
  Volume statistics, expansion and snapshots are optional capabilities; a driver
  that omits one is not defective, and the cases that need them say so rather
  than failing the storage system.

**The implementation in front of us**, the least authoritative source, enough
only for a statement labelled as being about this deployment: the provisioner's
flags, chart templates and source.

## Files renamed on 2026-09-11

Links from older pull requests point at the left column.

| Was | Is |
|---|---|
| `docs/plan.md` | [`docs/02-implementation-plan.md`](02-implementation-plan.md) |
| `docs/02-chaos-operations-design.md` | [`docs/03-chaos-operations-design.md`](03-chaos-operations-design.md) |
| `docs/03-grace-and-lock-reclaim-design.md` | [`docs/04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md) |
| `docs/04-data-path-and-locktool-design.md` | [`docs/05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md) |
| `docs/05-observability-design.md` | [`docs/06-observability-design.md`](06-observability-design.md) |

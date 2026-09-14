# F-010: A lock case accused the server of losing a lock that was plainly still held

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


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

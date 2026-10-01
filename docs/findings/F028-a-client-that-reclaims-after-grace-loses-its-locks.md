# F-028: A client that reclaims after grace loses its locks, and on `gke-w2` one did

Author: mikebz@
Created: 2026-09-27
Updated: 2026-10-01


**Found:** 2026-09-26, reading the bundle and the server's own log for a
CHAOS-06 failure in run `w2-e2e-run2-20260925-222955`: GKE cluster `gke-w2`,
control plane v1.37.0-gke.3165000, three `e2-medium` COS workers (kernel
6.12.94+), StorageClass `nfs`, server `nfs-provisioner:15.3` (Ganesha V15.3-mb),
profile `default` (lease 60s, grace 90s). One of three whole-suite runs on that
cluster that day, run concurrently with three on `gke-w1`, where CHAOS-06 passed
all three.

**Severity:** none for the cluster, none for the suite, high for the
deployment. A deployment margin, not a server defect, and not a harness defect. The server did what RFC 8881 allows. A client that held two locks lost
both across a failover and the application holding them was not told.

### What happened

CHAOS-06 takes four whole-file locks and two byte ranges across two clients, then
deletes the server pod. The writer, on the server's own node
`5035b2f7-68nj`, holds `chaos06a` to `chaos06c` and `[0..4096)`. The verifier on
`6b4278f5-9e9c` holds `chaos06d` and `[8192..12288)`. The fault was at 23:56:35Z.
After recovery:

```
chaos_test.go:855: after the failover writer was granted /mnt/share/chaos06/chaos06d.lock, which verifier never released (GRANTED)
chaos_test.go:865: 3 of 4 locks survived the failover (75%), want 100%
chaos_test.go:878: after the failover a third client on gke-w2-default-pool-a5180872-4dtw was granted write [8192..12288), which neither holder released
```

Every lock the writer held survived. Every lock the verifier held was lost.
`/proc/locks` on the verifier's node still listed both as held. The same
node's kernel said why:

```
NFS: 34.118.237.153: lost 2 locks
```

That is the Linux client reporting that its reclaim of the two locks it held was
refused. The dmesg `-T` stamp on the line reads 23:56:04, which is before the
fault. Those stamps run about two minutes early on these nodes, and #101 tracks
that.

The server's log (`/export/ganesha.log`, the file F-022 found) shows the other
side:

```
23:56:38  nfs_start_grace      NFS Server Now IN GRACE, duration 90
23:56:38 … 23:57:28            check grace:reclaim complete(0) clid count(4)
23:57:38 … 23:58:08            check grace:reclaim complete(1) clid count(4)
23:58:08  nfs_lift_grace_locked NFS Server Now NOT IN GRACE
```

One client finished reclaiming, 60 seconds in, and that was the writer's. Grace
ended on its timer, and the verifier's reclaim arrived after it.

### Why

RFC 8881 [Section 8.4.2.1](https://www.rfc-editor.org/rfc/rfc8881.html#section-8.4.2.1)
gives a client the grace period to reclaim what it held. After grace, a reclaim
is refused, with `NFS4ERR_NO_GRACE`, and the lock is free for anyone to take.
That is what happened here, in order: the server was lawful, and the client was
late.

How late clients usually are is in the same log. Across every grace window in
the six runs, 94 of them with 45 on `gke-w1` and 49 on `gke-w2`, the last client
to send `RECLAIM_COMPLETE` did so between 10 and 71 seconds after grace began,
and most often between 60 and 70. Against a 90 second grace, that is 20 to 30
seconds of margin on an ordinary failover, and this failover used all of it.

**Why this client was late is not established.** An idle client learns that
the server restarted only when its next lease renewal or RPC fails, and with a
60 second lease, a reclaim 60 to 70 seconds in fits that. It does not explain
this one being later than the rest. One more fact, from a single sample: the
verifier's node carries 2,047 stacked `/run/systemd/resolve` mounts left over
from F-005, which the other two nodes do not. That is recorded, not argued. The
server held four client records for three nodes (F-029). That is why grace ran
to its timer rather than ending early, but it has nothing to do with the verifier's
reclaim being refused.

### What changed

Nothing in the assertion, and nothing should. Plan Section 3.3 and RFC 8881
Section 8.4.2 say locks held before a restart survive it when the client
reclaims in time, and a deployment where a client does not is one where locks do
not survive. CHAOS-06 stays red when it happens.

What should change is what the failure says. Its message offered "either the
lock was not reclaimed or a conflicting one was granted", and the bundle already
held a line that narrows it: a `lost N locks` on the holder's node means that
node's client had reclaims refused. #100 tracked naming it, and the message now
does: CHAOS-06 reads both holders' ring buffers just before the fault, and where
a lock is found free and its holder's node has logged a `lost N locks` line
since, the failure names that node, quotes the line, and cites this finding.
With no such line it keeps the old wording. The read is a window rather than the
whole buffer, because in this very run the writer's node still carried a
`lost 1 locks` from 2026-09-19.

The line narrows the cause and does not decide it, which this entry first
claimed it did. It is one count per node and server, shared by every pod on the
node, and names no lock. And the kernel counts a lock as lost when its reclaim
is refused for any reason, including `NFS4ERR_RECLAIM_CONFLICT` and
`NFS4ERR_DENIED`, the errors a server returns to a reclaim of a lock it has
already granted to someone else (`nfs4_reclaim_locks` in `fs/nfs/nfs4state.c`).
So the line proves refused reclaims on that node, not a late one for this lock.
Here it was the count matching the verifier's two locks, and the server's log,
that showed which locks and why. The message says only what the line says, and
quoting that log waits until the harness can read it (F-022, #21).

### What it means for the system under test

**On this deployment a lock survives a server restart only if its client
reclaims within 90 seconds, and one client in six CHAOS-06 failovers did not.**
The application is not told. The kernel keeps the lock in its own table, and
under the documented default for `nfs.recover_lost_locks`
([kernel parameters](https://docs.kernel.org/admin-guide/kernel-parameters.html)),
the client fails I/O on that descriptor instead of re-acquiring the lock. This
run did not exercise that part. Meanwhile another client can take the same
range, which is the outcome locking exists to prevent.

For a platform owner, the lever is the grace period against the clients'
reclaim latency, not the server's lock table. Open: whether the tuned profile
(20s/30s) keeps the same margin in proportion, and whether a longer grace
would have saved this one or only moved the tail.

# F-013: A terminating server pod counted as the server being back

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


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

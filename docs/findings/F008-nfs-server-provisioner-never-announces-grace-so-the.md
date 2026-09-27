# F-008: `nfs-server-provisioner` never announces grace, so the grace cases cannot run against it

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-27


> **Refined by [F-022](F022-the-servers-grace-announcements-were-in-a-log-file.md).**
> What follows is true of the container's log stream, which is what `kubectl logs`
> and this suite read. It is not true of the server. The NFS daemon writes its
> grace entry and exit to `/export/ganesha.log` inside the export volume. On
> 2026-09-25, six whole-suite runs found those lines, `nfs_start_grace … IN GRACE,
> duration 90` and `nfs_lift_grace_locked … NOT IN GRACE`, for every restart on
> this deployment and on `gke-w2`'s Ganesha 15.3. The verdicts here are
> unchanged: OBS-03 failed and CHAOS-07 reported blocked in all six runs, because
> neither reads that file. Whether a case may read it is the open question in
> F-022 and #21.

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

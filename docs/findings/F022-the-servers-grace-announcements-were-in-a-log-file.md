# F-022: The server's grace announcements were in a log file, not in the container log stream

Author: mikebz@
Created: 2026-09-14
Updated: 2026-10-02


> **Refined by [F-030](F030-reading-the-servers-log-file-took-two-corrections.md).**
> The open question below is settled: the harness reads this file now, through
> the node agent, and OBS-03, CHAOS-05 and CHAOS-07 observe grace from it.

**Found:** 2026-09-13, hand probes against GKE cluster `gke-w1` while designing
the security cases, `nfs-server-provisioner` v4.0.8 in namespace
`nfs-provisioner`.

**Severity:** none for the cluster, medium for the suite, medium for the
deployment. F-008 concluded from an absence, and the absence was in the wrong
place. Two cases cannot reach a signal that exists: OBS-03 fails for want of it
and CHAOS-07 reports blocked.

### What happened

F-008 records that this provisioner never announces grace, on the evidence that
nothing in `kubectl logs` for the server pod ever says so. Reading the container
from the inside rather than through the log stream shows otherwise: the NFS
daemon's own log file, `/export/ganesha.log` on this deployment, carries the
ordinary `NFS Server Now NOT IN GRACE` lines.

### Why

The pod runs two things, and only one of them logs to stdout. The Go provisioner
writes klog to stdout, and that is what `kubectl logs` carries. The NFS daemon it
supervises is configured to log to a file, and on this deployment that file lives
on the export PVC rather than on `/dev/stdout`.

Nothing about this is specific to one NFS implementation: any server run under a
supervisor that owns stdout can put its own log somewhere else, and a case that
concludes from `kubectl logs` alone will read that as silence.

### What changed

Nothing yet, deliberately: reading it needs a case that execs into the server and
parses a log format nobody has pinned, and that is a phase of its own rather than
a line in the security work. F-008 stays true as written — *the container's log
stream* announces nothing — and this entry names where the signal actually was.

Worth stating, because the question comes up on reading this entry: nothing in
the harness parses that log, and no case or plan requirement is written against a
particular NFS implementation. The one place a vendor name appears in non-test
code is the server discovery heuristic
([`server.go`](../../pkg/framework/server.go)), where `ganesha` is one alternative
in a regex beside `nfs` and `nfsd`, matched against image and pod names. Timing
discovery matches generic `lease`/`grace` key spellings rather than any one
config format. The target is a Kubernetes-native NFS server, whichever one it is.

### What it means for the system under test

OBS-03 fails and CHAOS-07 reports blocked because of a logging configuration,
not because the server keeps its grace period secret. An operator who wants grace visible can point the
daemon's log at stdout; nothing about the server itself has to change.

### Open

How a case may read a file inside the export, which is where this server writes
its grace lines. That is the decision to settle before implementing, it is
#123, and #21 depends on it.

### What changed after this was written

**2026-09-27. Confirmed on both deployments.** Six whole-suite runs on
2026-09-25, three on `gke-w1` (upstream v4.0.8) and three on `gke-w2` (Ganesha
V15.3-mb), each found a grace entry in `/export/ganesha.log` for every server
restart, 94 in all, and an exit for every one except the single window a second
restart cut short. The lines were `nfs_start_grace … NFS Server Now IN GRACE,
duration 90` and `nfs_lift_grace_locked … NFS Server Now NOT IN GRACE`, with a
`check grace:reclaim complete(n) clid count(m)` line every ten seconds between
them. OBS-03 failed and CHAOS-07 was blocked in all six runs, as above.

The same file also completed the account of a CHAOS-06 failure (#100). The
bundle had the client's side: the holder node's kernel logged `lost 2 locks`,
so its reclaim was refused. It did not have the server's side. The server log
supplied that: when grace began and ended, and how many clients had reclaimed
by then. Neither source alone says the reclaim was late. Together they do.

**A second channel exists on `gke-w2`.** With its metrics exposer switched on
(F-023), the server publishes `compound__latency_*` series labelled by result.
`status="NFS4ERR_GRACE"` is among the labels present in OBS-07's scrape before
its restart, left there by the chaos cases' earlier failovers, and absent from
the scrape right after it (F-025). That counts operations refused because of
grace. It does not mark when grace began or ended, and no convention names it,
so it does not change the case for reading the log. It is worth knowing it is
there.

Still nothing reads either. How a case may read a file inside the export is
the decision to settle before implementing, and #21 depends on it.

**2026-10-02. The file is read now** ([F-030](F030-reading-the-servers-log-file-took-two-corrections.md),
[PR #133](https://github.com/mikebz/nfs-verification/pull/133)). The open
question, #123, was settled by finding the file from the serving process's
command line and reading it through the node agent at observe time, and #21
with it: CHAOS-05's re-entry check now has something to count. The deployment
half of this entry stands: `kubectl logs` still carries none of it.

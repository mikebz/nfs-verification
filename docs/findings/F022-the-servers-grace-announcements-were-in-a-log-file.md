# F-022: The server's grace announcements were in a log file, not in the container log stream

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-13, hand probes against GKE cluster `gke-w1` while designing
the security cases, `nfs-server-provisioner` v4.0.8 in namespace
`nfs-provisioner`.

**Severity:** F-008 concluded from an absence, and the absence was in the wrong
place. Two cases are blocked on a signal that exists.

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

OBS-03 and CHAOS-07 are blocked by a logging configuration, not by a server that
keeps its grace period secret. An operator who wants grace visible can point the
daemon's log at stdout; nothing about the server itself has to change.

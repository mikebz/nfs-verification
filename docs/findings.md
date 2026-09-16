# Findings

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-16

Things learned by running the suite against a real cluster that are worth
remembering. Each entry is dated, and says what happened, why, what changed in
the code, and what it implies for the system under test as opposed to the
harness.

**This file is the citation of record.** A case that skips or reports blocked, a
constant that is the value it is, a teardown step that looks like more work than
it should be: if the reason came from a real run, the code comment and the design
doc name the `F-NNN` rather than re-explaining it. A reason that lives only in a
commit message or a pull request comment is lost by the next one.

One finding, one file, in [`findings/`](findings/), named `F0NN-` and a slug of
its title. This file is the index and stays at this path, because that is what
the code cites: a comment reading "see F-009 in docs/findings.md" should land a
reader somewhere that still exists. A new finding takes the next number, gets a
file of its own, and a row at the top of the table below.

The entry files were split out of a single log on 2026-09-14, so every one of
them carries that date as `Created:` while describing something found earlier.
The date the finding was made is the `**Found:**` line inside the entry, and
that is the one that means anything.

| # | Found | What it says | Cited by |
|---|---|---|---|
| [F-024](findings/F024-identical-counters-after-a-restart-are-not-continuity.md) | 2026-09-15 | OBS-07's first end-to-end run passed with `resumed-continuous` and claimed the server keeps its counts, on a process whose uptime was 1 second: Ganesha's startup is deterministic and the server idle, so a restart re-derives identical counters. Classification now splits advanced from unchanged and reports `resumed-indeterminate` | OBS-07, `ClassifyMetrics`, doc 06 |
| [F-023](findings/F023-neither-nfs-deployment-declares-a-metrics-endpoint.md) | 2026-09-15 | Neither deployment publishes metrics as shipped, but for different reasons: `gke-w1` is built without `USE_MONITORING`, while `gke-w2` has `libganesha_monitoring` linked and is off only because `Enable_Metrics` defaults to false. Turning it on served 39 families including lease, lock and client-state metrics; the chart still declares no port or annotation, so a scraper finds nothing either way | OBS-07, doc 06 |
| [F-022](findings/F022-the-servers-grace-announcements-were-in-a-log-file.md) | 2026-09-13 | The grace lines F-008 said do not exist do exist, in the NFS daemon's own log file inside the export volume; `kubectl logs` carries only the Go provisioner's output | F-008, doc 07 |
| [F-021](findings/F021-a-root-owned-file-reads-back-as-nobody-for-a-reason.md) | 2026-09-13 | The server returns named owners as names, and the client's idmapper maps `root` to nobody while an unnamed uid survives numerically, so what `stat` shows cannot tell squash from a failed mapping; SEC-02 now probes whether a `chown` is permitted instead | SEC-02, doc 07 |
| [F-020](findings/F020-fsgroup-does-nothing-to-an-nfs-volume-here-in.md) | 2026-09-13 | `fsGroup` grants access through the AUTH_SYS gid list, which this export honours, but changes nothing about the volume: no chown storm, no ownership change, and new files keep the pod's primary gid. **Corrected 2026-09-14**: the original entry credited the world-writable root, from an assertion that could not fail | SEC-03, `slo.FSGroupStartOverhead` |
| [F-019](findings/F019-the-client-an-nfs-server-can-name-is-the-node-and.md) | 2026-09-13 | The server sees node addresses, never pod addresses, and an IPv4 client on its IPv6 listener appears as `::ffff:a.b.c.d`, so `/proc/net/tcp` alone shows no NFS connections at all | `peers.go`, SEC-06, SEC-08 |
| [F-018](findings/F018-the-export-admits-any-client-that-can-reach-it-so-a.md) | 2026-09-13 | A node with no claim mounted another claim's export and read its bytes; the exports carry no client rules and nothing in the network path restricts who may try | SEC-05's failure, SEC-08's record, doc 07 |
| [F-017](findings/F017-a-sleeping-workstation-understates-every-duration.md) | 2026-09-13 | A Mac asleep mid-run freezes Go's monotonic clock but not the pods, so the suite under-reports its own durations while cluster-side measurements stay right | the README's note on long runs |
| [F-016](findings/F016-concurrent-o-append-from-four-clients-loses-a.md) | 2026-09-13 | Four clients appending to one file landed 150 of 200, none torn, on three whole-suite runs: one appender loses its whole contribution while the rest lose none. Two data-only runs lost nothing, so it is intermittent | DATA-02's failure message, `CompactRanges` |
| [F-015](findings/F015-a-checksum-that-failed-came-back-as-an-empty-string.md) | 2026-09-13 | A checksum pipeline ending in `cut` exits zero when `sha256sum` fails, so a helper returned an empty string as a digest | `io.go`'s `sumCmd` and `parseSum`, `io_parse_test.go` |
| [F-014](findings/F014-recovery-was-measured-to-a-write-that-committed.md) | 2026-09-12 | Time to first I/O after a fault returned a write from before service was lost, reporting a 1m43s failover as 0s | `load.go`'s stall measurement, `slo.LoadStallFloor`, CHAOS-02/05/06/07 |
| [F-013](findings/F013-a-terminating-server-pod-counted-as-the-server.md) | 2026-09-11 | A gracefully deleted pod stays Running and ready, so the wait for a replacement was satisfied by the pod it was waiting past | `server.go`'s readiness check, `server_test.go` |
| [F-012](findings/F012-the-resize-diagnosis-was-unreachable-because-an.md) | 2026-09-11 | Listing every claim condition made the "nothing acted on the request" diagnosis unreachable on any mounted claim | `pvc.go`'s resize description, `pvc_test.go` |
| [F-011](findings/F011-a-record-sweep-came-back-empty-from-an-exec-that.md) | 2026-09-12 | A sweep exec returned success with no output at all, and the old message could not tell that from finding nothing | `sweep.go`'s short-answer error, `sweep_test.go` |
| [F-010](findings/F010-a-lock-case-accused-the-server-of-losing-a-lock.md) | 2026-09-12 | A file's identity on an NFS mount is per-node, so one identity cannot match two nodes' lock tables | CHAOS-06's range assertion, `lockmount_test.go` |
| [F-009](findings/F009-the-export-has-no-per-volume-quota-so-capacity.md) | 2026-09-11 | The export has no per-volume quota, so both capacity sources describe the backing filesystem rather than the claim | OBS-06's failure message |
| [F-008](findings/F008-nfs-server-provisioner-never-announces-grace-so-the.md) | 2026-09-11 | This provisioner never announces grace, so OBS-03 fails and CHAOS-07 reports blocked | CHAOS-07's blocked message, doc 04, doc 06 |
| [F-007](findings/F007-two-cases-in-the-data-path-phase-reported-results.md) | 2026-09-11 | Two cases reported results they had not measured | doc 05 |
| [F-006](findings/F006-scripts-lock-probe-sh-passed-flock-w-which-busybox.md) | 2026-09-11 | `flock -w` does not exist on busybox, so the lock probe never waited | `scripts_test.go` |
| [F-005](findings/F005-exponential-mount-propagation-in-gkes-mount-nfs.md) | 2026-09-11 | A GKE `mount.nfs` wrapper multiplies mounts until the node wedges | preflight records mount propagation |
| [F-004](findings/F004-allowvolumeexpansion-is-a-claim-not-a-capability.md) | 2026-09-10 | `allowVolumeExpansion` is advertised, not implemented, so PROV-04 cannot trust it | `pvc.go`'s expansion helper |
| [F-003](findings/F003-a-broken-umount-nfs-wrapper-on-gke-wedges-every.md) | 2026-09-10 | A broken `umount.nfs` wrapper wedges every terminating pod | teardown's terminate bound |
| [F-002](findings/F002-2gb-worker-nodes-cannot-host-the-suite.md) | 2026-09-10 | 2GB worker nodes cannot host the suite | the node shape a run reports |
| [F-001](findings/F001-force-deleting-a-mounted-pod-can-take-a-node-out-of.md) | 2026-09-10 | Force-deleting a mounted pod, then its claim, takes a node out of service | teardown, the force-delete helper, PROV-03, doc 05 |

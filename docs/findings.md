# Findings

Author: mikebz@
Created: 2026-09-10
Updated: 2026-10-02

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
file of its own, and a row at the top of the table below. A row's status moves
in the same change as whatever moved it: the fix, the later finding, the answer.

F-001 to F-022 were split out of a single log on 2026-09-14, so each of them
carries that date as `Created:` while describing something found earlier. Every
later entry carries the date its own file landed. Either way, the date the
finding was made is the `**Found:**` line inside the entry, and that is the one
that means anything.

## Severity

Every entry rates itself three times, on one scale: once for the cluster, once
for the suite and once for the deployment, in that order. A finding that wedges
a node and says nothing about NFS is a different finding from one that loses an
application's records, and a single word cannot say which it is.

- **The cluster**: the Kubernetes cluster the run used, its nodes and the other
  workloads on them.
- **The suite**: what a run's results can be trusted to say.
- **The deployment**: the system under test as configured: the NFS server, its
  provisioner and chart, and the node image's NFS mount path.

| Level | The cluster | The suite | The deployment |
|---|---|---|---|
| critical | A node out of service until a person resets or replaces it | Every result of one kind is void, and nothing in the output shows it | Breaks a guarantee the protocol, the Kubernetes API or the CSI spec states |
| high | Workloads stop: pods that cannot terminate, nodes rebooted under the run | A case reports something false: a pass that could not fail or measured less than it says, or a failure that blames the deployment for the harness | A workload loses data, locks, isolation or availability without a protocol violation, silently or until a person steps in |
| medium | Degraded, still serving | A result that is true but cannot be acted on: a missing diagnosis, a failure routed to the wrong owner, or a case that cannot run | An operator cannot see, size or rely on something they would reasonably expect: a missing signal, a misleading number, a capability advertised and absent, a recovery cost |
| low | Something left to clear by hand that harms nothing else | No result is wrong, but something the run prints misleads or costs time | Worth knowing before designing on it, and costs nothing by itself |
| none | Nothing | Nothing | Nothing |

Where a level depends on a question the entry leaves open, the entry says
**not known**, and the status below names the question.

## Status

Each row says where its entry stands, with one or more of:

| Status | Means |
|---|---|
| fixed | The entry records a harness defect, and the code no longer has it |
| deployment | The entry describes the deployment under test. The suite reports it, red where the assertion is red, and only the deployment's owner can change it |
| environment | The entry describes the cluster or the machine driving the run rather than the system under test: a node size, a workstation that sleeps |
| refined by F-NNN | A later finding corrected or narrowed it. Read both |
| open: … | The entry leaves something unresolved: a question it could not answer, or a follow-up it names that nobody has done |

## Index

| # | Found | Status | What it says | Cited by |
|---|---|---|---|---|
| [F-031](findings/F031-a-chaos-05-cycle-went-over-the-recovery-budget-and-the-extra.md) | 2026-10-02 | deployment; open: whether the client's `NFS4ERR_GRACE` retry wait accounts for it | A CHAOS-05 cycle on `gke-w2` recovered in 2m1s against the 2m0s budget. This was the first such cycle with a timeline. The server restarted in 3s and held grace for 90s; the writer resumed 28s after grace ended, where the other four cycles took 1 to 13s. The extra time came after grace, in the client or between client and server, not in the restart F-029 blamed. The budget stays, and the case is red for that run | F-029 |
| [F-030](findings/F030-reading-the-servers-log-file-took-two-corrections.md) | 2026-10-02 | fixed | Reading the server's own log, as F-022 asked, took two corrections. Ganesha holds no descriptor on its log between writes, so the file is found from the paths on the serving process's command line instead; and its lines, read whole, carry exit words in a function name and a progress line, so one grace period read as an exit and a re-entry. The classifier now reads only the words near "grace". OBS-02 is folded into OBS-03, and CHAOS-07 ran and passed on both deployments for the first time | OBS-03, CHAOS-05, CHAOS-07, `ServerLogFiles`, `classifyGrace`, doc 06, F-008, F-022 |
| [F-029](findings/F029-neither-deployment-ever-lifts-grace-early-for-different.md) | 2026-09-26 | deployment; refined by F-031; open: whether `gke-w2` lifts grace early once its stale record is removed, and where 15s of two 107s kills went (#101) | Neither server ever lifted grace early: 93 of 94 grace periods across six whole-suite runs ran their full 90s, and the 94th was cut short by a second restart. `gke-w1`'s Ganesha 4.0.8 holds grace even when every client has reclaimed. `gke-w2`'s Ganesha 15.3 waits for a stale 2026-09-10 recovery record left by a recreated node. Recovery is therefore grace plus restart (usually 92s after a kill, 102 to 106s after a pod delete), and one CHAOS-05 cycle took 119s against the 120s budget. The budget stays | F-028, #101 |
| [F-028](findings/F028-a-client-that-reclaims-after-grace-loses-its-locks.md) | 2026-09-26 | deployment; open: why the client reclaimed late | CHAOS-06 lost both of one client's locks across a failover on `gke-w2`, 1 of 6 runs: the kernel logged `lost 2 locks`, and the server's log shows grace ending on its 90s timer with one client's reclaim complete. The server was lawful (RFC 8881 Section 8.4.2.1); the client reclaimed late. The last reclaim across 94 grace windows landed 10 to 71s in, so the margin is 20 to 30s. Why this client was late is unestablished. The case stays red | CHAOS-06's failure message (#100), F-029 |
| [F-027](findings/F027-a-container-cannot-read-the-file-descriptors-of-its.md) | 2026-09-20 | fixed; open: what makes a server's descriptors unreadable | Inside a server pod, root without `CAP_SYS_PTRACE` cannot read the NFS server's file descriptors, so the socket-to-process join F-026 introduced fails, on both deployments, on every server that had been up more than a few minutes and on none that had just started. Capabilities do not explain it and traffic does not trigger it; what does is unestablished. Discovery now reads the socket in the pod and the holder on the node, through the node agent | CHAOS-01, DATA-12, DATA-13, `DiscoverServerProcess`, `Agent.RunScript`, F-026 |
| [F-026](findings/F026-the-process-serving-nfs-is-not-the-containers-command.md) | 2026-09-18 | fixed; refined by F-027; open: the server-did-not-restart subtests (#97) | The process serving NFS is `ganesha.nfsd`, a child of the `nfs-provisioner` supervisor at PID 1, so the container's declared command names the wrong process and the container's restart count cannot see the right one die. CHAOS-01, DATA-12 and DATA-13 were blocked for the life of the project over a name the pod could have been asked for: the harness now joins the listening socket on 2049 to the process holding it, and has no fallback because every candidate fallback names a process nobody checked | CHAOS-01, DATA-12, DATA-13, `DiscoverServerProcess`, `ResolveProcess`, F-027 |
| [F-025](findings/F025-a-metric-name-with-no-sample-yet-is-not-a-lost-one.md) | 2026-09-17 | fixed | OBS-07 scrapes seconds after the restart, before any traffic, and Ganesha creates a metric family only on its first sample: 21 byte, size and cache metric names were reported lost and all 21 were back on the same process once traffic resumed. 367 series under 66 names is this server's cold-start set, which is also what F-024's idle server published on both sides. Fixed for #81: the case drives its own I/O through the export either side of the restart and judges names after it, and `gke-w2` now returns `resumed-reset` | OBS-07, `ClassifyMetrics`, F-024, doc 06 |
| [F-024](findings/F024-identical-counters-after-a-restart-are-not-continuity.md) | 2026-09-15 | fixed; refined by F-025 | OBS-07's first end-to-end run passed with `resumed-continuous` and claimed the server keeps its counts, on a process whose uptime was 1 second: Ganesha's startup is deterministic and the server idle, so a restart re-derives identical counters. Classification now splits advanced from unchanged and reports `resumed-indeterminate` | OBS-07, `ClassifyMetrics`, doc 06 |
| [F-023](findings/F023-neither-nfs-deployment-declares-a-metrics-endpoint.md) | 2026-09-15 | deployment | Neither deployment publishes metrics as shipped, but for different reasons: `gke-w1` is built without `USE_MONITORING`, while `gke-w2` has `libganesha_monitoring` linked and is off only because `Enable_Metrics` defaults to false. Turning it on served 39 families including lease, lock and client-state metrics; the chart still declares no port or annotation, so a scraper finds nothing either way | OBS-07, doc 06 |
| [F-022](findings/F022-the-servers-grace-announcements-were-in-a-log-file.md) | 2026-09-13 | deployment; refined by F-030 | The grace lines F-008 said do not exist do exist, in the NFS daemon's own log file inside the export volume; `kubectl logs` carries only the Go provisioner's output | F-008, doc 07 |
| [F-021](findings/F021-a-root-owned-file-reads-back-as-nobody-for-a-reason.md) | 2026-09-13 | fixed | The server returns named owners as names, and the client's idmapper maps `root` to nobody while an unnamed uid survives numerically, so what `stat` shows cannot tell squash from a failed mapping; SEC-02 now probes whether a `chown` is permitted instead | SEC-02, doc 07 |
| [F-020](findings/F020-fsgroup-does-nothing-to-an-nfs-volume-here-in.md) | 2026-09-13 | deployment | `fsGroup` grants access through the AUTH_SYS gid list, which this export honours, but changes nothing about the volume: no chown storm, no ownership change, and new files keep the pod's primary gid. **Corrected 2026-09-14**: the original entry credited the world-writable root, from an assertion that could not fail | SEC-03, `slo.FSGroupStartOverhead` |
| [F-019](findings/F019-the-client-an-nfs-server-can-name-is-the-node-and.md) | 2026-09-13 | deployment | The server sees node addresses, never pod addresses, and an IPv4 client on its IPv6 listener appears as `::ffff:a.b.c.d`, so `/proc/net/tcp` alone shows no NFS connections at all | `peers.go`, SEC-06, SEC-08 |
| [F-018](findings/F018-the-export-admits-any-client-that-can-reach-it-so-a.md) | 2026-09-13 | deployment | A node with no claim mounted another claim's export and read its bytes; the exports carry no client rules and nothing in the network path restricts who may try | SEC-05's failure, SEC-08's record, doc 07 |
| [F-017](findings/F017-a-sleeping-workstation-understates-every-duration.md) | 2026-09-13 | environment | A Mac asleep mid-run freezes Go's monotonic clock but not the pods, so the suite under-reports its own durations while cluster-side measurements stay right | the README's note on long runs |
| [F-016](findings/F016-concurrent-o-append-from-four-clients-loses-a.md) | 2026-09-13 | deployment; open: what decides a losing run, and which client loses (#16) | Four clients appending to one file landed 150 of 200, none torn, on 9 of 13 whole-suite runs across two clusters and two server builds: one appender loses its whole contribution while the rest lose none. The run shape does not predict it. Every named loser (7 of 7) shared its node with another pod on the share, and the one appender alone on its node never lost. Intermittent | DATA-02's failure message, `CompactRanges` |
| [F-015](findings/F015-a-checksum-that-failed-came-back-as-an-empty-string.md) | 2026-09-13 | fixed; open: why `sha256sum` failed after the failover | A checksum pipeline ending in `cut` exits zero when `sha256sum` fails, so a helper returned an empty string as a digest | `io.go`'s `sumCmd` and `parseSum`, `io_parse_test.go` |
| [F-014](findings/F014-recovery-was-measured-to-a-write-that-committed.md) | 2026-09-12 | fixed | Time to first I/O after a fault returned a write from before service was lost, reporting a 1m43s failover as 0s | `load.go`'s stall measurement, `slo.LoadStallFloor`, CHAOS-02/05/06/07 |
| [F-013](findings/F013-a-terminating-server-pod-counted-as-the-server.md) | 2026-09-11 | fixed | A gracefully deleted pod stays Running and ready, so the wait for a replacement was satisfied by the pod it was waiting past | `server.go`'s readiness check, `server_test.go` |
| [F-012](findings/F012-the-resize-diagnosis-was-unreachable-because-an.md) | 2026-09-11 | fixed | Listing every claim condition made the "nothing acted on the request" diagnosis unreachable on any mounted claim | `pvc.go`'s resize description, `pvc_test.go` |
| [F-011](findings/F011-a-record-sweep-came-back-empty-from-an-exec-that.md) | 2026-09-12 | open: why the exec returned success with nothing on stdout | A sweep exec returned success with no output at all, and the old message could not tell that from finding nothing | `sweep.go`'s short-answer error, `sweep_test.go` |
| [F-010](findings/F010-a-lock-case-accused-the-server-of-losing-a-lock.md) | 2026-09-12 | fixed | A file's identity on an NFS mount is per-node, so one identity cannot match two nodes' lock tables | CHAOS-06's range assertion, `lockmount_test.go` |
| [F-009](findings/F009-the-export-has-no-per-volume-quota-so-capacity.md) | 2026-09-11 | deployment | The export has no per-volume quota, so both capacity sources describe the backing filesystem rather than the claim | OBS-06's failure message |
| [F-008](findings/F008-nfs-server-provisioner-never-announces-grace-so-the.md) | 2026-09-11 | deployment; refined by F-022, F-030 | This provisioner never announces grace *in its container log stream*, so OBS-03 fails and CHAOS-07 reports blocked. Refined by F-022: the daemon's own log file carries the lines | CHAOS-07's blocked message, doc 04, doc 06 |
| [F-007](findings/F007-two-cases-in-the-data-path-phase-reported-results.md) | 2026-09-11 | fixed; refined by F-010 | Two cases reported results they had not measured | doc 05 |
| [F-006](findings/F006-scripts-lock-probe-sh-passed-flock-w-which-busybox.md) | 2026-09-11 | fixed | `flock -w` does not exist on busybox, so the lock probe never waited | `scripts_test.go` |
| [F-005](findings/F005-exponential-mount-propagation-in-gkes-mount-nfs.md) | 2026-09-11 | deployment; open: the nodes were never cleaned of mounts already stacked | A GKE `mount.nfs` wrapper multiplies mounts until the node wedges | preflight records mount propagation |
| [F-004](findings/F004-allowvolumeexpansion-is-a-claim-not-a-capability.md) | 2026-09-10 | deployment | `allowVolumeExpansion` is advertised, not implemented, so PROV-04 cannot trust it | `pvc.go`'s expansion helper |
| [F-003](findings/F003-a-broken-umount-nfs-wrapper-on-gke-wedges-every.md) | 2026-09-10 | deployment; open: a preflight check for the wrapper | A broken `umount.nfs` wrapper wedges every terminating pod | teardown's terminate bound |
| [F-002](findings/F002-2gb-worker-nodes-cannot-host-the-suite.md) | 2026-09-10 | environment; open: a preflight check for node memory | 2GB worker nodes cannot host the suite | the node shape a run reports |
| [F-001](findings/F001-force-deleting-a-mounted-pod-can-take-a-node-out-of.md) | 2026-09-10 | fixed | Force-deleting a mounted pod, then its claim, takes a node out of service | teardown, the force-delete helper, PROV-03, doc 05 |

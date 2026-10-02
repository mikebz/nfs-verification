# F-030: Reading the server's log file took two corrections: Ganesha holds no descriptor on it, and its lines read whole carry exit words

Author: mikebz@
Created: 2026-10-02
Updated: 2026-10-02


**Found:** 2026-10-02, implementing
[#19](https://github.com/mikebz/nfs-verification/issues/19) and
[#123](https://github.com/mikebz/nfs-verification/issues/123) against GKE
clusters `gke-w1` (upstream `nfs-server-provisioner` v4.0.8) and `gke-w2`
(`nfs-provisioner:15.3`, Ganesha V15.3-mb), reading `ganesha.nfsd`'s descriptors
and `/export/ganesha.log` through the node agent, then running OBS-03, CHAOS-07
and CHAOS-05 on both (`w{1,2}-issue19-*`, `w{1,2}-i19-*`).

**Severity:** none for the cluster, high for the suite, none for the
deployment. Since step 4, OBS-03 failed and CHAOS-07 reported blocked on both
reference deployments for want of a signal that was there all along (F-022),
and CHAOS-05's re-entry check passed without having read anything. Had the file
simply been read with the old classifier, every failover would have been
reported as a grace re-entry loop against a healthy server.

### What happened

[F-022](F022-the-servers-grace-announcements-were-in-a-log-file.md) located the
grace lines in `/export/ganesha.log`, and #123 asked how a case may read them.
The plan was to find the log as the file the serving process holds open for
append, and to run the lines through the existing classifier. Both halves were
wrong when tried against the real servers.

**Ganesha holds no descriptor on its log.** The only regular file
`ganesha.nfsd` had open on either cluster was `/run/ganesha.pid`. It opens the
log for each write and closes it again, so a scan of `/proc/<pid>/fd` finds
nothing to read.

**The classifier read Ganesha's lines wrongly.** It took the earliest exit or
entry word anywhere in a line. Ganesha's lines carry a function name and a
component before the message, and while in grace it prints a progress line
every ten seconds:

```
nfs_start_grace :STATE :EVENT :NFS Server Now IN GRACE, duration 90
nfs_start_grace :STATE :EVENT :grace reload client info completed from backend
nfs_try_lift_grace :STATE :EVENT :check grace:reclaim complete(0) clid count(2)
nfs_lift_grace_locked :STATE :EVENT :NFS Server Now NOT IN GRACE
```

The second and third lines both read as exits, on "complete" and on "lift" in
the function name. One grace period would have been an entry, an immediate
exit, and then, at the next entry-shaped line, a re-entry.

### Why

The suite's earlier rules each assumed something no real server had been
checked against: that a log is a file held open, which is how a process that
logs often usually behaves and is not how this one does; and that a line is
prose, where a server's line is a record with fields, and only one of them is
the message.

### What changed

In [PR #135](https://github.com/mikebz/nfs-verification/pull/135):

- The log is found from the serving process's command line instead. Every
  absolute path on it is a candidate, read through the process's root by the
  node agent, and a candidate is a log when its lines carry timestamps the suite
  reads. On both clusters that picks `/export/ganesha.log` out of
  `-L /export/ganesha.log -p /var/run/ganesha.pid -f /export/vfs.conf` without
  parsing Ganesha's option syntax. ([`serverlogfile.go`](../../pkg/framework/serverlogfile.go),
  [`server-log.sh`](../../pkg/framework/scripts/server-log.sh))
- A file has no runtime timestamp, so each line's own is read and the file is
  used only where its newest line agrees with its modification time. On both
  clusters the container's time zone is UTC and they agree to the second.
- The classifier reads only the words near "grace", per clause, with identifiers
  dropped. The four lines above, from both servers, are a unit test.
- `/export/vfs.conf` says `Grace_Period = 90;`. A file that mentions grace but
  carries no timestamp is reported as unreadable only when a line in it would
  classify as a transition, which that one does not.
- OBS-02 is folded into OBS-03, CHAOS-05 and CHAOS-07 read the same observer,
  and what was read goes into each case's bundle as `grace*.txt`.

The first OBS-03 runs (`w{1,2}-issue19-TestObsGracePeriodIsObservable-20261002-030639`)
failed with the file unread and said why in the failure message: the run ID was
long enough that `"listener-"` plus it exceeded the 64-character script id the
process discovery builds. That limit predates this change and applies to every
case that names the serving process. With shorter run IDs the reads succeed.

### What it means for the system under test

Both servers announce grace, enter it once per failover, and hold it for its
configured 90 seconds, as [F-029](F029-neither-deployment-ever-lifts-grace-early-for-different.md)
found by hand. CHAOS-07, blocked since it was written, ran on both clusters on
2026-10-02 and passed: no new lock was granted inside the observed window, and
the first was granted five seconds after it ended. That is RFC 8881 Section
8.4.2.1's bar on new state, observed rather than assumed, on two deployments.

What F-022 said for an operator still holds: `kubectl logs` shows none of this,
and the file is on the export volume.

### Open

Nothing about Ganesha's format is pinned. A server writing its log in a local
time zone or with a month-first date is caught by the modification-time check
and reported, not read; one writing a format the suite does not parse at all is
reported only if a line in it would classify as a transition.

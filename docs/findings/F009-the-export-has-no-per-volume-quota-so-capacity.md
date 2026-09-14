# F-009: The export has no per-volume quota, so capacity monitoring describes the backing filesystem and not the claim

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-11, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, StorageClass `nfs` backed by
`cluster.local/nfs-provisioner-nfs-server-provisioner`, default profile (60s
lease, 90s grace) from flags. First run of OBS-06.

**Severity:** none for the data path, high for anyone who sets a capacity
threshold on this deployment. Nothing is broken; a number an operator would
reasonably monitor means something other than what its name suggests.

### What happened

OBS-06 got as far as its last assertion and failed there. Everything before it
passed, and that is the part worth reading:

- The kubelet publishes per-volume usage for this claim through the node proxy,
  so the CSI driver does implement volume statistics.
- The two sources agreed exactly, before and after the write: 0 bytes used
  each, then 134,217,728 bytes each.
- Both moved by the full 128 MiB the workload wrote, the kubelet's reading
  arriving 1m33s later, which is its documented aggregation period.

The last check is what failed. The claim is provisioned at 1 GiB and `df`
inside the pod reports a total of 10,464,788,480 bytes, which is the server's
own backing volume.

### Why

This provisioner hands out an export per claim as a subdirectory of one
filesystem, and its XFS quota option is off by default. There is no per-volume
limit, so `statfs` on the mount answers with the filesystem behind it. Both
sources read that same filesystem, which is why they agree so precisely.

### What it means for the system under test

Not a defect in the NFS server, and not a failing harness: it is a limitation of
this deployment, which is the distinction the case exists to draw. An operator
who sets an alert at 80% of the reported capacity is watching the provisioner's
10 GiB volume rather than the 1 GiB their workload was promised. A claim can
reach its own nominal size with the monitored number barely moving, and the
first sign of trouble is the application getting ENOSPC with every dashboard
reading healthy. Turning the quota option on is what changes the answer.

The same run leaves the other half of Section 3.5 open: OBS-02 failed for an
unrelated reason on this cluster and OBS-03 failed as F-008 predicts, neither of
which OBS-06 touches.

### What changed

Two things in the harness, both exposed by this run rather than by the
provisioner:

- **The agreement tolerance is now a fraction of the smaller of the claim's
  provisioned size and the capacity the workload is shown.** It had been a
  fraction of the workload's view alone, which on this deployment is the backing
  filesystem: the tolerance came out at 199 MiB against a 128 MiB write, so the
  two sources could have disagreed by more than the entire workload and still
  been reported as agreeing. On a multi-terabyte pool it would have swallowed
  anything. The assertion that caught the quota was unaffected, but the one
  above it had quietly stopped being able to fail.
- **`TestWriteBytesScriptReportsWhatLanded` skips where `stat` does not take
  `-c`.** The script runs in a pod on busybox, and the test ran it on the
  workstation, so `make unit` failed on macOS for a reason that says nothing
  about the script. It now probes first, the way the capacity parsers already
  probe for `stat -f`.

The case itself is unchanged, and stays red here. That is the point of it: an
assertion is not relaxed because one provisioner cannot meet it, and this one
would pass unchanged on a deployment whose exports carry a quota. A run against
this cluster reports OBS-06 as a failure naming the provisioner's
configuration, with F-009 as the explanation.

### What the second run settled

Re-run the same day on the same cluster with the corrected tolerance, which is
what it was there to check. The tolerance came out at 20.5 MiB, two percent of
the 1 GiB claim rather than of the 10 GiB volume behind it, and both sources
still tracked the 128 MiB write and cleared the 64 MiB movement floor. So the
agreement assertion above the quota check can now fail, and the quota check is
still the only thing failing here.

OBS-02 passed on this run, having failed on the first with a record sweep that
answered nothing. Nothing in this change reaches that case, so the first result
was environmental; it is noted because two runs of the same target disagreeing
is worth knowing when the next one is read.

# F-023: Neither NFS deployment publishes metrics as shipped, but Ganesha's exposer is compiled into one of them and is one config line away

Author: mikebz@
Created: 2026-09-15
Updated: 2026-09-15


**Found:** 2026-09-15, the first runs of OBS-07, against GKE clusters `gke-w1`
(run `20260915-200033`) and `gke-w2` (run `20260915-200048`). Substantially
revised the same day after a review question asked why
[`prometheus_exposer.cc`](https://github.com/nfs-ganesha/nfs-ganesha/blob/next/src/monitoring/prometheus_exposer.cc)
exists upstream if nothing is listening. It does, and the first version of this
entry would have left the next person to rediscover that.

**Severity:** as shipped, the channel that carries everything an operator could
know about NFS itself is empty on both clusters. Unlike the first version of
this entry implied, that is a configuration state on `gke-w2`, not a missing
capability.

### What happened

OBS-07 scrapes the server's metrics endpoint either side of a restart. On both
clusters it never got as far as the restart: no endpoint could be discovered, so
the case reported the `absent` verdict and failed, naming the deployment.

Both server pods carry **no annotations at all**, and both declare twelve
container ports, every one of them an NFS protocol port: `nfs` 2049, `nlockmgr`
32803, `mountd` 20048, `rquotad` 875, `rpcbind` 111 and `statd` 662, each in a
TCP and a UDP flavour. Nothing named for metrics, and no `prometheus.io/scrape`,
`prometheus.io/port` or `prometheus.io/path`.

### Why: two gates, and the clusters fail different ones

NFS-Ganesha has had a Prometheus exposer in tree since well before either of
these images. It is gated twice.

**Compile time.** The whole body of `src/monitoring/prometheus_exposer.cc` is
inside `#ifdef USE_MONITORING`, and the monitoring sources build into a separate
`libganesha_monitoring` shared library.

**Run time.** `src/MainNFSD/nfs_main.c` calls `prometheus_exposer__start()` only
when `nfs_param.core_param.enable_metrics` is set. From
[`ganesha-core-config.rst`](https://github.com/nfs-ganesha/nfs-ganesha/blob/next/src/doc/man/ganesha-core-config.rst):
`Enable_Metrics` is a bool defaulting to **false**, and the documentation is
explicit that it is off "even when the `USE_MONITORING` macro is defined".
`Monitoring_Port` defaults to **9587**; `Monitoring_Addr` falls back to
`Bind_Addr`.

The two clusters fail different gates:

| | `gke-w1` | `gke-w2` |
|---|---|---|
| image | `registry.k8s.io/sig-storage/nfs-provisioner:v4.0.8` | locally built `nfs-provisioner:15.3` |
| Ganesha | `V4.0.8` | `V15.3-mb` |
| `prometheus_exposer__start` in the binary | absent | **present** |
| `libganesha_monitoring.so` | not in the image | **linked into the running process** |
| `Enable_Metrics` in the generated config | n/a | **not set, so false** |
| listening on 9587 | connection refused | connection refused |

So `gke-w1` was built without monitoring and cannot publish without a rebuild.
`gke-w2` **can**, and does not only because of a default.

### Verified by turning it on

Rather than reason about it, the switch was thrown on `gke-w2`. The generated
config lives at `/export/vfs.conf` on the server's own backing volume, not in a
ConfigMap. Adding two lines to its `NFS_Core_Param` block and restarting the pod:

    Enable_Metrics = true;
    Monitoring_Port = 9587;

The provisioner does **not** regenerate that file when it already exists, so the
edit survived the restart. Port 9587 came up, and the endpoint served **39
metric families, 367 series**, including precisely the things the first version
of this entry said an operator could not see:

- `clients__confirmed_count`, `clients__lease_expire_count` — who holds state,
  and leases expiring
- `locks__count` — locks held
- `nfsv4__op_count`, `nfsv4__op_latency`, `compound__ops_count`,
  `compound__latency` — operation and error rates, per op and per status
- `rpcs_received_total`, `rpcs_completed_total`, `rpcs_in_flight`
- `ganesha_uptime_seconds`, `ganesha_build_info`

### The second gate nobody would guess

Enabling the exposer is **not sufficient** to make the deployment monitorable.
The chart templates no metrics port and no annotation, so with `Enable_Metrics`
alone the endpoint exists and no scraper can find it — not OBS-07, and not a
real Prometheus either, for the same reason. Discovery here deliberately reads
only what the pod declares (doc 06 section 6), so it kept reporting `absent`
until the StatefulSet's pod template was patched with
`prometheus.io/port: "9587"`.

Two changes are needed, and they are in different places: one in the Ganesha
config the provisioner writes, one in the chart's pod template.

### What changed

Nothing in the harness because of this entry. The `absent` verdict was correct
for both clusters as they were configured, per the uniform verdict contract in
[doc 06](../06-observability-design.md) section 4.

What the entry changes is the **wording of the claim**. The failure message and
the first version of this finding leaned on "publishes no metrics endpoint", and
a reader could reasonably take that as "this server cannot". For `gke-w2` that
is false, and
[F-022](F022-the-servers-grace-announcements-were-in-a-log-file.md) is the
standing warning about exactly this move: F-008 concluded from an absence, and
the absence was in the wrong place. The claim that survives is the narrow one:
**as configured, this pod marks no scrape target a scraper could discover, and
nothing is listening behind one either.** Both halves were checked on `gke-w2`,
the second by sweeping 9587 from the API server's pod proxy before the change.

### What it means for the system under test

An operator on `gke-w1` has no NFS-level telemetry and cannot get any without
rebuilding the image with `USE_MONITORING`.

An operator on `gke-w2` has none either, and can have all of it by setting
`Enable_Metrics = true` and annotating the pod. That is a configuration defect
in the deployment, not a limitation of the server, and it is cheap to fix.

Read next to F-008 and F-022, both channels the test plan allows for NFS-level
observability are empty as configured, and in both cases the content exists and
is being sent somewhere nobody is looking: grace announcements to a log file
inside the export, and metrics to an exposer that is never started.

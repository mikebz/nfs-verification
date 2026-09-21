# F-023: Neither NFS deployment publishes metrics as shipped, but Ganesha's exposer is compiled into one of them and is one config line away

Author: mikebz@
Created: 2026-09-16
Updated: 2026-09-17


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

The provisioner does **not** regenerate that file when it already exists (see
the chart section below), so the edit survived the restart. Port 9587 came up
and served **39 metric families in 367 series**, read through the API server's
pod proxy with no scraper deployed.

#### What it publishes

The full family list, from the `# TYPE` lines of a scrape taken
2026-09-15 on Ganesha `V15.3-mb`. This is recorded in full because the point of
the finding is that the channel is not thin, it was switched off, and a summary
would leave the next person guessing which of these exist.

**NFSv4 protocol and client state** — the channel OBS-07 exists to check, and
the part with no substitute anywhere else:

| Family | Type | Why it matters here |
|---|---|---|
| `clients__confirmed_count` | gauge | how many clients hold state; the thing F-023's first draft said was invisible |
| `clients__lease_expire_count` | counter | leases actually expiring, directly relevant to the grace and lease cases |
| `clients__per_state_protection_count` | counter | state protection in use, by `sp_how` |
| `locks__count` | gauge | locks held |
| `nfsv4__op_count` | counter | per-op, per-status operation counts, labelled by `op` and `status` |
| `nfsv4__op_latency` | histogram | per-op latency |
| `nfsv4__dropped_gss_requests_count` | counter | dropped GSS requests |
| `compound__ops_count` | histogram | ops per COMPOUND |
| `compound__latency` | histogram | COMPOUND latency |
| `session__connections_count` | histogram | connections per session |
| `session__denied_xprt_associations_count` | counter | refused transport associations |

**RPC and transport**: `rpcs_received_total`, `rpcs_completed_total`,
`rpcs_in_flight`, `libntirpc__tcp_connections_count`,
`libntirpc__svc_auth_request_latency`,
`libntirpc__gss_svc_auth_steps_latency`,
`libntirpc__gss_svc_auth_ops_latency`, `xprt__sessions_count`,
`xprt__per_custom_data_status_count`.

**Cache and id mapping**: `mdcache_cache_misses_total`,
`idmapping__cache_entries_total`, `idmapping__cache_uses_total`,
`idmapping__resolutions_total`, `idmapping__cache_entries_reaped_total`,
`idmapping__evicted_entries_cached_duration`,
`idmapping__external_request_latency`, `idmapping__user_groups_total`,
`idmapping__max_groups_exceeded`.

**Process and build**: `ganesha_build_info`, `ganesha_uptime_seconds`,
`ganesha_export_metadata`, `Threads_currently_scheduled`,
`threads_schedulable`, `connection_manager__clients`,
`connection_manager__connection_started_latencies`,
`connection_manager__drain_local_client_latencies`.

**The exposer's own**: `monitoring__scraping_latencies`.

Two notes for anyone reading a scrape of this:

- `ganesha_uptime_seconds` is the restart witness. It is what proved the process
  in [F-024](F024-identical-counters-after-a-restart-are-not-continuity.md) was
  seconds old while its counters read identical to the previous one's.
- `monitoring__scraping_latencies_*{status="success"}` does not exist on a
  freshly started server until the first scrape completes, so it shows up as 27
  series "lost within a surviving family" across a restart. That is the
  exposer measuring itself, not a server defect, and OBS-07 reports it as a
  diagnostic rather than asserting on it.

Note also that `Enable_Dynamic_Metrics` defaults to true, which is what produces
the per-client and per-export labels. Ganesha's own documentation warns it
"significantly reduces performance", so a deployment turning metrics on for
production should decide about that separately.

### The second gate nobody would guess

Enabling the exposer is **not sufficient** to make the deployment monitorable.
The chart templates no metrics port and no annotation, so with `Enable_Metrics`
alone the endpoint exists and no scraper can find it — not OBS-07, and not a
real Prometheus either, for the same reason. Discovery here deliberately reads
only what the pod declares (doc 06 section 6), so it kept reporting `absent`
until the StatefulSet's pod template was patched with
`prometheus.io/port: "9587"`.

### Neither gate is reachable from supported configuration

Both clusters run the upstream chart `nfs-server-provisioner-1.8.0` from
kubernetes-sigs, with only the image overridden. Neither gate can be opened
through it.

**No chart parameter, and no generic escape hatch.** `helm get values --all` on
the live release lists every knob the chart accepts: `affinity`, `extraArgs`,
`image`, `nodeSelector`, `persistence`, `priorityClass`, `rbac`, `replicaCount`,
`resources`, `securityContext`, `service`, `storageClass`, `tolerations`. The
128-line `values.yaml` contains no occurrence of monitor, metric, prometheus or
annotation, in 1.8.0 or on master. `templates/` has no `servicemonitor.yaml`,
and `templates/statefulset.yaml` has no annotations block at all, so there is
not even a `podAnnotations` to hang the convention on. The rendered release
contains zero occurrences of prometheus, monitor or metric.

**`extraArgs` cannot reach it.** The provisioner has twenty flags and none
concern monitoring. The only two that touch the Ganesha config at all are
`-grace-period` and `-device-based-fsids`.

**Because the config is a Go byte literal.**
[`pkg/server/server.go`](https://github.com/kubernetes-sigs/nfs-ganesha-server-and-external-provisioner/blob/master/pkg/server/server.go)
holds `defaultGaneshaConfigContents` with a fixed `NFS_Core_Param` block
(`MNT_Port`, `NLM_Port`, `fsid_device`, `allow_set_io_flusher_fail`). Only grace
period and fsid device are adjustable, and they are done by string replacement
on the file after it is written, by `setGracePeriod` and `setFsidDevice`. There
is no hook for anything else.

The same function is why the hand edit above survives a restart:

```
// Use defaultGaneshaConfigContents if the ganeshaConfig doesn't exist yet
if _, err = os.Stat(ganeshaConfig); os.IsNotExist(err) {
        err = ioutil.WriteFile(ganeshaConfig, defaultGaneshaConfigContents, 0600)
```

The default is written **only when the file is absent**. So an edit to
`/export/vfs.conf` persists across pod restarts, is invisible to Helm, and is
lost when the backing claim is recreated. Useful for an experiment, not a
configuration.

**What would actually fix it**, in two different places:

| Gate | Blocked by | Fix |
|---|---|---|
| Ganesha publishes | config template hardcoded in Go | a provisioner flag pair, `-enable-metrics` and `-monitoring-port`, applied the way `setGracePeriod` already is, which then becomes reachable from the chart's `extraArgs` |
| A scraper can find it | no `podAnnotations`, ports hardcoded | a chart change: `podAnnotations` is worth having regardless, or a templated metrics port |

Both live in the kubernetes-sigs repository. A deployment building its own image,
as `gke-w2` does, can take the first half now by changing the literal, but only
for a **fresh** export, since an existing `/export/vfs.conf` is never rewritten.

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

### What changed after this was written

**2026-09-17**: `gke-w2` no longer matches the description above. Its server pod
now carries `prometheus.io/scrape`, `prometheus.io/port` and `prometheus.io/path`
alongside the `Enable_Metrics` edit, both added by hand after this entry was
written, so the endpoint is now discoverable as well as live. The sentence
"as configured, this pod marks no scrape target a scraper could discover" was
true of `gke-w2` when it was written and is not true of it today.

Nothing here is retracted: both statements still describe the chart and the
image, which is what the entry is about, and `gke-w1` is unchanged in every
respect. What is different is that **`gke-w2` is no longer an as-shipped
deployment**, and a run against it says nothing about what an operator who
installed this chart would see. The first whole-suite run to reach OBS-07's
later steps did so only because of that drift, and what it found is
[F-025](F025-a-metric-name-with-no-sample-yet-is-not-a-lost-one.md).


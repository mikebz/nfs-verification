# F-025: A metric name the server has not sampled yet is not one it lost, and OBS-07 scrapes before any traffic has recreated them

Author: mikebz@
Created: 2026-09-17
Updated: 2026-09-17


**Found:** 2026-09-17, the first whole-suite runs against two clusters,
`gke-w1` (run `w1-e2e-20260916-2015`) and `gke-w2` (run `w2-e2e-20260916-2015`),
run IDs named in local time. Reproduced deliberately the same day on `gke-w2`
(run `w2-obs07-repro-20260917`).

**Severity:** OBS-07 reports `never-resumed`, a failure naming the deployment,
for a server whose telemetry came back intact. The verdict's size depends on how
busy the server was before the fault, which is a property of the run rather than
of the deployment.

### What happened

This was the first whole-suite run in which OBS-07 got past discovery. It failed:

    never-resumed: 1634 series under 87 names before, 367 under 66 after;
    21 names lost, 0 new; 61 counters reset, 0 advanced, 133 unchanged,
    1126 series lost under a surviving name

`ClassifyMetrics` ranks a lost metric name above everything else, so 21 lost
names decided the verdict even though the 61 resets sitting behind them are
`resumed-reset`, which is a pass and the correct description of a process that
restarted.

The 21 are not an arbitrary subset. Every one of them counts bytes, sizes a
request or a response, or counts a cache hit:

    client_bytes_received_total          nfs_bytes_received_total
    client_bytes_sent_total              nfs_bytes_sent_total
    mdcache_cache_hits_total             nfs_request_size_bytes_{bucket,count,sum}
    mdcache_cache_hits_by_export_total   nfs_response_size_bytes_{bucket,count,sum}
    mdcache_cache_misses_by_export_total
    nfs_bytes_received_by_export_total   nfs_request_size_by_export_bytes_{bucket,count,sum}
    nfs_bytes_sent_by_export_total       nfs_response_size_by_export_bytes_{bucket,count,sum}

### Why

Ganesha creates a metric family when it first records a sample into it. A
process that has served no NFS traffic has nothing to put in a byte counter or a
size histogram, so those families do not exist yet and the exposition does not
mention them. OBS-07 scrapes seconds after the replacement pod reports ready,
which is before any client has done anything through it.

Three measurements, all on `gke-w2`, say so:

| Scrape | Names | Series |
|---|---|---|
| Before the restart, after a whole suite's traffic | 87 | 1634 |
| Seconds after the restart | 66 | 367 |
| Same process, 9h later, traffic having resumed | **87** | 2190 |

The third is the one that settles it. Same pod, same UID, no further restart:
all 21 names came back and the total returned to exactly the 87 it was before.
Nothing had been lost. The scrape had simply arrived before the server had
anything to say.

Re-running the case on its own reproduces it exactly, which rules out timing
noise: 2302 series under 87 names before, **367 under 66 after** — the same 66
and the same 367 as the whole-suite run. **367 series under 66 names is this
server's cold-start set**, and what a scrape taken immediately after a restart
returns regardless of what preceded it.

That number also explains
[F-024](F024-identical-counters-after-a-restart-are-not-continuity.md). Its
server was idle, so it published the cold-start 66 on **both** sides of the
restart, saw no name loss, and reached the counter comparison — where every
comparable counter matched, because both scrapes were of a server that had done
nothing. F-024 and this entry are the same mechanism seen from either side: the
verdict OBS-07 reaches is decided by how much traffic the server had served
before the fault, and the case controls neither end of that today.

### What changed

**Nothing in the code yet, and the case stays red for now.** This entry is the
record; the harness change is the open item below, tracked as
[issue #81](https://github.com/mikebz/nfs-verification/issues/81).

The assertion itself is right and is not being relaxed: a metric name that
disappears across a restart breaks every dashboard and alert built on it, which
is exactly what the test plan asks OBS-07 to check. What is wrong is the moment
the second scrape is taken. The case must put the server back into a state
comparable with the one it measured before the fault — drive I/O through the
mount it already holds, then scrape — so that a name still missing afterwards is
a name the server really lost. Until that lands, a `never-resumed` verdict
carrying only lazily-created families in its lost list should be read against
this entry rather than filed against the deployment.

The narrower alternative, comparing only names present in both cold-start sets,
was considered and rejected: it would exclude precisely the traffic counters an
operator cares most about, and a case that asserts only over metrics nobody
watches is the green nobody earned that F-024 is about.

### What it means for the system under test

Less than the red suggests, and it is worth being exact about what is left after
the harness bug is subtracted.

`gke-w2`'s telemetry **does** come back after a restart: the endpoint answers,
the names return once traffic does, and the counters reset from zero, which is
what a restarted process is supposed to look like and what every monitoring
system already knows how to read.

What an operator does lose is the ability to distinguish, in the first moments
after a failover, "this server is serving nothing" from "this series no longer
exists" — the two look identical from a scrape, and it is the moment anyone is
looking hardest. That is a property of a lazily-created exposition and not a
defect, but it is the thing to know before writing an alert that fires on a
missing series.

Both statements are about a cluster that is **not** in its as-shipped state:
`Enable_Metrics` and the `prometheus.io` annotations on `gke-w2` were both added
by hand, per
[F-023](F023-neither-nfs-deployment-declares-a-metrics-endpoint.md). On `gke-w1`
OBS-07 still fails at discovery with `absent`, and nothing here applies to it.

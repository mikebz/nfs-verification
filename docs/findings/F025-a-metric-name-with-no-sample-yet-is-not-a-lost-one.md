# F-025: A metric name the server has not sampled yet is not one it lost, and OBS-07 scrapes before any traffic has recreated them

Author: mikebz@
Created: 2026-09-17
Updated: 2026-09-28


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

Until 2026-09-28 nothing in the code had changed and the case stayed red; this
entry was the record, and the harness change was open as
[issue #81](https://github.com/mikebz/nfs-verification/issues/81).

The assertion itself was right and has not been relaxed: a metric name that
disappears across a restart breaks every dashboard and alert built on it, which
is exactly what the test plan asks OBS-07 to check. What was wrong was the
moment the second scrape was taken. The fix for #81 changes that moment and
nothing else about what fails:

- OBS-07 now provisions a claim and a pod of its own and drives one round of
  I/O through that export before the fault: write and sync a file, stat it,
  read it back. It scrapes either side of that round, so the case controls
  the pre-fault end instead of inheriting whatever the suite happened to do.
- After the restart it takes the replacement's first answer as before, then
  drives the same round through the same mount and scrapes again. It repeats
  until every name from before is back, or a round brings back no name the one
  before it had not, inside the restart budget plus `slo.ObservationMargin`
  counted from the replacement's first answer. That is a bound of its own, the
  same length the replacement and its first answer were each given, not what
  is left of theirs.
- `ClassifyMetrics` takes all four scrapes. A name is lost only when neither
  post-restart scrape publishes it. Counter direction is still read off the
  first answer alone, because the case's own traffic would push counters
  upward and turn a restarted process into `resumed-continuous`, which is
  [F-024](F024-identical-counters-after-a-restart-are-not-continuity.md)
  reached from the other side.
- The label sets the case expects back are stated: the series its own traffic
  touched before the fault. They are reported apart from the rest of the lost
  series, which is where the `NFS4ERR_GRACE`, `NFS4ERR_BADSESSION` and
  `NFS4ERR_STALE_CLIENTID` labels from the update below land. Neither group is
  asserted, and the first run showed why (see below).
- `TestClassifyMetricsJudgesNamesAfterTraffic` is the regression test.
  `TestClassifyMetricsLostNameSurvivesTraffic` checks that a name missing even
  after traffic still fails. `TestClassifyMetricsDirectionIgnoresOwnTraffic`
  covers the continuity hazard, and `TestClassifyMetricsSaysWhichLabelsItExpects`
  the label split.

The narrower alternative, comparing only names present in both cold-start sets,
was considered and rejected: it would exclude precisely the traffic counters an
operator cares most about, and a case that asserts only over metrics nobody
watches is the green nobody earned that F-024 is about.

**Run on `gke-w2` on 2026-09-28**, exposer and annotations still hand-enabled,
`make test-case CASE=TestObsMetricsSurviveServerRestart`, Kubernetes
v1.37.0-gke.3165000 control plane with v1.37.0-gke.2941000 e2-medium nodes,
StorageClass `nfs`, profile `default` (`-lease-seconds=60 -grace-seconds=90`).
Four runs, all passing with `resumed-reset` after a single round. The third
was taken after the review fix that keeps a failed pre-fault scrape in the
bundle, and the fourth after the one that does the same for the rounds after
the restart; neither changes what a passing run does:

```
w2-issue81-obs07-20260928-025244:
resumed-reset: 1738 series under 87 names before, 369 under 67 after, 1169 under 87 once exercised
by 1 rounds of traffic; 0 names lost, 0 new; 62 counters reset, 0 advanced, 107 unchanged;
571 series lost under a surviving name, 35 of them touched by the case's own traffic

w2-issue81-obs07-20260928-025525:
resumed-reset: 1351 series under 87 names before, 369 under 67 after, 1134 under 87 once exercised
by 1 rounds of traffic; 0 names lost, 0 new; 62 counters reset, 0 advanced, 107 unchanged;
216 series lost under a surviving name, 35 of them touched by the case's own traffic

w2-issue81-obs07-20260928-030620:
resumed-reset: 1350 series under 87 names before, 369 under 67 after, 1134 under 87 once exercised
by 1 rounds of traffic; 0 names lost, 0 new; 62 counters reset, 0 advanced, 107 unchanged;
215 series lost under a surviving name, 0 of them touched by the case's own traffic

w2-issue81-obs07-20260928-032320:
resumed-reset: 1350 series under 87 names before, 369 under 67 after, 1134 under 87 once exercised
by 1 rounds of traffic; 0 names lost, 0 new; 62 counters reset, 0 advanced, 107 unchanged;
215 series lost under a surviving name, 0 of them touched by the case's own traffic
```

The first answer is the cold-start set again, give or take a name. One round
of traffic brought the name count back to exactly 87 on every run. That is the
expected reading this entry predicted and the one #81 left for the run to
decide.

The touched series that did not come back are the reason label sets are not
asserted. In the first run all 35 were `op="CREATE"`: the round files sat in a
subdirectory, so only the pre-fault round made it, and the harness was fixed
to write at the top of the mount. In the second run they were all
`op="LOOKUP"`, which the first run had brought back. In the third and fourth,
on the same harness as the second, there were none. Whether a lookup reaches
the server is the client's dentry cache deciding
([`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html)), not the server
forgetting a label, and asserting on it would have swapped this entry's false
failure over names for one over labels.

On `gke-w1` the case still fails at discovery with `absent` (run
`w1-issue81-obs07-20260928-025533`), before it provisions anything, per F-023.

### What it means for the system under test

Less than the red suggests, and it is worth being exact about what is left after
the harness bug is subtracted.

`gke-w2`'s telemetry **does** come back after a restart: the endpoint answers,
the names return once traffic does, and the counters reset from zero, which is
what a restarted process is supposed to look like and what every monitoring
system already knows how to read. Since the fix for #81 that last part is
measured rather than inferred: 62 counters reset and none advanced, on all
four runs above.

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

### Updated 2026-09-27

**Reproduced three out of three, identically after the restart.** OBS-07 ran in
three whole-suite runs on `gke-w2` on 2026-09-25
(`w2-e2e-run{1,2,3}-20260925-222955`), with the exposer and annotations still
hand-enabled. Every run returned the same verdict and the same post-restart
figures as this entry's first occurrence. Only the pre-restart series count
varied, with the traffic that came before: 1632 in run 1 and 1633 in runs 2 and
3, against 1634 the first time. Run 2:

```
never-resumed: 1633 series under 87 names before, 367 under 66 after; 21 names lost, 0 new;
61 counters reset, 0 advanced, 133 unchanged, 1126 series lost under a surviving name
```

The 21 lost names are the same byte, size and cache families listed above. The
367 series under 66 names are the same cold-start set. The 1126 series lost
under a surviving name are per-status and per-client labels. They include
`status="NFS4ERR_GRACE"`, `"NFS4ERR_BADSESSION"` and `"NFS4ERR_STALE_CLIENTID"`,
which a server returns only to clients recovering from a restart. The process
scraped before the restart had accumulated them over the chaos cases' earlier
failovers. The fresh process, scraped before any client had come back to it, had
returned none of them yet. That is the same mechanism reaching labels rather
than names. The fix in #81, which drives traffic before the second scrape,
covers both only as far as that traffic recreates them: ordinary I/O that never
meets grace will not bring the `NFS4ERR_GRACE` series back, so the case needs to
say which label sets it expects to return.

On `gke-w1`, OBS-07 failed at discovery with `absent` in all three runs, per
F-023.

"Nothing in the code yet" was still true then. It stopped being true with the fix
for #81 on 2026-09-28, described under "What changed" above.

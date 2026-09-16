# F-024: Identical counters after a restart are not continuity, and reported as continuity they are a green nobody earned

Author: mikebz@
Created: 2026-09-15
Updated: 2026-09-15


**Found:** 2026-09-15, the first end-to-end run of OBS-07 against `gke-w2` (run
`20260915-233101`), immediately after
[F-023](F023-neither-nfs-deployment-declares-a-metrics-endpoint.md) made a real
metrics endpoint available to scrape.

**Severity:** the case passed and said something false about the deployment.
That is worse than the red it replaced.

### What happened

With Ganesha's exposer enabled, OBS-07 finally ran all five of its steps. It
scraped 367 series across 66 families, deleted the server pod, waited for the
replacement, scraped again, and reported:

    resumed-continuous: 0 counters reset, 14 continuous, 0 families lost

and logged "this server keeps its counts across a restart". Both scrapes were
identical, series for series:

    rpcs_received_total:                     68 -> 68
    rpcs_completed_total:                    68 -> 68
    mdcache_cache_misses_total{lookup}:       2 -> 2

A counter that survives a process restart is unusual enough to check, and the
check showed the opposite of what the verdict claimed. The pod's UID had changed
(`93103441-…` to `c8db5e98-…`) and `ganesha_uptime_seconds` read **1** on the
second scrape. The process was seconds old. Its counters had not carried across
anything; they had reset to zero and been driven back to exactly the same
numbers.

### Why

Two conditions hold together on this deployment and both are ordinary:

1. **Startup is deterministic.** The same config with the same single export
   produces the same internal traffic every time the process boots, so
   `rpcs_received_total` lands on 68 on every start.
2. **The server is idle.** OBS-07 drives no workload on purpose — movement is
   OBS-05 and OBS-06's job — and there are no other clients, so nothing perturbs
   the counters after startup.

The classifier split counters two ways, `now < was` into reset and everything
else into continued, so equality fell into the same bucket as a genuine advance.
On any busy server that is harmless, because something will have moved. On an
idle one with a deterministic boot, every comparable counter lands in the
"carried across" bucket and the verdict inverts.

The design doc had already written down that "a counter that is equal on both
sides is reported as continuous rather than as evidence of anything". Knowing it
was not enough: the bucket name, the verdict name and the log line all went on
claiming continuity, and it took a real run to notice that the honest sentence
in the doc and the sentence the case printed were different sentences.

### What changed

`ClassifyMetrics` now splits counters three ways rather than two:
`Reset` for `now < was`, `Advanced` for `now > was`, and `Unchanged` for
equality. `MetricsResumedContinuous` requires at least one `Advanced` and no
`Reset`. A comparison where every counter merely matches gets a new verdict,
`MetricsResumedIndeterminate`, which still passes — the case's assertion is that
the metric families survive, and they did — while explicitly claiming nothing
about continuity. The bundle labels the bucket "counters unchanged, which is not
evidence of continuity".

`TestClassifyMetricsEqualityIsNotContinuity` is the regression test, carrying
the numbers above.

`TestClassifyMetricsSeparatesFamiliesFromLabels` had to stop asserting
`resumed-continuous`. When every series is relabelled there is no counter
present on both sides at all, so the honest verdict is indeterminate, and the
test now checks that the verdict is not a failure rather than pinning which pass
it is.

Re-run on `gke-w2` afterwards: `resumed-indeterminate`, 0 reset, 0 advanced, 14
unchanged. Still a pass, and now it says what it actually observed.

### The same false pass had a second cause, found in review

Fourteen counters out of 367 series is the number that should have been
questioned at the time, and was not. Review asked why `Families()` splits a
histogram into `_bucket`, `_sum` and `_count`, and the answer turned up the
larger half of the problem: the `# TYPE` line names the parent, nothing declares
a type for the three components, and the direction check looked up the exact
name. Every histogram and summary series in the scrape was therefore classified
as neither counter nor gauge and skipped.

The consequence is the same shape as the original finding, reached from the
other side. A server whose histogram counts restarted at zero — the single
clearest evidence of a process that did not carry its counts — produced no
`Reset` entry and the case reported a passing verdict. On this deployment the
unchecked series were the bulk of what the server publishes.

`isCounter` now resolves a component back through its parent's declared type.
`_sum` stays out: it is monotonic only when every observation is non-negative,
which holds for these latencies but is not something the exposition format
promises, and a series that may legitimately fall must not be read as a reset.

Re-run on `gke-w2` with that in place: **306 comparable counters instead of 14**,
all still identical, so the verdict is unchanged at `resumed-indeterminate`. The
conclusion did not move; what moved is how much of the scrape was capable of
contradicting it. A verdict that only 4% of the data could have overturned was
weaker than it read, and it read no differently.

### What it means for the system under test

Nothing yet, which is the point. The deployment's counter behaviour across a
restart is **still unmeasured**: this run could not distinguish a server that
keeps its counts from one that resets them, and no longer pretends it could.

Answering it needs traffic across the restart — a client doing work either side,
so that the counters have somewhere to move — which is a different case from
this one. Until something drives that, `resumed-indeterminate` is the correct
result on an idle deployment and not a gap to be closed by relaxing the verdict.

The general lesson is the one worth carrying: **a passing verdict derived from
two readings that are byte-identical deserves the same suspicion as a failing
one.** Equality is the shape both "nothing changed because the system is
healthy" and "nothing changed because you measured the same thing twice" take,
and on this run it was very nearly filed as the first.

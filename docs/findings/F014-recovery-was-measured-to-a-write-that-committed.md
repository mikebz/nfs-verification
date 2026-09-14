# F-014: Recovery was measured to a write that committed before the outage

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-12, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, kernel 6.12.94+, StorageClass `nfs`
backed by `nfs-server-provisioner` as a single-replica StatefulSet, profile
`default` (lease 60s, grace 90s). Runs `pr39-chaos-20260912` and
`pr39v2-chaos-20260912`, `make test-chaos`.

**Severity:** every recovery number the chaos cases have ever reported is
suspect, and the SLO comparison that uses it could not fail.

### What happened

CHAOS-05 injects the same fault five times. In one passing run it reported:

| cycle | reported |
|---|---|
| 1 | 1m43s |
| 2 | **0s** |
| 3 | 1m38s |
| 4 | 1m43s |
| 5 | **0s** |

A server pod deletion cannot recover in `0s` on a deployment that takes about
90 seconds to come back, and the same case closed with `longest gap between
attempts was 1m44s`, so the outage plainly happened in every cycle.

CHAOS-06 and CHAOS-07 printed the contradiction on adjacent lines:

```
first committed write 0s after the fault (budget 2m0s, profile default)
longest gap between attempts was 1s
```

A one-second longest gap does not mean a seamless failover. It means the
measurement **finished before the outage started**, and the gap statistic was
then computed from that same premature snapshot of the log.

Both numbers were wrong in the permissive direction: `0s` passes a two-minute
budget, so the assertion could not fail no matter how bad recovery was.

### Why

Recovery was read as `FirstSuccessAfter(faultAt)`, the first committed write
timestamped at or after the fault. Three things conspire:

- The workload writes one record per second and stamps it with `date +%s`, so
  record times have **one-second resolution**.
- `faultAt` comes from the same clock, read by an exec **before** the `DELETE`
  is issued.
- `FirstSuccessAfter` is inclusive, so a write in the same one-second bucket as
  `faultAt` — possibly committed *before* it — satisfies the wait at once.

So the poll returned on its first iteration, with a write that committed while
the server was still up, and recovery came out as zero. Whether a given cycle
reported honestly was a race between the exec round trip and the writer's
one-second cadence, which is why the same run produced both `1m43s` and `0s`.

**Moving the anchor to after the `DELETE` would not have fixed it.** A deleted
pod keeps serving through its termination grace, so a write committed in the
seconds after the call returns is still a pre-outage write. Anchoring is not the
problem; asking the wrong question is.

### What changed

The outage is now found by its **shape** rather than its timestamps. On a hard
NFSv4.1 mount a client that loses its server blocks rather than erroring
([`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html)), so an outage is a
silence in a log that is otherwise one line per second, and that silence is the
only unambiguous evidence of when service was actually lost.

- `LoadReport.StallAfter` returns the first silence that left the client with no
  progress for longer than a floor at some point **after** the fault, and
  recovery is measured to the write that ended it. `LongestGap` was already
  reporting this honestly from the same log; the recovery path simply was not
  using it.
- Only the part of a silence that falls after the fault counts towards the
  floor. The first version of this fix took any gap over the floor whose
  resuming write landed at or after the fault, which walks straight back into
  the same one-second hole in CHAOS-05: the previous cycle's outage is still in
  the log, and a fault read in the same second as its resuming write would match
  that gap and report the new cycle recovered before the fault had done
  anything. Clamping the start of the measured silence to the fault, rather than
  demanding the whole gap follow it, also keeps a client that blocks at the
  instant of the fault — last write stamped in the second before it — from being
  missed.
- `slo.LoadStallFloor` is the floor, at 10s: well above the one-to-two second
  cadence of a healthy stream, well below the 60s smallest recovery bound any
  profile states, so it separates the two without being near either.
- Both ends of a silence are committed writes. A failed attempt is not progress,
  so it neither starts nor ends an outage; pairing each record with the one next
  to it in the log would see `OK, ERR, OK` as two short intervals and miss the
  outage between them, costing the case its measurement on top of the error it
  was already going to report.
- Measuring an outage as a silence means an outage shorter than the floor cannot
  be measured at all. That is the deployment beating the SLO, so a workload that
  writes its way past the whole budget with no gap over the floor is a pass with
  a message that says recovery was too fast to observe — not a case that waits
  out the budget and then fails a deployment for recovering well.
- Cases now log what was measured, not just the result: `recovered 1m43s after
  the fault: the client made no progress for 1m41s, from record 12 to record
  13`. A reader can check the claim against the stream.
- A workload that never stalls is reported as such rather than scored as an
  instant recovery, and `LastRecord` separates that from a client still blocked
  in an outage that never ended. Those are opposite results that previously both
  arrived as silence.
- `FirstSuccessAfter` keeps its narrower meaning and now says in its doc comment
  that it is not the recovery measurement.

The unit test deliberately asserts that the **old** reading still returns the
pre-outage write, so the difference between the two questions is pinned in code
rather than argued in a comment.

**Verified on the cluster**, run `d3-stall-20260912`, `make test-chaos` against
the same cluster and profile. Every recovery the suite measured is now an honest
number, and the zeros are gone:

| case | before | after |
|---|---|---|
| CHAOS-02 | 1m42s | 1m46s |
| CHAOS-05 cycle 1 | 1m43s | 1m43s |
| CHAOS-05 cycle 2 | **0s** | 1m44s |
| CHAOS-05 cycle 3 | 1m38s | 1m45s |
| CHAOS-05 cycle 4 | 1m43s | 1m43s |
| CHAOS-05 cycle 5 | **0s** | 1m43s |
| CHAOS-06 | **0s** | 1m44s |
| CHAOS-07 | **0s** | 1m43s |

Eight measurements spanning 1m43s to 1m46s, where before they were bimodal
between a plausible number and zero. Each now states its own evidence, so the
claim can be checked against the stream rather than taken on trust:

```
recovered 1m44s after the fault: the client made no progress for 1m44s,
from record 7 to record 8 (budget 2m0s, profile default)
```

The stall duration agrees with the independently computed `longest gap between
attempts` in every case, which is the cross-check that was contradicting the old
measurement.

One consequence worth expecting: **CHAOS-05 got slower, from 443s to 547s.** The
old code was quick because on two cycles it returned without waiting for
anything. Those cycles now wait out the real outage. A case that speeds up when
a measurement is broken is a shape to watch for.

### What it means for the system under test

This is a harness defect, so the first thing it means is that **no recovery
figure from a run before `d3-stall-20260912` is evidence of anything**, in
either direction. The older `1m38s`–`1m44s` readings look right in hindsight,
but they came from a measurement that demonstrably returns the wrong answer
under a race, and a number that is right by luck is not a measurement. The `0s`
readings were never evidence of a fast failover; they were the absence of one.

What the verification run does give is the first trustworthy statement about
this deployment: **a delete of the single-replica server pod costs the client
1m43s to 1m46s of blocked I/O**, tightly clustered across eight faults, against
a 2m0s budget on the `default` profile. No committed write was lost in any of
them, and no I/O error was returned, which is what a `hard` mount is supposed to
do. So the deployment is inside its budget, with about 15 seconds of headroom.

That headroom is the part worth watching rather than celebrating. The budget on
this profile is grace + 30s, and the measured recovery sits just under it
because the outage is dominated by the 90-second grace period this server never
announces (F-008). A deployment tuned to the `tuned` profile would be asserting
against a 60s target that these numbers would not meet, so the pass here is a
statement about the `default` profile and not about the storage system in
general.

The general rule: **a measurement that can only be wrong in the permissive
direction will never fail, so nothing about it looks broken.** Every green
recovery assertion in this suite passed for two weeks while this was live. When
a measured quantity has an obvious reading and a correct one, the obvious one
needs a test that pins the difference, and a number printed next to the evidence
that contradicts it is the cheapest way to notice.

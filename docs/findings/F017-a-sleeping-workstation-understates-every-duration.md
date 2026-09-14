# F-017: A sleeping workstation understates every duration the suite measures about itself

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-13, run `pr42v2-chaos-20260913`, `make test-chaos` against
GKE cluster `gke-w1`, driven from a macOS workstation.

**Severity:** nothing measured is wrong, but the run reads as though the harness
has a false-positive bug, and it costs an hour to work out that it does not.

### What happened

CHAOS-05 injects five faults and measures recovery after each. It reported:

```
recovered 1m43s ... from record 4 to record 5
recovered 1m43s ... from record 6 to record 7
recovered 1m46s ... from record 799 to record 800
recovered 1m43s ... from record 1681 to record 1682
recovered 1m43s ... from record 2609 to record 2610
5 failovers took 4m41s in total
```

Two things look impossible. Five outages of about 1m45s cannot fit inside 4m41s.
And the record indices jump by hundreds between cycles, where the same case on
the previous run went 4, 7, 10, 12, 14.

This was chased as a suspected regression in the recovery measurement, which had
just been rewritten (F-014) and had just had a stale-gap bug found in review. It
is neither.

### Why

The workstation went to sleep part-way through the run.

- `time.Since` uses Go's **monotonic** clock. On macOS that clock does not
  advance while the machine is asleep, so `5 failovers took 4m41s` counts only
  the time the laptop was awake.
- The workload runs **in a pod**, on a node that never slept. It kept writing
  one record per second throughout. The jump from record 7 to record 799 is
  thirteen minutes of a perfectly healthy writer, while the test process was
  frozen mid-poll.

Both halves are consistent once that is known: 2610 records is 2610 seconds of
writing, and the case's own accounting of 4m41s is simply the part of it the
workstation was awake for.

### What changed

Nothing in the harness, and that is the point of recording it.

The recovery numbers from that run are **correct**, because both ends of the
measurement are taken from the pod's clock: `PodNow` for the fault and the
record's own stamp for the resumption. That is a decision the chaos design took
deliberately — one clock, the pod's — and this run is what it buys. A
measurement anchored on the workstation would have reported five recoveries of
well under a second and passed.

The README now says to run long suites under `caffeinate -is` on macOS.

### What it means for the system under test

Nothing. It is a fact about the machine driving the suite, recorded because a
future run will hit it and see what looks exactly like a harness that has
started inventing outages.

**Durations the suite reports about itself, and durations it measures inside a
pod, are not the same kind of number. Only the second kind survives a laptop
lid.**

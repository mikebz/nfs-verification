# F-011: A record sweep came back empty from an exec that reported success

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-12, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, StorageClass `nfs` backed by
`cluster.local/nfs-provisioner-nfs-server-provisioner`, default profile (60s
lease, 90s grace) from flags. CHAOS-06 and CHAOS-07, in run
`pr36-chaos-20260912`.

**Severity:** high, as a harness defect. It stops the durability assertion from
reaching a verdict, and the message it stopped with pointed at the data rather
than at the harness.

**Mechanism: open.** This entry records the shape and what has been ruled out.
It does not claim a cause.

**Intermittent.** It did not reproduce on the next run against the same cluster
with the same flags, an hour later: `pr37-chaos-20260912` swept 15 of 15 and 5
of 5, all correct, in the same two cases. Two runs is not a rate, and nothing
here says what makes the difference. A run that sweeps cleanly is not evidence
that this is fixed, and no change so far has tried to fix it.

### What happened

Both cases failed at the same place, which is the first assertion after the
fault:

> sweeping committed records from verifier: the sweep answered for 0 of 14
> records, so what it did say cannot stand for the set

CHAOS-07 said the same for 5 records. CHAOS-02, in the same run and through the
same helper, swept 4 records and got 4 correct.

Nothing else in either case had gone wrong yet. CHAOS-06's own
`byte-ranges-before-failover` subtest had passed, the workload reported a
longest gap of 1s and no I/O errors, and the verifier pod was `Running`, ready,
with no restarts and no deletion timestamp.

### Why

Unknown. What is established:

- Zero results **and** zero unparsed lines means stdout held nothing but
  whitespace, because the parser keeps every line it cannot read.
- The exec reported success. `MustSh`, which this used at the time, returns
  stdout only when the stream returned no error, so the shell was not reported
  as having failed and was not reported as having been cut off.
- `verify-records.sh` prints exactly one line per index on every path through
  its loop, including a directory that does not exist. That is now held down by
  `TestVerifyRecordsScriptAnswersForEveryIndex`, which runs the real script
  under a real shell. So "the records were missing" does not produce this; it
  produces fourteen `absent` lines.
- The only way the script itself prints nothing is being handed no indices at
  all, and the caller returns early on an empty set and counted fourteen.
- The sweep code, the script and the parser are untouched by anything that
  landed between the 2026-09-11 full-suite run, where the same two cases swept
  15 and 5 records successfully with identical `0s` recovery and `1s` gap
  readings, and this one.
- It is not a slow or hung mount. A `hard` mount with no server blocks, and
  both cases returned in well under two minutes.

What is left is the exec returning success with an empty stdout for a command
that did run, or for one that never started. Neither has been demonstrated.

The reason this was not diagnosed on the spot is the message. It named a count
and nothing else: no exit status, no stderr, no indication of whether anything
arrived at all. The artifact bundle was no better, because the verdict table is
only written on the success path, so the run that most needed evidence filed
none.

### What changed

- `VerifyRecords` runs the sweep through `Sh` rather than `MustSh`, so the
  short-answer error carries both streams with their lengths and states that the
  exec itself reported no error. The message says that a short answer is the
  sweep failing to run or its output being lost rather than a verdict about the
  data.
- The raw output goes into the bundle as `record-sweep-raw.txt` whenever the
  sweep fails, which is exactly when the verdict table is empty and useless.
- `TestVerifyRecordsScriptAnswersForEveryIndex` holds the script to one line per
  index across a missing directory, an empty one and a mixed set. Without it the
  new message is an assertion nobody checked, and a genuine lost record could be
  filed as a harness problem.
- `TestShortSweepErrorNamesWhatItSaw` holds the message to naming the pod, the
  directory, both counts and both stream lengths.

None of that has yet been exercised by a real short sweep, because the failure
has not recurred. What the next occurrence produces is the evidence this entry
is waiting on: `record-sweep-raw.txt` in the bundle, and a message saying how
many bytes arrived on each stream.

### What it means for the system under test

Nothing, so far, and that is the point of writing it down. No verdict was
reached about the records, so neither case says anything about durability on
this deployment. A reader of that run's log should not take the failure as data
loss, and should not take the pass of the other chaos case as covering it.

The general rule: **a sweep that answered for nothing is not a sweep that found
nothing.** Any check that reads a verdict out of a pod has to be able to tell
those apart, and has to file what the pod actually said.

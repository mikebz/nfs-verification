# F-016: Concurrent O_APPEND from four clients loses a quarter of the records, and tears none

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-27


**Found:** 2026-09-13, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, kernel 6.12.94+, StorageClass `nfs`
backed by `nfs-server-provisioner`, profile `default`. DATA-02 has now run nine
times on that cluster and six times on `gke-w2`:

| Run | Cluster | Target | Result |
|---|---|---|---|
| `e2e-full-20260911` | `gke-w1` | `make test-e2e` | 150 of 200, 0 torn |
| `full-e2e-20260912b` | `gke-w1` | `make test-e2e` | 150 of 200, 0 torn |
| `pr46-data-20260913` | `gke-w1` | `make test-data` | 200 of 200 |
| `pr47-data-20260913` | `gke-w1` | `make test-data` | 200 of 200 |
| `pr47-e2e-20260913` | `gke-w1` | `make test-e2e` | 150 of 200, 0 torn, `appender0` lost records 1-50 |
| `w1-e2e-20260916-2015` | `gke-w1` | `make test-e2e` | 150 of 200, 0 torn, `appender3` lost records 1-50 |
| `w2-e2e-20260916-2015` | `gke-w2` | `make test-e2e` | 150 of 200, 0 torn, `appender2` lost records 1-50 |
| `w2-e2e-run1-20260918-211600` | `gke-w2` | `make test-e2e` | 200 of 200 |
| `w2-e2e-run2-20260918-211600` | `gke-w2` | `make test-e2e` | 150 of 200, 0 torn, `appender2` lost records 1-50 |
| `w1-e2e-run1-20260925-222953` | `gke-w1` | `make test-e2e` | 150 of 200, 0 torn, `appender3` lost records 1-50 |
| `w1-e2e-run2-20260925-222953` | `gke-w1` | `make test-e2e` | 200 of 200 |
| `w1-e2e-run3-20260925-222953` | `gke-w1` | `make test-e2e` | 150 of 200, 0 torn, `appender3` lost records 1-50 |
| `w2-e2e-run1-20260925-222955` | `gke-w2` | `make test-e2e` | 200 of 200 |
| `w2-e2e-run2-20260925-222955` | `gke-w2` | `make test-e2e` | 150 of 200, 0 torn, `appender3` lost records 1-50 |
| `w2-e2e-run3-20260925-222955` | `gke-w2` | `make test-e2e` | 200 of 200 |

**Updated 2026-09-17**: the last two rows are the first whole-suite runs against
a **second** cluster, `gke-w2`, on a different server image (locally built
Ganesha V15.3-mb rather than the upstream v4.0.8), run concurrently with the
`gke-w1` run above. Both lost fifty records, neither tore one, and in each case a
single appender lost its entire contribution — a different appender each time,
which is the third distinct one across the five reds and rules out anything
particular to `appender0`. Whole-suite runs are now **5 for 5** and data-only
runs remain 0 for 2. Reproducing on a second deployment makes a defect peculiar
to one server build the least likely explanation, and the client-side mechanism
described below the most likely.

**Updated 2026-09-27**: eight more whole-suite runs. There were two on `gke-w2` on
2026-09-18 (#86), and six on 2026-09-25, three per cluster, with both clusters
running at the same time. Four of the eight lost fifty records, again whole and
from one appender, and four lost nothing. Two things the entry said above no
longer hold:

- **Whole-suite runs do not reliably reproduce it.** They are now 9 red out of
  13. The 2026-09-18 pair was the same binary against the same cluster an hour
  apart, and it disagreed. On 2026-09-25, three runs that were identical in shape
  passed. "The chaos cases leave the caches stale" is not what separates a red
  run from a green one. The data-only runs are still 0 of 2, too few to mean
  anything.
- **It is not a different appender each time.** Of seven reds that named the
  loser, `appender3` lost four times, `appender2` twice and `appender0` once.
  The case places pods with `nodes[i%len(nodes)]` over a name-sorted list, so on
  these three-node clusters, `appender0` and `appender3` share a node, and
  `appender2` shares one with the reader. `appender1`, the only pod alone on
  its node, has never lost. That is 7 of 7 losses on a shared client. With three
  of four appenders on shared nodes, chance alone gives that about one time in
  eight. It fits the client being the node rather than the pod (F-019), but it
  is not established. The failure message does not name each appender's node,
  so the next red run cannot confirm it from the message alone.

**Severity:** a property of this deployment, for the boundary discussion. It is
not a protocol violation and must not be filed against the server as one.

### What happened

DATA-02 puts four pods on the worker nodes, each appending fifty short records
to one file through a single descriptor held open for the whole loop, and reads
the file back from a pod that never wrote to it:

```
the file holds 150 lines, want 200 from 4 appenders writing 50 records each; 150 whole, 0 torn
```

Fifty records lost. **Zero torn.** By 2026-09-13 that had happened three times,
with the same count every time, and on the third the message named what went:

```
the file holds 150 lines, want 200 from 4 appenders writing 50 records each; 150 whole, 0 torn. Missing: appender0 lost 50 of 50 (records 1-50)
```

**One appender's entire contribution, contiguously, and none of the other
three's.** Not fifty singles scattered across four writers, which would have
been a different mechanism and a worse one. Three of the four appenders landed
every record they wrote; the fourth landed none. Every red since has had the
same shape (see the table).

In between, it passed twice on the same cluster and the same day with every
record present. **The loss is intermittent, and a green DATA-02 does not clear
this deployment** — it means the race did not fire that time.

At that point all three reds were whole-suite runs and both greens were
data-only runs. The entry proposed that 3-2 split as a hypothesis: in
`make test-e2e` the chaos cases run before the data cases and delete the server
pod repeatedly, so DATA-02 there starts against a server that has recently
failed over, and the appenders' cached sizes are that much staler. **That
hypothesis has since failed.** Whole-suite runs are now 9 red of 13, identical
runs disagree, and run shape does not separate red from green (see the
2026-09-27 update above).

Those two numbers point in opposite directions and both matter:

- **0 torn** is the assertion that holds under any reading of the protocol. Every
  record that arrived, arrived whole. Nothing is corrupt.
- **150 of 200** is the count, which the test plan already flags as stronger
  than NFSv4.1 promises.

### Why

NFSv4.1 has no append operation. There is no `WRITE` that means "at end of
file": [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 18.32
takes an explicit offset. A client implements `O_APPEND` by writing at the
offset it believes to be the end of the file, and that belief is a cached
attribute.

With one descriptor held open for the whole loop there is nothing to refresh it.
The Linux client's cache consistency is close-to-open
([`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html)): it revalidates
size on **open**, not before each write. Four clients each believing the file
ends where it ended when they opened it will write the same offsets, and the
last writer wins. A record is lost, not torn, which is exactly the pattern
observed.

That the harness holds one descriptor open is deliberate, and documented in the
case: it is the shape of a real log appender, and it is the harder case. A
client that reopened per record would revalidate each time and mostly avoid
this.

The measured shape narrows it further, and this is what the per-appender
reporting was added to find out. If each client were racing each other write by
write, the losses would be scattered across all four writers, and roughly even.
Instead one writer lost all fifty of its records and the other three lost none.
That is the signature of a client whose belief about the end of the file never
moved for the whole run: `appender0` wrote every record at offsets three other
clients had already claimed, and each of its writes was overwritten in turn. It
is one client out of step, not four clients interleaving badly.

What this does not settle is why that client and not another, or why the same
four pods land every record on other runs.

### What changed

Nothing about the assertion. The case stays red and keeps routing the failure to
the boundary discussion rather than to the server owner, because a suite that
relaxed this would have nothing left to say about append behaviour anywhere.

What changed is what the failure tells you. It now names the missing records per
appender, because at the time the claim was deleted at teardown and the artifact
bundle kept pod logs rather than the share: whatever the message said was the
only evidence the run left behind. One appender's whole contribution gone is a
different story from scattered singles across all four, and the old message
could not tell them apart.

**Updated 2026-09-14**: that last constraint is gone. The bundle now keeps the
files a case names as its evidence, on a pass as well as a failure, and DATA-02
names the appended file, so the run leaves the file the count was taken from and
not only the count. The per-appender message stays: it is the reading the case
did, and it is what reaches whoever triages the failure before they open
anything. The next red run should be triaged against both, since the file can
answer questions the message was never asked — where in the file the survivors
sit, and whether the lost writer's offsets were claimed by one other client or
several.

### What it means for the system under test

**An application that appends to a shared file from several pods on this storage
class will silently lose records, and will not notice.** No error is returned to
any writer; every `write` succeeds. Nothing is corrupted, which is the good news
and also why nothing downstream catches it. That it does not happen on every run
makes it worse rather than better: a workload can do this for months and then
lose a quarter of a file.

This is worth saying to a platform owner plainly, because "RWX" invites exactly
this design. The options are a single writer per file, a file per writer, or an
application-level lock around the append — DATA-05 covers that last one, and it
passes here.

Answered, by `pr47-e2e-20260913`: the loss is one appender's whole contribution,
contiguous, and the message printed it the first time it went red after being
added. The diagnostic did its job, which is the only reason this entry can say
"one client out of step" rather than "fifty records short".

Still open: what makes the difference between a run that loses fifty records and
a run that loses none, and why the client that falls out of step is the one it
is. The run-shape hypothesis this paragraph proposed testing has since failed
(see the 2026-09-27 update above). The one pattern left is placement: every
named loser shared its node with another pod on the share. The next thing to
try is the same case with one pod per node, which needs five nodes, or with
`appender3` moved to a node of its own.

**A count that the protocol does not promise is still worth asserting, as long
as the failure says who it belongs to.**

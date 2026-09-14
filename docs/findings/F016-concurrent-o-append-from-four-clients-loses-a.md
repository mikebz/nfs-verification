# F-016: Concurrent O_APPEND from four clients loses a quarter of the records, and tears none

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-13, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, kernel 6.12.94+, StorageClass `nfs`
backed by `nfs-server-provisioner`, profile `default`. DATA-02 has now run five
times on this cluster:

| Run | Target | Result |
|---|---|---|
| `e2e-full-20260911` | `make test-e2e` | 150 of 200, 0 torn |
| `full-e2e-20260912b` | `make test-e2e` | 150 of 200, 0 torn |
| `pr46-data-20260913` | `make test-data` | 200 of 200 |
| `pr47-data-20260913` | `make test-data` | 200 of 200 |
| `pr47-e2e-20260913` | `make test-e2e` | 150 of 200, 0 torn, `appender0` lost records 1-50 |

**Severity:** a property of this deployment, for the boundary discussion. It is
not a protocol violation and must not be filed against the server as one.

### What happened

DATA-02 puts four pods on the worker nodes, each appending fifty short records
to one file through a single descriptor held open for the whole loop, and reads
the file back from a pod that never wrote to it:

```
the file holds 150 lines, want 200 from 4 appenders writing 50 records each; 150 whole, 0 torn
```

Fifty records lost. **Zero torn.** Three times now, with the same count every
time, and on the third the message named what went:

```
the file holds 150 lines, want 200 from 4 appenders writing 50 records each; 150 whole, 0 torn. Missing: appender0 lost 50 of 50 (records 1-50)
```

**One appender's entire contribution, contiguously, and none of the other
three's.** Not fifty singles scattered across four writers, which would have
been a different mechanism and a worse one. Three of the four appenders landed
every record they wrote; the fourth landed none.

In between, it passed twice on the same cluster and the same day with every
record present. **The loss is intermittent, and a green DATA-02 does not clear
this deployment** — it means the race did not fire that time.

All three reds were whole-suite runs and both greens were data-only runs, which
is 3-2 and still a hypothesis rather than a cause, though a harder one to
dismiss than it was at 2-2. It is testable: in `make test-e2e` the chaos cases
run before the data cases and delete the server pod repeatedly, so DATA-02 there
starts against a server that has recently failed over, and the appenders' cached
sizes are that much staler. Nothing yet rules out plain timing.

Those two numbers point in opposite directions and both matter:

- **0 torn** is the assertion that holds under any reading of the protocol. Every
  record that arrived, arrived whole. Nothing is corrupt.
- **150 of 200** is the count, which the test plan already flags as stronger
  than NFSv4.1 promises.

### Why

NFSv4.1 has no append operation. There is no `WRITE` that means "at end of
file": [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 18.2
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
appender, because the claim is deleted at teardown and the artifact bundle keeps
pod logs rather than the share: whatever the message says is the only evidence
the run leaves behind. One appender's whole contribution gone is a different
story from scattered singles across all four, and the old message could not tell
them apart.

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
is. The whole-suite runs are 3 for 3 and the data-only runs are 0 for 2, so the
next thing to try is a data-only run immediately after a chaos run, which
separates "the server recently failed over" from "the suite has been running a
while".

**A count that the protocol does not promise is still worth asserting, as long
as the failure says who it belongs to.**

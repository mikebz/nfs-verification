# F-007: Two cases in the data path phase reported results they had not measured

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-11, GKE cluster `gke-w1` with three workers, first real run of
the step 6 cases from [PR #14](https://github.com/mikebz/nfs-verification/pull/14).

**Severity:** high for the suite, none for the cluster. Both cases were green,
and neither was wrong about anything it claimed. They were wrong about how much
they had claimed.

### What happened

Every case in the phase that could run did: DATA-05 through DATA-09 and DATA-12
and DATA-13 passed, DATA-11 skipped, DATA-14 skipped for want of an image, and
CHAOS-06 passed with both its byte-range subtests. Mount tables on all three
nodes were at baseline afterwards and nothing leaked.

Two results did not say what they appeared to say.

**DATA-12 verified three records and DATA-13 verified four.** Both passed.

**DATA-11 reported SKIP for thirteen seconds of work.** The sparse write, the
zero-filled hole, the byte past it and the logical size had all been asserted
and had all passed before the hole-punch probe stopped the case.

### Why

The durability pair inherited its pre-fault warm-up from the recovery cases,
which wait for three committed writes and then inject. That is the right warm-up
for a case measuring an *interruption*: what matters there is that the workload
was demonstrably running when the fault landed, and the assertion is about the
gap, not about the records.

The durability pair asserts over the records themselves, so the length of the
set is the resolution of the measurement. At one record per second, three
records is a three second window. DATA-13 is the worse of the two: its entire
job is to record how many un-fsynced records came back absent or short, and over
four records on a server whose host page cache survived the SIGKILL it was
always going to report four correct and document nothing. A pass there looks
identical to a pass over a set large enough to mean something.

DATA-11 was one case with two halves gated by different things, and Go reports a
case, not a half. The punch probe correctly found that the Alpine image's
busybox `fallocate` parses `-l` and `-o` only, and correctly reported blocked
rather than filing a tool gap as a protocol gap. But `t.Skipf` marks the whole
case skipped whatever ran before it, so the sparse result went in the bin with
it.

### What changed

The warm-up moved into `awaitRecords`, and the durability pair asks for thirty
records rather than three. Thirty is a minute of load before the fault; nothing
asserts on the number, and it is cheap against a case that already waits out a
server restart. `requireDurabilitySet` fails either case whose set came back
under ten, because a pass over a handful of records is worse than a failure: it
looks the same as a real result.

DATA-11 is now two subtests, `sparse-write-and-read-back` and `hole-punch`, so
the half that can run on a busybox image reports its own result and only the
half that cannot reports blocked.

### What it says about the harness, not the system under test

Nothing about NFS. It says that "the case passed" and "the case measured
something" are different claims, and that the suite had no way to tell them
apart in either of these shapes.

The same pattern had already been caught twice in review on this branch, in
DATA-05 and CHAOS-06, where a mount option blocking one half would have blocked
a case that could still run the other. DATA-11 was the third instance and was
missed because its two halves are gated by an image rather than by a mount
option. A case with halves that can be blocked separately reports them
separately; that is now true of every such case in the phase.

The set-size problem is the more general one, and it has no structural fix here.
A case that asserts over a set it produced has to state how large that set must
be to carry its conclusion, and fail below it. Only the durability pair does
that today.

### What the run settled

Four things the design had left open, recorded here because they are facts about
a real image and a real cluster rather than decisions:

- **busybox `dd` on `alpine:3.20` does carry `oflag=direct`.** DATA-07 passed.
  Section 5.4 of the design holds, and `locktool` does not need a `dio`
  subcommand.
- **busybox `fallocate` does not carry `-p`**, as predicted, so the hole punch
  is unreachable on the default image whatever the mount version is.
- **A lock held by a force-deleted pod came back in 2.27 seconds**, which
  DATA-06 classified as the descriptor-close path rather than lease expiry. On
  this cluster kubelet is prompt, so an application losing a lock to a dying pod
  is back in about a second; losing a *node* is the case that costs a lease, and
  that is CHAOS-03 and SEC-07.
- **The clone volume did not disturb the dynamic claim that owns the export.**
  DATA-08 passed and open question 6 of the design is answered for this driver.

Still unmeasured: DATA-10, which did not run, so whether 100k entries fit on a
directory-backed export is still open; and DATA-14, which needed `-fio-image`.

*Amended 2026-09-11:* DATA-14 was deferred rather than run. It is not a gap
waiting on an image any more, it is a case this suite is not doing yet, and
Section 3.2 of the test plan says why.

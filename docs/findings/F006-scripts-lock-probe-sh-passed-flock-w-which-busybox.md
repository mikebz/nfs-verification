# F-006: `scripts/lock-probe.sh` passed `flock -w`, which busybox does not have

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-11, by reading the applet sources while writing
[`05-data-path-and-locktool-design.md`](../05-data-path-and-locktool-design.md).
Not found by a run: on a workstation and on the default `alpine:3.20` tools
image the probe works, because both carry the util-linux `flock`.

**Severity:** high for the suite, none for the cluster. It makes a merged case
pass while asserting nothing.

### What happened

`lock-probe.sh` ran `flock -w "$wait" -x 9` for each attempt.
`hold-flock.sh`'s own comment two files away already said busybox `flock` has
no timeout flag, and the probe contradicted it.

### Why it matters

busybox `util-linux/flock.c` parses `-s`, `-x`, `-u` and `-n` and nothing else.
On an image carrying the applet, every attempt exits on a usage error before a
lock is ever requested, so every record in the probe log is `ERR`.

CHAOS-07 asserts **on grants**: no grant inside the grace window, and at least
one after it. An all-`ERR` log satisfies the first half for free. The second
half would have failed, which is the only reason this was ever going to be
noticed, and it would have been read as "the server never resumed granting new
state" rather than as a broken probe.

### What changed

The attempt is now a retry loop around `flock -n`, bounded by the same
`wait-seconds` argument. `-n` is carried by busybox and by util-linux alike, so
one loop serves both images, and the `timeout` bound around the attempt is
unchanged.

`TestScriptsUseOnlyPortableFlockOptions` now reads every embedded script and
rejects any `flock` option outside busybox's four. The two existing probe tests
could not catch this: they run against the workstation's `flock`, which accepts
`-w`, so a portability defect has to be asserted against the option set rather
than against behaviour.

### What it says about the harness, not the system under test

Nothing about NFS. It says that "assume nothing beyond busybox" is a rule the
repository states and had no check for. Every case that reports on grants can
pass vacuously if its instrument never gets as far as asking, and an instrument
that fails the same way on every attempt looks exactly like a quiet system.

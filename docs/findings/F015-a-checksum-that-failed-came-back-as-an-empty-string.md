# F-015: A checksum that failed came back as an empty string, and a success

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-13, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, kernel 6.12.94+, StorageClass `nfs`
backed by `nfs-server-provisioner` as a single-replica StatefulSet, profile
`default` (lease 60s, grace 90s). Run `full-e2e-20260912b`, `make test-e2e`,
the whole suite: 25 pass, 6 fail, 4 skip.

**Severity:** a case could compare two checksums, get two empty strings, and
pass having verified nothing.

### What happened

PROV-07 failed with a message that is its own bug report:

```
prov_test.go:800: checksum mismatch: got 27b6c42aeab701165276442cccea836fdfda33a1b64205eb20a9a40be56aeb9f want
```

`want` comes from `WriteFile`, which writes a file and returns its sha256. It
returned an **empty string and no error**. The case had passed on the two
previous runs of the same code.

The case was right to fail, and it failed for the wrong reason: nothing was
wrong with the data as far as anyone can tell. What it caught was its own
helper.

### Why

The script ended `sha256sum "$f" | cut -d' ' -f1`.

A POSIX pipeline reports the status of its **last** command. When `sha256sum`
fails, `cut` reads nothing, prints nothing, and exits zero. So:

- the pipeline exits zero, and `set -e` never fires;
- the exec is a success, so the caller never looks at `Combined()`, and
  `sha256sum`'s complaint on stderr is discarded unread;
- `MustSh` trims empty stdout and returns `("", nil)`;
- `WriteFile` hands that back as a checksum.

Reproducible on a workstation in one line, and now asserted in
`TestSumReportsAFailedChecksum`:

```
$ sh -c "set -e; sha256sum /tmp/not-here | cut -d' ' -f1"; echo "exit $?"
sha256sum: /tmp/not-here: No such file or directory
exit 0
```

The near-miss is the interesting part. `ReadDirect` and `CountNonZeroBytes` in
the same file already carried comments warning about exactly this — *"a POSIX
pipeline reports its last command's status, so piping a failed read into a
counter yields a confident zero"* — and both keep their `dd` out of a pipeline
for that reason. `ReadDirect` then ended its own script with `sha256sum | cut`,
which recreated the very pattern its comment had just described: a command whose
failure matters, followed by one that does not care, one line below the `dd` the
comment was protecting.

`CountNonZeroBytes` did not have a checksum pipeline; it ends with
`tr -d '\000' < block | wc -c`, and `wc` would likewise report a confident zero
if `tr` could not read. That one is safe, but only because the `dd` above it
runs as its own command where `set -e` can see it fail, which is the whole
point: the guard is the shape of the script, not the tools in it.

### What changed

- `sumCmd` is the last command of every script in the package that produces a
  checksum: `sha256sum` alone, nothing downstream. Its status is now the
  script's status, so a failure arrives as a failure with stderr attached.
- `parseSum` splits the digest off in Go and refuses anything that is not 64 hex
  characters, naming the pod and the path. An empty answer is an error, not
  data.
- Five call sites: `WriteFile`, `Sha256`, `WriteDirect`, `ReadDirect`, and the
  `locktool` integrity check, which could previously blame the exec stream for a
  `sha256sum` that never ran.
- `TestSumReportsAFailedChecksum` runs the generated command under a real shell
  against a missing file and asserts a non-zero exit. Restoring the `| cut` form
  fails it.

### What it means for the system under test

**Not yet known, and that is the finding's open item.** Something made
`sha256sum` fail on the share at that moment, and the evidence went to a stderr
nobody kept. PROV-07 writes immediately after the server pod has been deleted
and replaced, so a stale handle or a transient error on the freshly recovered
mount is the obvious suspect — and it is only a suspect. A later read of the
same file in the same pod produced a digest, so the file was there.

If this is a real post-failover error on the client, it is a deployment finding
worth having. The reason it is not one today is that the harness threw the
evidence away, which is the same reason F-011 is still unexplained: an exec that
reports success with empty output, where the interesting half was on stderr.
This entry does not claim F-011 has the same cause; it does show that the shape
is producible without anything going wrong at the exec layer at all.

**A pipeline is not a chain of assertions. Only its last command can fail it,
so nothing that matters may be followed by something that does not care.**

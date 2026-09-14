# F-021: A root-owned file reads back as nobody for a reason that is not squash

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-13, hand probes against GKE cluster `gke-w1`, confirmed by
`make test-sec` run `20260913-171950`.

**Severity:** SEC-02 passed while printing a sentence about this deployment that
was wrong in a way that would send somebody to the export configuration. Fixed
below; the failure mode is worth keeping because nothing in the run was red.

### What happened

A file written as root through the mount is uid 0 on the server's own
filesystem, read directly inside the server pod. The same file read from any
client reports 65534:65534. A file written as uid 1234 reports 1234 on both
sides.

### What root squash is, and why an export would want it

Root squash is a server-side rule: a request arriving as uid 0 is rewritten to an
unprivileged identity, conventionally `nobody` (65534), before the server acts on
it. It exists because NFS trusts the client to state who the user is. `AUTH_SYS`,
the `sec=sys` this deployment uses, puts the uid in the request and the server
believes it. So anyone with root on any machine that can reach the export can
claim to be root on the server's files, and squashing is the server declining to
extend its own root to a client's root. It is the default on almost every NFS
server for that reason, and it is turned off — `no_root_squash` — when a
workload genuinely needs to own files as root, which on a shared RWX volume means
accepting that every node that can mount it can do the same.

### Why this is not that

NFSv4 carries owners as strings, not as integers. This server answers with a name
where one exists in its passwd database and with a numeric string where none
does. So `root` goes out as `root@domain`, the client's idmapper has no mapping
for that domain and substitutes nobody; `1234` goes out as `1234`, which the
client parses as a number and keeps.

The visible result — root's writes appearing as nobody — is exactly what root
squash looks like from a client, and it is not root squash: the rewriting is the
*client's* idmapper failing to resolve a name, after the server has already
stored the file as uid 0. The export block says `Squash = no_root_squash`, and
the server's own filesystem agrees with it.

### What changed

SEC-02 was rewritten, in the same PR that recorded this, to probe the operation
instead of the display. It no longer derives anything from the number `stat`
prints: it asks whether a `chown` is permitted, which is a question the idmapper
cannot answer wrongly. An ordinary uid being refused a `chown` of its own file is
asserted outright, because that holds on every server; root being permitted or
refused is compared against `-root-squash` only when that flag states the intent,
and is required to be the same on every client either way.

The previous version would have failed a correctly configured export the moment
anyone passed `-root-squash=off`, because it read 65534 as the server's answer.
A run with exactly that flag now passes and prints both halves at once: root's
`chown` permitted on both nodes, and the file root wrote displaying as
`65534(nobody):65534(nobody)`.

### What it means for the system under test

The deployment does not squash root, and a suite that says it does is reporting
a client-side identity mapping as a server-side policy. **Any claim about squash
needs both ends: what the server stored, and what a client makes of it.**

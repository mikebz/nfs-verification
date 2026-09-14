# F-019: The client an NFS server can name is the node, and it arrives IPv4-mapped

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-13, hand probes against GKE cluster `gke-w1` while designing
the security cases, single-stack IPv4, and confirmed by the first `make test-sec`
run.

**Severity:** it decides what a per-client export rule can say, and it is a trap
for anything that reads the server's socket table.

### What happened

With two pods on two nodes mounting one claim, the server's socket table shows
the two **node** addresses, 10.138.15.232 and 10.138.0.17, and neither pod
address (10.28.3.91, 10.28.0.206). The connections appear in `/proc/net/tcp6` as
`::ffff:10.138.15.232`, with a reserved source port. `/proc/net/tcp` on its own
shows no NFS connections at all.

### Why

The mount is made by the node's kernel in the host network namespace, not by the
pod, so the pod's address is never on the wire; the reserved source port is the
kernel's. The server binds `:::2049`, and a socket bound to the IPv6 wildcard
accepts IPv4 clients and reports them in the IPv4-mapped form, in the tcp6 table.

### What changed

`ServerConns` reads both `/proc/net/tcp` and `/proc/net/tcp6` and fails if it
cannot read the pair; `PeersOn` compares unmapped addresses. A reader of only the
first file would have reported a busy server as one nobody is talking to, and the
cases that read the table would have failed a healthy deployment.

SEC-04 was one of those cases when this was written and is no longer: review
replaced it with a behavioral case that reads nothing from the server, on the
grounds that the address is a precondition and not an observable consequence.
SEC-06 and SEC-08 still read the table, and this entry is why they read both
files.

### What it means for the system under test

**The finest client an export rule can name is a node, and it covers every
workload scheduled there.** Two clients are distinguishable, which is better than
the proxy degradation the security phase was written to look for — kube-proxy
does not rewrite the source address here — but no rule can separate two pods, and
on this deployment there are no rules at all (F-018).

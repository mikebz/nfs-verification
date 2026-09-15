# F-023: Neither NFS deployment declares a metrics endpoint, so the only NFS-level telemetry channel is empty

Author: mikebz@
Created: 2026-09-15
Updated: 2026-09-15


**Found:** 2026-09-15, the first runs of OBS-07, against GKE clusters `gke-w1`
(run `20260915-200033`) and `gke-w2` (run `20260915-200048`).

**Severity:** the channel that carries everything an operator could know about
NFS itself is not merely thin here, it is absent. OBS-07 is red on both clusters
and stays red.

### What happened

OBS-07 scrapes the server's metrics endpoint either side of a restart. On both
clusters it never got as far as the restart: no endpoint could be discovered, so
the case reported the `absent` verdict and failed, naming the deployment.

The two clusters are not the same deployment. `gke-w1` runs
`registry.k8s.io/sig-storage/nfs-provisioner:v4.0.8`; `gke-w2` runs a locally
built `nfs-provisioner:15.3` from an Artifact Registry repository. Both server
pods carry **no annotations at all**, and both declare twelve container ports,
every one of them an NFS protocol port: `nfs` 2049, `nlockmgr` 32803, `mountd`
20048, `rquotad` 875, `rpcbind` 111 and `statd` 662, each in a TCP and a UDP
flavour. Nothing named for metrics, and no `prometheus.io/scrape`,
`prometheus.io/port` or `prometheus.io/path`.

### Why

The chart templates the ports the protocol needs and nothing else, and the Go
provisioner serves no Prometheus registry that anything points at. There is no
misconfiguration to correct: this is what the reference deployment is.

### What this entry does and does not claim

[F-022](F022-the-servers-grace-announcements-were-in-a-log-file.md) is the
reason to be careful here, and it is the direct precedent: F-008 concluded from
an absence, and the absence turned out to be in the wrong place — the grace
lines existed all along, in a log file the container's stdout never carried.

So the claim is deliberately the narrower one: **this pod marks no scrape target
that a scraper could discover**, neither under the `prometheus.io` annotation
convention nor as a port named for metrics. Whether some process inside the
container serves an exposition endpoint on a port nobody declared is a different
question, and this case cannot answer it — but neither can an operator's
scraper, which is the question OBS-07 exists to ask. Telling the two apart would
need an exec into the server container and a port sweep from inside, which is
the same unresolved shape F-022 left behind for grace.

### What changed

Nothing in the harness. The verdict is the finding, per the uniform verdict
contract in [doc 06](../06-observability-design.md) section 4: a deployment that
does not publish what a case verifies fails and names the deployment, rather
than skipping. A capability that turned this into a silent skip would hide it on
exactly the clusters where it matters.

Discovery reads what the deployment declares and never sweeps undeclared ports,
for the reason above: an endpoint found by probing is one the monitoring an
operator actually runs would not have found.

### What it means for the system under test

An operator on either cluster has **no NFS-level telemetry whatsoever**. What
remains is the outside channel: the container lifecycle, and the kubelet's
volume statistics. During a failover they can see that a pod restarted. They
cannot see that NFS failed over, how many clients were holding state, whether
any reclaim succeeded, or what the operation and error rates were on either side
of it.

Read next to F-008 and F-022, both of the channels the test plan allows for
NFS-level observability are empty as configured: grace is announced only to a
log file inside the export, and metrics are not published at all. Neither gap
requires a different NFS server to close — a metrics port and a log destination
are both deployment configuration — which is what makes them findings about this
deployment rather than about NFS.

# F-002: 2GB worker nodes cannot host the suite

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-10, GKE cluster, `e2-small` node pool (2GB RAM).

**Severity:** medium. It does not produce a wrong result, it produces node
reboots that look like storage failures and cost a day to attribute.

### What happened

On `e2-small` workers, the GKE system daemons alone (fluentbit, gmp-collector,
gke-metrics-agent, filestore-node, pdcsi-node) account for roughly 87% of
requested memory and well over 100% of limits. Adding test pods, their image
pulls and the page cache from a 1MiB write pushed nodes into kernel memory
pressure:

```
virtio_balloon: Out of puff! Can't get 1 pages
systemd-journald: Under memory pressure
```

Kubelet heartbeats then dropped, MIG health checks fired, and nodes rebooted
mid-run.

### Why it matters to the results, not just the runtime

A rebooting node is indistinguishable, from inside a case, from the failure
modes the plan is actually hunting: I/O that stalls, a mount that does not come
back, a lock that is not reclaimed. A suite that cannot tell an undersized node
from a storage defect produces findings nobody can act on.

This is the same class of problem as Appendix C item 4 in the test plan, which
already requires node auto-repair and auto-upgrade to be off: if the platform is
restarting nodes underneath the run, chaos results are invalid.

### What changed

- The node agent now sets resource requests (10m CPU, 32Mi) so it is not
  BestEffort and not the first thing evicted. No limits: it must not be OOM
  killed while a case is reading the node it is inspecting.
- Minimum node size is stated in the README: at least 4GB per worker,
  `e2-medium` or larger on GKE.

  **Corrected 2026-09-14.** This bullet claimed the README said so from the day
  it was written, and the README did not say it until today; an audit found it,
  not a reader who needed it, which is what being cited nowhere buys. The bullet
  also asked for 8GB (`e2-standard-2`) for anything beyond the presubmit cases.
  Nothing has ever measured that, and the whole suite has since run end to end
  on three `e2-medium` workers, twice over, so the 8GB half is dropped rather
  than written into the README on no evidence.

### Open

The node pool requirement belongs alongside the other cluster preconditions in
Section 0 of the test plan, as something preflight could check rather than
something a person has to remember. Allocatable memory per node is readable from
the node status, so a preflight warning is cheap. Not done yet.

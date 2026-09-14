# F-004: `allowVolumeExpansion` is a claim, not a capability

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-10, GKE cluster with e2-medium worker nodes, running PROV-04 while reviewing
[PR #3](https://github.com/mikebz/nfs-verification/pull/3).

**Severity:** low for the cluster, medium for the suite. It makes one case fail
for a reason that is not obvious from its failure.

### What happened

The StorageClass under test sets `allowVolumeExpansion: true`, so preflight
recorded `canExpand` and PROV-04 took the expansion path. The resize request was
accepted by the API and then nothing happened. The case failed with:

```
claim capacity did not reach 2Gi: timed out after 5m0s
```

### Why

`allowVolumeExpansion` on a StorageClass is what the class asserts, not what its
provisioner can do. The in-cluster `nfs-server-provisioner` behind that class
performs no expansion, and no external-resizer sidecar was running to act on the
request, so the claim sat with the larger request and the smaller status
forever. No resize condition was ever posted, because nothing was resizing.

### What changed

`WaitPVCCapacity`'s timeout message now distinguishes the three answers. A
resize condition present means expansion is in progress or is waiting on
something (a pod restart, for instance). **No condition at all** means nothing
acted on the request, and the message now says so and names the two things to
check: whether the provisioner supports expansion, and whether an
external-resizer sidecar is running.

### What it says about capability discovery

`Caps.CanExpand` is optimistic by construction, because the only thing the
Kubernetes API offers is the class's own assertion. Every other capability in
the suite is probed; this one is declared, and it is the one that lied. The case
still fails rather than skipping, which is right: a class advertising a
capability its provisioner lacks is a real deployment defect, and a claim
accepted for a resize that never happens is a trap for any workload, not only
this suite. What the case owed was a failure message that says where to look.

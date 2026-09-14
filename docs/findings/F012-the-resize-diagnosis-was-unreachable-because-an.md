# F-012: The resize diagnosis was unreachable because an unrelated condition was always there

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-11, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three workers on Container-Optimized OS, StorageClass `nfs` backed by
`cluster.local/nfs-provisioner-nfs-server-provisioner`, default profile from
flags. PROV-04 and PROV-11, in run `e2e-full-20260911`.

**Severity:** medium. Nothing is asserted wrongly. What is lost is the
explanation, on the two cases whose failure most needs one, and what replaced it
points at the wrong thing.

### What happened

Both expansion cases timed out and said:

> claim `nfsv-prov-04-e2e-full-20260911-prov04` reports 1Gi, want at least 2Gi
> [Unused=False(A pod is currently referencing this PVC)]

PROV-11 said the same. The bracket is meant to hold the resize condition the
driver left behind, or, where there is none, the F-004 diagnosis: that the class
advertises `allowVolumeExpansion`, nothing picked the request up, and the place
to look is whether an external-resizer sidecar exists at all.

That diagnosis never appeared, and the condition that appeared instead has
nothing to do with expansion. A reader chasing `Unused=False` is chasing the
fact that the claim is mounted, which is exactly what the case arranged on
purpose.

### Why

`describeResizeConditions` listed every condition in `status.conditions` and
fell back to the diagnosis only when the list came out empty. This cluster posts
an `Unused` condition on a claim a pod references, so the list is never empty on
a mounted claim, and every claim these two cases expand is mounted by design.
The fallback was unreachable on the only path that reaches it.

Whether `Unused` is upstream Kubernetes or a GKE addition has not been
established, and it does not matter to the fix: the helper must not assume that
every condition on a claim is about the thing it is describing.

### What changed

- `describeResizeConditions` selects the four expansion conditions
  (`Resizing`, `FileSystemResizePending`, `ControllerResizeError`,
  `NodeResizeError`) instead of listing everything.
- Conditions it does not recognise are named, so a reader knows the claim was
  not condition-free, but kept out of the verdict, so they do not read as the
  reason for the timeout.
- `TestDescribeResizeConditions` covers the claim carrying only the unrelated
  condition, using the exact condition the cluster posted. Reverting the filter
  makes it fail and reproduces the original message verbatim.

Verified on the cluster, 2026-09-12, run `pr38-prov-20260912` against `gke-w1`.
Both cases still time out, and both now print the diagnosis:

> no resize condition was ever posted on the claim: nothing acted on the
> request. The StorageClass advertises allowVolumeExpansion, so check whether
> its provisioner supports expansion at all and whether an external-resizer
> sidecar is running alongside the CSI driver. The claim does carry conditions
> that say nothing about expansion: Unused

### What it means for the system under test

Nothing new. F-004 stands unchanged: this provisioner advertises
`allowVolumeExpansion` and does not perform one, and the two timeouts in that
run are that behaviour, not a harness fault. The only thing that was wrong is
that the run did not say so.

The general rule: **a fallback branch guarded by "the list is empty" is a
fallback that something else gets to disable.** Where the interesting answer is
the absence of a signal, select the signal rather than counting everything.

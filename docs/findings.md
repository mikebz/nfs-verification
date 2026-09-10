# Findings

Things learned by running the suite against a real cluster that are worth
remembering. Each entry says what happened, why, what changed in the code, and
what it implies for the system under test as opposed to the harness.

New entries go at the top.

---

## F-001: Force-deleting a mounted pod can take a node out of service

**Found:** 2026-09-10, cluster `gke-w1` (GKE v1.37.0, Container-Optimized OS,
`e2-small` node pool), reviewing the first three cases on
[PR #1](https://github.com/mikebz/nfs-verification/pull/1).

**Severity:** high. The failure is not a failed test, it is a node that stops
accepting work, and the harness caused it.

### What happened

Teardown deleted the case's pods with `GracePeriodSeconds: 0` and then deleted
the claim.

```go
// pkg/framework/framework.go, before the fix
pods.DeleteCollection(ctx, DeleteNow(), ListOptions(f.Selector()))
// ... immediately followed by
PersistentVolumeClaims(Namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, ...)
```

### Why it wedges the node

1. A force delete removes the Pod object from the API at once. It does not wait
   for kubelet on the node to stop the containers or unmount the volume.
2. The pod is gone from the API, so the wait that follows returns immediately
   and the claim is deleted next.
3. The provisioner destroys the export while the node's kernel still holds an
   active mount of it.
4. The mount is NFSv4.1 and `hard`, so the kernel retries the RPCs
   indefinitely rather than returning an error. The retry is uninterruptible.
5. That blocks kubelet's volume manager, which stops further mounts on that
   node, disturbs PLEG, and hangs anything that reads the mount table,
   including `cat /proc/mounts`.

The ordering is the whole bug. Every step after the force delete behaves
exactly as documented; the force delete simply removed the only thing that was
keeping the export alive until the unmount finished.

### What changed

- Teardown deletes pods gracefully (`metav1.DeleteOptions{}`) and waits for them
  to leave the API before touching any claim. Test pods carry
  `terminationGracePeriodSeconds: 5`, so this costs seconds, not minutes.
- `Framework.DeletePod` is the graceful delete a case should use when it is done
  with a pod.
- `Framework.DeletePodNow` still force-deletes, because some cases need to model
  a client that vanished without unlocking (DATA-06, SEC-07). Its doc comment
  now says plainly that it must never be used for teardown.
- Artifact collection bounds each node's inspection separately
  (`nodeInspectTimeout`), so a node in this state cannot starve the bundle for
  the healthy nodes. This is how the state gets diagnosed next time.

### What it says about the system under test, not the harness

The harness triggered this, but the hazard belongs to the architecture, and the
plan already predicts the shape of it: the server is a singleton in the data
path, and a hard mount blocks rather than failing. Deleting an export out from
under a live mount is therefore not a recoverable client error, it is an
indefinite stall on the client node.

Worth carrying into later work:

- PROV-03 asserts the API-level protection: a claim that a pod still mounts
  stays `Terminating` until the mount is gone. That protection is what stands
  between a normal `kubectl delete pvc` and this failure, so it deserves the
  presubmit gate it has.
- A case for the uglier path is worth adding once the chaos vector lands:
  destroy the export while a node holds the mount, and assert what the node
  does. The honest expected result is an indefinite stall, so the assertion is
  about blast radius (does kubelet keep serving other pods?) and about whether
  the state is observable, not about recovery.
- Node recovery after this state is a reboot in practice. Any run that hits it
  should treat the node as spent.

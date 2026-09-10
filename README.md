# nfs-verification

End-to-end verification of NFS RWX persistent volumes on Kubernetes.

- [`doc/01-test-plan.md`](doc/01-test-plan.md) is the test plan: what gets
  verified and why.
- [`plan.md`](plan.md) is the implementation plan: the test approach, and the
  order the plan gets built in.
- [`doc/findings.md`](doc/findings.md) records what running the suite against a
  real cluster taught us.

This repository currently holds the harness skeleton, preflight, and the first
three cases. The remaining cases land in the steps listed in `plan.md`.

## Layout

| Path | Contents |
|---|---|
| `pkg/slo` | Timing and correctness targets, and the two lease/grace profiles |
| `pkg/env` | The environment record written to `artifacts/<run-id>/environment.json` |
| `pkg/framework` | Clients, per-case fixture, pods and PVCs from embedded manifests, exec, locks, the privileged node agent, artifact collection |
| `pkg/framework/manifests` | The YAML the suite applies: the client pod and the node agent DaemonSet |
| `pkg/preflight` | Section 0 checks and all discovery |
| `test/e2e` | The cases, named for their plan ID |
| `cmd/preflight` | `make preflight` |

## Running

```sh
make unit                                            # harness unit tests, no cluster
make preflight FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
make test-e2e  FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
```

The suite creates no namespaces. Everything lands in `default`, named and
labelled per case so that teardown deletes exactly what the case made. The node
agent is privileged by design, so a cluster enforcing a restricted Pod Security
level on `default` cannot run the suite as it stands.

Preflight runs once per cluster. A passing result is cached at
`artifacts/preflight-<context>.json`, keyed by kubeconfig context, and reused by
later runs for `-preflight-max-age` (8h by default). Pass `-refresh-preflight`
to redo it, or `-env-file` to point at a specific record.

Nothing runs until preflight passes. Preflight writes
`artifacts/<run-id>/environment.json`; a failed case writes its own bundle under
`artifacts/<run-id>/<CASE-ID>/` with pod logs, Kubernetes Events, `/proc/mounts`
and dmesg from every involved node, and the injected-fault timeline.

## Cases in this repository so far

| ID | Case |
|---|---|
| PROV-01 | Dynamic provision, bind, mount, write, delete, backing volume reclaimed |
| DATA-03 | Close-to-open across two nodes |
| DATA-05 | flock mutual exclusion across two nodes, clean handover on release |

All three are fast enough for a presubmit. Categories become a `go test -run`
pattern when there are slow cases to keep out of the fast path.

## Flags

Everything discoverable is discovered. These exist because a cluster cannot
answer them:

| Flag | Needed for | Why it cannot be discovered |
|---|---|---|
| `-server-namespace`, `-server-selector` | identifying the NFS server pods | without it the suite falls back to a heuristic (port 2049, or a known server name) and records that it guessed |
| `-lease-seconds`, `-grace-seconds` | every timing assertion | only discoverable when the server exposes them in its pod spec or a mounted ConfigMap |
| `-storage-class` | pinning the class under test | optional: preflight otherwise probes each class by asking it for an RWX volume and mounting it from two nodes |
| `-pvc-size` | claim size | defaults to 1Gi: a backing volume that cannot satisfy the request fails every case, and no case here needs more |
| `-refresh-preflight` | forcing rediscovery | the cached result is reused until it ages out |
| `-tools-image` | client pods and the node agent | needs `dd`, `sha256sum`, `flock`, `stat` and `nsenter`; defaults to `alpine:3.20`, whose busybox carries all five |

Lease and grace must match one of the two profiles in `pkg/slo`: tuned (20s/30s)
or default (60s/90s). A third value fails preflight rather than silently
invalidating every timing assertion.

## Two details that bite on real clusters

**StorageClasses that bind on first consumer.** GKE's Filestore classes, and
plenty of others, set `volumeBindingMode: WaitForFirstConsumer`. Such a claim
never binds until a pod referencing it is scheduled, so the suite creates the
pod first and checks the bind afterwards. For the same reason a pinned pod
carries a `kubernetes.io/hostname` node selector rather than `nodeName`:
`nodeName` bypasses the scheduler, and the annotation that triggers binding is
one the scheduler writes.

**Teardown deletes pods gracefully, on purpose.** Force-deleting a pod that
still has the share mounted removes it from the API before kubelet unmounts; the
claim then goes, the export is destroyed, and the node retries RPCs against it
forever on a hard mount. That takes the node out of service. See F-001 in
[`doc/findings.md`](doc/findings.md).

**Lease and grace often are not discoverable.** A server that keeps them in a
config file the pod spec does not reference will fail preflight, and the run
needs `-lease-seconds` and `-grace-seconds`. That is deliberate: a default value
here would silently invalidate every timing assertion.

## State of this code

The harness compiles, `go vet` is clean, and the unit tests in `pkg/slo` and
`pkg/framework` pass. The three end-to-end cases have not been run against a
real cluster from this repository yet; they need one with an RWX-capable
StorageClass and a reachable NFS server workload. Expect the first run to
surface flag values that need setting for the deployment at hand, which is what
the specific preflight failure messages exist to make quick.

# nfs-verification

End-to-end verification of NFS RWX persistent volumes on Kubernetes.

- [`doc/01-test-plan.md`](doc/01-test-plan.md) is the test plan: what gets
  verified and why.
- [`plan.md`](plan.md) is the implementation plan: the test approach, and the
  order the plan gets built in.

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
make preflight   FLAGS="-server-namespace=nfs -server-selector=app=nfs-server"
make test-presubmit FLAGS="-server-namespace=nfs -server-selector=app=nfs-server"
```

The suite creates no namespaces. Everything lands in `default`, or in
`-namespace`, and is named and labelled per case so that teardown deletes
exactly what the case made. The node agent is privileged by design, so a
namespace enforcing a restricted Pod Security level will reject it; preflight
says so and names the remedy.

Nothing runs until preflight passes. Preflight writes
`artifacts/<run-id>/environment.json`; a failed case writes its own bundle under
`artifacts/<run-id>/<CASE-ID>/` with pod logs, Kubernetes Events, `/proc/mounts`
and dmesg from every involved node, and the injected-fault timeline.

## Cases in this repository so far

| ID | Case | Gate |
|---|---|---|
| PROV-01 | Dynamic provision, bind, mount, write, delete, backing volume reclaimed | presubmit |
| DATA-03 | Close-to-open across two nodes | presubmit |
| DATA-05 | flock mutual exclusion across two nodes, clean handover on release | presubmit |

## Flags

Everything discoverable is discovered. These exist because a cluster cannot
answer them:

| Flag | Needed for | Why it cannot be discovered |
|---|---|---|
| `-server-namespace`, `-server-selector` | identifying the NFS server pods | without it the suite falls back to a heuristic (port 2049, or a known server name) and records that it guessed |
| `-lease-seconds`, `-grace-seconds` | every timing assertion | only discoverable when the server exposes them in its pod spec or a mounted ConfigMap |
| `-storage-class` | pinning the class under test | optional: preflight otherwise probes each class by asking it for an RWX claim |
| `-tools-image` | client pods and the node agent | needs `dd`, `sha256sum`, `flock`, `stat` and `nsenter`; defaults to `alpine:3.20`, whose busybox carries all five |

Lease and grace must match one of the two profiles in `pkg/slo`: tuned (20s/30s)
or default (60s/90s). A third value fails preflight rather than silently
invalidating every timing assertion.

## State of this code

The harness compiles, `go vet` is clean, and the unit tests in `pkg/slo` and
`pkg/framework` pass. The three end-to-end cases have not been run against a
real cluster from this repository yet; they need one with an RWX-capable
StorageClass and a reachable NFS server workload. Expect the first run to
surface flag values that need setting for the deployment at hand, which is what
the specific preflight failure messages exist to make quick.

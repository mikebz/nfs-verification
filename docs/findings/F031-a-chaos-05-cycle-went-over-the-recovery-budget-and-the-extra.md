# F-031: A CHAOS-05 cycle went over the recovery budget, and the extra time came after grace, not in the restart

Author: mikebz@
Created: 2026-10-02
Updated: 2026-10-02


**Found:** 2026-10-02. CHAOS-05 ran on GKE cluster `gke-w2`
(`nfs-provisioner:15.3`, Ganesha V15.3-mb; control plane v1.37.0-gke.3165000,
kubelet v1.37.0-gke.2941000, 3× e2-medium) with `make test-case
CASE=TestChaosRepeatedFailover RUN_ID=w2-issue19-TestChaosRepeatedFailover-20261002-033751
FLAGS="-context=gke-w2 -storage-class=nfs -lease-seconds=60 -grace-seconds=90"`,
on the `default` profile. This was the re-run after the review of #135. The
times below come from the bundle: `fault-timeline.json`, `grace-cycle{1..5}.txt`
(the server's own `/export/ganesha.log`, read by the observer
[F-030](F030-reading-the-servers-log-file-took-two-corrections.md) describes)
and the writer's `load-chaos05.log`.

**Severity:** none for the cluster, none for the suite, medium for the
deployment. No data was lost: all three committed records survived. The cost
is a recovery number, which is the cost
[F-029](F029-neither-deployment-ever-lifts-grace-early-for-different.md)
predicted.

### What happened

Cycle 4 of 5 recovered in 2m1s. The budget on the `default` profile is 2m0s,
so the case failed. This is the first time a timeline was kept for a cycle near
the budget:

| Cycle | Grace entered after the delete | Grace lasted | Writer resumed after grace ended | Recovery |
|---|---|---|---|---|
| 1 | 8s | 90s | 5s | 103s |
| 2 | 12s | 90s | 1s | 103s |
| 3 | 6s | 90s | 7s | 103s |
| **4** | **3s** | **90s** | **28s** | **121s** |
| 5 | 3s | 90s | 13s | 106s |

F-029 explained its 119s cycle as grace plus a 29-second restart. It had no
timeline to check that against (#101). This cycle had the quickest restart of
the five. Grace ran its usual 90 seconds and ended on the server's timer. The
extra 23 seconds or so came after the server left grace, before the writer's
next record. The writer runs on node `gke-w2-default-pool-5035b2f7-68nj`.

On `gke-w1`, the same build of the suite ran at the same moment and all five
cycles passed (`w1-issue19-TestChaosRepeatedFailover-20261002-033749`, 9m5s).
That run kept no bundle to compare.

### Why

Not established. The server was out of grace and had been serving for 28
seconds before this client wrote. So the time was spent in the client, or
between the client and the server.

One candidate is how the Linux client handles a refusal during grace, but it
has not been confirmed. When the server answers `NFS4ERR_GRACE`, the client
waits and retries, and it doubles the wait each time, up to 15 seconds
(`nfs4_delay`, `NFS4_POLL_RETRY_MAX` in `fs/nfs/nfs4proc.c`). A refusal near
the end of grace could leave the client in a 15-second wait after grace has
ended. Two operations that each wait, such as an open and then the write behind
it, could account for 28 seconds. That is this client's behaviour, not
something the protocol says. RFC 8881 Section 8.4.2.1 says what the server may
refuse during grace, not how soon a refused client must try again.

The node's `dmesg` cannot settle this. Its `-T` timestamps are not aligned to
wall time on these nodes, so its `server OK` lines cannot be placed against the
grace window.

### What changed

Nothing in the code. The budget stays where `pkg/slo` puts it, for the reason
F-029 gives. If the suite loosened the bound because one client backs off, it
would stop being able to say whether any client recovers in time. The case is
red for this run. The failure is the deployment's, client side included, and
not the harness's.

### What it means for the system under test

- On these deployments, recovery after a server pod delete is the restart, plus
  grace, plus however long the client waits before it retries. F-029 counted
  the first two. On this run the third ranged from 1 to 28 seconds, and that
  range alone is enough to put a cycle over the budget.
- The fault is not on the server side: the server came back quickly and held
  grace for exactly as long as it was configured to. An operator looking only
  at the server's log would see a 93-second outage. The application saw 121
  seconds.
- The headroom F-029 warned about is gone on `gke-w2`. A `default`-profile run
  of CHAOS-05 there should be expected to fail now and then until the 90s grace
  period is shortened or the client's wait is explained.

### Open

- Whether the 28 seconds is the client's retry wait after `NFS4ERR_GRACE`. To
  confirm it, a trace on the client node around the end of grace would show
  each refused call and when it was retried, for example the node's RPC debug
  log or a packet capture. The harness collects neither.

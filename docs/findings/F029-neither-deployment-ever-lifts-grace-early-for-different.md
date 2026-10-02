# F-029: Neither deployment ever lifts grace early, for different reasons, so every recovery is grace plus restart

Author: mikebz@
Created: 2026-09-27
Updated: 2026-10-02

> **2026-10-02:** narrowed by
> [F-031](F031-a-chaos-05-cycle-went-over-the-recovery-budget-and-the-extra.md).
> The first cycle over the budget to keep a timeline restarted in 3s. Its extra
> time came after grace ended, before the client's next write, not in the
> restart this entry blames for the 119s cycle below.


**Found:** 2026-09-26, reading both servers' `/export/ganesha.log` after three
whole-suite runs per cluster on 2026-09-25 (`w1-e2e-run{1,2,3}-20260925-222953`,
`w2-e2e-run{1,2,3}-20260925-222955`). Clusters `gke-w1` (upstream
`nfs-server-provisioner` v4.0.8) and `gke-w2` (`nfs-provisioner:15.3`, Ganesha
V15.3-mb), both with `Grace_Period = 90` in `/export/vfs.conf` and Ganesha's
default 60s lease, profile `default`.

**Severity:** none for the cluster, none for the suite, medium for the
deployment. No data is at risk; the cost is to every recovery number the suite
reports. The headroom under the default profile's 2m0s budget is whatever
the pod restart leaves, and one cycle left one second.

### What happened

Across the six runs the servers entered grace 94 times, 45 on `gke-w1` and 49 on
`gke-w2`. 93 of those grace periods ran for 90 or 91 seconds. The 94th, on
`gke-w2` at 23:16:08Z, was cut short a second after it began by a second
restart, which started a grace period of its own. Neither server ever lifted
grace early, that is, ended it before the timer because reclaim was complete.
The reason differs by deployment.

### Why

**`gke-w1`, Ganesha 4.0.8, keeps grace for its full length even when every
client it knows has reclaimed.** A typical window:

```
23:03:10  NFS Server Now IN GRACE, duration 90
23:03:10  check grace:reclaim complete(0) clid count(2)
23:03:20  check grace:reclaim complete(2) clid count(2)
   …      (unchanged, every 10s)
23:04:40  NFS Server Now NOT IN GRACE
```

Both clients were done ten seconds in, and the server stayed in grace for
another eighty.

**`gke-w2`, Ganesha 15.3, could end grace early, and never can, because it
waits for a client that no longer exists.** Its recovery store has four client
records for three nodes:

```
/export/v4recov/
  ::ffff:10.138.0.13-(47:Linux NFSv4.1 gke-w2-default-pool-a5180872-4dtw)   Sep 10 23:45
  ::ffff:10.138.0.22-(47:Linux NFSv4.1 gke-w2-default-pool-a5180872-4dtw)   Sep 26 02:04
  ::ffff:10.138.15.239-(47:Linux NFSv4.1 gke-w2-default-pool-6b4278f5-9e9c) Sep 26 02:05
  ::ffff:10.20.2.1-(47:Linux NFSv4.1 gke-w2-default-pool-5035b2f7-68nj)     Sep 26 02:05
```

Node `a5180872-4dtw` was recreated at 2026-09-11 00:13Z, half an hour after
the old record was last written, and came back as `10.138.0.22`. The record for its old address is still there, and every start
logs a failed attempt to clean it up (`Failed to rmdir … Directory not empty
(39)`). In all 49 windows, the reclaim count reached two of four at most, and
grace ended on its timer.

### What changed

Nothing. The budget stays where `pkg/slo` puts it, because relaxing it for a
deployment that holds grace for its full length would make the number
meaningless for one that does not. The 107s outliers below cannot be explained
after the fact, because a passing case keeps no fault timeline (#101).

### What it means for the system under test

Recovery is therefore grace plus restart, on both deployments, as simple sums:

| Fault | Recovery observed | = grace | + restart |
|---|---|---|---|
| SIGKILL of `ganesha.nfsd` (CHAOS-01 in four of six runs, DATA-12, DATA-13) | 92–93s | 90s | about 2s for the supervisor to respawn it |
| SIGKILL of `ganesha.nfsd` (CHAOS-01 in `w1-e2e-run2` and `w2-e2e-run1`) | 107s | 90s | about 17s, unexplained: the grace window itself was 90s, and a passing case keeps no timeline to show where the other 15s went (#101) |
| Server pod delete (CHAOS-02, -05, -06, -07, OBS-02, -03) | 102–106s | 90s | about 13s to reschedule and start |
| Worst single cycle (CHAOS-05, `w2-e2e-run1`, cycle 4) | **119s** | 90s | about 29s |

The budget is 120s. A pod restart that takes 30 seconds instead of 13 fails a
chaos case for a reason that has nothing to do with NFS. The earlier worst,
1m58s on 2026-09-17, was recorded in the test plan as "worth watching rather
than acting on". This is what it was watching.

F-028's lock loss happened in one of these windows. The stale record is not why
that client was late, but it is why nothing in the log could say grace was
complete: on this server, "every client reclaimed" is never true.

- **On these servers the recovery floor is the configured grace period.** An
  operator who wants faster failover has one lever, `Grace_Period`, and the
  tuned profile (20s/30s) is how the plan expresses that. Reclaim finishing
  early buys nothing on either build as deployed.
- **Replacing a node leaves `gke-w2`'s server waiting for it forever.** The
  recovery store is on the export PVC and outlives the pod. Nothing prunes a
  record for a client that will never return.
- For the harness: a grace period that ends early and one that runs out look the
  same from the client. Only the server's log tells them apart, which is one
  more reason to settle how cases may read it (F-022, #21).

### Open

- Whether `gke-w2` ends grace early once the stale directory is removed. That
  is a change to the deployment, and it has not been made.
- Where the other 15s went in the two 107s kills. A passing case keeps no fault
  timeline (#101).

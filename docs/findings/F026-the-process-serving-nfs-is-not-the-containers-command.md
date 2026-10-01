# F-026: The process serving NFS is not the container's command, and the restart count cannot see it die

Author: mikebz@
Created: 2026-09-19
Updated: 2026-10-01


> **Refined by [F-027](F027-a-container-cannot-read-the-file-descriptors-of-its.md).**
> The join described under What changed read the server's file descriptors from
> inside its pod, which fails on a server that has been up for more than a few
> minutes. Discovery now reads the socket in the pod and the process holding it
> on the node, through the node agent.

**Found:** 2026-09-18, GKE cluster `gke-w2`, Kubernetes v1.37.0-gke.2941000,
server `nfs-provisioner:15.3` (Ganesha V15.3-mb), while asking why CHAOS-01,
DATA-12 and DATA-13 had been blocked on every run against this deployment.

**Severity:** none for the cluster, high for the suite, none for the
deployment. It is the difference between three cases that never run and three
that do, and, taken the other way, between a SIGKILL aimed at the NFS server and
one aimed at the supervisor that restarts it.

### What happened

The three cases that inject an in-place kill had reported blocked on both
reference deployments since they were written, with:

```
blocked: cannot tell which process serves NFS in pod nfs-provisioner-nfs-server-provisioner-0:
its containers declare no command, so the process name lives in the image entrypoint where the
cluster cannot see it; pass -server-process
```

The PodSpec does carry `args`, but no `command`:

```
nfs-server-provisioner cmd= args=["-provisioner=cluster.local/...","-device-based-fsids=false"]
```

Reading the pod from the inside shows why supplying the missing name by hand is
not the whole answer either. The container runs five processes, and the one
serving NFS is neither PID 1 nor the one those `args` belong to:

```
/proc/1   nfs-provisioner
/proc/14  rpcbind
/proc/16  rpc.statd
/proc/19  dbus-daemon
/proc/152 ganesha.nfsd
```

With the name supplied by hand, DATA-12 and DATA-13 passed first time. CHAOS-01
did not, and its failure is the second half of this finding: the kill landed on
the right process, the client blocked and recovered in 1m33s, every committed
write survived, and the case still failed on

```
server container restart count is still 0 after SIGKILL, so the process that was killed
was not the one serving NFS; pass -server-process to name it
```

every clause of which is false.

### Why

Two separate mistakes, both from treating the container as though it held one
process.

**Naming.** A container's `command` is what the container starts. On a
supervised server that is the supervisor: here PID 1 is `nfs-provisioner`, which
starts `ganesha.nfsd` and restarts it if it dies. Had the chart declared a
command, the harness would have derived `nfs-provisioner` from it and SIGKILLed
the supervisor, which is a different fault with a different recovery, reported
under CHAOS-01's name. The absence of a `command` is what stopped that, by
accident.

**Confirming.** A container's restart count only moves when its PID 1 dies.
Killing a supervised child leaves the container untouched, by design: that is
what the supervisor is for. So the check could not distinguish "the signal
missed" from "the server was restarted in place", and reported the second as the
first.

The fact that identifies an NFS server without reference to any of this is that
it holds the listening socket on port 2049. On this deployment that socket is in
`/proc/net/tcp6` and **not** in `/proc/net/tcp` at all, so the search has to read
both files, for the same reason [F-019](F019-the-client-an-nfs-server-can-name-is-the-node-and.md)
gives.

### What changed

`framework.DiscoverServerProcess` reads the server pod's own socket tables and
file descriptors, and names the process holding the listening socket by joining
the two on the socket's inode. **Preflight does this once per server pod** and
records the name in `environment.json`, alongside the exports and images that
are already there; where it cannot, the record carries the reason and preflight
adds a note saying which three cases will report blocked. The name is a property
of the image, so the cases that signal it read the record rather than probing
again: it was written in the fault path first, and review moved it, because
three cases each paying an exec into the server for an answer that cannot have
changed is three chances to fail for an unrelated reason.

`chaos.ResolveProcess` reads that record and nothing else, and puts the source
on the fault timeline:

```
pattern "ganesha.nfsd", from observed by preflight to be serving NFS, hit 1 of 1 matching processes:
1103539 ganesha.nfsd -F -L /export/ganesha.log -p /var/run/ganesha.pid -f /export/vfs.conf
```

It was written with two fallbacks behind that, the container's declared command
and a `-server-process` flag, and review removed both. The declared command is
the thing this finding is about: taking it kills the supervisor, which stops the
pod, and every recovery measured afterwards describes a container restart rather
than the fault the case meant to inject. A flag is no better evidence, a name
nobody checked against the running server, signalled node-wide. Neither failure
is loud: both produce a confident measurement of the wrong event, which is worse
than a case that does not run. Preflight is a precondition for the suite anyway,
so a cluster where it cannot read the server pod has a problem no fallback here
would have fixed.

**Supersedes the candidate-list discovery of
[PR #83](https://github.com/mikebz/nfs-verification/pull/83)**, which landed
first and answered the same question by matching container probe text and the
comm names in a container's cgroup against `ServerDaemonCandidates`, a list of
daemon names someone had written down. It worked on both reference deployments,
and that is the problem: it can only recognise a server that is already on the
list, and a deployment serving NFS from anything else reads as having no server
at all, silently. Matching a name also does not establish that the process
matched is the one serving this pod. Holding the listening socket does, for a
server nobody has heard of, which is why the list, `cgroup-comm.sh` and the
suite-wide `Environment.ServerProcess` field are gone. The per-pod
`ServerInfo.Process` replaces that field because with fan-out greater than one
the servers need not be the same process.

The join refuses rather than guesses wherever it is not conclusive: nothing
listening, a socket no visible process holds, an image with no `readlink`, or
two differently-named processes holding one socket. A refusal goes straight to
blocked, so the failure mode is a case that does not run rather than a node
taken out of service.

CHAOS-01's step 5 now compares the process serving before the fault with the one
serving after. That pair **is** observed live, because a pid is the one thing
here that changes on every restart, and the pid is the whole question. The
restart count is not consulted at all, not even where the server is the
container's PID 1: the pid compared here comes from the node's process
namespace, since naming the process needs the node agent
([F-027](F027-a-container-cannot-read-the-file-descriptors-of-its.md)), and a
containerized process never has node pid 1.

### What it means for the system under test

For the harness: a fact about the running system beats a field in a manifest
whenever both are available, and the cost of not looking was three cases
unexercised for the life of the project. The guard against killing something
generic (`usableAsPattern`) applies to what is observed, and not only to a name
somebody might have typed, because "I saw it holding the socket" is not a reason
to SIGKILL everything on a node called `sh`.

For the deployment: nothing. A supervised NFS server is an ordinary way to
package one, and the restart-in-place it provides is the reason CHAOS-01's
recovery here is 1m33s rather than a pod restart. The cases that were blocked
were blocked by the harness, not by the server, and the test plan's record of
them as a deployment property was wrong.

### Open

Two of the three places the 2026-09-27 update below lists still assume the
container is the server: the `server-did-not-restart` subtests (#97) and SEC-09's
capability read (#98). The third was closed on 2026-09-28 (#99).

### What changed after this was written

**2026-09-27. The fix held on both deployments.** In six whole-suite runs on
2026-09-25, three on `gke-w1` and three on `gke-w2`
(`w{1,2}-e2e-run{1,2,3}-20260925-*`), preflight named `ganesha.nfsd` as the
process holding 2049 every time. CHAOS-01, DATA-12 and DATA-13 passed 18 of 18.
CHAOS-01 recovered in 92s in four runs and 107s in two, and the DATA pair in 92
to 93s. That is grace plus a respawn (see
[F-029](F029-neither-deployment-ever-lifts-grace-early-for-different.md)), not a
pod restart.

**The lesson reached CHAOS-01 and not the rest of the harness.** The two things
this entry says are wrong about a supervised server are still assumed in three
places:

| Where | Assumes | Consequence | Tracked |
|---|---|---|---|
| `watchServerRestarts` / `assertNoRestart` in `test/e2e/helpers_test.go` | the container restart count sees the server die | the `server-did-not-restart` subtests of PROV-02, PROV-09, PROV-10, PROV-11 and DATA-10 cannot fail on a respawned `ganesha.nfsd` | #97 |
| `framework.ContainerCaps`, read by SEC-09 | PID 1 is the server | SEC-09 asserts on the supervisor's capabilities. `ganesha.nfsd` drops `CAP_SYS_RESOURCE` at start on `gke-w2`, so the two differ | #98 |
| DATA-12, DATA-13 | the kill hit the serving process | neither makes CHAOS-01's before and after pid check. The comment that points at #90 for this is stale, because #90 was about something else and is closed | #99 |

None of these changes a verdict today. They are greens that could not have been
reds, which is the kind of result this suite is built not to report.

**2026-09-28. The third row is closed.** The gap was in how the harness injects
a kill, not in anything about this server: DATA-12 and DATA-13 signalled without
confirming what they signalled. They now make CHAOS-01's before and after
checks, through helpers the three cases share, so every case that kills the
server process in place confirms the kill landed, whatever process serves NFS
(#99). The rule is in the test plan's harness design section. Run twice each on
`gke-w1` on 2026-09-28 (`w1-pr99-*` and, after review made the after check stop
the case, `w1-pr99r2-*`): all six passed, each observing the serving process
before the kill and a new pid serving after it.

**2026-09-30. The second row is closed.** SEC-09 now reads two processes, one per question
(#98). The server is whatever holds 2049, named the way CHAOS-01 names it, and
its set is read from its node through the node agent, after checking that the
pid still has the name discovery saw and sits in the server pod's cgroup. The
container's PID 1 is still read, but as the witness for what the runtime
delivered, which is the one thing PID 1 is good for. Preflight makes both reads,
beside the process name it already recorded for this finding, and SEC-09 judges
the record, so the case itself needs no node agent.

The difference was not cosmetic. Read through the node agent on 2026-09-30:

| | PID 1 `nfs-provisioner` | `ganesha.nfsd` permitted | `ganesha.nfsd` bounding |
|---|---|---|---|
| `gke-w1`, Ganesha 4.0.8 | `a90425ff` | `a90425ff` | `a90425ff` |
| `gke-w2`, Ganesha 15.3-mb | `a90425ff` | `a80425ff` | `a90425ff` |

Both server pods declare `DAC_READ_SEARCH` and `SYS_RESOURCE`. On `gke-w2` the server
holds one of them and the old case could not have seen it. What it means for the
verdict is the decision the fix had to make, and it is in
[doc 07](../07-security-design.md), SEC-09: a declared capability neither the
server nor PID 1 holds was taken by the platform and fails; one the server gave up after the
platform delivered it is recorded. Asserting the server's own set against the
declaration would have turned `gke-w2` red for `ganesha.nfsd` lowering its own
privileges, which is the plan's "no more", not a breach of it.

Run with `make test-case CASE=TestSecServerCapabilities` on `gke-w1` and `gke-w2`
(`w{1,2}-issue98-sec09-*`, and after each review round `w{1,2}-issue98-review-sec09-*` and `w{1,2}-issue98-review2-sec09-*`):
both pass each time. `gke-w2`'s runs record `SYS_RESOURCE` as dropped by the
server; `gke-w1`'s record nothing dropped. The same verdicts came back once
preflight made the reads, with `-refresh-preflight`
(`w{1,2}-issue98-preflight-sec09-20260930-213321`, and after review
`w{1,2}-issue98-review3-sec09-20260930-225727` and
`w{1,2}-issue98-nodigest-sec09-20260930-232559` and
`w{1,2}-issue98-review5-sec09-20260930-233138` and
`w{1,2}-issue98-review6-sec09-20260930-234209` and
`w{1,2}-issue98-review7-sec09-20261001-012811`). Runs on `gke-w1` fed older
records through `-env-file` reported blocked, naming `-refresh-preflight`: one
with no capabilities (`w1-issue98-oldrecord-sec09-20260930-213407`) and one
with capabilities but no pod UID (`w1-issue98-nodigest-stale-sec09-20260930-232559`).

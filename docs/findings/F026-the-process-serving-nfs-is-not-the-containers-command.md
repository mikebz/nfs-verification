# F-026: The process serving NFS is not the container's command, and the restart count cannot see it die

Author: mikebz@
Created: 2026-09-19
Updated: 2026-09-19


**Found:** 2026-09-18, GKE cluster `gke-w2`, Kubernetes v1.37.0-gke.2941000,
server `nfs-provisioner:15.3` (Ganesha V15.3-mb), while asking why CHAOS-01,
DATA-12 and DATA-13 had been blocked on every run against this deployment.

**Severity:** it is the difference between three cases that never run and three
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

### What it implies

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

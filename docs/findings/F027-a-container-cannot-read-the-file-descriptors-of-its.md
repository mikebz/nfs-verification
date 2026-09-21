# F-027: A container cannot read the file descriptors of its own NFS server, except for the first few minutes

Author: mikebz@
Created: 2026-09-21
Updated: 2026-09-21


**Found:** 2026-09-20, GKE cluster `gke-w2`, Kubernetes v1.37.0-gke.3165000,
server `nfs-provisioner:15.3` (Ganesha V15.3-mb), when CHAOS-01 reported blocked
on a mechanism that had worked every time it was tried the day before.

**Severity:** the mechanism it breaks is the one F-026 introduced to unblock
three cases, and it breaks it in the worst available way: it works on a cluster
the suite has already run against and fails on a fresh one.

### What happened

CHAOS-01 reported blocked, correctly and for a reason that should not have been
possible:

```
blocked: preflight did not name the process serving NFS in pod nfs-provisioner-nfs-server-provisioner-0:
port 2049 is listening on inode 8890493 and no visible process holds it
```

The process was plainly there. Reading the pod's `/proc` as root showed
`ganesha.nfsd` at pid 20, and the socket table showed the listener on 2049. What
failed was the join between them, because every file descriptor of that process
was unreadable:

```
ls: cannot read symbolic link '/proc/20/fd/0': Permission denied
```

`/proc/1/fd`, the supervisor's, read fine from the same shell.

The denial is a ptrace-mode access check. Reading another process's file
descriptors is permitted for a process whose dumpable flag is set, and otherwise
needs `CAP_SYS_PTRACE`, which a default Kubernetes container does not have:

```
our exec session  CapEff a90425ff     (no CAP_SYS_PTRACE)
pid 1  nfs-provisioner  CapEff a90425ff
pid 20 ganesha.nfsd     CapEff a80425ff
```

### Which server it happens to

Four observations, and the first explanation they suggest is wrong:

| server | age | `CapEff` | `/proc/<pid>/fd` from inside the pod |
|---|---|---|---|
| `gke-w2`, pod created the previous day | 33h | `a80425ff` | denied |
| `gke-w2`, ganesha respawned by the supervisor after a SIGKILL | minutes | `a80425ff` | readable |
| `gke-w2`, freshly created pod, after preflight mounted and wrote through it | minutes | `a80425ff` | readable |
| `gke-w1`, upstream `nfs-server-provisioner` v4.0.8 | 4.5 days | `a90425ff` | denied |

The dropped `CAP_SYS_RESOURCE` looked like the cause and is not: `gke-w1`'s
server holds the full set and is denied just the same, and `gke-w2`'s fresh
server has already dropped it and is readable. Serving traffic is not the
trigger either; the fresh pod was readable after preflight had mounted it from
two nodes and written through it.

What the two denials have in common is that they had been running for a day or
more. **What flips a server from readable to not, and when, is not established
here.** An explicit `prctl(PR_SET_DUMPABLE, 0)`, a credential switch made while
impersonating a client, or something else entirely would all fit; the evidence
does not separate them, and a guess in this file would be worth less than this
sentence.

It does not need to be separated to act on, because the direction is the wrong
way round for a test harness. The in-pod read works on a server that has just
been created and fails on one that has been up for a day: it succeeds in exactly
the conditions a freshly set up test environment provides, and fails in the ones
every real cluster is in. It passed a smoke test, passed a live case twice, and
was broken.

### What changed

Discovery is now a read from two places, in `framework.DiscoverServerProcess`:

- The **pod** names the socket. Which socket belongs to this server is a
  question about its network namespace, and the node's socket tables do not
  contain it. This half needs no `readlink` and no fd access, so it no longer
  cares about any of the above.
- The **node** names the process holding that inode, through the node agent,
  which is privileged and is already a precondition for the kill these three
  cases exist to perform. `Agent.RunScript` runs the reader in the agent pod
  itself rather than through `nsenter`: the pod is declared `hostPID`, so the
  `/proc` it already sees is the node's.

The same script serves both, with an optional list of inodes so the node scan
reports only the process it was looking for. A node runs hundreds of processes
and the unfiltered output would be most of them.

This is also more correct than what it replaces, independently of permissions.
The kill matches a name in the host PID namespace; the old code observed a pid
in the container's. The two were never the same number and were never compared.
The pid now recorded is the node's, which is the one CHAOS-01's before-and-after
check is about.

### What it implies

For the harness: a mechanism that reads another process's `/proc` is not
verified by one that worked. This one failed only on servers that had been up
for a day, so every check made against a server the run had just created, or had
just restarted, agreed that it worked. The state a test environment is in
minutes after it is built is not the state a cluster is in, and where the two
differ the fresh one is the easier of the two. Two deployments disagreeing is
what settled it here, which is the argument for keeping both.

For the deployment: nothing. Both servers are entitled to whatever they do with
their own credentials, and the upstream v4.0.8 build does it while holding the
full capability set, so it is not even a property of the local build. The
lesson is about where the harness looks, not about what the server does.

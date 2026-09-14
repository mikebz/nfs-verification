# F-003: A broken `umount.nfs` wrapper on GKE wedges every terminating pod

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-10, GKE worker nodes, reviewing
[PR #3](https://github.com/mikebz/nfs-verification/pull/3) against a live
cluster.

**Severity:** high, and it is not the harness. Every pod holding an NFS mount
stays `Terminating` forever, on any workload, whether or not this suite is
running.

### What happened

Pods with the share mounted would not go away. Kubelet's volume teardown failed
with:

```
mount.nfs.real: no mount point provided (exit status 32)
```

### Why

GKE wraps `/sbin/mount.nfs` with `/home/kubernetes/bin/mount.nfs`, a script that
arranges a private mount namespace so an in-cluster Service DNS name resolves.
`/sbin/umount.nfs` is a symlink to the same file, because the real binary is
multi-call: it decides whether it is mounting or unmounting from `argv[0]`.

The wrapper ended in:

```sh
exec /home/kubernetes/bin/mount.nfs.real "$@"
```

which sets `argv[0]` to `mount.nfs.real`. The multi-call binary therefore chose
mount mode no matter how it was invoked, so `umount.nfs <mountpoint>` was read
as a mount with no mount point, and failed. Nothing on the unmount path could
ever succeed.

### The fix applied to the nodes

Handle the unmount case before the wrapper's mount logic, preserving `argv[0]`
with `exec -a`:

```sh
if [[ "$(basename "$0")" == *"umount"* ]]; then
  out=$(exec -a "$0" /home/kubernetes/bin/mount.nfs.real "$@" 2>&1)
  status=$?
  if [[ $status -eq 0 || $status -eq 16 ]] || [[ "$out" == *"not mounted"* ]]; then
    exit 0
  fi
  echo "$out" >&2
  exit $status
fi
```

Exit status 16 and "not mounted" are treated as success on purpose: an unmount
of something already gone is the outcome the caller wanted. With this in place
kubelet unmounts finished in under two seconds.

### What it means for the suite

Nothing in this repository changed. The value of the finding is that the
symptom is indistinguishable, from inside a case, from the failures this plan
is actually hunting:

- Pods stuck `Terminating` are what teardown treats as "the node has stopped
  answering" (F-001), so a run against an affected cluster reports leaked
  claims and stuck pods in every case, for a reason that has nothing to do with
  NFS.
- It arrives as a storage-shaped failure and routes to the storage owner, when
  the defect is in a node image's wrapper script.
- **Node reset / reboot recurrence**: Because the wrapper lives in the node root
  filesystem, resetting, rebooting, or replacing a node reinstalls the stock
  image wrapper without the fix. Nodes restarted or added during chaos or
  maintenance must have the wrapper updated.

Anyone seeing `Terminating` pods across the board should check
`/sbin/umount.nfs` on the node before suspecting the server or the driver. The
tell is that unmount fails while everything else about the mount works.

### Open

Preflight could catch this in seconds: the node agent already runs in the host
namespaces, so it could unmount a path that is not mounted and check that the
failure is "not mounted" rather than "no mount point provided". That would turn
a day of attribution into a preflight message. Not built yet, and it would be a
recorded warning rather than a gate, since the wrapper is specific to one node
image and the plan admits no distro-specific checks.

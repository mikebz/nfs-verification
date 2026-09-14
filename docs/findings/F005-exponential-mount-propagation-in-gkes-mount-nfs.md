# F-005: Exponential mount propagation in GKE's `mount.nfs` wrapper wedges worker nodes

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-11, GKE cluster with e2-medium worker nodes, running PROV and DATA test suites.

**Severity:** critical for the cluster, high for the suite. It exhausts kernel mount structures, driving worker nodes into uninterruptible D-state and requiring a hard compute instance reset.

### What happened

During test runs with dynamic provisioning and teardown, worker nodes gradually slowed down and eventually stopped responding to `kubectl exec`, `runc` container management, and kubelet liveness probes. Containerd reported context deadlines on container startup and exit.

Inspection of `/proc/mounts` on an affected node revealed that the mount table had exploded from ~80 lines to **16,510 lines** (1.85 MB), containing **8,192 stacked bind mounts of `/etc`** and **8,191 stacked bind mounts of `/run/systemd/resolve`**.

### Why

GKE wraps `/sbin/mount.nfs` with `/home/kubernetes/bin/mount.nfs` so that in-cluster Kubernetes Service DNS names (`*.cluster.local`) resolve from the host. The wrapper isolates DNS configuration inside a private mount namespace:

```sh
exec unshare --mount --propagation shared -- bash -c '
  ...
  mount --bind /etc /etc
  mount --make-private /etc
  if [[ -d "$RESOLV_DIR" ]]; then
    mount --bind "$RESOLV_DIR" "$RESOLV_DIR"
    mount --make-private "$RESOLV_DIR"
  fi
  mount --bind "$TMP_RESOLV" "$RESOLV_TARGET"
  exec /home/kubernetes/bin/mount.nfs.real "$@"
' ...
```

The bug is the combination of `--propagation shared` with `mount --bind` *before* `mount --make-private`:

1. `unshare --mount --propagation shared` places the new namespace root and `/etc` into the **same shared peer group** as the host namespace.
2. Inside that shared namespace, executing `mount --bind /etc /etc` causes Linux VFS mount propagation to immediately clone the new bind mount across all peers in the group, including the host namespace.
3. The subsequent `mount --make-private /etc` only makes the child namespace's mount private; the cloned mount in the host namespace remains shared.
4. Each subsequent NFS mount starts with $2^N$ mounts on the host, duplicating all existing peer mounts on every execution: $1 \to 2 \to 4 \to 8 \to 16 \dots \to 8192$.
5. By mount 13, the kernel mount table holds over 16,384 mounts. Every subsequent operation iterating mounts (container creation, exec, `cat /proc/mounts`, kubelet housekeeping) acquires `namespace_sem` and spends excessive CPU traversing stacked mounts. Worker threads enter uninterruptible D-state, container runtimes hang, and the node becomes unresponsive.

### The fix applied to the nodes

In `/home/kubernetes/bin/mount.nfs`:

1. **Make directories slave before bind-mounting:**
A slave mount receives propagation from its master (the host) but never propagates changes back. Making `/etc` and `/run` `rslave` before bind-mounting ensures bind mounts remain confined to the private namespace:

```sh
mount --make-rslave /etc 2>/dev/null || true
mount --bind /etc /etc || exit 1
mount --make-private /etc || exit 1

mount --make-rslave /run 2>/dev/null || true
if [[ -d "$RESOLV_DIR" ]]; then
  mount --bind "$RESOLV_DIR" "$RESOLV_DIR" || exit 1
  mount --make-private "$RESOLV_DIR" || exit 1
fi
```
*(Note: Making `/` slave is incorrect, as that would prevent the NFS mount itself under `/var/lib/kubelet` from propagating back to the host).*

2. **Bypass namespace creation for IP-based targets:**
When the target is a raw IP address (e.g. `10.x.x.x:/export`), cluster DNS resolution is not involved. Skipping `unshare` entirely avoids namespace overhead and eliminates propagation hazards:

```sh
if [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+: ]] || [[ "$1" =~ ^\[[0-9a-fA-F:]+\]: ]]; then
  exec /home/kubernetes/bin/mount.nfs.real "$@"
fi
```

### What it means for the system under test

- Like F-003, this is an infrastructure defect in GKE's host mount wrapper, not an NFS protocol bug or harness failure. Any dynamic RWX workload on GKE that provisions and mounts volumes repeatedly will eventually wedge its worker nodes.
- With the fix applied across worker nodes, mount table size remained stable at baseline (~80-84 lines) across hundreds of mounts throughout the full PROV, DATA, SEC, OBS, and CHAOS test suites.

# F-018: The export admits any client that can reach it, so a claim's access control ends at the mount

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-13, `make test-sec` run `20260913-171950`, SEC-05, GKE cluster
`gke-w1`, Kubernetes v1.37.0-gke.2941000, three workers on Container-Optimized
OS, kernel 6.12.94+, StorageClass `nfs` backed by `nfs-server-provisioner`
v4.0.8, profile `default` (lease 60s, grace 90s).

**Severity:** every other security case in the suite describes what a client the
export admits may do. This one says the export admits everything.

### What happened

A pod on node A owns a claim and wrote a file with checksum `ff11c8352bc5…`. The
node agent's container on node B — a node this export was never provisioned for,
with no claim on it and no pod mounting it — ran

```
mount -t nfs4 -o vers=4.1,soft,timeo=50,retrans=1 34.118.226.20:/export/pvc-709039f8… /tmp/…
```

which was granted, and read the owner's file back with the same checksum. A
control mount from the same container of an export node B *is* a client of was
granted too, so the instrument was proven before the result was reported.

SEC-08 records the rest of the picture from the same run: no NetworkPolicy in
the server's namespace, and a pod with no claim at all opening a TCP connection
to `34.118.226.20:2049`. The server also listens on 111, 662, 875, 20048 and
32803, the NFSv3 ancillary services, alongside 2049.

### Why

The provisioner writes one export block per claim, and it carries no per-client
rule at all:

```
Access_Type = RW; Squash = no_root_squash; SecType = sys;
```

There is nothing to match a client against, so the global access type applies to
whoever connects. AUTH_SYS then takes the uid on the wire on trust, and
`no_root_squash` means a client asserting uid 0 is root on the export.

### What changed

SEC-05 exists and is red. The probe it uses runs in the node agent's own
container rather than the host mount namespace (F-003, F-005), is `soft` where
every other mount in the suite is `hard`, and always unmounts; the control probe
is what separates a server's refusal from a container that cannot mount.

### What it means for the system under test

**The access control around a PersistentVolumeClaim is a Kubernetes-side
convention that ends at the mount.** Anything in the cluster that can route to
the server — any pod, on any node, in any namespace, with no claim and no RBAC
on one — can mount any claim's export and read and write it as root. The
Kubernetes objects say who may *ask kubelet* to mount; they are not what the
server enforces, because the server was given nothing to enforce.

Three things would each narrow it, and none of them is a harness change: a
`CLIENT` block per export naming the nodes the volume is scheduled on, a
NetworkPolicy in front of 2049, and `root_squash`. On a stock installation of
this chart, none is present.

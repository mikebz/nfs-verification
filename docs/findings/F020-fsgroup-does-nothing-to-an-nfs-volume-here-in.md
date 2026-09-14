# F-020: fsGroup does nothing to an NFS volume here, in either direction

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14


**Found:** 2026-09-13, `make test-sec` run `20260913-171950`, SEC-03, GKE cluster
`gke-w1`, Kubernetes v1.37.0-gke.2941000, StorageClass `nfs`.

**Severity:** none to the cluster. It decides what SEC-03 may assert, and it is
the answer to a question every restricted-Pod-Security workload asks.

### What happened

Over a directory of 2000 files, a pod declaring `runAsUser: 1234, fsGroup: 5678`
started in the same 1s as an identical pod without `fsGroup`, and the ownership
of all 2000 entries was byte-for-byte unchanged. Inside the pod, `id` reports
`uid=1234 gid=1234 groups=1234,5678`. The file it wrote landed `1234:65534`.

### Why

`fsGroup`'s volume half is conditional on the volume plugin supporting ownership
management, and this one does not: kubelet applies the supplementary group and
leaves the volume alone. The write succeeded anyway because the export's root is
`drwxrwsrwx 65534 65534` — 0777 with the setgid bit — so any uid may write, and
a new file takes its group from the directory rather than from the pod.

### What changed

SEC-03 asserts both halves and records which one it got, rather than assuming
the storm it was written to catch. `slo.FSGroupStartOverhead` and
`slo.FSGroupPopulatedEntries` are the bound and the population it measures
against.

### Corrected 2026-09-14, and why the first answer was wrong

The paragraph below originally said the write had succeeded **only** because the
export's root is world-writable, and that a workload relying on `fsGroup` here
was relying on the mode rather than on the gid. That was not measured. SEC-03
wrote into a 0777 directory, so the write could not have failed for lack of the
group, and a pod that never received `fsGroup` would have passed the same
assertion. Review caught it (PR #57), and the case now writes into a directory
owned by the fsGroup gid with mode 0770, with the identical pod that declares no
`fsGroup` as the control.

The control is refused and the `fsGroup` pod succeeds. So the supplementary
group **does** reach this server and **does** grant access: AUTH_SYS carries the
gid list on every request and the export honours it. The world-writable root was
never load-bearing; it was just the only thing being exercised.

The ownership half stands, and is now measured without the setgid bit confusing
it: the file the `fsGroup` pod writes lands `1234:1234`, its own primary gid.
`fsGroup` is a supplementary group inside the pod and nothing more.

### What it means for the system under test

**`fsGroup` works for access on an RWX NFS claim here, through the gid list
rather than through volume ownership.** A workload can be granted a share by
group, and the group it declares is the group the server sees. What `fsGroup`
does *not* do is change the volume: no recursive chown runs on mount, so a large
shared volume does not pay for a pod that declares a group, and one workload's
`fsGroup` cannot rewrite another's ownership. A workload that expects the
volume itself to be chowned to its gid — the second half of what Kubernetes
documents — does not get it here, and files it creates keep its primary gid.

The wider lesson is the one in the correction above: **an assertion made where
it cannot fail produces a finding, and the finding is wrong.** This entry said
something confident and false about the deployment for a day.

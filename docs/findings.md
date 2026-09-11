# Findings

Author: mikebz@
Updated: 2026-09-11

Things learned by running the suite against a real cluster that are worth
remembering. Each entry says what happened, why, what changed in the code, and
what it implies for the system under test as opposed to the harness.

New entries go at the top.

---

## F-008: `nfs-server-provisioner` never announces grace, so the grace cases cannot run against it

**Found:** 2026-09-11, GKE cluster `gke-w1`, Kubernetes v1.37, StorageClass `nfs`
backed by `cluster.local/nfs-provisioner-nfs-server-provisioner:v4.0.8`. First
run of CHAOS-07 on this deployment.

**Severity:** none for the cluster, high for what can be concluded from a run on
it. A whole class of assertion is unreachable here and the results do not look
empty, they look green.

### What happened

CHAOS-07 ran for 366 seconds and reported blocked: the server emits no grace
entry or exit line that the log stream can classify, so there is no window to
place a lock grant inside or outside of.

The case is right to report blocked rather than fail. What it cannot do is
assert the thing it exists for.

### Why

Grace is the dominant term in every failover measurement in this plan, and the
only way a client-side harness can see it is a signal the server chooses to
emit. This provisioner emits none that the built-in wording rule matches, and
`-grace-enter-pattern` cannot help unless someone first establishes that a line
exists to match.

### What it means for a run on this deployment

Three assertions are out of reach, and two of them are silent about it:

- **CHAOS-07** reports blocked, visibly. Nothing was learned about whether the
  server bars new lock acquisition during grace.
- **OBS-03** is the case that *fails* for this rather than skipping, and it
  should be run here to put the finding on the record where an operator sees it.
- **Every grace-window narrowing elsewhere** falls back to whatever the case
  does without a window. CHAOS-06's reclaim assertion does not need one, which
  is why it passed.

The SLO profile on this run was the default one (60s lease, 90s grace) taken
from flags, not from discovery. That is the documented fallback and it is sound,
but it is worth stating that on this deployment the grace value is *declared*
rather than observed, so a failover measured against it is measured against a
number nobody confirmed.

### What changed

No code. This is a property of the deployment, and both cases already report it
correctly. It is recorded so that a green CHAOS run against this provisioner is
not read as evidence that grace behaves.

---

## F-007: Two cases in the data path phase reported results they had not measured

**Found:** 2026-09-11, GKE cluster `gke-w1` with three workers, first real run of
the step 6 cases from [PR #14](https://github.com/mikebz/nfs-verification/pull/14).

**Severity:** high for the suite, none for the cluster. Both cases were green,
and neither was wrong about anything it claimed. They were wrong about how much
they had claimed.

### What happened

Every case in the phase that could run did: DATA-05 through DATA-09 and DATA-12
and DATA-13 passed, DATA-11 skipped, DATA-14 skipped for want of an image, and
CHAOS-06 passed with both its byte-range subtests. Mount tables on all three
nodes were at baseline afterwards and nothing leaked.

Two results did not say what they appeared to say.

**DATA-12 verified three records and DATA-13 verified four.** Both passed.

**DATA-11 reported SKIP for thirteen seconds of work.** The sparse write, the
zero-filled hole, the byte past it and the logical size had all been asserted
and had all passed before the hole-punch probe stopped the case.

### Why

The durability pair inherited its pre-fault warm-up from the recovery cases,
which wait for three committed writes and then inject. That is the right warm-up
for a case measuring an *interruption*: what matters there is that the workload
was demonstrably running when the fault landed, and the assertion is about the
gap, not about the records.

The durability pair asserts over the records themselves, so the length of the
set is the resolution of the measurement. At one record per second, three
records is a three second window. DATA-13 is the worse of the two: its entire
job is to record how many un-fsynced records came back absent or short, and over
four records on a server whose host page cache survived the SIGKILL it was
always going to report four correct and document nothing. A pass there looks
identical to a pass over a set large enough to mean something.

DATA-11 was one case with two halves gated by different things, and Go reports a
case, not a half. The punch probe correctly found that the Alpine image's
busybox `fallocate` parses `-l` and `-o` only, and correctly reported blocked
rather than filing a tool gap as a protocol gap. But `t.Skipf` marks the whole
case skipped whatever ran before it, so the sparse result went in the bin with
it.

### What changed

The warm-up moved into `awaitRecords`, and the durability pair asks for thirty
records rather than three. Thirty is a minute of load before the fault; nothing
asserts on the number, and it is cheap against a case that already waits out a
server restart. `requireDurabilitySet` fails either case whose set came back
under ten, because a pass over a handful of records is worse than a failure: it
looks the same as a real result.

DATA-11 is now two subtests, `sparse-write-and-read-back` and `hole-punch`, so
the half that can run on a busybox image reports its own result and only the
half that cannot reports blocked.

### What it says about the harness, not the system under test

Nothing about NFS. It says that "the case passed" and "the case measured
something" are different claims, and that the suite had no way to tell them
apart in either of these shapes.

The same pattern had already been caught twice in review on this branch, in
DATA-05 and CHAOS-06, where a mount option blocking one half would have blocked
a case that could still run the other. DATA-11 was the third instance and was
missed because its two halves are gated by an image rather than by a mount
option. A case with halves that can be blocked separately reports them
separately; that is now true of every such case in the phase.

The set-size problem is the more general one, and it has no structural fix here.
A case that asserts over a set it produced has to state how large that set must
be to carry its conclusion, and fail below it. Only the durability pair does
that today.

### What the run settled

Four things the design had left open, recorded here because they are facts about
a real image and a real cluster rather than decisions:

- **busybox `dd` on `alpine:3.20` does carry `oflag=direct`.** DATA-07 passed.
  Section 5.4 of the design holds, and `locktool` does not need a `dio`
  subcommand.
- **busybox `fallocate` does not carry `-p`**, as predicted, so the hole punch
  is unreachable on the default image whatever the mount version is.
- **A lock held by a force-deleted pod came back in 2.27 seconds**, which
  DATA-06 classified as the descriptor-close path rather than lease expiry. On
  this cluster kubelet is prompt, so an application losing a lock to a dying pod
  is back in about a second; losing a *node* is the case that costs a lease, and
  that is CHAOS-03 and SEC-07.
- **The clone volume did not disturb the dynamic claim that owns the export.**
  DATA-08 passed and open question 6 of the design is answered for this driver.

Still unmeasured: DATA-10, which did not run, so whether 100k entries fit on a
directory-backed export is still open; and DATA-14, which needed `-fio-image`.

*Amended 2026-09-11:* DATA-14 was deferred rather than run. It is not a gap
waiting on an image any more, it is a case this suite is not doing yet, and
Section 3.2 of the test plan says why.

---

## F-006: `scripts/lock-probe.sh` passed `flock -w`, which busybox does not have

**Found:** 2026-09-11, by reading the applet sources while writing
[`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md).
Not found by a run: on a workstation and on the default `alpine:3.20` tools
image the probe works, because both carry the util-linux `flock`.

**Severity:** high for the suite, none for the cluster. It makes a merged case
pass while asserting nothing.

### What happened

`lock-probe.sh` ran `flock -w "$wait" -x 9` for each attempt.
`hold-flock.sh`'s own comment two files away already said busybox `flock` has
no timeout flag, and the probe contradicted it.

### Why it matters

busybox `util-linux/flock.c` parses `-s`, `-x`, `-u` and `-n` and nothing else.
On an image carrying the applet, every attempt exits on a usage error before a
lock is ever requested, so every record in the probe log is `ERR`.

CHAOS-07 asserts **on grants**: no grant inside the grace window, and at least
one after it. An all-`ERR` log satisfies the first half for free. The second
half would have failed, which is the only reason this was ever going to be
noticed, and it would have been read as "the server never resumed granting new
state" rather than as a broken probe.

### What changed

The attempt is now a retry loop around `flock -n`, bounded by the same
`wait-seconds` argument. `-n` is carried by busybox and by util-linux alike, so
one loop serves both images, and the `timeout` bound around the attempt is
unchanged.

`TestScriptsUseOnlyPortableFlockOptions` now reads every embedded script and
rejects any `flock` option outside busybox's four. The two existing probe tests
could not catch this: they run against the workstation's `flock`, which accepts
`-w`, so a portability defect has to be asserted against the option set rather
than against behaviour.

### What it says about the harness, not the system under test

Nothing about NFS. It says that "assume nothing beyond busybox" is a rule the
repository states and had no check for. Every case that reports on grants can
pass vacuously if its instrument never gets as far as asking, and an instrument
that fails the same way on every attempt looks exactly like a quiet system.
## F-005: Exponential mount propagation in GKE's `mount.nfs` wrapper wedges worker nodes

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

---

## F-004: `allowVolumeExpansion` is a claim, not a capability

**Found:** 2026-09-10, GKE cluster with e2-medium worker nodes, running PROV-04 while reviewing
[PR #3](https://github.com/mikebz/nfs-verification/pull/3).

**Severity:** low for the cluster, medium for the suite. It makes one case fail
for a reason that is not obvious from its failure.

### What happened

The StorageClass under test sets `allowVolumeExpansion: true`, so preflight
recorded `canExpand` and PROV-04 took the expansion path. The resize request was
accepted by the API and then nothing happened. The case failed with:

```
claim capacity did not reach 2Gi: timed out after 5m0s
```

### Why

`allowVolumeExpansion` on a StorageClass is what the class asserts, not what its
provisioner can do. The in-cluster `nfs-server-provisioner` behind that class
performs no expansion, and no external-resizer sidecar was running to act on the
request, so the claim sat with the larger request and the smaller status
forever. No resize condition was ever posted, because nothing was resizing.

### What changed

`WaitPVCCapacity`'s timeout message now distinguishes the three answers. A
resize condition present means expansion is in progress or is waiting on
something (a pod restart, for instance). **No condition at all** means nothing
acted on the request, and the message now says so and names the two things to
check: whether the provisioner supports expansion, and whether an
external-resizer sidecar is running.

### What it says about capability discovery

`Caps.CanExpand` is optimistic by construction, because the only thing the
Kubernetes API offers is the class's own assertion. Every other capability in
the suite is probed; this one is declared, and it is the one that lied. The case
still fails rather than skipping, which is right: a class advertising a
capability its provisioner lacks is a real deployment defect, and a claim
accepted for a resize that never happens is a trap for any workload, not only
this suite. What the case owed was a failure message that says where to look.

---

## F-003: A broken `umount.nfs` wrapper on GKE wedges every terminating pod

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

---

## F-002: 2GB worker nodes cannot host the suite

**Found:** 2026-09-10, GKE cluster, `e2-small` node pool (2GB RAM).

**Severity:** medium. It does not produce a wrong result, it produces node
reboots that look like storage failures and cost a day to attribute.

### What happened

On `e2-small` workers, the GKE system daemons alone (fluentbit, gmp-collector,
gke-metrics-agent, filestore-node, pdcsi-node) account for roughly 87% of
requested memory and well over 100% of limits. Adding test pods, their image
pulls and the page cache from a 1MiB write pushed nodes into kernel memory
pressure:

```
virtio_balloon: Out of puff! Can't get 1 pages
systemd-journald: Under memory pressure
```

Kubelet heartbeats then dropped, MIG health checks fired, and nodes rebooted
mid-run.

### Why it matters to the results, not just the runtime

A rebooting node is indistinguishable, from inside a case, from the failure
modes the plan is actually hunting: I/O that stalls, a mount that does not come
back, a lock that is not reclaimed. A suite that cannot tell an undersized node
from a storage defect produces findings nobody can act on.

This is the same class of problem as Appendix C item 4 in the test plan, which
already requires node auto-repair and auto-upgrade to be off: if the platform is
restarting nodes underneath the run, chaos results are invalid.

### What changed

- The node agent now sets resource requests (10m CPU, 32Mi) so it is not
  BestEffort and not the first thing evicted. No limits: it must not be OOM
  killed while a case is reading the node it is inspecting.
- Minimum node size is stated in the README: at least 4GB per worker
  (`e2-medium`), and 8GB (`e2-standard-2`) for anything beyond the presubmit
  cases.

### Open

The node pool requirement belongs alongside the other cluster preconditions in
Section 0 of the test plan, as something preflight could check rather than
something a person has to remember. Allocatable memory per node is readable from
the node status, so a preflight warning is cheap. Not done yet.

---

## F-001: Force-deleting a mounted pod can take a node out of service

**Found:** 2026-09-10, GKE cluster (GKE v1.37.0, Container-Optimized OS,
`e2-small` node pool), reviewing the first three cases on
[PR #1](https://github.com/mikebz/nfs-verification/pull/1).

**Severity:** high. The failure is not a failed test, it is a node that stops
accepting work, and the harness caused it.

### What happened

Teardown deleted the case's pods with `GracePeriodSeconds: 0` and then deleted
the claim.

```go
// pkg/framework/framework.go, before the fix
pods.DeleteCollection(ctx, DeleteNow(), ListOptions(f.Selector()))
// ... immediately followed by
PersistentVolumeClaims(Namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, ...)
```

### Why it wedges the node

1. A force delete removes the Pod object from the API at once. It does not wait
   for kubelet on the node to stop the containers or unmount the volume.
2. The pod is gone from the API, so the wait that follows returns immediately
   and the claim is deleted next.
3. The provisioner destroys the export while the node's kernel still holds an
   active mount of it.
4. The mount is NFSv4.1 and `hard`, so the kernel retries the RPCs
   indefinitely rather than returning an error. The retry is uninterruptible.
5. That blocks kubelet's volume manager, which stops further mounts on that
   node, disturbs PLEG, and hangs anything that reads the mount table,
   including `cat /proc/mounts`.

The ordering is the whole bug. Every step after the force delete behaves
exactly as documented; the force delete simply removed the only thing that was
keeping the export alive until the unmount finished.

### What changed

- Teardown deletes pods gracefully (`metav1.DeleteOptions{}`) and waits for them
  to leave the API before touching any claim. Test pods carry
  `terminationGracePeriodSeconds: 5`, so this costs seconds, not minutes.
- `Framework.DeletePod` is the graceful delete a case should use when it is done
  with a pod.
- `Framework.DeletePodNow` still force-deletes, because some cases need to model
  a client that vanished without unlocking (DATA-06, SEC-07). Its doc comment
  now says plainly that it must never be used for teardown.
- Artifact collection bounds each node's inspection separately
  (`nodeInspectTimeout`), so a node in this state cannot starve the bundle for
  the healthy nodes. This is how the state gets diagnosed next time.

### What it says about the system under test, not the harness

The harness triggered this, but the hazard belongs to the architecture, and the
plan already predicts the shape of it: the server is a singleton in the data
path, and a hard mount blocks rather than failing. Deleting an export out from
under a live mount is therefore not a recoverable client error, it is an
indefinite stall on the client node.

Worth carrying into later work:

- PROV-03 asserts the API-level protection: a claim that a pod still mounts
  stays `Terminating` until the mount is gone. That protection is what stands
  between a normal `kubectl delete pvc` and this failure, so it deserves the
  presubmit gate it has.
- A case for the uglier path is worth adding once the chaos vector lands:
  destroy the export while a node holds the mount, and assert what the node
  does. The honest expected result is an indefinite stall, so the assertion is
  about blast radius (does kubelet keep serving other pods?) and about whether
  the state is observable, not about recovery.
- Node recovery after this state is a reboot in practice. Any run that hits it
  should treat the node as spent.

### Follow-up, 2026-09-10

Teardown has a second path through the same hazard: if a pod does not leave the
API within `PodTerminateTimeout`, the node has stopped answering, and deleting
the claim then is exactly the dangerous act. Teardown now deletes only the
claims no surviving pod mounts, keeps the rest, and fails with the pods, their
nodes and the kept claims named. Leaking a claim is recoverable; wedging a node
is not.

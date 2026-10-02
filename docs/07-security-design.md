# 07: Security and identity: who the server thinks you are

Author: mikebz@
Created: 2026-09-13
Updated: 2026-10-02
Status: shipped, delivery steps 2 ([PR #3](https://github.com/mikebz/nfs-verification/pull/3)),
2b ([PR #4](https://github.com/mikebz/nfs-verification/pull/4)) and 8
([PR #57](https://github.com/mikebz/nfs-verification/pull/57)).
SEC-01 through SEC-09 shipped.
Serves: SEC-01 through SEC-09, the complete Security and Identity
[test group](storage_terms.md#step-phase-category-section-test-group-delivery-group).
Requirements in [`01-test-plan.md`](01-test-plan.md) Section 3.6.
Builds on [`03-chaos-operations-design.md`](03-chaos-operations-design.md),
[`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md),
[`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md) and
[`06-observability-design.md`](06-observability-design.md), whose rules all still
hold. It takes doc 06's verdict rule unchanged and applies it to a different
question.

---

## 1. The decision that shapes everything here

**The client is the node, not the pod, and this [phase](storage_terms.md#step-phase-category-section-test-group-delivery-group) measures identity rather
than asserting a policy.**

Two facts decide every case below. The first is architectural: the NFS mount is
made by the node's kernel, so every identity the server can see belongs to the
node and is shared by every pod and every mount on it ([client
identity](storage_terms.md#client-identity)). A pod is invisible to the server.
The second is that access control here is AUTH_SYS (plan Appendix B), which is
an assertion of identity, not a proof of one.

So a case in this section cannot assert that the right policy is configured:
nobody told this suite what the export's policy is meant to be, and inventing one
turns a deployment choice into a test failure, which is the mistake SEC-02
already refuses to make with `-root-squash`. What a case **can** assert is that
the identity machinery works as the deployment must rely on it working:

- that an identity written through one client is the identity a second client
  reads back (SEC-01), because ownership that does not survive the crossing makes
  every permission on the share a function of where the pod landed,
- that privilege is required to give a file away, whatever the export's squash
  setting is (SEC-02),
- that one client's state is not destroyed by another client's departure
  (SEC-04), because NFSv4.1 state is held per client and the client identity is
  therefore the blast radius of every recovery,
- that a client the export never named is refused, or, if it is not, that the
  suite says so in the strongest terms and proves it by reading the bytes
  (SEC-05),
- that the server records a node under that node's own address in each address
  family (SEC-06), because that address is what an export rule is matched
  against,
- that two pods sharing one client identity do not take each other's state
  (SEC-07),
- that the identity Kubernetes thinks it is granting is the one the volume
  actually honours (SEC-03),
- and that the capability set the server runs with is the one it declared
  (SEC-09).

SEC-08 asserts nothing at all, by the plan's own wording, and Section 7 says how
that is kept honest rather than decorative.

## 2. What the probes already settled

Seven hand-run probes against the cluster this project uses (GKE v1.37, three COS
[workers](storage_terms.md#schedulable-node-and-worker), `nfs-server-provisioner` v4.0.8 behind a ClusterIP Service, StorageClass
`nfs`) were run before this document, because most of the design decisions below
turn on answers the code could otherwise only guess at. These are probes, **not
suite runs**; no case existed yet, and nothing here is a result.

| Probe | What came back | What it decides |
|---|---|---|
| The server's socket table while a pod held the share | The peer is the **node's** address on a reserved port, never the pod's | The client is the node, shared by every pod on it. That fact underlies SEC-04 and SEC-07, though neither reads it: it is why a node is the unit whose state can be lost. Recorded as F-019 |
| The same table, address family | The listener is IPv6 and an IPv4 client arrives **IPv4-mapped** | Every IPv4 client of this server already arrives in the form SEC-06 is about. On a single-stack cluster SEC-06 skips, so there the mapped form is a record: F-019, and what SEC-08 reads |
| An unrelated pod, no claim, mounting another claim's export | **Granted**, read-write, and the other tenant's bytes came back | SEC-05 has an outcome to report and the report is red |
| The same mount with `CAP_SYS_ADMIN` only | Refused with `EACCES`, for a **client-side** reason | The trap this phase most needs to avoid: a probe that cannot mount reports a refusal that never happened |
| Ownership of a root-written file, server side and client side | uid 0 on the server, **nobody** on the client | The export does not squash; the client cannot map the owner *name*. SEC-02 read this as squash, which was a finding against this suite; it now asks whether a `chown` is permitted, which the idmapper cannot answer wrongly. F-021 |
| A pod with `fsGroup`, on a populated share | Supplementary gid granted, **volume ownership untouched** | SEC-03's two halves, and which of them can fail. The access half was probed in the export root, which is world-writable and setgid, so the write proved nothing about the gid and the group the new file inherited was the directory's, not the volume's. Both mistakes are corrected in F-020, and SEC-03 now asks the question behind a gate |
| The server's grace announcements | Present in a log **file inside the export**, absent from the container's stdout | Refines F-008: the signal exists, the channel does not carry it |

The last two rows are findings in their own right, filed as F-020 and F-022.
Neither changes what this phase builds.

## 3. What this phase builds

Four readers and one probe, and nothing else:

- **The peer table.** The connections a server pod has, read from `/proc/net`
  inside it and parsed in Go: local and peer address, port, state, with
  IPv4-mapped addresses normalised. Implementation-neutral: nothing reads an
  export config file, because that syntax is the server's, not the protocol's.
  SEC-06 and SEC-08 read it.
- **The capability set.** The declared set from the server pod's spec, the set
  of the process holding 2049, read on its node, and the set of its container's
  PID 1, which is what the runtime delivered. Decoded from the mask to names.
  Preflight reads all three and SEC-09 judges the record; until #98 the case
  read PID 1 alone, as though it were the server (Section 11).
- **The volume ownership sweep.** How many files under a path have which owner
  and group, from one exec, so "nothing was chowned" is a count and not an
  impression.
- **`fsGroup` on the fixture's pod spec**, the field `pod.go` has been reserving
  for SEC-03 since step 2.
- **A probe mount**: an NFSv4.1 mount attempt, from a context the export never
  granted, whose outcome is classified as server refusal, client-side failure or
  success. Section 5 is the safety case for it, and it is the only genuinely new
  hazard in this phase.

No new capability flag, following doc 06: every absence these cases can meet is a
finding they exist to report, and a capability would gate them off on exactly the
runs that matter. One existing gate is reused, `Caps.DualStack`, which already
skips SEC-06 where the plan says it should.

## 4. Verdicts

Doc 06's rule, unchanged, plus one row this section adds:

| What happened | Verdict |
|---|---|
| The suite could not reach a source for its own reasons: exec into the server pod refused, `/proc` unreadable, no binary for the node's architecture | **[blocked](storage_terms.md#blocked-and-blocks)**, naming what was refused |
| The deployment does not do what the case is about | **fail**, naming the deployment |
| The case could not create its own precondition | **blocked**, with what it reached |
| **The probe could not establish that its instrument works** | **blocked**, never a pass |

The last row is this phase's own trap, and it is not hypothetical: a mount probe
that fails for a client-side reason looks exactly like an export refusing a
stranger, and the `CAP_SYS_ADMIN` probe in Section 2 produced precisely that
shape. A refusal is reported only when the failure is attributable to the server;
anything else is blocked. **A denial the suite cannot attribute is not a denial.**

## 5. The probe mount, and why it is safe

SEC-05 is the only case here that mounts NFS outside kubelet, and three findings
constrain it.

**It does not run on the host.** F-005 is a node image's `mount.nfs` wrapper
doubling the host mount table on every mount until the node goes to D-state, and
F-003 is the same wrapper making every unmount fail. Both live on the path
`nsenter -m` would take. The probe therefore runs in the **node agent
container's own mount namespace**, where the mount is the kernel's and no wrapper
is involved, and where anything left behind dies with the container rather than
with the node. The agent is already the suite's one privileged component, so
nothing new is deployed.

**It is soft, and it is bounded.** Every other mount in this suite is [`hard`](storage_terms.md#hard-mount),
and preflight rejects `soft` for exactly the right reason. A probe mount is the
exception and says so: a `hard` mount of an export that is about to be deleted is
the F-001 wedge, and a probe whose whole purpose is to attempt something that may
be refused must not be able to retry forever.

**It always unmounts, and it never holds a claim.** The unmount is registered
before the mount is attempted, the probe target is read from the PV rather than
constructed, and the owner pod is torn down by the ordinary path, pods first.
The script arms a trap the moment the mount succeeds and disarms it only once an
unmount has been reported, so a probe killed between the two still unmounts,
lazily if it must, since a mount left in a long-lived privileged container is a
reference to an export the suite is about to delete. Review added the trap; the
ordering above covers the script returning, not the script dying.

## 6. What these nine cases assert

Conventions shared across the suite (capability guards, bounded timeouts, bounds
from `pkg/slo`) live in [`01-test-plan.md`](01-test-plan.md) Section 4.1. The
table below covers the assertions specific to the Security and Identity test
group:

| Case | Assertion | Source & Basis |
|---|---|---|
| **SEC-01** | ✅ A file written as an ordinary uid and gid reads back with both on the writer's own client and on a second node; wrong on both is the server, wrong only on the reader is that node's idmapper | NFSv4 owner strings and the client idmapper; [F-021](findings.md) |
| **SEC-02** | ✅ An owner is refused a `chown` of its own file after a `chmod` control succeeds; root's `chown` gets the same answer on every client, asserted against `-root-squash` where stated and recorded otherwise | [`chown(2)`](https://man7.org/linux/man-pages/man2/chown.2.html); [F-021](findings.md) |
| **SEC-03** | ✅ A non-root pod declaring `fsGroup` writes behind a 0770 directory owned by the gid where an identical pod without it is refused; files already on the share keep their ownership; startup overhead against that control pod within `slo.FSGroupStartOverhead` | Kubernetes [security context](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/) (`fsGroup`); [F-020](findings.md) |
| **SEC-04** | ✅ When node B's pod leaves gracefully and its mount goes, node B's lock is freed, node A's is still held, and node A still reads and writes, all asked from a third node | RFC 8881 Section 8 (state per client) and Section 9 (locking); [`fcntl(2)`](https://man7.org/linux/man-pages/man2/fcntl.2.html) |
| **SEC-05** | ✅ A node with no claim on an export is refused a mount of it, attributably by the server; granted fails with the owner's checksum as evidence; an unproven instrument reports blocked | Plan Section 3.6, "rejected, not silently granted"; [F-018](findings.md) |
| **SEC-06** | ✅ On a dual-stack cluster, the peer the server records over each family is the node's own address in that family, and whether the IPv4 client arrived mapped is recorded; skips on a single-stack cluster | Plan Appendix A, the IPv4 and IPv4-mapped normalization report; [F-019](findings.md) |
| **SEC-07** | ✅ Two pods on one node hold disjoint ranges and one is force-deleted: its range frees within the lease bound, the survivor's stays held and writable, and a replacement is granted the freed range | Kernel [client-identifier](https://docs.kernel.org/filesystems/nfs/client-identifier.html); RFC 8881 Section 9; [F-001](findings.md) |
| **SEC-08** | ✅ Records the mount's security flavour and transport, whether a transport-secured port is offered, whether a pod with no claim reaches 2049, and the NetworkPolicies in the server's namespace; fails only if none could be read | Plan Appendix B; [`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) `sec=`, `xprtsec=`; [F-018](findings.md) |
| **SEC-09** | ✅ Every declared capability reached the server's container: fails on one neither the server process nor PID 1 holds, and on a privileged container; records one the server gave up; judged from the preflight record | [`capabilities(7)`](https://man7.org/linux/man-pages/man7/capabilities.html), [`open_by_handle_at(2)`](https://man7.org/linux/man-pages/man2/open_by_handle_at.2.html); [F-026](findings.md), [F-027](findings.md) |

## 7. Detailed case walkthroughs

### SEC-01: Ownership preserved across pods

A pod running as an ordinary uid and gid writes a file on one node; a pod on
another node reads the ownership back. The point is not that `stat` agrees with
itself, it is that permissions on an RWX share mean the same thing to every
consumer of it: an ownership that changes between clients makes every mode bit
on the volume a function of where the pod was scheduled. NFSv4 carries owners as
strings rather than integers, so the crossing is a real translation and not a
copy, and the classic failure is a client whose idmapper substitutes nobody.

The case separates the two failures it can see, because they belong to different
people. Ownership is read first **on the writer's own client**, where a wrong id
means the mapping broke on the way in and no reader will fix it, and then on the
second client. Wrong on both is the server; wrong only on the reader is that
node's idmapper.

- **Steps**:
  1. Pin a writer running as an ordinary uid and gid to node A and a root
     reader to node B, on one claim.
  2. From the reader, make a directory the ordinary user can write to. If root
     cannot, report blocked: that is export configuration.
  3. Write a file there as the ordinary user. If that is refused, report
     blocked.
  4. Read the uid and gid on the writer's own client.
  5. Read it again on node B, and attribute a wrong id to the server or to
     node B's idmapper as above.

### SEC-02: Who may change a file's ownership

Two questions live here and only one has a single correct answer, which is why
the case treats them differently.

- *Settled everywhere, so asserted.* Changing a file's owner requires privilege,
  and owning the file is not privilege
  ([`chown(2)`](https://man7.org/linux/man-pages/man2/chown.2.html)). An owner who
  could hand a file to somebody else could evade anything counted per owner or
  plant a file in another tenant's name. A `chmod` of the same file runs first as
  a control: a server that refuses every metadata change would otherwise pass the
  assertion vacuously.
- *A deployment choice, so recorded.* What the server does with a client claiming
  uid 0 is compared against `-root-squash` where that states the intent and
  recorded otherwise, but the same answer is required on **every** client.
  A half-squash is worse than either setting, because what a workload may do to
  the shared volume then depends on where it was scheduled.

**The probe is the operation, never the number `stat` prints.** This is the one
decision in the case worth remembering. Reading 65534 and calling it squash is
how the case was wrong from the day it shipped: root squash and a client
idmapper that cannot resolve `root@domain` display identically, so the old
version would have failed a correctly configured export the moment anyone passed
`-root-squash=off` (F-021). Whether a privileged operation is permitted cannot be
confused that way.

- **Steps**:
  1. Start a root pod and an ordinary-uid pod on each of two nodes, on one claim.
  2. Each ordinary user creates a file it owns and `chmod`s it. The `chmod` is
     the control and must succeed; a refusal reports blocked.
  3. Each ordinary user tries to `chown` its own file to a different uid. Both
     must be refused.
  4. Root tries to `chown` a file on each node. Whatever the answer, both nodes
     must give the same one.
  5. Compare against `-root-squash` where it was stated; otherwise record which
     rule is in force and what it means on a shared cluster.
  6. Record separately what the ownership reads back as, naming the idmapper
     when that is what the display reflects.

### SEC-03: What `fsGroup` actually does to an NFS volume

A pod declaring `fsGroup` is promised two things by Kubernetes: the gid as a
supplementary group, and a volume
[modified to be owned and writable by it](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/).
The second is conditional on the volume plugin supporting ownership management,
and the case reports which of the two this deployment gives. Two assertions, both
able to fail and pointing opposite ways:

- *Group access.* A non-root pod declaring `fsGroup` must be able to read and
  write the share. This is the `runAsNonRoot` plus `fsGroup` pattern every
  restricted Pod Security profile pushes workloads towards, so a deployment where
  it cannot write is a deployment where the standard pattern does not work.
  **It has to be asked behind a directory the group actually gates**, mode 0770
  owned by the fsGroup gid, with the identical pod that declares no `fsGroup` as
  the control that must be refused. The populated directory the storm is measured
  over is 0777 by necessity, and a write there succeeds on world permission: the
  first version of this case asked the question there, could not have failed it,
  and produced a wrong finding about the deployment (F-020).
- *No chown storm.* The ownership of files that existed before the pod started
  must be **unchanged**, and the pod must reach Ready within a bound derived from
  a control pod on the same claim without `fsGroup`. A recursive chown on a
  shared RWX volume is not slow, it is destructive: it rewrites the ownership
  another workload is relying on, and it would erase what SEC-01 asserts. The
  sweep runs over a populated directory, so the storm has something to be
  measured against, and the count is recorded whether or not the case fails.

Both startup times come from the cluster, the creation stamp from the API server
and the container start from the kubelet, never from the workstation (F-017).

- **Steps**:
  1. Provision a claim and populate a directory on it from a root pod.
  2. Count the ownership of everything under it.
  3. Start a control pod, non-root, without `fsGroup`, and record how long it
     took to become Ready.
  4. Start an identical pod declaring `fsGroup`, on the same node, and record
     the same interval.
  5. Count the ownership again: anything rewritten is the storm.
  6. Assert the startup difference is within `slo.FSGroupStartOverhead`.
  7. Make a second directory owned by the `fsGroup` gid with mode 0770, so that
     reaching it requires the group rather than world permission. If the export
     will not produce that gate, report blocked.
  8. The control pod must be refused there. If it is not, the gate is not a
     gate, and the case reports blocked rather than pass on world permission.
  9. The `fsGroup` pod must carry the gid, and must be able to write there.
  10. Record which gid the new file landed with, which is the deployment's
      choice rather than a promise.

### SEC-04: Client state isolation between nodes

The upstream report behind this row is per-client rules degrading to the global
access type when connections arrive through a proxy carrying no client identity,
and Kubernetes always inserts a Service.

That report has two possible consequences, and only one of them is this case's.
The first is access control, and **SEC-05 settles it end to end** by having an
uninvited client actually attempt the mount: whether the export refuses a
stranger is a stronger answer than any inspection of what addresses the server
can see, because it is the outcome rather than a precondition of it. The second
is the one nothing else covers. NFSv4.1 holds state per client
([RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 8), so the
client identity is the blast radius of every recovery: when a client goes away
the server discards that client's opens and locks, and if two nodes are one
client it discards both.

That failure is the worst shape an application can get. The surviving pod is
told nothing. It still believes it holds a lock, the file it was protecting is
now writable by anyone, and it will never retry, because from where it stands
nothing happened.

So the case is behavioral and reads nothing from the server. Two pods, one per
node, take write locks on disjoint ranges of one file. Node B's pod is then
removed **gracefully**, and the case waits for node B's mount to go, so that node
B stops being a client of this server rather than merely losing a file
descriptor, which is SEC-07's fault, within one node. Graceful is also the safe
order F-001 requires and the ordinary way a workload leaves a node, so this case
introduces no new hazard.

| What the server does when node B stops being a client | Verdict |
|---|---|
| Node B's range freed, node A's still held, node A still reads and writes | **pass**: the two clients' state is separate |
| Node A's range released too | **fail**: one node's departure discarded another node's locks |
| Node A's lock held but its I/O now fails | **fail**: the departure took the session, not only the lock |
| Node B's range **also** still held | **fail**, and the case says the survival above proves nothing: nothing was discarded, so the fault never landed |
| Locks never reached the server, or node B's pod would not leave the API | **blocked** |

The fourth row is the control, and it is the reason the case can claim anything.
Without it a server that discarded nothing at all would pass on node A's lock
surviving.

**Every lock query comes from a third node.** The Linux client answers `F_GETLK`
out of its own lock table when the conflict is one it already knows about
locally, without asking the server, so node A asking whether node A's lock
survived returns held whatever the server thinks, and that is precisely the
failure this case hunts. A third pod on a third node, holding nothing, does the
asking; its answers can only have come from the server. Node B cannot do it: node
B is the client that had to leave. That makes three schedulable nodes a
precondition, and a smaller cluster skips.

- **Steps**:
  1. One claim, and one pod on each of three nodes: a holder on node A, a
     leaver on node B, and an observer on node C that takes no lock.
  2. Require byte-range locks to reach the server on all three mounts.
  3. The holder and the leaver each take a write lock on a disjoint range of
     one file.
  4. Confirm from the observer that the server holds both at once.
  5. Delete the leaver's pod gracefully and wait until node B has no mount.
  6. From the observer, the leaver's range must be free. This is the control:
     it proves state was discarded at all, so step 7 cannot pass because
     nothing happened.
  7. From the observer, the holder's range must still be held, and the holder
     must still be able to read and write through its mount.

### SEC-05: A client the export never named

An owner pod writes a file with a known checksum. A stranger, with no claim on
that volume and no relationship to it, attempts an NFSv4.1 mount of the owner's
export path, through the probe mount in Section 5.

| Outcome | Verdict |
|---|---|
| Refused, attributably by the server | **pass**, recording the error the server produced |
| Granted | **fail**, and the case reads the owner's bytes and compares the checksum, so the report is evidence rather than an inference |
| Failed for a client-side reason, or the probe could not run | **blocked** (Section 4) |

Reading the bytes on failure is deliberate. "The mount succeeded" invites the
answer that the mount point was empty; a matching checksum of another workload's
file does not.

**The instrument is proven before a refusal is believed.** A container that
cannot mount NFS at all and an export refusing a stranger both reach userspace as
`EACCES`. So the stranger first probes a second export, from the same node and
the same container, that its node already mounts through kubelet. That probe can
only fail for a client-side reason, and when it fails the case reports blocked.

- **Steps**:
  1. Two claims: the owner's, mounted on node A, and a neighbour's, mounted on
     node B.
  2. Write a file into the owner's claim and checksum it.
  3. From node B's node agent container, probe the neighbour's export. This is
     the control, and a refusal here reports blocked.
  4. From the same container, probe the owner's export, which node B has never
     been served and has no claim on.
  5. Granted fails, with the owner's checksum read through the probe as the
     evidence. An attributable refusal passes. Anything else is blocked.

### SEC-06: One client, two address families

Gated on `Caps.DualStack`, so it skips where the plan says it should. A server
that normalises addresses inconsistently has two identities for one client, and
the consequence is an export rule that matches over one family and not the
other: a node that mounts over IPv4 today and over IPv6 after a restart gets a
different answer from the same rule. The IPv4-mapped form (`::ffff:a.b.c.d`) is
where this goes wrong, because a server listening on IPv6 sees every IPv4 client
in it, and a rule written as a plain IPv4 address may or may not be compared
against it.

Both mounts address the server pod directly, one address per family, and bypass
the Service. A Service is single-family on most clusters, so going through it
could only ever exercise one side, and what is under test is the server's view
of one client rather than the proxy's.

**The case asserts the address, not the identity.** It establishes that the
server records, in each family, the node's own address and no other party's,
and it records whether the IPv4 client arrived mapped: that is the normalisation
the upstream report is about, and what an export rule is written against. It
does not establish that the server assigns one NFSv4 client identity across the
two families. A server can record both addresses correctly and still hold two
sets of state, and nothing visible from a client tells those apart: the identity
lives in the server's state tables, both mounts are on one node whose kernel
would answer a lock query locally, and reading the server's state is out of
bounds here. So the case says what it asserts rather than claiming the identity
and testing the address.

Where the cluster is single-stack the case skips, and the IPv4-mapped form is
asserted by nobody: it is recorded in F-019 and visible in what SEC-08 reads.

- **Steps**:
  1. Provision a claim on one node, so an export exists that this node is
     already a client of.
  2. Read the server pod's own addresses; report blocked unless it has both
     families.
  3. Clone the export twice, once per family, and mount both on that node.
  4. Write through each, so each has an established connection.
  5. The peer the server records for each must be that node's own address in
     that family. Anything else means the address an export rule would be
     matched against is not the client's.
  6. Record whether the IPv4 client arrived mapped.

### SEC-07: Two pods, one client identity, one of them gone

Two pods on **one node**, which is what makes them one client to the server, hold
byte-range locks on disjoint ranges of one file. One is force-deleted, modelling
a client that vanished without unlocking, which is the use F-001 blesses and
names this case for, and is then replaced on the same node. The survivor's lock
must still be held throughout, and the replacement must be granted the range the
vanished pod held. What this rules out is the state collision the shared client
identifier makes possible: one pod's departure taking another pod's state with
it, because the server cannot tell them apart.

The lock state is read from a prober on the other node. A prober on the same
node would be asking its own kernel, which knows the ranges locally; a prober on
another node is a different client, and its query is answered by the server.

- **Steps**:
  1. Two pods on node A on one claim, and a prober on node B.
  2. Require byte-range locks to reach the server on both mounts.
  3. Each node A pod takes a write lock on a disjoint range of one file.
  4. Confirm from the prober that the server holds both.
  5. Force-delete one of the node A pods. The mount is not waited out: the
     survivor still holds it, legitimately (Section 10).
  6. Wait for the vanished pod's range to become free, within
     `slo.LockReleaseBound`.
  7. The survivor's range must still be held, and the survivor must still be
     able to write through its mount.
  8. A replacement pod on node A must be granted the freed range, and the
     survivor's lock must still be held after it is.

### SEC-08: The data path, stated

The plan makes this a finding rather than a verdict, and this document does not
quietly upgrade it. It records four things and asserts none: the mount's
security flavour and transport as `/proc/mounts` reports them, whether the
server offers a transport-secured port at all, whether a pod with no claim can
reach 2049, and whether anything in the Kubernetes API restricts who may. It
**fails only if it could read none of them**, which is the one way a recording
case can be wrong. A reading the case could not take is recorded as not taken,
never as a negative: a connection attempt whose exec failed says nothing about
the network (Section 4).

No packet capture is taken. It would be the direct evidence, and it needs
`tcpdump` on a node image that has none; F-006 is the precedent for what an
instrument nobody verified is worth. The case says that it did not capture, and
what it substituted, in the record it writes.

- **Steps**:
  1. Mount a claim and read the mount's own options: the security flavour, the
     transport, and whether any transport security appears at all.
  2. Read the server's listening ports, to say whether a secured port is
     offered.
  3. From a pod with no claim, attempt a connection to the server's NFS port.
  4. List the NetworkPolicies in the server's namespace.
  5. Write all four into the [bundle](storage_terms.md#bundle-and-evidence),
     and fail only if none could be read.

### SEC-09: The capability set the server declared, and the one it has

Read the server pod's declared capabilities, the set of the process serving NFS,
and the set of that container's PID 1. Two processes, because the question has
two halves and each process answers one. The server is whatever holds the
listening socket on 2049, found and read through the node agent (F-026, F-027),
and its set is what the server actually holds. PID 1 is what the runtime
started, so its set is what the platform delivered. Preflight reads all three
and records them; the case judges the record, and needs no node agent itself.

| What was found | Verdict |
|---|---|
| A declared capability the server process holds | nothing to report, whatever PID 1 holds: a supervisor may drop a capability from its own set after starting the server, so PID 1 lacking it later proves nothing, and the server holding it proves the platform delivered it |
| A declared capability neither the server process nor PID 1 holds | **fail**, naming it: the platform took it between the spec and the process, which is the EPERM trap the plan's row describes. A file-handle backend without `CAP_DAC_READ_SEARCH` fails operations rather than failing to start |
| A declared capability PID 1 holds and the server process does not | **record**, naming it: something inside the container gave it up, the server or its supervisor |
| The container is privileged | **fail**: nothing is constraining the server, so "the set it needs and no more" cannot be true |
| Otherwise | **pass**, recording both sets and how far the server's exceeds what was declared |

Held means the permitted set, not the effective one: a process may lower a
capability from its effective set and raise it again when it needs it, and only
one gone from the permitted set is gone for good
([`capabilities(7)`](https://man7.org/linux/man-pages/man7/capabilities.html)).

The third row is a record and not a failure, and that is the decision in this
case. The plan's failure is a policy stripping a capability, and a capability the
platform delivered and the server then discarded is the opposite: the server
narrowing its own set, which is what "no more" asks for. Failing it would report
a server's hardening as a platform defect and point the operator at the wrong
people. What it does say is that the declaration asks for more than the server
keeps, so the record is where that goes. The one thing PID 1 cannot rule out as
a witness is a first process that dropped a capability itself before the case
read it, and the failure message says so rather than claiming the platform for
certain.

PID 1 is only a witness where it is the container's own first process. With
`hostPID` it is the node's init, and with a process namespace shared across the
pod it is the pause process; both would give a real answer about the wrong
process, so the case reports blocked on either rather than reading it.

The last row's excess is a record on purpose. The runtime's default set is the
platform's choice, not the server's, and a suite that failed on it would fail on
every conformant cluster.

**The sets are read by preflight, not by the case.** Preflight reads the
declaration and both sets once, beside the server process name it already
records, and writes them to `environment.json`; SEC-09 judges that record and
reads nothing from a node. The sets follow from the image, the pod spec and the
platform, none of which a case changes, and naming the server needs the node
agent (F-027), so reading them per case would buy a privileged dependency and no
information. The test plan's harness design section states the rule for every
case.

What the case still reads live is the pod, through the API: a record read from
another pod instance does not describe it, and the case reports blocked with
`-refresh-preflight` rather than judge it. The record carries the pod's UID for
this: the security context, the process namespace and the node cannot change
without a new pod. A server that preforks is read in every process holding the
socket, so a master that kept a capability its workers dropped does not stand in
for them; where their permitted sets differ the case reports blocked. Preflight
carries whether an unread set was a condition of the cluster or the harness
failing, so the case reports the first blocked and the second failed.

- **Steps**:
  1. Take each server preflight recorded. Report blocked where preflight could
     not read the sets for a reason of this cluster's, and fail where it could
     not read them for a reason another cluster would share, which is the
     harness failing.
  2. Check the record against the live pod: the same pod UID, or blocked with
     `-refresh-preflight`.
  3. Fail on a declared capability neither the server process nor PID 1 held.
     Record one the server gave up after PID 1 was given it.
  4. Fail if the container is privileged.
  5. Record both sets, and how far the server's exceeds the declaration.
  6. Report blocked, after judging the rest, if any server went unjudged.

## 8. Decisions worth keeping

**No export configuration is read.** Every server states its rules in its own
syntax, and a case that parsed one would be testing a config file rather than a
deployment. Everything here is read from the protocol's own effects: who
connected, what was granted, what the file says afterwards.

**No case asserts a policy the operator did not state.** SEC-02 set this
precedent with `-root-squash` and no flag is added to widen it. Where the correct
answer depends on intent, the case records; where it depends on mechanism, the
case asserts.

**Perform the operation, and ask a witness that cannot answer locally.** Where a
case can perform the operation whose outcome it cares about, inspecting a
precondition instead is the weaker test, and on this deployment it was the wrong
one. Performing it is not enough either: the answer has to come from a witness
that must go to the server for it. Review caught a case breaking one half or the
other five times, across three cases (Section 11).

**SEC-05 fails rather than blocks when everyone is granted.** The plan's expected
result is "rejected, not silently granted", so an export that grants any client
that can reach it has produced the failing outcome, not an absent one. F-008 is
the precedent: a finding goes where an operator will see it.

**One node for SEC-07, two for SEC-04.** The two cases need opposite topologies
for the same reason, and getting either backwards makes it pass vacuously: two
pods on two nodes are two clients and cannot collide, and two pods on one node
are one client and cannot be told apart. Each also needs one more node than its
subject, for the witness above.

**Not built, and what each would have served:**

| Not built | Would have served | Why not |
|---|---|---|
| A packet capture | SEC-08, directly | No `tcpdump` on the node image, and an unverified instrument reports silence (F-006) |
| An export-config parser | SEC-04, SEC-05 | Server-specific syntax; the effects are portable and the config is not |
| A NetworkPolicy fault | SEC-05's denial half | `Caps.CanNetworkPolicy` records that the API is served, not that the CNI enforces it, and a partition is step 10's |
| An RPC-over-TLS client | SEC-08 | Out of scope for v1 (plan Appendix B), and contingent on this case's finding |
| A second privileged pod | SEC-05 | The node agent already is one |

## 9. What a green run does and does not claim

A green SEC section says: an identity written through one client is the identity
another reads back, an owner cannot give a file away, the server distinguishes
its clients, a stranger is refused, one client's state survives another's
disappearance, `fsGroup` grants what it promises without rewriting the volume,
and the platform delivered every capability the server declared.

It does not say the server kept them all. A server may give up what it was
delivered, and SEC-09 records that rather than failing on it.

It does not say the export's rules are the right rules, that AUTH_SYS is
sufficient, that traffic is confidential, or that a pod cannot impersonate
another pod's uid: on AUTH_SYS over a shared network, it can, and nothing in v1
is asked to pretend otherwise. It says nothing about Kerberos or TLS, which
Appendix B defers.

## 10. What real runs taught

What the security cases return is test plan
[Section 5.2](01-test-plan.md#52-what-the-latest-runs-returned)'s, and the runs
behind each lesson are in its finding. What the design took from them:

- **[F-018](findings.md) (The export admits any client that can reach it)**:
  SEC-05's red, which is the finding this phase was written to reach, and it
  stays red. The same server offers the NFSv3 ancillary ports (111, 662, 875,
  20048, 32803) alongside 2049. SEC-08 records them; what an NFSv3 mount of the
  same export would be granted is not asked by any case here.
- **[F-019](findings.md) (The client an NFS server can name is the node)**: the
  IPv4-mapped peer form is not hypothetical on a single-stack cluster. The server
  binds `:::2049`, so every client appears in `/proc/net/tcp6` and
  `/proc/net/tcp` alone shows no NFS connections at all. SEC-06 still skips
  there, and the normalisation it was written for is visible in what SEC-08
  reads.
- **[F-021](findings.md) (A root-owned file reads back as nobody for a
  reason)**: SEC-02 run with `-root-squash=off`, the branch that asserts rather
  than records. What `stat` shows cannot tell squash from a failed mapping, so
  the case asks whether a `chown` is permitted.
- **[F-026](findings.md) and [F-027](findings.md) (The process serving NFS is
  not the container's command)**: SEC-09's pass before #98 read the container's
  PID 1 rather than the server and could not have failed. Naming the server
  needs the node agent, which is why preflight reads the sets.
- **SEC-07 cannot use `ForceDeletePodAndAwaitUnmount`.** The survivor
  legitimately holds the same mount on the same node, so there is no unmount to
  wait for; the case force-deletes and leaves the claim to the survivor's
  ordinary teardown. DATA-06's shape does not transfer to two pods on one node.

## 11. What changed after this was written

- **Consolidation by test group.** SEC-01 shipped in step 2 and SEC-02 in step
  2b, before any design doc covered them. They are documented here for the first
  time, so that a reader asking what the suite checks about security finds all
  of it in one place.
- **Step 8 went in as one change, not three (2026-09-14,
  [PR #57](https://github.com/mikebz/nfs-verification/pull/57)).** It was
  planned as three pull requests, all `TestSec` under `make test-sec`, category
  budget 30 minutes: SEC-03 and SEC-09, with `fsGroup` on the pod spec, the
  ownership sweep and the capability reader, and no new privilege or hazard;
  SEC-04, SEC-06 and SEC-08, with the peer table, the two cases that read it,
  and the recording case; and SEC-05 and SEC-07, with the probe mount and its
  safety case, and the identity collision case on `locktool`. The split was
  drawn to keep the probe mount away from everything else, and the harness it
  needed turned out to be read-only code with unit tests of its own, so
  separating it bought review isolation the hazard did not need. The probe
  mount's safety case is Section 5 either way, and it is the part to read first.
  The plan also said to expect red: the probes in Section 2 already said SEC-05
  would fail on this provisioner, and why. It did (F-018).
- **Review rewrote three cases ([PR #57](https://github.com/mikebz/nfs-verification/pull/57)),**
  all for the same reason: each was reading something *about* the server, or
  about a precondition, instead of asking the server the question it cared
  about.
  - SEC-04 first read the source addresses the server attributed to each node,
    from the server's own socket table, and later each node's `clientaddr=` with
    the socket table as corroboration. Review rejected both, and correctly.
    Reading the server's internals is not a client-observable property, and the
    address version measured a *precondition* for access control while SEC-05
    already measures access control itself. It now reads nothing from the
    server: it takes two clients' locks and removes one client. The peer-table
    helpers survive because SEC-06 and SEC-08 use them; SEC-04 no longer does.
  - SEC-02 read what `stat` printed, and now asks whether a `chown` is
    permitted (F-021).
  - SEC-03 wrote into the world-writable export root, where the write would have
    succeeded with no supplementary group at all, and now writes behind a gated
    directory with a control pod that must be refused.
- **A second review round, automated, in the same PR,** found the cases could
  still pass without having tested anything, in two more places. SEC-04's
  behavioural version asked node A whether node A's lock had survived, which the
  Linux client answers locally; the third-node observer is the fix, and three
  schedulable nodes is its cost. SEC-03's gate arrived setgid, so the file
  inherited the gate's group and the case reported that the volume manages
  ownership, which it does not. Both did the right operation and then asked the
  wrong witness. The SEC-03 fix also overturned F-020, which had been published
  from the ungated probe; the correction is recorded there rather than here.
- **SEC-09 judges the server, not PID 1, from a preflight record (2026-10-01,
  [#98](https://github.com/mikebz/nfs-verification/issues/98),
  [PR #120](https://github.com/mikebz/nfs-verification/pull/120)).** The case
  shipped reading PID 1 alone and calling it the server, which on a supervised
  server it is not (F-026). It asserted on `nfs-provisioner` instead of
  `ganesha.nfsd`, and passed six runs on two deployments where the two differ:
  on `gke-w2`, Ganesha 15.3-mb lowers `CAP_SYS_RESOURCE` at every start, so the
  server holds one of its two declared capabilities and PID 1 holds both. #98
  split the read. The verdict on each deployment did not change, and that is
  correct rather than a sign the change did nothing: the platform delivered both
  capabilities on both clusters, and `gke-w2`'s run now records the one its
  server gave up, which no earlier run could see. The same change moved the
  reads out of the case into preflight (SEC-09 in Section 7). Three approaches
  were tried and dropped on the way:
  - Comparing the declaration and the node field by field, to detect a stale
    record. Dropped in review: it missed `runAsUser`, and every field it did not
    list. The pod UID replaced it.
  - Recording the resolved image digest as well, to catch an image edited in
    place. Dropped at the owner's direction: this is a test suite, not a
    defence, and whoever edits the server mid-run can refresh preflight.
  - Intersecting the permitted sets of a preforking server's processes. Dropped
    in review, because it loses the holder that proves a capability reached the
    container and files an in-container drop as the platform's. The case now
    reports blocked where those sets differ.
- **Run results are no longer kept here (2026-10-01,
  [#126](https://github.com/mikebz/nfs-verification/issues/126)).** A table of
  the 2026-09-14 run on `gke-w1` stood in Section 10 and still listed SEC-09 as
  the pass that read PID 1. It was removed rather than corrected, because a
  result in a design doc is out of date by the next run. What the security cases
  return is test plan
  [Section 5.2](01-test-plan.md#52-what-the-latest-runs-returned)'s.
- **SEC-06's description now matches what it asserts (2026-10-02).** This
  document said the case required the identity the server records for a client to
  be one client, not two. The case as shipped in PR #57 asserts that the peer the
  server records over each family is the node's own address in that family, and
  says why it cannot assert one identity from a client (SEC-06 in Section 7).
  The description was corrected, and so were Sections 1, 2 and 6, which said
  the same thing or that the case records on a single-stack cluster, where it
  skips. Test plan Section 3.6's SEC-06 row said "client identity consistent"
  and was amended to name what the case verifies and what it does not, since
  what a case verifies belongs there, as DATA-11's narrowing did (doc 05). The
  case did not change.

## 12. Open

- What bound a `fsGroup` mount should be held to when a chown storm *is*
  happening has no ratified value. The case measures against a control pod on the
  same claim rather than inventing one, and the number belongs in `pkg/slo`.
- ~~The grace log channel in Section 2's last row may unblock OBS-03 and
  CHAOS-07.~~ **Settled 2026-10-02**: the harness reads that file now, through the
  node agent ([F-030](findings.md)), and CHAOS-07 runs.

## 13. Sources

- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 2.4 for client
  identity, Section 8.3 for the single lease per client, Section 2.2.1.1 for
  AUTH_SYS as an optional security flavor, Section 9 for byte-range locks, and
  Section 21 for what AUTH_SYS does not authenticate.
- Kernel
  [client-identifier](https://docs.kernel.org/filesystems/nfs/client-identifier.html):
  one lease per client per server, shared by every mount and every pod on the
  node. SEC-07 is written around it, and SEC-06 says why it cannot assert one
  client ID across address families.
- [`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) for `sec=`,
  `xprtsec=`, `soft` and the reserved source port, which is the identity the
  server sees.
- [`chown(2)`](https://man7.org/linux/man-pages/man2/chown.2.html) for who may
  change a file's owner (SEC-02), and
  [`fcntl(2)`](https://man7.org/linux/man-pages/man2/fcntl.2.html) for
  `F_GETLK`, which the Linux client may answer locally (SEC-04, SEC-07).
- Kubernetes
  [security context](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/)
  for what `fsGroup` promises and on which volumes,
  [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
  for the capability expectations SEC-09 reads against, and
  [Services](https://kubernetes.io/docs/concepts/services-networking/service/)
  for the proxy in front of every server in this architecture.
- [`capabilities(7)`](https://man7.org/linux/man-pages/man7/capabilities.html)
  for the mask SEC-09 decodes, and
  [`open_by_handle_at(2)`](https://man7.org/linux/man-pages/man2/open_by_handle_at.2.html)
  for why `CAP_DAC_READ_SEARCH` is the one that matters to a file-handle backend.
- [`findings.md`](findings.md): F-001 for force deletion and teardown order,
  F-003 and F-005 for the node-side mount path this phase refuses to use, F-006
  for unverified instruments, F-008 for where a finding belongs.

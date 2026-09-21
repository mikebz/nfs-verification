# 07: Security and identity: who the server thinks you are

Author: mikebz@
Created: 2026-09-13
Updated: 2026-09-17
Status: **in review**, [PR #57](https://github.com/mikebz/nfs-verification/pull/57).
SEC-03 to SEC-09 landed in delivery step 8; SEC-01 shipped in Step 2
([PR #3](https://github.com/mikebz/nfs-verification/pull/3)) and SEC-02 in Step 2b
([PR #4](https://github.com/mikebz/nfs-verification/pull/4)), and both are
documented here for the first time. Consolidated to serve the complete Security
test group. Every case has been run against a live cluster; Section 10 is what
that run returned.
Serves: SEC-01 through SEC-09. Requirements in
[`01-test-plan.md`](01-test-plan.md) Section 3.6.
Builds on [`03-chaos-operations-design.md`](03-chaos-operations-design.md),
[`04-grace-and-lock-reclaim-design.md`](04-grace-and-lock-reclaim-design.md),
[`05-data-path-and-locktool-design.md`](05-data-path-and-locktool-design.md) and
[`06-observability-design.md`](06-observability-design.md), whose rules all still
hold. It takes doc 06's verdict rule unchanged and applies it to a different
question.

---

## 1. The decision that shapes everything here

**The client is the node, not the pod, and this phase measures identity rather
than asserting a policy.**

Two facts decide every case below. The first is architectural: the NFS mount is
made by the node's kernel, so every identity the server can see — the source
address, the reserved source port, the NFSv4 client identifier — belongs to the
node, and is shared by every pod and every mount on it
([client-identifier](https://docs.kernel.org/filesystems/nfs/client-identifier.html)).
A pod is invisible to the server. The second is that access control here is
AUTH_SYS (plan Appendix B), which is an assertion of identity, not a proof of
one.

So a case in this section cannot assert that the right policy is configured:
nobody told this suite what the export's policy is meant to be, and inventing one
turns a deployment choice into a test failure — the mistake SEC-02 already
refuses to make with `-root-squash`. What a case **can** assert is that the
identity machinery works as the deployment must rely on it working:

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
- that one client's state is not confused with another's (SEC-06, SEC-07),
- that the identity Kubernetes thinks it is granting is the one the volume
  actually honours (SEC-03),
- and that the capability set the server runs with is the one it declared
  (SEC-09).

SEC-08 asserts nothing at all, by the plan's own wording, and Section 5 says how
that is kept honest rather than decorative.

## 2. What the probes already settled

Seven hand-run probes against the cluster this project uses (GKE v1.37, three COS
workers, `nfs-server-provisioner` v4.0.8 behind a ClusterIP Service, StorageClass
`nfs`) were run before this document, because most of the design decisions below
turn on answers the code could otherwise only guess at. These are probes, **not
suite runs**; no case exists yet, and nothing here is a result.

| Probe | What came back | What it decides |
|---|---|---|
| The server's socket table while a pod held the share | The peer is the **node's** address on a reserved port, never the pod's | The client is the node, shared by every pod on it. That fact underlies SEC-04 and SEC-07, though neither reads it: it is why a node is the unit whose state can be lost. Recorded as F-019 |
| The same table, address family | The listener is IPv6 and an IPv4 client arrives **IPv4-mapped** | SEC-06 has a subject even on a single-stack cluster, and it is a record rather than an assertion there |
| An unrelated pod, no claim, mounting another claim's export | **Granted**, read-write, and the other tenant's bytes came back | SEC-05 has an outcome to report and the report is red |
| The same mount with `CAP_SYS_ADMIN` only | Refused with `EACCES`, for a **client-side** reason | The trap this phase most needs to avoid: a probe that cannot mount reports a refusal that never happened |
| Ownership of a root-written file, server side and client side | uid 0 on the server, **nobody** on the client | The export does not squash; the client cannot map the owner *name*. SEC-02 read this as squash, which was a finding against this suite; it now asks whether a `chown` is permitted, which the idmapper cannot answer wrongly. F-021 |
| A pod with `fsGroup`, on a populated share | Supplementary gid granted, **volume ownership untouched** | SEC-03's two halves, and which of them can fail. The access half was probed in the export root, which is world-writable and setgid, so the write proved nothing about the gid and the group the new file inherited was the directory's, not the volume's. Both mistakes are corrected in F-020, and SEC-03 now asks the question behind a gate |
| The server's grace announcements | Present in a log **file inside the export**, absent from the container's stdout | Refines F-008: the signal exists, the channel does not carry it |

The last two rows are findings in their own right and are filed as such when the
code that met them lands, per AGENTS.md. Neither changes what this phase builds.

## 3. What this phase builds

Four readers and one probe, and nothing else:

- **The peer table.** The connections a server pod has, read from `/proc/net`
  inside it and parsed in Go: local and peer address, port, state, with
  IPv4-mapped addresses normalised. Implementation-neutral: nothing reads an
  export config file, because that syntax is the server's, not the protocol's.
- **The capability set.** The declared set from the server pod's spec, and the
  effective set of its PID 1, decoded from the mask to names.
- **The volume ownership sweep.** How many files under a path have which owner
  and group, from one exec, so "nothing was chowned" is a count and not an
  impression.
- **`fsGroup` on the fixture's pod spec**, the field `pod.go` has been reserving
  for SEC-03 since step 2.
- **A probe mount**: an NFSv4.1 mount attempt, from a context the export never
  granted, whose outcome is classified as server refusal, client-side failure or
  success. Section 6 is the safety case for it, and it is the only genuinely new
  hazard in this phase.

No new capability flag, following doc 06: every absence these cases can meet is a
finding they exist to report, and a capability would gate them off on exactly the
runs that matter. One new gate is reused, `Caps.DualStack`, which already exists
and already skips SEC-06 where the plan says it should.

## 4. Verdicts

Doc 06's rule, unchanged, plus one row this section adds:

| What happened | Verdict |
|---|---|
| The suite could not reach a source for its own reasons: exec into the server pod refused, `/proc` unreadable, no binary for the node's architecture | **blocked**, naming what was refused |
| The deployment does not do what the case is about | **fail**, naming the deployment |
| The case could not create its own precondition | **blocked**, with what it reached |
| **The probe could not establish that its instrument works** | **blocked**, never a pass | 

The last row is this phase's own trap, and it is not hypothetical: a mount probe
that fails for a client-side reason looks exactly like an export refusing a
stranger, and the `CAP_SYS_ADMIN` probe in Section 2 produced precisely that
shape. A refusal is reported only when the failure is attributable to the server;
anything else is blocked. **A denial the suite cannot attribute is not a denial.**

## 5. The nine cases

The first two shipped before this document existed, in Steps 2 and 2b. They are
described here because a reader asking what the suite checks about security
should find all of it in one place, and because SEC-02's assertion changed in
review (F-021).

**SEC-01: does an identity survive the crossing.** A pod running as an ordinary
uid writes a file on one node; a pod on another node reads the ownership back.
The point is not that `stat` agrees with itself, it is that permissions on an
RWX share mean the same thing to every consumer of it: an ownership that changes
between clients makes every mode bit on the volume a function of where the pod
was scheduled. NFSv4 carries owners as strings rather than integers, so the
crossing is a real translation and not a copy, and the classic failure is a
client whose idmapper substitutes nobody.

The case separates the two failures it can see, because they belong to different
people. Ownership is read first **on the writer's own client**, where a wrong id
means the mapping broke on the way in and no reader will fix it, and then on the
second client. Wrong on both is the server; wrong only on the reader is that
node's idmapper.

**SEC-02: who may change a file's ownership.** Two questions live here and only
one has a single correct answer, which is why the case treats them differently.

- *Settled everywhere, so asserted.* Changing a file's owner requires privilege,
  and owning the file is not privilege
  ([`chown(2)`](https://man7.org/linux/man-pages/man2/chown.2.html)). An owner who
  could hand a file to somebody else could evade anything counted per owner or
  plant a file in another tenant's name. A `chmod` of the same file runs first as
  a control: a server that refuses every metadata change would otherwise pass the
  assertion vacuously.
- *A deployment choice, so recorded.* What the server does with a client claiming
  uid 0 is compared against `-root-squash` where that states the intent and
  recorded otherwise — but the same answer is required on **every** client.
  A half-squash is worse than either setting, because what a workload may do to
  the shared volume then depends on where it was scheduled.

**The probe is the operation, never the number `stat` prints.** This is the one
decision in the case worth remembering. Reading 65534 and calling it squash is
how the case was wrong from the day it shipped: root squash and a client
idmapper that cannot resolve `root@domain` display identically, so the old
version would have failed a correctly configured export the moment anyone passed
`-root-squash=off` (F-021). Whether a privileged operation is permitted cannot be
confused that way.

**SEC-03: what `fsGroup` actually does to an NFS volume.** A pod declaring
`fsGroup` is promised two things by Kubernetes: the gid as a supplementary group,
and a volume
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

**SEC-04: does one client's departure destroy another's state.** The upstream
report behind this row is per-client rules degrading to the global access type
when connections arrive through a proxy carrying no client identity, and
Kubernetes always inserts a Service.

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
descriptor — which is SEC-07's fault, within one node. Graceful is also the safe
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
survived returns held whatever the server thinks — which is precisely the
failure this case hunts. A third pod on a third node, holding nothing, does the
asking; its answers can only have come from the server. Node B cannot do it: node
B is the client that had to leave. That makes three schedulable nodes a
precondition, and a smaller cluster skips.

**What changed after this was written.** The first implementation asked a
different question: it read the source addresses the server attributed to each
node, from the server's own socket table, and later from each node's
`clientaddr=` with the socket table as corroboration. Review rejected both, and
correctly. Reading the server's internals is not a client-observable property,
and the address version measured a *precondition* for access control while
SEC-05 already measures access control itself. The behaviour above is what
distinguishable clients actually buy an application, and it is observable
entirely through pods. The peer-table helpers survive because SEC-06 and SEC-08
use them; SEC-04 no longer does.

A third rewrite followed, in the same PR. The behavioural version above shipped
asking node A whether node A's lock had survived, which the Linux client can
answer without the server; automated review caught it. The observer described
above is the fix, and the cost is the three-node precondition.

**SEC-05: a client the export never named.** An owner pod writes a file with a
known checksum. A stranger, with no claim and no relationship to that volume,
attempts an NFSv4.1 mount of the owner's export path.

| Outcome | Verdict |
|---|---|
| Refused, attributably by the server | **pass**, recording the error the server produced |
| Granted | **fail**, and the case reads the owner's bytes and compares the checksum, so the report is evidence rather than an inference |
| Failed for a client-side reason, or the probe could not run | **blocked** (Section 4) |

Reading the bytes on failure is deliberate. "The mount succeeded" invites the
answer that the mount point was empty; a matching checksum of another workload's
file does not.

**SEC-06: one client, two address families.** Gated on `Caps.DualStack`, so it
skips where the plan says it should. Where both families are present, the same
export is mounted over each and the identity the server records for the client is
required to be one client, not two: an inconsistently normalised address is the
upstream report, and its consequence is a rule that matches on one family and not
the other. Where the cluster is single-stack the case skips, and the IPv4-mapped
form is asserted by nobody: it is recorded in F-019 and visible in what SEC-08
reads.

**SEC-07: two pods, one client identity, one of them gone.** Two pods on **one
node**, which is what makes them one client to the server, hold byte-range locks
on disjoint ranges of one file. One is force-deleted, modelling a client that
vanished without unlocking — the use F-001 blesses and names this case for — and
is then replaced on the same node. The survivor's lock must still be held
throughout, and the replacement must be granted a lock of its own. What this
rules out is the state collision the shared client identifier makes possible: one
pod's departure taking another pod's state with it, because the server cannot
tell them apart.

**SEC-08: the data path, stated.** The plan makes this a finding rather than a
verdict, and this document does not quietly upgrade it. It records four things
and asserts none: the mount's security flavour and transport as `/proc/mounts`
reports them, whether the server offers a transport-secured port at all, whether
a pod with no claim can reach 2049, and whether anything in the Kubernetes API
restricts who may. It **fails only if it could read none of them**, which is the
one way a recording case can be wrong.

No packet capture is taken. It would be the direct evidence, and it needs
`tcpdump` on a node image that has none; F-006 is the precedent for what an
instrument nobody verified is worth. The case says that it did not capture, and
what it substituted, in the record it writes.

**SEC-09: the capability set the server declared, and the one it has.** Read the
server pod's declared capabilities and the effective set of its PID 1.

| What was found | Verdict |
|---|---|
| A declared capability is absent at runtime | **fail**, naming it: this is the EPERM trap the plan's row describes, and a file-handle backend without `CAP_DAC_READ_SEARCH` fails operations rather than failing to start |
| The container is privileged | **fail**: nothing is constraining the server, so "the set it needs and no more" cannot be true |
| Otherwise | **pass**, recording the effective set and how far it exceeds what was declared |

The third row is a record and not an assertion on purpose. The runtime's default
set is the platform's choice, not the server's, and a suite that failed on it
would fail on every conformant cluster.

## 6. The probe mount, and why it is safe

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

**It is soft, and it is bounded.** Every other mount in this suite is `hard`,
and preflight rejects `soft` for exactly the right reason. A probe mount is the
exception and says so: a `hard` mount of an export that is about to be deleted is
the F-001 wedge, and a probe whose whole purpose is to attempt something that may
be refused must not be able to retry forever.

**It always unmounts, and it never holds a claim.** The unmount is registered
before the mount is attempted, the probe target is read from the PV rather than
constructed, and the owner pod is torn down by the ordinary path, pods first.
The script arms a trap the moment the mount succeeds and disarms it only once an
unmount has been reported, so a probe killed between the two still unmounts —
lazily if it must, since a mount left in a long-lived privileged container is a
reference to an export the suite is about to delete. Review added the trap; the
ordering above covers the script returning, not the script dying.

## 7. Decisions worth keeping

**No export configuration is read.** Every server states its rules in its own
syntax, and a case that parsed one would be testing a config file rather than a
deployment. Everything here is read from the protocol's own effects: who
connected, what was granted, what the file says afterwards.

**No case asserts a policy the operator did not state.** SEC-02 set this
precedent with `-root-squash` and no flag is added to widen it. Where the correct
answer depends on intent, the case records; where it depends on mechanism, the
case asserts.

**SEC-05 fails rather than blocks when everyone is granted.** The plan's expected
result is "rejected, not silently granted", so an export that grants any client
that can reach it has produced the failing outcome, not an absent one. F-008 is
the precedent: a finding goes where an operator will see it.

**One node for SEC-07, two for SEC-04.** The two cases need opposite topologies
for the same reason, and getting either backwards makes it pass vacuously: two
pods on two nodes are two clients and cannot collide, and two pods on one node
are one client and cannot be told apart.

**Not built, and what each would have served:**

| Not built | Would have served | Why not |
|---|---|---|
| A packet capture | SEC-08, directly | No `tcpdump` on the node image, and an unverified instrument reports silence (F-006) |
| An export-config parser | SEC-04, SEC-05 | Server-specific syntax; the effects are portable and the config is not |
| A NetworkPolicy fault | SEC-05's denial half | `Caps.CanNetworkPolicy` records that the API is served, not that the CNI enforces it, and a partition is step 10's |
| An RPC-over-TLS client | SEC-08 | Out of scope for v1 (plan Appendix B), and contingent on this case's finding |
| A second privileged pod | SEC-05 | The node agent already is one |

## 8. What a green run does and does not claim

A green SEC section says: an identity written through one client is the identity
another reads back, an owner cannot give a file away, the server distinguishes
its clients, a stranger is refused, one client's state survives another's
disappearance, `fsGroup` grants what it promises without rewriting the volume,
and the server holds the capabilities it declared and no more than the platform
gave it.

It does not say the export's rules are the right rules, that AUTH_SYS is
sufficient, that traffic is confidential, or that a pod cannot impersonate
another pod's uid: on AUTH_SYS over a shared network, it can, and nothing in v1
is asked to pretend otherwise. It says nothing about Kerberos or TLS, which
Appendix B defers.

## 9. Delivery

SEC-01 went in with Step 2 and SEC-02 with Step 2b, both before any design doc
covered them; the plan's delivery table has the PRs. Step 8 planned the remaining
seven as three pull requests, all `TestSec` under `make test-sec`, category
budget 30 minutes:

1. **SEC-03 and SEC-09.** `fsGroup` on the pod spec, the ownership sweep, the
   capability reader. No new privilege and no new hazard.
2. **SEC-04, SEC-06 and SEC-08.** The peer table, and the two cases that read it,
   plus the recording case.
3. **SEC-05 and SEC-07.** The probe mount and its safety case, and the identity
   collision case on `locktool`.

**Expect red.** On the provisioner this project runs against, the probes in
Section 2 already say SEC-05 will fail and say why. That is the phase working.

**What actually happened:** the three went in as one change. The split was drawn
to keep the probe mount away from everything else, and the harness it needed —
the peer table, the capability reader, the ownership census — turned out to be
read-only code with unit tests of its own, so separating it bought review
isolation that the hazard did not need. The probe mount's safety case is
Section 6 either way, and it is the part to read first.

Review then rewrote three cases, all for the same reason: each was reading
something *about* the server, or about a precondition, instead of asking the
server the question it cared about. SEC-04 read the server's socket table, then
each node's own `clientaddr`, and now reads nothing — it takes two clients' locks
and removes one client. SEC-02 read what `stat` printed, and now asks whether a
`chown` is permitted. SEC-03 wrote into the world-writable export root, where the
write would have succeeded with no supplementary group at all, and now writes
behind a gated directory with a control pod that must be refused. The pattern is
worth naming for the next phase: **where a case can perform the operation whose
outcome it cares about, inspecting a precondition instead is the weaker test**,
and on this deployment it was the wrong one.

A second review round, automated, found the cases could still pass without
having tested anything, in two more places. SEC-04's lock queries came from the
holder's own node, which the Linux client answers locally. SEC-03's gate arrived
setgid, so the file inherited the gate's group and the case reported that the
volume manages ownership, which it does not. Both are the same failure as the
three above, one step further in: the case did the right operation and then
asked the wrong witness. The SEC-03 fix also overturned F-020, which had been
published from the ungated probe; the correction is recorded there rather than
here.

## 10. What the runs returned

`make test-sec FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"`,
run `20260914-011748`, GKE cluster `gke-w1`, Kubernetes v1.37.0-gke.2941000,
three `e2-standard` workers on Container-Optimized OS with kernel 6.12.94+,
StorageClass `nfs` backed by `nfs-server-provisioner` v4.0.8, profile `default`
(lease 60s, grace 90s). The whole category took 2m45s against a 30 minute budget.

This is the sixth run of the category, and the only one that describes the code
as it stands. The five before it returned the same verdicts on the same cluster,
but each exercised a case review has since replaced: SEC-04 twice, then SEC-02,
then SEC-03 and SEC-04 again. The result of a case that has been rewritten is not
a result for the case in the tree, and two of those rewrites changed what a green
verdict means rather than only how it is reached.

SEC-02 was also run once on its own with `-root-squash=off`, which is what this
export is actually configured for, to exercise the branch that asserts rather
than records. It passes, and prints both halves of F-021 in one output: root's
`chown` permitted on both nodes, and the file root wrote displaying as
`65534(nobody):65534(nobody)`. No other case takes a flag the table above does
not.

| Case | Result | What it said |
|---|---|---|
| SEC-01 | pass | ownership survives the crossing |
| SEC-02 | pass | an owner was refused a `chown` of its own file on both nodes; root was permitted it on both, so this export does not squash, whatever the ownership displays as (F-021) |
| SEC-03 | pass | no chown storm, no ownership change, 1s against 1s, and the fsGroup gid grants access through the AUTH_SYS gid list while leaving the volume alone (F-020) |
| SEC-04 | pass | the server held both nodes' locks at once, discarded only the departing node's, and the survivor kept its lock and its I/O — all read from a third node that held nothing |
| SEC-05 | **fail** | a node with no claim mounted the owner's export and read its bytes (F-018) |
| SEC-06 | skip | the cluster is single-stack IPv4 |
| SEC-07 | pass | the survivor kept its lock, the vanished pod's range came back in 2s, the replacement was granted it |
| SEC-08 | pass | `sec=sys`, no `xprtsec`, no 20049, no NetworkPolicy, and 2049 reachable from a pod with no claim |
| SEC-09 | pass | both declared capabilities held; 14 more from the runtime's defaults, recorded |

SEC-05's red is the finding the phase was written to reach, and it stays red:
the deployment cannot meet a correct assertion, which is a statement about the
deployment. F-018 has the mechanism and what would narrow it.

Three things the run taught that the design did not anticipate:

- The IPv4-mapped peer form is not hypothetical on a single-stack cluster. The
  server binds `:::2049`, so every client appears in `/proc/net/tcp6` and
  `/proc/net/tcp` alone shows no NFS connections at all (F-019). SEC-06 still
  skips, and the normalisation it was written for is visible in what SEC-08
  reads.
- The server offers the NFSv3 ancillary ports — 111, 662, 875, 20048, 32803 —
  alongside 2049. SEC-08 records them; what an NFSv3 mount of the same export
  would be granted is not asked by any case here.
- SEC-07 cannot use `ForceDeletePodAndAwaitUnmount`. The survivor legitimately
  holds the same mount on the same node, so there is no unmount to wait for; the
  case force-deletes and leaves the claim to the survivor's ordinary teardown.
  DATA-06's shape does not transfer to two pods on one node.

## 11. Open

- What bound a `fsGroup` mount should be held to when a chown storm *is*
  happening has no ratified value. The case measures against a control pod on the
  same claim rather than inventing one, and the number belongs in `pkg/slo`.
- The grace log channel in Section 2's last row may unblock OBS-03 and CHAOS-07;
  F-022 says where it is. That belongs to step 7 and step 10, not here.

## 12. Sources

- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html) Section 2.4 for client
  identity and the single lease per client, and Section 13 for AUTH_SYS.
- Kernel
  [client-identifier](https://docs.kernel.org/filesystems/nfs/client-identifier.html):
  one lease per client per server, shared by every mount and every pod on the
  node. SEC-06 and SEC-07 are written around it.
- [`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) for `sec=`,
  `xprtsec=`, `soft` and the reserved source port, which is the identity the
  server sees.
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

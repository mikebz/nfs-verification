# Conventions for this repository

This file is for anyone, human or agent, writing code here. The test plan in
[`docs/01-test-plan.md`](docs/01-test-plan.md) says what gets verified;
[`docs/plan.md`](docs/plan.md) says in what order; this file says how the code
is written.

## Every test says what it is for

A test that fails at three in the morning has to explain itself to someone who
has never read it. So every test function, unit test and end-to-end case alike,
carries a comment above it with two things:

1. **The goal.** What property is under test, and why that property matters.
   For an end-to-end case, lead with the plan's case ID.
2. **The steps.** A numbered outline of what the test does, at the level a
   reviewer can check against the plan without reading the body.

```go
// PROV-03: delete a claim a pod still mounts. The claim must stay Terminating
// until the mount is gone, and the pod must keep working while it does.
//
// Steps:
//  1. Provision an RWX claim, mount it in a pod, write a file.
//  2. Delete the claim while the pod still has it mounted.
//  3. Watch for 20s: the claim must stay, with the pvc-protection finalizer.
//  4. Read and write through the mount while the claim is Terminating.
//  5. Delete the pod and wait for it to leave the API.
//  6. The claim must then finish deleting.
func TestDeleteClaimUnderLiveMount(t *testing.T) {
```

Test functions are named for what they do, not for their plan ID. The ID lives
in the comment, where a reader who does not know the scheme still gets a
sentence of English.

## Assertions say what a failure means

A failure message is a defect report written in advance. It names the object,
the node, what was expected, what happened, and where to look next. "checksum
mismatch" costs an hour; "pod on node-2 read /mnt/share/f as X, but the writer
on node-1 wrote Y" costs a minute.

Where a failure is likely to be misrouted, the message says where it belongs.
DATA-02 carries a note routing a lost record to the boundary discussion rather
than to the server owner, because the protocol does not promise what that half
of the case asserts.

## Blocked is not failed

A case that cannot run because of cluster or export configuration reports
blocked, with `t.Skipf` and the reason, including the underlying error. A six
minute recovery caused by a Kubernetes controller default is a deployment
defect; filing it against the NFS server wastes a week.

Cases skip **by capability, never by platform name**: `if !f.Caps.CanStopNode`,
never `if platform == "gke"`. Capabilities are discovered at preflight and
recorded in `environment.json`.

## Assert only what the architecture promises

The guarantee under test is NFSv4.1, not POSIX. Close-to-open is asserted
directly; the absence of anything stronger is asserted just as deliberately. No
case asserts how failover happens: the HA mechanism is a black box, and every
failover case measures only what a client can observe.

Timing bounds are never literals in a case. They come from `pkg/slo`, against
the lease and grace profile preflight pinned.

## Discovery over declaration

Nothing readable from the cluster is passed in by hand. A flag exists only for
something a cluster genuinely cannot answer, and every one is listed in the
README with why. A default value for something unknowable is not acceptable
where it would silently invalidate an assertion.

## The harness is a Kubernetes client, not an NFS client

It never mounts the share. Every byte of I/O in every assertion comes from a
pod, over `pods/exec`, using binaries present on a stock Linux image. That is
what keeps the suite portable and keeps a workstation kernel out of the result.

## Shell in pods

Helpers that build shell run it inside a pod, so:

- Quote every value that reaches the shell, with `shellQuote`, the derived
  paths included.
- Validate any identifier that becomes a filename, with `CheckScriptID`.
  Quoting does not stop a path escape.
- Keep a helper's own state files on the pod's filesystem, never on the share.
  A helper that reads its own progress through the filesystem under test cannot
  tell a harness stall from a storage stall.
- Unit test the script by running it under a real shell. A heredoc or quoting
  slip should fail on a workstation, not inside a case that was testing
  something else.

## Objects and teardown

Everything a case creates is named `nfsv-<case>-<run>-<what>` and labelled with
the run and the case. Teardown deletes exactly that selector, **pods first,
gracefully, waited out, and only then claims**. Read F-001 in
[`docs/findings.md`](docs/findings.md) before touching teardown: force deleting
a mounted pod and then deleting its claim destroys an export under a live hard
mount, and takes the node out of service.

Cluster-scoped objects, such as a PersistentVolume a case creates itself, are
outside that selector and need their own cleanup on the fixture.

## Small, reviewable changes

One vector per change, each landing with the harness pieces it needs and
nothing more. A change that adds a helper no case calls yet does not land.

## Before pushing

`make all`: fmt, vet, unit tests, build. The unit tests need no cluster and
must stay that way.

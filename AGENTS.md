# Agent Guide for nfs-verification

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-12

This repository is a test harness, not a product. It verifies NFS RWX
persistent volumes on Kubernetes by driving a real cluster and asserting what a
client can observe.

This guide is about **how to work here**: how to approach a change, how to write
a case, what to claim when you are done, how to behave in review. It is not a
description of what gets tested, and it must not become one.

**What gets tested lives in `docs/`.** Start with the map at the top of that
folder: it names every document, the order to read them in, which document owns
which fact, and the NFS, Kubernetes and CSI sources every assertion traces to.
Read the requirements and the delivery order before you start, and read the
findings log before touching teardown, deletion, or anything that unmounts. The
repository README covers layout, flags and how to build and run. This guide
names documents by what they are, never by filename, so that renaming or
renumbering one does not leave a stale pointer here.

## Three facts that shape the work

1. **The harness can damage the cluster it tests.** The mounts are NFSv4.1 and
   `hard`, so a client that loses its export does not get an error, it retries
   forever, uninterruptibly. Two ordinary API calls in the wrong order take a
   node out of service (F-001). Wrong code here does not fail a test, it costs
   someone a node and a day of attribution.
2. **You almost certainly cannot run the thing you are changing.** Unit tests
   run anywhere. Preflight and the cases need a cluster with an RWX class, a
   reachable NFS server, and two schedulable workers. Every real bug found so
   far was found by running against a live cluster, not by reading the code.
3. **A wrong assertion is worse than no assertion.** A suite that reports a node
   reboot as a storage defect, or that measures failover against an unknown
   grace period, produces findings nobody can act on.

## Approach to a change

- **Requirements live in the test plan.** If what a case must verify changes,
  edit the test plan in `docs/`. Do not bury the new version in a PR comment or
  a commit message.
- **One vector per change.** A case lands with the harness pieces it needs and
  nothing more. A change that adds a helper no case calls yet does not land.
- **A large change is two PRs.** Anything you estimate at 1000 lines or more
  lands as a design doc first, approved before implementation code is written.
  Code written ahead of an approved design gets thrown away.
  - The doc is the next numbered file in `docs/`, and it links the cases in the
    test plan it serves so the requirement traces back to one place. Add its row
    to the map at the top of `docs/` in the same change.
  - It opens with a header block: `Author:`, `Created:`, `Updated:`, `Status:`
    (designed, shipped, or superseded, with the step and the PR), and `Serves:`
    (the case IDs and the test plan sections). A reader who gets no further than
    the header should still know whether the document describes code that exists.
    Every document in `docs/`, plus this file and the README, carries `Author:`,
    `Created:` and `Updated:` in UTC. `Created:` never changes once the file has
    landed. **Move `Updated:` in the same change that alters what the document
    says**, so a doc nobody has reconciled with the code says so on its first
    line; a typo fix does not move it. The map in `docs/` has the two git
    commands that check both against history.
  - Keep it short enough to be read in one sitting. Decisions and the reasons
    for them, the rules a reviewer can accept or reject the phase from, the data
    contract, and what was deferred. Not a restatement of the test plan's case
    table, and not a narrative of every draft.
  - When a later phase overturns a decision, record the supersession in the
    later document and in the earlier one's "what changed after this was
    written" section. The decision itself stays where it was made.
  - The design PR contains only the doc. **No code inside the doc either.**
    State object shapes, exec flows and interfaces in prose and tables, not Go
    or YAML: code in a design doc goes stale the moment implementation starts,
    and reviewers end up reviewing a sketch instead of the decision.
  - Mark what is unresolved as open rather than inventing a constraint to fill
    the gap.
  - If the estimate was wrong and the change grows past that mark mid-flight,
    stop and write the doc rather than finishing.
- **Run things through `make`.** The targets are the interface: they carry the
  flags, the timeouts and the run ID, and they are what a reviewer will run
  when they check your claim. If you need something `make` does not do, add the
  target instead of running a one-off command, so that what you ran is what
  anyone else can run. The repository README lists the targets.
- **Settle the open question before implementing, not in the PR.** Where the
  plan marks something unresolved, resolve it first. Implementing against a
  guess and explaining the guess in the PR description is the expensive order.
- **Prefer the change that fails loudly.** Given a choice between a default that
  lets a run proceed and a hard stop that names what is missing, take the hard
  stop. Lease and grace fail preflight rather than defaulting, because a wrong
  base makes every timing assertion silently meaningless.

## The test is the test

A red result is not a reason to change what the suite asserts. When a run comes
back red, the question is whether the assertion was right, never whether it can
be relaxed until this deployment passes. A bound loosened, a check deleted or a
verdict downgraded because one provisioner cannot meet it produces a suite that
passes everywhere and says nothing anywhere, and the next person to run it has
no way to tell which of its greens were earned.

So the order is: establish what the correct assertion is, from the documents
below; assert that; and report what this deployment does about it. A deployment
that cannot meet a correct assertion is a finding about that deployment. It goes
in the findings log and stays red, or reports blocked where the rules for the
case say blocked, and either way the message says whose configuration produced
it. What it does not do is change the assertion.

**State the assumption, and say where it came from.** Every assertion rests on
something somebody wrote down, and the citation is what lets a reviewer disagree
with the assertion rather than with you:

The specific documents, with links, are listed in the map in `docs/` under
*Sources the assertions rest on*. In order of authority:

- **NFSv4.1 semantics**: RFC 8881, and RFC 7530 where v4.0 behaviour is being
  contrasted. Close-to-open, post-COMMIT durability, lock reclaim during grace
  and client identity all come from here.
- **The Linux NFS client**: `Documentation/filesystems/nfs/` in the kernel tree,
  for anything about mount options, `local_lock`, the shared lease per server,
  or what a `hard` mount does on retry. Client behaviour is not protocol
  behaviour and the difference belongs in the comment.
- **Kubernetes**: the API reference and the component docs, for pod conditions,
  endpoint readiness, finalizers, kubelet timers and aggregation periods.
- **CSI**: the specification, for what a driver must implement against what it
  may implement. Volume statistics, expansion and snapshots are all optional
  capabilities, and a driver that omits one is not defective.
- **The implementation in front of us**: the provisioner's flags, its chart
  templates, its source. The least authoritative source, but enough for a
  statement about this deployment as long as it is labelled as one.

Where no document settles it, say that in the same breath as the assertion:
"this is this implementation's behaviour, not a protocol guarantee" is a
legitimate basis as long as the case says so and the failure message repeats it,
so that the failure reaches the boundary discussion instead of being filed as a
server defect. An assumption nobody can trace is the one that gets quietly
relaxed the first time it fails.

## Say what you actually ran

State plainly, per change, which of these happened:

- `make all` passed. Say so; it is the minimum, not a result.
- The cases were not run, because no cluster was available. Say this outright
  rather than leaving it to be inferred from silence.
- The cases were run: name the target, the cluster, the Kubernetes version, the
  node shape, the storage class, and the profile in force. Report the flags you
  passed, since a run with different flags is a different run.

Never describe an unrun case as passing, working, or verified. "`make all` is
clean, the cases have not been run" is the honest claim when that is what
happened. A
reviewer who runs the suite on real hardware and finds it broken after being
told it works stops trusting every later claim, including the true ones.

## Writing a case

- One test function per plan case, standard `testing`, no Ginkgo. Table-driven
  where the case has a table.
- The function name says what the case does. The **plan ID goes in the comment
  above it**, along with what the case asserts and, where it matters, what it
  deliberately does not.
- **Every case carries an outline**: the goal, then numbered steps, at the level
  a reviewer can check against the plan without reading the body. A case that
  fails at three in the morning has to explain itself to someone who has never
  read it.

  ```go
  // PROV-03: delete a claim a pod still mounts. The claim must stay Terminating
  // until the mount is gone, and the pod must keep working while it does.
  //
  // Steps:
  //  1. Provision an RWX claim, mount it in a pod, write a file.
  //  2. Delete the claim while the pod still has it mounted.
  //  3. Watch for the SLO-bound interval: the claim must stay, with the pvc-protection finalizer.
  //  4. Read and write through the mount while the claim is Terminating.
  //  5. Delete the pod and wait for it to leave the API.
  //  6. The claim must then finish deleting.
  ```
- **Skip by capability, never by platform name.** `requireCap(t, f.Caps.MultiNode,
  ...)`, never `if platform == "gke"`. Every case that schedules on two nodes,
  reads a node, stops a node, snapshots or expands needs its guard. A missing
  guard turns a single-worker cluster into a fatal failure instead of a skip.
- **A case blocked by cluster configuration reports blocked, not failed.** A six
  minute recovery caused by a Kubernetes controller default is a deployment
  defect. Filing it against the NFS server wastes a week.
- **No timing literals.** Bounds come from `pkg/slo` against the pinned profile.
  A literal in a case invalidates itself the moment the profile changes, and
  nothing tells you.
- **Assert what NFSv4.1 guarantees, no more**, and name the document it comes
  from, per *The test is the test* above. Close-to-open is asserted
  directly. The absence of anything stronger is asserted just as deliberately.
  Where a case must assert an implementation property rather than a protocol
  guarantee, the failure message says so, so the failure reaches the boundary
  discussion instead of being filed as a server defect.
- **I/O happens in pods, never on the workstation.** The harness is a Kubernetes
  client, not an NFS client. Drive `dd`, `sha256sum`, `flock`, `stat` and `df`
  over `pods/exec`. A workstation kernel must never appear in a result.
- **Assume nothing about the tools image beyond busybox.** Anything else comes
  from a flag, and the cases that need it skip without it.
- **Every case is bounded.** A hung case that never fails teaches nothing.
- **Failure messages name the node, the pod and the profile.** "checksum
  mismatch" is not triageable. "reader on node B did not see what the writer on
  node A closed" is.
- **The artifact bundle is part of the case.** A new fault or a new node
  interaction adds itself to the bundle in the same change, and the bundle is
  written before teardown. Deleted pods tell no stories, and a failure filed
  without the bundle is closed as unreproducible.

## Not breaking the cluster

The rules below are the ones the harness has already violated once. The findings
log has the mechanism; this is what it means for code you write:

- Teardown deletes pods gracefully and waits for them to leave the API before
  touching a claim.
- Force deletion belongs only to cases that model a client that vanished. It is
  never teardown.
- Never delete a claim a surviving pod still mounts. A leaked claim is
  recoverable by hand; a wedged node is not.
- A pod that does not leave the API in time means the node stopped answering.
  Treat that as the dangerous state it is, not as a slow unmount to wait out.
- Anything that reads a node can block forever, so bound each node separately.
  One sick node must not starve collection for the healthy ones.
- Anything the suite creates must be identifiable as the suite's from outside,
  because server discovery has to exclude it. A heuristic that matches the
  harness's own pods points the chaos cases at the harness.

## Writing unit tests

Unit tests here exist for logic that fails silently or fails expensively: the
SLO table, the server discovery heuristic, manifest rendering, path resolution,
the claim-retention rule in teardown.

- Keep them hermetic. A test under `pkg/` that needs a cluster is in the wrong
  place; it belongs with the cases, behind a capability check.
- Short names in `TestSubjectBehavior` form. Do not encode the scenario in the
  name; the doc comment carries the detail.
- Say why the test exists when that is not obvious. "Getting this wrong means
  the chaos cases kill the harness instead of the server" is worth more than a
  restatement of the assertions.
- Unit tests carry the same outline as a case: the goal, then numbered steps.
  The goal is the half that ages well; a reader who knows why the test exists
  can tell a real failure from a stale assertion.
- **Prefer the real tool over a fixture.** A golden string only proves the
  parser matches what someone typed into it. Where the tool a case drives in a
  pod also exists on a workstation, run it and parse what it actually prints;
  keep fixtures for the shapes a healthy system will not produce, such as error
  text or a `stat` built without `-f` echoing its format string back.
- **Run generated shell under a real shell.** A heredoc or quoting slip should
  fail on a workstation, not inside a case that was testing something else.
- Render every manifest in a unit test and assert on the decoded object, so an
  indentation slip fails on a workstation rather than against a cluster.
- **Test the failure that has no symptom.** The preflight cache path bug
  produced no error, only a silent reprobe on every run. That class of bug is
  found by a unit test or not at all.

## Keep it simple

Recurring review feedback, all of it from this repository:

- **Build only what is needed now.** No gates, modes, registries or abstraction
  layers ahead of a case that uses them. Test categories are `go test -run` and
  a comment until there is more than one category in the repository.
- **No wrappers around the standard library.** No bespoke logging layer, no
  helper that reimplements what `testing` already provides.
- **Fixed values stay fixed until something needs them to vary.** The namespace
  is `default`. It does not become a flag until a case needs a second one.
- **Every flag earns its place.** Discovery over declaration: if the cluster can
  answer it, discover it. A flag exists only for what a cluster genuinely cannot
  answer, and the README table says why for each one. Adding a flag means adding
  its row.
- **Wire every config value.** A flag that is parsed and never read is a bug.

## Comments and docs

- Every exported function gets a doc comment: what it is for, plus anything
  unobvious. Comments explain why.
- **A comment that contradicts the code is a bug, and it will be caught.** A
  package comment once said the harness creates namespaces while the README and
  the code both said it creates none. When you change behavior, grep for the
  prose that describes it.
- Keep the README, the flag table and `docs/` in sync with the code. A README row
  for a flag the code ignores is a bug.
- Where a comment enforces a plan rule, say which rule, so the next reader knows
  it is not arbitrary.

## Record what a real run teaches

The findings log in `docs/` is the memory of this project. When a run against a
real cluster teaches something worth keeping, add an `F-NNN` entry at the top saying
what happened, why, what changed in the code, and what it implies for the system
under test as opposed to the harness. A finding that lives only in a PR comment
is lost by the next PR.

Not every bug is a finding. A finding is something a future run would otherwise
have to rediscover: a hazard in the architecture, a cluster precondition nobody
would guess, a failure mode that mimics the defects the plan is hunting.

## Go style

`gofmt` and `go vet` are the authority on mechanical style.
[Effective Go](https://go.dev/doc/effective_go) and
[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) cover the rest.
The rules below are the ones tooling cannot check:

- Wrap errors with `fmt.Errorf("context: %w", err)`. Check with `errors.Is` and
  `errors.As`, never by matching text. Error strings are lowercase, no trailing
  punctuation.
- `context.Context` is the first parameter, never stored in a struct.
- Do not panic for normal errors. Do not discard errors with `_`.
- Timeouts and intervals are named constants, not literals at call sites.
- Define interfaces in the consuming package. Return concrete types from
  constructors. Do not create an interface before something uses it.
- Avoid package-level globals. The suite-wide client, environment and
  capabilities set in `TestMain` are the deliberate exception.
- Shell that runs in a pod is a file under `pkg/framework/scripts`, shipped by
  `framework.RunScript` and given its values as positional arguments. A script
  in a Go format string is unreadable in both languages at once, and cannot be
  run by hand. Nothing is interpolated into a script body: the file on disk is
  the file that runs.
- Shell strings built for exec go through `framework.Quote`. Never concatenate a
  path into a script unquoted, the paths a helper derives for itself included.
  Validate any identifier that becomes a filename with `framework.CheckScriptID`:
  quoting does not stop a path escape, and a helper that is safe only because of
  who calls it today is one refactor away from not being safe at all.

## PRs and review

- Keep PRs small and single-purpose. Never mix a rename or a move with a
  behavior change. A change large enough to need a design doc is two PRs, not
  one large one; see *Approach to a change*.
- Commit only when `make all` is clean.
- Address every review comment with a real change or a reasoned reply. Where you
  deliberately do not take a suggestion, say which part you took, which you did
  not, and why. The second half of a suggestion is sometimes the dangerous half.
- Real-cluster review findings are the most valuable input this repository gets.
  When one lands, fix it, add the unit test that would have caught it, and
  record the finding if it teaches something about the system under test.

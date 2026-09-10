# Agent Guide for nfs-verification

This repository is a test harness, not a product. It verifies NFS RWX
persistent volumes on Kubernetes by driving a real cluster and asserting what a
client can observe.

This guide covers **how to work in this repo**: process, tests, conventions,
style. It does not describe the test plan or the harness design, and it must
not start to: those have one home each, and a copy here would drift. Before
changing code, read:

- `docs/01-test-plan.md`: what gets verified and why. Sections, case IDs, the
  SLO table, the preconditions in Section 0. This is the requirements document.
- `docs/plan.md`: how the plan gets built, in what order, and what each step is
  allowed to assume. Section 2 is the delivery order; one step is one PR.
- `docs/findings.md`: what running the suite against a real cluster taught us.
  **Read this before touching teardown, deletion, or anything that unmounts.**
- `README.md`: layout, flags, how to run.

## What makes this repo different

Three things drive most of the rules below:

1. **The harness can damage the cluster it tests.** The mounts are NFSv4.1 and
   `hard`, so a client that loses its export does not get an error, it retries
   forever, uninterruptibly. Two ordinary API calls in the wrong order take a
   node out of service (F-001). Wrong code here does not fail a test, it costs
   someone a node and a day of attribution.
2. **You almost certainly cannot run the thing you are changing.** Unit tests
   run anywhere. Preflight and `test/e2e` need a cluster with an RWX class, a
   reachable NFS server, and two schedulable workers. Every real bug found so
   far, without exception, was found by running against a live cluster, not by
   reading the code.
3. **A wrong assertion is worse than no assertion.** A suite that reports a
   node reboot as a storage defect, or that measures failover against an
   unknown grace period, produces findings nobody can act on.

## Development process

1. **Requirements live in the test plan.** `docs/01-test-plan.md` is the single
   home for what a case must verify. If a requirement changes, edit the plan,
   do not bury the new version in a PR comment or a commit message.
2. **Work the delivery order in `docs/plan.md`.** One step is one PR. One
   vector per change, landing with the harness pieces it needs and nothing
   more. A change that adds a helper no case calls yet does not land.
3. **Clarify before implementing.** Where the plan marks something open
   (Section 4, "Known gaps"), resolve it in the issue or the plan first. Do not
   implement against a guess and explain it in the PR.
4. **New cases carry their plan ID.** The ID goes in the comment above the test
   function, not in the function name. The function name says what it does.
5. **Findings get recorded.** Anything a real cluster teaches that is worth
   remembering goes into `docs/findings.md` as a new `F-NNN` entry at the top,
   with what happened, why, what changed in the code, and what it implies for
   the system under test as opposed to the harness. A finding that only lives
   in a PR comment is lost.

## Build and test

Use the Makefile. Do not hand-roll commands or add tooling:

- `make all`: fmt, vet, unit, build. Run before every commit. Everything must
  pass.
- `make unit`: `go test ./pkg/...`. No cluster, no network, no kubeconfig.
- `make preflight FLAGS="..."`: Section 0 checks, needs a cluster.
- `make test-e2e FLAGS="..."`: the cases, needs a cluster.
- `make clean`: removes `artifacts/`.

Unit tests must stay hermetic. A test in `pkg/...` that needs a cluster is in
the wrong package: it belongs in `test/e2e` behind a capability check.

## Report what you actually ran

State plainly which of these happened, per change:

- `make all` passed (say so, it is the minimum).
- The e2e cases were not run, because no cluster was available. Say this
  explicitly rather than leaving it to be inferred.
- The e2e cases were run: name the cluster, the Kubernetes version, the node
  shape, the storage class, and the profile in force.

Never describe an unrun case as passing, working, or verified. "Compiles and
`go vet` is clean" is the honest claim when that is what happened. A reviewer
who runs the suite on real hardware and finds it broken after being told it
works stops trusting every later claim.

## Do not damage the cluster

These come from F-001, which is in this repository because the harness did it:

- **Teardown deletes pods gracefully and waits for them to leave the API before
  touching a claim.** Test pods carry a 5 second grace period, so the wait costs
  seconds. Use `Framework.DeletePod`.
- **`DeletePodNow` (grace period 0) is for cases that model a client that
  vanished** (DATA-06, SEC-07). It is never teardown. Force deletion removes the
  pod from the API before kubelet unmounts, which is the whole bug.
- **Never delete a claim a surviving pod still mounts.** Deleting the export out
  from under a live hard mount is not a recoverable client error, it is an
  indefinite stall on that node. Teardown deletes only the claims no surviving
  pod mounts, keeps the rest, and fails with the pods, their nodes and the kept
  claims named. A leaked claim is recoverable by hand. A wedged node is not.
- **A pod that does not leave the API inside `PodTerminateTimeout` means the
  node stopped answering.** Treat that as the dangerous state it is, not as a
  slow unmount to wait out.
- **Anything that reads a node can block forever.** `cat /proc/mounts` hangs on
  a node with a wedged mount. Every node inspection gets its own bounded
  context (`nodeInspectTimeout`), so one sick node cannot starve collection for
  the healthy ones.

## Timeouts and budgets

Timeout bugs here do not look like timeout bugs, they look like a test runner
SIGQUIT and a pile of leaked PVCs.

- Timeouts are named constants in `pkg/framework/wait.go`. No literals at call
  sites.
- Every wait is bounded, and the bound reflects what is being waited on. The
  90s `PodTerminateTimeout` is not a rounder `DeleteTimeout`, it is the point
  past which the node is not answering.
- Cleanup budgets are nested, not shared. Artifact collection runs inside its
  own `ArtifactTimeout` so a failed collection still leaves time to clean up.
- Per-case budgets: every case runs under `caseCtx`. A hung case that never
  fails teaches nothing.
- Sum the worst case before adding a wait. A 5 minute cleanup on every case
  eats the `go test -timeout` budget for the whole package.

## Writing cases

- One test function per plan case, standard `testing`, no Ginkgo. Table-driven
  where the case has a table.
- The comment above the function opens with the plan ID and states what the
  case asserts and, where it matters, what it deliberately does not assert.
- **Skip by capability, never by platform name.** `requireCap(t, f.Caps.MultiNode,
  ...)`, never `if platform == "gke"`. Every case that schedules on two nodes,
  reads a node, stops a node, snapshots, or expands needs its guard. A missing
  guard turns a single-worker cluster into a fatal failure instead of a skip,
  which is how DATA-03 shipped in PR #1.
- **A case blocked by cluster configuration reports blocked, not failed.** A six
  minute recovery caused by a Kubernetes controller default is a deployment
  defect. Filing it against the NFS server wastes a week.
- **No timing literals.** Bounds come from `pkg/slo` against the pinned profile.
  A literal in a case silently invalidates itself the moment the profile
  changes.
- **Assert what NFSv4.1 guarantees, no more.** Close-to-open is asserted
  directly (DATA-03). The absence of anything stronger is asserted just as
  deliberately (DATA-04). Where a case does assert an implementation property
  rather than a protocol guarantee (DATA-02 and append), the failure message
  says so, so the failure is routed to the boundary discussion instead of filed
  as a server defect.
- **I/O happens in pods, never on the workstation.** The harness is a
  Kubernetes client, not an NFS client. Drive `dd`, `sha256sum`, `flock`,
  `stat`, `df` over `pods/exec`. A workstation kernel must never be in a
  result.
- Assume nothing about the tools image beyond busybox. `fio` and `locktool`
  arrive with the cases that need them, from a flag, and those cases skip
  without it.

## Writing unit tests

Unit tests here exist for the logic that fails silently or fails expensively:
the SLO table, the server discovery heuristic, manifest rendering, path
resolution, the claim-retention rule in teardown.

- Short names in `TestSubjectBehavior` form. Do not encode the scenario in the
  name.
- The doc comment carries the detail, and it says why the test exists when that
  is not obvious: "getting this wrong means the chaos cases kill the harness
  instead of the server" is worth more than a restatement of the assertions.
- Render every manifest in a unit test and assert on the decoded object, so an
  indentation slip fails on a workstation rather than against a cluster.
- Test the failure that has no symptom. The preflight cache path bug produced
  no error, only a silent 4 minute reprobe on every run, so both the relative
  and absolute path cases are covered.

## Keep it simple

Recurring review feedback on PR #1, all of it:

- **Build only what is needed now.** No gates, modes, registries or abstraction
  layers ahead of a case that uses them. Test categories are `go test -run` and
  a comment until there is more than one category in the repository.
- **No wrappers around the standard library.** No bespoke logging layer, no
  helper that reimplements what `testing` already gives you.
- **Fixed values stay fixed until something needs them to vary.** The namespace
  is `default`. It does not become a flag until a case needs a second one.
- **Every flag earns its place.** Discovery over declaration: if the cluster can
  answer it, discover it. A flag exists only for what a cluster genuinely cannot
  answer, and the README table says why for each one. Adding a flag means adding
  its row.
- **A default value that invalidates a measurement is not acceptable.** Lease
  and grace fail preflight rather than defaulting, because a wrong base makes
  every timing assertion silently meaningless.
- **Wire every config value.** A flag that is parsed and never read is a bug.

## Comments and docs

- Every exported function gets a doc comment: what it is for, plus anything
  unobvious. Comments explain why.
- **A comment that contradicts the code is a bug, and it will be caught.** The
  package comment in `config.go` said the harness creates namespaces while the
  README, the plan and the code all said it creates none. When you change
  behavior, grep for the prose that describes it.
- Keep README, `docs/` and the flag table in sync with the code. A README row
  for a flag the code ignores is a bug.
- Where a comment enforces a plan rule, say which rule ("the plan forbids a
  timing literal in a case"), so the next reader knows it is not arbitrary.

## Object naming and cluster hygiene

- The suite creates no namespaces. Everything lands in `default`, kept apart by
  name and label.
- Objects are named `nfsv-<case>-<run>-<what>` and labelled with the run and the
  case. **Keep the full run ID in the name.** Truncating it (splitting on `-`
  from a `YYYYMMDD-HHMMSS` run ID drops the date) collides across runs and turns
  triage into guesswork.
- Teardown deletes exactly the label selector, pods first, then claims.
- Anything the suite creates must be identifiable as the suite's from outside,
  because server discovery has to exclude it. A heuristic that matches the
  harness's own pods points the chaos cases at the harness.

## Paths and working directory

`go run ./cmd/preflight` starts at the repository root, `go test ./test/e2e`
starts in the package directory. A relative path therefore names two different
directories, which is how the preflight cache silently never hit.

- Relative paths from flags are anchored to the module root by walking up for
  `go.mod`. Absolute paths are left alone.
- Any new path flag follows the same rule and gets the same two unit tests.

## Manifests

- Pods and the node agent DaemonSet are embedded YAML under
  `pkg/framework/manifests`, rendered and decoded into typed objects. A manifest
  reads like something a person would apply and can be diffed against what was
  applied.
- Do not build pod specs field by field in Go where a manifest exists.
- The node agent is privileged and runs with host namespaces. It sets resource
  requests (10m CPU, 32Mi) so it is not BestEffort and not the first thing
  evicted, and no limits, because it must not be OOM killed while a case is
  reading the node it is inspecting.

## Artifacts

- A failed case writes `artifacts/<run-id>/<CASE-ID>/`: `environment.json`,
  client and server pod logs including previous-container logs, Kubernetes
  Events, `/proc/mounts` and dmesg from every involved node, and the
  injected-fault timeline.
- The bundle is written **before** teardown. Deleted pods tell no stories.
- A new fault or a new node interaction adds itself to the bundle in the same
  change. A failure filed without the bundle is closed as unreproducible.

## Go style

`gofmt` and `go vet` run in `make all` and are the authority on mechanical
style. [Effective Go](https://go.dev/doc/effective_go) and
[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) cover the
rest. The rules below are the ones tooling cannot check:

- Wrap errors with `fmt.Errorf("context: %w", err)`. Check with `errors.Is` and
  `errors.As`, never by matching text. Error strings are lowercase, no trailing
  punctuation.
- `context.Context` is the first parameter, never stored in a struct.
- Do not panic for normal errors. Do not discard errors with `_`. In a case,
  `t.Fatalf` with the operation and the node in the message.
- Failure messages name the node, the pod, and the profile. "checksum mismatch"
  is not triageable. "reader on node B did not see what the writer on node A
  closed" is.
- Define interfaces in the consuming package. Return concrete types from
  constructors. Do not create an interface before something uses it.
- Avoid package-level globals. The suite-wide client, environment and
  capabilities set in `TestMain` are the deliberate exception.
- Shell strings built for `exec` go through `framework.Quote`. Never
  concatenate a path into a script unquoted.

## PRs and review

- Keep PRs small and single-purpose, one delivery step from `docs/plan.md`.
  Never mix a rename or a move with a behavior change.
- Commit only when `make all` passes.
- Address every review comment with a real change or a reasoned reply. Where you
  deliberately do not take a suggestion, say which part you took, which you did
  not, and why. The second half of a suggestion is sometimes the dangerous half.
- Real-cluster review findings are the most valuable input this repository gets.
  When one lands, fix it, add the unit test that would have caught it, and
  record the finding if it teaches something about the system under test.

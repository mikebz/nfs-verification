package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/mikebz/nfs-verification/pkg/chaos"
	"github.com/mikebz/nfs-verification/pkg/framework"
	"github.com/mikebz/nfs-verification/pkg/slo"
)

// The chaos cases are named TestChaos... so that the fast gate can exclude them
// by name. They are slow, their failures need human triage, and red in the fast
// path trains people to ignore red.
//
// Every assertion here is client-observable. Nothing asserts how failover
// happens: the HA mechanism is a black box by design, and the server is a
// singleton in the data path, so what a client can see is the whole of what the
// architecture promises. Timing comes from pkg/slo against the profile
// preflight pinned, never from a literal.

// chaosSetup is what both cases need before anything is injured: a live server
// to injure, a workload writing to the share, and the recovery budget for the
// event about to be caused.
type chaosSetup struct {
	f        *framework.Framework
	target   chaos.Target
	load     *framework.WriteLoad
	dir      string
	writer   string
	verifier string
	budget   time.Duration
}

// startChaosCase builds that setup, and skips rather than fails when the
// cluster gives it nothing to injure. A cluster whose server pods were never
// discovered is a configuration gap, not a storage defect.
func startChaosCase(ctx context.Context, t *testing.T, f *framework.Framework, id string) chaosSetup {
	t.Helper()
	requireCap(t, f.Caps.MultiNode, "verifying committed data from a second client needs two schedulable workers")
	target, err := chaos.ServerTarget(ctx, f)
	if err != nil {
		t.Skipf("blocked: %v", err)
	}
	t.Logf("target is server pod %s/%s on %s (%s)", target.Namespace, target.Pod, target.Node, describeController(target))

	budget, err := slo.Recovery(profile(t), slo.EventServerRestart)
	if err != nil {
		t.Fatalf("%v", err)
	}

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, id)
	f.MustPod(ctx, toolsPod("writer", pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod("verifier", pvc.Name, nodeB))

	dir := fileIn(id)
	load, err := f.StartWriteLoad(ctx, "writer", dir, id)
	if err != nil {
		t.Fatalf("starting the workload on %s: %v", nodeA, err)
	}
	f.Defer(func(ctx context.Context) { _, _ = load.Stop(ctx) })
	// A few records before the fault, so that the case is measuring an
	// interruption to something rather than a cold start.
	if err := framework.Poll(ctx, framework.PollInterval, 2*time.Minute, func(ctx context.Context) (bool, error) {
		rep, err := load.Report(ctx)
		if err != nil {
			return false, err
		}
		return len(rep.Committed()) >= 3, nil
	}); err != nil {
		t.Fatalf("the workload never got going before the fault: %v", err)
	}
	return chaosSetup{f: f, target: target, load: load, dir: dir,
		writer: "writer", verifier: "verifier", budget: budget}
}

func describeController(t chaos.Target) string {
	if t.Controller == "" {
		return "owned by no controller"
	}
	return "owned by " + t.Controller
}

// assertRecovered is the shared assertion for a failover: I/O blocked rather
// than errored, it resumed inside the SLO, and every write the server had
// already acknowledged is still there when read from another client.
func assertRecovered(ctx context.Context, t *testing.T, s chaosSetup, faultAt time.Time) {
	t.Helper()
	before, err := s.load.Report(ctx)
	if err != nil {
		t.Fatalf("reading the workload log: %v", err)
	}
	committed := before.CommittedBefore(faultAt)

	// Waited out well past the budget on purpose: a case that gives up at the
	// SLO reports "timed out" where it could report how long recovery actually
	// took, and the second is what a defect report needs.
	waitFor := s.budget + 5*time.Minute
	var resumed framework.LoadRecord
	err = framework.Poll(ctx, framework.PollInterval, waitFor, func(ctx context.Context) (bool, error) {
		rep, err := s.load.Report(ctx)
		if err != nil {
			return false, err
		}
		rec, ok := rep.FirstSuccessAfter(faultAt)
		if ok {
			resumed = rec
		}
		return ok, nil
	})
	if err != nil {
		t.Fatalf("no write committed in the %s after the fault: the client never recovered. %v", waitFor, err)
	}
	recovery := resumed.At.Sub(faultAt)
	if recovery < 0 {
		recovery = 0
	}

	final, err := s.load.Stop(ctx)
	if err != nil {
		t.Fatalf("stopping the workload: %v", err)
	}
	t.Logf("first committed write %s after the fault (budget %s, profile %s); longest gap between attempts was %s",
		recovery.Round(time.Second), s.budget, profile(t).Name, final.LongestGap().Round(time.Second))

	if recovery > s.budget {
		t.Errorf("time to first successful I/O was %s, above the %s budget for a server restart on the %s profile",
			recovery.Round(time.Second), s.budget, profile(t).Name)
	}
	// Hard mounts block, they do not fail. An error here is a protocol
	// violation, not a slow recovery, so it is asserted separately.
	if errs := final.Errors(); len(errs) > slo.MaxIOErrors {
		t.Errorf("the workload saw %d I/O errors across the failover, want %d: a hard NFSv4.1 mount is "+
			"specified to block and retry, not to return an error (first at index %d)",
			len(errs), slo.MaxIOErrors, errs[0].Index)
	}
	if len(final.Unparsed) > 0 {
		t.Errorf("the workload log holds %d lines the harness could not read, so the error count above "+
			"cannot be trusted: %q", len(final.Unparsed), final.Unparsed[0])
	}

	// Durability, read from the other node so the check crosses the server
	// rather than the writer's own page cache.
	missing, err := s.f.MissingRecords(ctx, s.verifier, s.dir, committed)
	if err != nil {
		t.Fatalf("checking committed records from the second client: %v", err)
	}
	if len(missing) > slo.MaxCommittedWritesLost {
		t.Errorf("%d of %d writes the server had already committed before the fault are gone: %v. "+
			"Post-COMMIT durability is a protocol guarantee, so this is data loss, not a slow recovery",
			len(missing), len(committed), missing)
	} else {
		t.Logf("all %d writes committed before the fault survived it", len(committed))
	}
}

// CHAOS-01: SIGKILL the server process during an active write. The server is
// the singleton in the data path, so this is the heaviest-weighted fault in the
// plan: the client must block rather than error, resume inside the restart SLO,
// and lose nothing the server had already committed.
//
// Steps:
//  1. Find a server pod to injure, start a workload on a client pod, and let
//     it commit a few writes.
//  2. Record the server's restart count and read the writer's clock.
//  3. SIGKILL the server process on its node, through the node agent. Report
//     blocked if the fault cannot be injected, rather than measuring a
//     recovery from a fault that never happened.
//  4. Assert recovery, zero I/O errors and no lost committed writes.
//  5. Confirm the server container actually restarted, since a recovery
//     measured after killing the wrong process means nothing.
func TestChaosServerProcessKill(t *testing.T) {
	f := framework.New(t, "CHAOS-01")
	requireCap(t, f.Caps.NodeAgent, "signalling a process on a node needs the privileged node agent")
	ctx, cancel := caseCtx(t, 45*time.Minute)
	defer cancel()

	s := startChaosCase(ctx, t, f, "chaos01")
	restartsBefore, err := framework.ServerRestartCount(ctx, f.C)
	if err != nil {
		t.Fatalf("reading server restart counts: %v", err)
	}

	// The fault reference comes from the writer's own clock, because the write
	// that ends the outage is timestamped by that same clock. Reading it just
	// before the kill biases the measurement upwards by the length of one exec,
	// which is the safe direction.
	faultAt, err := f.PodNow(ctx, s.writer)
	if err != nil {
		t.Fatalf("reading the writer's clock: %v", err)
	}
	killed, err := chaos.KillServerProcess(ctx, f, s.target, "KILL")
	if err != nil {
		// Every path out of KillServerProcess means the fault was not
		// injected. Measuring a recovery from a fault that never happened
		// would pass for the wrong reason.
		t.Skipf("blocked: %v", err)
	}
	t.Logf("SIGKILLed %d process(es) in %s on %s", killed, s.target.Pod, s.target.Node)

	assertRecovered(ctx, t, s, faultAt)

	// A confirmation, not the assertion: the container should have been
	// restarted by the kill. If it was not, the process that died was not the
	// one serving NFS, and the recovery measured above means nothing.
	restartsAfter, err := framework.ServerRestartCount(ctx, f.C)
	if err != nil {
		t.Fatalf("re-reading server restart counts: %v", err)
	}
	if restartsAfter <= restartsBefore {
		t.Errorf("server container restart count is still %d after SIGKILL, so the process that was killed "+
			"was not the one serving NFS; pass -server-process to name it", restartsAfter)
	}
}

// CHAOS-02: delete the server pod during an active write. Everything CHAOS-01
// asserts, plus the locks: a client holding a byte-range lock across the
// failover must still hold it afterwards, and a client on another node must
// still be refused. Whether the state survived is measured here, never inferred
// from the recovery backend preflight recorded.
//
// Steps:
//  1. Find a server pod to injure, start a workload, and let it commit a few
//     writes. Report blocked if no controller owns the pod, since it would not
//     come back.
//  2. Take a lock in the writer and confirm the second client is refused, so
//     that grace has outstanding state to reclaim and the case cannot pass
//     vacuously.
//  3. Read the writer's clock and delete the server pod.
//  4. Assert recovery, zero I/O errors and no lost committed writes.
//  5. Assert the holder still holds the lock and the second client is still
//     refused. A lock granted to two clients at once is what this case exists
//     to catch.
func TestChaosServerPodDelete(t *testing.T) {
	f := framework.New(t, "CHAOS-02")
	ctx, cancel := caseCtx(t, 45*time.Minute)
	defer cancel()

	s := startChaosCase(ctx, t, f, "chaos02")
	if s.target.Controller == "" {
		t.Skipf("blocked: server pod %s has no controller, so deleting it would not bring it back", s.target.Pod)
	}

	// The lock is taken before the fault and never released, so grace has
	// something real to reclaim. Without an outstanding lock the case would
	// pass vacuously.
	lockPath := s.dir + "/chaos02.lock"
	holder, err := f.HoldFlock(ctx, s.writer, lockPath, "chaos02")
	if err != nil {
		t.Fatalf("taking a lock before the fault: %v", err)
	}
	granted, out, err := f.TryFlock(ctx, s.verifier, lockPath)
	if err != nil {
		t.Fatalf("probing the lock before the fault: %v", err)
	}
	if granted {
		t.Fatalf("mutual exclusion was already broken before any fault was injected (%s)", out)
	}

	faultAt, err := f.PodNow(ctx, s.writer)
	if err != nil {
		t.Fatalf("reading the writer's clock: %v", err)
	}
	if err := chaos.DeleteServerPod(ctx, f, s.target); err != nil {
		t.Skipf("blocked: %v", err)
	}

	assertRecovered(ctx, t, s, faultAt)

	if err := chaos.WaitServerBack(ctx, f, framework.PodReadyTimeout); err != nil {
		t.Errorf("no server pod is ready again after the delete, even though I/O resumed: %v", err)
	}

	// The lock, from both ends. The holder still believing it holds the lock
	// proves nothing on its own; the assertion that matters is that a client on
	// another node is still refused, because a lock granted to two clients at
	// once is the failure this case exists to catch.
	state, err := holder.State(ctx)
	if err != nil {
		t.Fatalf("reading the lock holder's state: %v", err)
	}
	if state != "held" {
		t.Errorf("the lock holder in %s reports %q after the failover, so it lost the lock it never released",
			s.writer, state)
	}
	granted, out, err = f.TryFlock(ctx, s.verifier, lockPath)
	if err != nil {
		t.Fatalf("probing the lock after the failover: %v", err)
	}
	if granted {
		t.Errorf("after the failover a client on another node was granted a lock the original holder never "+
			"released (%s). Either the lock was not reclaimed or a conflicting one was granted; "+
			"the recovery state backend recorded at preflight is %q, which is where triage starts",
			out, f.Env.RecoveryStateBackend)
	} else {
		t.Logf("the lock survived the failover: still held in %s, still refused in %s", s.writer, s.verifier)
	}
	if err := holder.Release(ctx); err != nil {
		t.Logf("releasing the lock after the case: %v", err)
	}
}

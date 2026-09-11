package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mikebz/nfs-verification/pkg/chaos"
	"github.com/mikebz/nfs-verification/pkg/framework"
	"github.com/mikebz/nfs-verification/pkg/slo"
)

// The chaos cases are named TestChaos... so that the fast gate can exclude them
// by name. They are slow, their failures need human triage, and red in the fast
// path trains people to ignore red. The prefix marks what a case does rather
// than where it sits in the plan: OBS-02 and OBS-03 injure the server too, so
// they carry it, and the plan ID in the comment above each case stays the only
// place its section is recorded.
//
// Every assertion here is client-observable. Nothing asserts how failover
// happens: the HA mechanism is a black box by design, and the server is a
// singleton in the data path, so what a client can see is the whole of what the
// architecture promises. Timing comes from pkg/slo against the profile
// preflight pinned, never from a literal.

// chaosSetup is what every case needs before anything is injured: a live server
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

// startChaosCase builds that setup with a workload that commits every record,
// which is what every case except the negative durability one wants.
func startChaosCase(ctx context.Context, t *testing.T, f *framework.Framework, id string) chaosSetup {
	t.Helper()
	return startChaosCaseWith(ctx, t, f, id, framework.WriteLoadSpec{})
}

// startChaosCaseWith builds it with a workload the caller shapes, and skips
// rather than fails when the cluster gives it nothing to injure. A cluster
// whose server pods were never discovered is a configuration gap, not a storage
// defect.
func startChaosCaseWith(ctx context.Context, t *testing.T, f *framework.Framework, id string,
	load framework.WriteLoadSpec) chaosSetup {
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
	load.Pod, load.Dir, load.ID = "writer", dir, id
	running, err := f.StartWriteLoadSpec(ctx, load)
	if err != nil {
		t.Fatalf("starting the workload on %s: %v", nodeA, err)
	}
	f.Defer(func(ctx context.Context) { _, _ = running.Stop(ctx) })
	// A few records before the fault, so that the case is measuring an
	// interruption to something rather than a cold start.
	if err := framework.Poll(ctx, framework.PollInterval, 2*time.Minute, func(ctx context.Context) (bool, error) {
		rep, err := running.Report(ctx)
		if err != nil {
			return false, err
		}
		return len(rep.Committed()) >= 3, nil
	}); err != nil {
		t.Fatalf("the workload never got going before the fault: %v", err)
	}
	return chaosSetup{f: f, target: target, load: running, dir: dir,
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
// already acknowledged is still there when read from another client. It returns
// the outage as the client experienced it, which the observability cases report
// next to what an operator could see from outside.
func assertRecovered(ctx context.Context, t *testing.T, s chaosSetup, faultAt time.Time) time.Duration {
	t.Helper()
	before, err := s.load.Report(ctx)
	if err != nil {
		t.Fatalf("reading the workload log: %v", err)
	}
	committed := before.CommittedBefore(faultAt)

	recovery := waitRecovered(ctx, t, s, faultAt)
	if recovery > s.budget {
		t.Errorf("time to first successful I/O was %s, above the %s budget for a server restart on the %s profile",
			recovery.Round(time.Second), s.budget, profile(t).Name)
	}
	assertLoadHealthy(ctx, t, s, committed)
	return recovery
}

// waitRecovered waits for the first write committed after the fault and returns
// how long that took. It is separate from the assertion because the repeated
// failover case measures five of these before it asserts anything about the
// workload as a whole.
func waitRecovered(ctx context.Context, t *testing.T, s chaosSetup, faultAt time.Time) time.Duration {
	t.Helper()
	// Waited out well past the budget on purpose: a case that gives up at the
	// SLO reports "timed out" where it could report how long recovery actually
	// took, and the second is what a defect report needs.
	waitFor := s.budget + 5*time.Minute
	var resumed framework.LoadRecord
	err := framework.Poll(ctx, framework.PollInterval, waitFor, func(ctx context.Context) (bool, error) {
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
	t.Logf("first committed write %s after the fault (budget %s, profile %s)",
		recovery.Round(time.Second), s.budget, profile(t).Name)
	return recovery
}

// assertLoadHealthy stops the workload and reads the rest of the story out of
// its log: no I/O errors, nothing unreadable, and every write the server had
// already committed still there when a second client looks for it.
func assertLoadHealthy(ctx context.Context, t *testing.T, s chaosSetup, committed []int) {
	t.Helper()
	final, err := s.load.Stop(ctx)
	if err != nil {
		t.Fatalf("stopping the workload: %v", err)
	}
	t.Logf("longest gap between attempts was %s", final.LongestGap().Round(time.Second))

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
	// rather than the writer's own page cache. By content, not by existence: a
	// record that is there and says something else is worse than one that is
	// gone, and an existence check reports the two the same way.
	assertCommittedRecordsIntact(ctx, t, s.f, s.verifier, s.dir, committed)
}

// observeGrace reads what the server's log stream said about grace since a
// moment. It never fails a case on its own: a server that says nothing is
// OBS-03's finding, and one missing signal should produce one failure rather
// than four.
func observeGrace(ctx context.Context, t *testing.T, f *framework.Framework, since time.Time) framework.GraceObservation {
	t.Helper()
	obs, err := framework.ObserveGrace(ctx, f.C, since)
	if err != nil {
		t.Logf("could not read the server log stream for grace: %v", err)
		return framework.GraceObservation{}
	}
	t.Logf("grace: %s", obs.Describe())
	for _, line := range obs.Unclassified {
		t.Logf("a server log line mentions grace in a wording this suite does not classify, which is a gap "+
			"in the harness rather than in the server: %q", line)
	}
	return obs
}

// waitGraceWindow polls the log stream until grace has been both entered and
// left, which is the only window a case may measure against. A window derived
// from the configured grace value anchored at the fault would end after the
// real one, because grace begins when the server restarts, and it would report
// a lawful lock grant as a violation.
func waitGraceWindow(ctx context.Context, t *testing.T, f *framework.Framework, since time.Time,
	within time.Duration) (framework.GraceWindow, framework.GraceObservation, bool) {
	t.Helper()
	var obs framework.GraceObservation
	var window framework.GraceWindow
	ok := false
	err := framework.Poll(ctx, framework.PollInterval, within, func(ctx context.Context) (bool, error) {
		var err error
		obs, err = framework.ObserveGrace(ctx, f.C, since)
		if err != nil {
			return false, err
		}
		window, ok = obs.Window()
		return ok, fmt.Errorf("grace has not been observed to start and finish yet")
	})
	if err != nil && !ok {
		// Not a failure here. Whether an unobservable grace period is a finding
		// or a reason to report blocked is the caller's to decide, and the two
		// cases that ask make opposite choices.
		t.Logf("grace was not observed to start and finish within %s: %v", within, err)
	}
	t.Logf("grace: %s", obs.Describe())
	for _, line := range obs.Unclassified {
		t.Logf("a server log line mentions grace in a wording this suite does not classify, which is a gap "+
			"in the harness rather than in the server: %q", line)
	}
	return window, obs, ok
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

// failoverCycles is the repeated failover count from Section 3.3 of the plan.
const failoverCycles = 5

// CHAOS-05: five failovers in a row. Each one must recover on its own, and
// grace must be entered once per failover rather than repeatedly. Derived from
// reports of clients stalled for hours after repeated grace entry during
// address takeover, which presents as a hung client in front of a healthy
// server.
//
// The plan says five cycles inside ten minutes. The wall clock is recorded and
// not asserted: on the default profile grace alone is ninety seconds, so five
// lawful recoveries do not fit inside ten minutes and a case that asserted it
// would fail with no defect present. Every cycle is asserted against the
// restart SLO instead, which is the assertion the wall clock was standing in
// for.
//
// Steps:
//  1. Start a workload and record what the server had committed before the
//     first fault.
//  2. Five times: resolve the server pod again, since the last cycle replaced
//     it, read the writer's clock, delete the pod, and wait for the first
//     write committed afterwards.
//  3. Assert each cycle recovered inside the restart budget.
//  4. Assert grace was entered at most once per cycle, where the server makes
//     grace observable at all.
//  5. Assert zero I/O errors across the whole run, and that nothing committed
//     before the first fault was lost.
func TestChaosRepeatedFailover(t *testing.T) {
	f := framework.New(t, "CHAOS-05")
	ctx, cancel := caseCtx(t, 90*time.Minute)
	defer cancel()

	s := startChaosCase(ctx, t, f, "chaos05")
	if s.target.Controller == "" {
		t.Skipf("blocked: server pod %s has no controller, so deleting it would not bring it back", s.target.Pod)
	}

	before, err := s.load.Report(ctx)
	if err != nil {
		t.Fatalf("reading the workload log: %v", err)
	}
	firstFaultAt, err := f.PodNow(ctx, s.writer)
	if err != nil {
		t.Fatalf("reading the writer's clock: %v", err)
	}
	committed := before.CommittedBefore(firstFaultAt)

	graceObserved := false
	started := time.Now()
	for cycle := 1; cycle <= failoverCycles; cycle++ {
		// Resolved again every cycle: the previous cycle deleted the pod this
		// target names, and a stale target either fails to act or acts on
		// something else.
		target, err := chaos.ServerTarget(ctx, f)
		if err != nil {
			t.Fatalf("cycle %d: the server did not come back as a pod this case can find again: %v", cycle, err)
		}
		faultAt, err := f.PodNow(ctx, s.writer)
		if err != nil {
			t.Fatalf("cycle %d: reading the writer's clock: %v", cycle, err)
		}
		since := time.Now()
		if err := chaos.DeleteServerPod(ctx, f, target); err != nil {
			t.Skipf("blocked at cycle %d: %v", cycle, err)
		}

		recovery := waitRecovered(ctx, t, s, faultAt)
		if recovery > s.budget {
			t.Errorf("cycle %d of %d recovered in %s, above the %s budget on the %s profile",
				cycle, failoverCycles, recovery.Round(time.Second), s.budget, profile(t).Name)
		}
		// Waited out before the next cycle resolves a target, so the pod this
		// case aims at next is the replacement rather than the one still
		// terminating from this cycle.
		if err := chaos.WaitServerBack(ctx, f, framework.PodReadyTimeout); err != nil {
			t.Fatalf("cycle %d: no server pod is ready again after the delete, even though I/O resumed, "+
				"so the next cycle has nothing safe to injure: %v", cycle, err)
		}

		// Attributed to this cycle rather than summed over the run: a count
		// over five failovers cannot tell a re-entry loop from five ordinary
		// grace periods.
		obs := observeGrace(ctx, t, f, since)
		enters := len(obs.Entries())
		if enters > 0 {
			graceObserved = true
		}
		if enters > 1 {
			t.Errorf("cycle %d entered grace %d times for one failover, which is a grace re-entry loop: %s. "+
				"This is a finding about the server, not about the client that stalled behind it",
				cycle, enters, obs.Describe())
		}
	}
	t.Logf("%d failovers took %s in total, which is recorded rather than asserted: on the %s profile "+
		"grace alone is %s per cycle", failoverCycles, time.Since(started).Round(time.Second),
		profile(t).Name, profile(t).Grace)
	if !graceObserved {
		t.Logf("grace was never observable in the server's log stream, so the re-entry check above proved " +
			"nothing. OBS-03 is the case that fails for that missing signal")
	}

	assertLoadHealthy(ctx, t, s, committed)
}

// lockUnderTest is one lock held across a failover, and the client that must
// keep being refused it.
type lockUnderTest struct {
	holder *framework.LockHolder
	held   string
	probe  string
	path   string
}

// CHAOS-06: a failover while clients hold locks. Every lock must be reclaimed,
// and no conflicting lock may be granted to a different client. The SLO row is
// 100% reclaimed, so the case takes several locks from two clients rather than
// one from one: a case that checks a single lock cannot report a fraction.
//
// The locks here are whole-file. A Linux NFSv4 client sends one to the server
// as a lock over the whole byte range, so reclaim and exclusivity travel the
// same protocol path a sub-file range would. What is not covered yet is two
// clients holding disjoint ranges of one file, which needs the locktool binary
// arriving with DATA-06 in step 6.
//
// Steps:
//  1. Start a workload, and take locks on several files: three held by the
//     writer, one by the client on the other node.
//  2. Confirm every lock excludes the other client before anything is injured,
//     so a case that was broken from the start cannot pass.
//  3. Delete the server pod and assert the ordinary recovery.
//  4. Assert every holder still holds its lock, and every probe from the other
//     node is still refused. Report the reclaimed fraction against the SLO.
func TestChaosLockReclaimAcrossFailover(t *testing.T) {
	f := framework.New(t, "CHAOS-06")
	ctx, cancel := caseCtx(t, 45*time.Minute)
	defer cancel()

	s := startChaosCase(ctx, t, f, "chaos06")
	if s.target.Controller == "" {
		t.Skipf("blocked: server pod %s has no controller, so deleting it would not bring it back", s.target.Pod)
	}

	// Three locks from the writer and one from the client on the other node, so
	// the reclaim path is exercised from both clients rather than from one.
	want := []struct{ held, probe, id string }{
		{s.writer, s.verifier, "chaos06a"},
		{s.writer, s.verifier, "chaos06b"},
		{s.writer, s.verifier, "chaos06c"},
		{s.verifier, s.writer, "chaos06d"},
	}
	var locks []lockUnderTest
	for _, w := range want {
		path := fmt.Sprintf("%s/%s.lock", s.dir, w.id)
		holder, err := f.HoldFlock(ctx, w.held, path, w.id)
		if err != nil {
			t.Fatalf("taking lock %s in %s before the fault: %v", w.id, w.held, err)
		}
		// Registered before the case can fail, so a failure does not leave a
		// lock held on the share by a pod that outlives the case.
		f.Defer(func(ctx context.Context) { _ = holder.Release(ctx) })
		granted, out, err := f.TryFlock(ctx, w.probe, path)
		if err != nil {
			t.Fatalf("probing lock %s from %s before the fault: %v", w.id, w.probe, err)
		}
		if granted {
			t.Fatalf("lock %s was granted to %s while %s held it, before any fault was injected (%s)",
				w.id, w.probe, w.held, out)
		}
		locks = append(locks, lockUnderTest{holder: holder, held: w.held, probe: w.probe, path: path})
	}
	t.Logf("%d locks held across two clients, each excluding the other client", len(locks))

	faultAt, err := f.PodNow(ctx, s.writer)
	if err != nil {
		t.Fatalf("reading the writer's clock: %v", err)
	}
	since := time.Now()
	if err := chaos.DeleteServerPod(ctx, f, s.target); err != nil {
		t.Skipf("blocked: %v", err)
	}

	assertRecovered(ctx, t, s, faultAt)
	if err := chaos.WaitServerBack(ctx, f, framework.PodReadyTimeout); err != nil {
		t.Errorf("no server pod is ready again after the delete, even though I/O resumed: %v", err)
	}
	observeGrace(ctx, t, f, since)

	// Both ends, for every lock. The holder still believing it holds the lock
	// proves nothing on its own: a client that lost a lock does not find out
	// until it uses it, and the failure this case exists to catch is one lock
	// granted to two clients at once.
	reclaimed := 0
	for _, l := range locks {
		state, err := l.holder.State(ctx)
		if err != nil {
			t.Fatalf("reading the state of the holder in %s: %v", l.held, err)
		}
		if state != "held" {
			t.Errorf("the holder in %s reports %q for %s after the failover, so it lost a lock it never released",
				l.held, state, l.path)
			continue
		}
		granted, out, err := f.TryFlock(ctx, l.probe, l.path)
		if err != nil {
			t.Fatalf("probing %s from %s after the failover: %v", l.path, l.probe, err)
		}
		if granted {
			t.Errorf("after the failover %s was granted %s, which %s never released (%s). Either the lock "+
				"was not reclaimed or a conflicting one was granted; the recovery state backend recorded at "+
				"preflight is %q, which is where triage starts",
				l.probe, l.path, l.held, out, f.Env.RecoveryStateBackend)
			continue
		}
		reclaimed++
	}
	fraction := float64(reclaimed) / float64(len(locks))
	if fraction < slo.LockReclaimFraction {
		t.Errorf("%d of %d locks survived the failover (%.0f%%), want %.0f%%: grace exists so that state "+
			"held before a restart is still held after it",
			reclaimed, len(locks), fraction*100, slo.LockReclaimFraction*100)
	} else {
		t.Logf("all %d locks survived the failover, still held and still exclusive", len(locks))
	}
}

// CHAOS-07: a second client attempting a lock it has never held, while the
// server is in grace. During grace a server accepts reclaims of state that
// existed before the crash and refuses everything new, so this lock must never
// be granted inside the window, and must be granted once grace ends.
//
// The assertion is on grants, never on refusals. During an outage every attempt
// fails for the ordinary reason that the server is not there, so a refusal
// carries no guarantee and a grant carries all of it. The window comes from the
// server's own signal: a window derived from the configured grace value
// anchored at the fault would end after the real one, and would report a lawful
// grant as a violation.
//
// Steps:
//  1. Start a workload and take a lock in the writer that is never released, so
//     the server has outstanding state and cannot lift grace early. Section 3.8
//     of the plan requires this, or the case passes vacuously.
//  2. Start a probe in the client on the other node, attempting a lock on a
//     file nothing has ever locked.
//  3. Delete the server pod, and assert the ordinary recovery.
//  4. Wait for grace to be observed entered and left. Report blocked if the
//     server never makes it observable, since there is then no window to place
//     a grant inside or outside of.
//  5. Assert no attempt was granted inside the window, allowing for the two
//     clocks the window and the probe come from.
//  6. Assert an attempt was granted after grace ended, since a server that
//     never resumes granting new state has not recovered.
func TestChaosNewLockDuringGrace(t *testing.T) {
	f := framework.New(t, "CHAOS-07")
	ctx, cancel := caseCtx(t, 45*time.Minute)
	defer cancel()

	s := startChaosCase(ctx, t, f, "chaos07")
	if s.target.Controller == "" {
		t.Skipf("blocked: server pod %s has no controller, so deleting it would not bring it back", s.target.Pod)
	}

	// Outstanding state that is never released. Without it a server may
	// conclude no further clients will reclaim and lift grace early, and the
	// probe would then run against a server that is no longer enforcing
	// anything.
	holder, err := f.HoldFlock(ctx, s.writer, s.dir+"/chaos07-held.lock", "chaos07held")
	if err != nil {
		t.Fatalf("taking the lock that keeps the server in grace: %v", err)
	}
	f.Defer(func(ctx context.Context) { _ = holder.Release(ctx) })

	// A path nothing has ever locked: the case is about new state, not about
	// state anyone could reclaim.
	probe, err := f.StartLockProbe(ctx, s.verifier, s.dir+"/chaos07-new.lock", "chaos07")
	if err != nil {
		t.Fatalf("starting the lock probe on the second client: %v", err)
	}
	f.Defer(func(ctx context.Context) { _, _ = probe.Stop(ctx) })

	faultAt, err := f.PodNow(ctx, s.writer)
	if err != nil {
		t.Fatalf("reading the writer's clock: %v", err)
	}
	since := time.Now()
	if err := chaos.DeleteServerPod(ctx, f, s.target); err != nil {
		t.Skipf("blocked: %v", err)
	}

	assertRecovered(ctx, t, s, faultAt)

	window, _, ok := waitGraceWindow(ctx, t, f, since, slo.GraceExitBound(profile(t))+s.budget)
	if !ok {
		t.Skipf("blocked: this server does not make grace observable, so there is no window to place a " +
			"lock grant inside or outside of. OBS-03 is the case that fails for that")
	}
	t.Logf("grace ran %s", window)

	rep, err := probe.Stop(ctx)
	if err != nil {
		t.Fatalf("stopping the lock probe: %v", err)
	}
	if len(rep.Unparsed) > 0 {
		t.Errorf("the probe log holds %d lines the harness could not read, so what follows cannot be "+
			"trusted: %q", len(rep.Unparsed), rep.Unparsed[0])
	}

	// The window is stamped by the server's node and the attempts by the
	// client's, so only a grant unambiguously inside the window counts.
	granted := rep.GrantsWithin(window, slo.ClockSkewGuard)
	if len(granted) > slo.MaxNewLocksDuringGrace {
		t.Errorf("%d new locks were granted to %s while the server was in grace (%s), want %d. "+
			"Grace bars new state acquisition so that a lock held before the crash cannot be taken by "+
			"someone else before its owner reclaims it: either it was granted deliberately or the state "+
			"that would have refused it was lost, and neither is acceptable. The recovery state backend "+
			"recorded at preflight is %q",
			len(granted), s.verifier, window, slo.MaxNewLocksDuringGrace, f.Env.RecoveryStateBackend)
	}
	if after, ok := rep.FirstSuccessAfter(window.End.Add(slo.ClockSkewGuard)); !ok {
		t.Errorf("no new lock was granted after grace ended at %s, so the server never resumed accepting "+
			"new state: %d attempts, %d of them refused",
			window.End.UTC().Format(time.RFC3339), len(rep.Records), len(rep.Errors()))
	} else {
		t.Logf("the first new lock after grace was granted at %s, %s after grace ended",
			after.At.UTC().Format(time.RFC3339), after.At.Sub(window.End).Round(time.Second))
	}
}

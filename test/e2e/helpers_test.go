package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mikebz/nfs-verification/pkg/framework"
	"github.com/mikebz/nfs-verification/pkg/slo"
)

// mountPath is where every test pod mounts the share.
const mountPath = "/mnt/share"

// caseCtx returns a context bounded by the case's own budget. No case runs
// unbounded: a hung case that never fails teaches nothing.
func caseCtx(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), d)
}

// mounts attaches a claim at the standard path.
func mounts(claim string) []framework.MountSpec {
	return []framework.MountSpec{{Claim: claim, Path: mountPath}}
}

// toolsPod is the standard client pod: a shell holding the mount open, so that
// the case drives I/O through exec rather than through a bespoke image.
func toolsPod(name, claim, node string) framework.PodSpec {
	return framework.PodSpec{Name: name, Node: node, Mounts: mounts(claim)}
}

// fileIn builds a path on the share.
func fileIn(name string) string { return fmt.Sprintf("%s/%s", mountPath, name) }

// profile returns the pinned lease/grace profile the run measures against.
// Cases that assert timing read their bounds from here, never from a literal.
func profile(t *testing.T) slo.Profile {
	t.Helper()
	p, err := framework.Profile()
	if err != nil {
		t.Fatalf("%v", err)
	}
	return p
}

// requireCap skips a case whose capability is absent. Cases skip by capability,
// never by platform name.
func requireCap(t *testing.T, have bool, what string) {
	t.Helper()
	if !have {
		t.Skipf("capability unavailable: %s", what)
	}
}

// blocked reports a case that could run somewhere but not here: a tool the
// image does not carry, a mount option that makes the thing under test local, a
// PV whose fields could not be read.
//
// It is neither a pass nor a failure, and it always names the probe that failed
// and the flag or option that would fix it. "blocked" with no reason is the
// failure mode the protocol cases add the most opportunities for.
func blocked(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Skipf("blocked: "+format, args...)
}

// failOrBlock reports a harness error as a failure, unless it is a condition
// that would not arise on a different cluster or image, in which case the case
// reports blocked instead.
//
// The two answers are routed differently by whoever reads the run, and a helper
// that could not tell them apart would file "no locktool built for arm64" as a
// storage defect.
func failOrBlock(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	what := fmt.Sprintf(format, args...)
	if framework.IsBlocked(err) {
		blocked(t, "%s: %v", what, err)
		return
	}
	t.Fatalf("%s: %v", what, err)
}

// requireServerSideLocking blocks a lock case running on a mount that keeps
// locks on the client.
//
// nolock and local_lock=all|flock|posix tell the Linux NFS client never to send
// a lock to the server. On such a mount two pods on two nodes both get the
// lock, every time, correctly, by configuration; a cross-node exclusion case
// would fail and the failure would name the server for a mount option's fault.
func requireServerSideLocking(ctx context.Context, t *testing.T, f *framework.Framework,
	pvName string, kind framework.LockKind, nodes ...string) {
	t.Helper()
	for _, node := range nodes {
		m, err := f.CheckLockMount(ctx, node, pvName, kind)
		if err != nil {
			t.Fatalf("reading the mount options of %s on %s: %v", pvName, node, err)
		}
		t.Logf("mount on %s: %s", node, m.Line.Options)
		if m.Local {
			blocked(t, "%s", m.Blocked())
		}
	}
}

// assertCommittedRecordsIntact sweeps a set of committed records and fails on
// anything but correct.
//
// Every one of these was written with conv=fsync and acknowledged by the
// server, so post-COMMIT durability applies to all of them and there is no
// verdict here that is lawful except correct. Absent is data loss, short is a
// record the server acknowledged and then truncated, and wrong is corruption.
func assertCommittedRecordsIntact(ctx context.Context, t *testing.T, f *framework.Framework,
	pod, dir string, committed []int) {
	t.Helper()
	sweep, err := f.VerifyRecords(ctx, pod, dir, committed)
	if err != nil {
		writeRawSweep(t, f, sweep)
		t.Fatalf("sweeping committed records from %s: %v", pod, err)
	}
	recordSweep(t, f, sweep)
	if len(sweep.Unparsed) > 0 {
		t.Errorf("the sweep produced %d lines the harness could not read, so the verdicts below cannot "+
			"be trusted: %q", len(sweep.Unparsed), sweep.Unparsed[0])
	}

	if wrong, ok := sweep.FirstWrong(); ok {
		t.Errorf("%d of %d committed records hold bytes nobody wrote, the first in record %d at offset "+
			"%d. A wrong byte at an offset that was written is corruption under every reading of the "+
			"protocol, and it is a worse finding than a lost write",
			sweep.Count(framework.VerdictWrong), len(committed), wrong.Index, wrong.Offset)
	}
	lost := sweep.Count(framework.VerdictAbsent) + sweep.Count(framework.VerdictShort)
	if lost > slo.MaxCommittedWritesLost {
		t.Errorf("%d of %d writes the server had already committed before the fault are gone or "+
			"truncated (absent %v, short %v). Post-COMMIT durability is a protocol guarantee, so this "+
			"is data loss, not a slow recovery",
			lost, len(committed), sweep.Indices(framework.VerdictAbsent), sweep.Indices(framework.VerdictShort))
	}
	if lost == 0 && sweep.Count(framework.VerdictWrong) == 0 {
		t.Logf("all %d writes committed before the fault survived it, and still say what they said",
			len(committed))
	}
}

// recordSweep puts the verdict table in the bundle. Written whether the case
// passed or not: a run where three records came back short and recovered is a
// different run from one where none did, and the pass looks identical.
func recordSweep(t *testing.T, f *framework.Framework, sweep framework.RecordSweep) {
	t.Helper()
	t.Logf("record sweep: %s", sweep)
	if err := f.WriteArtifact("record-verdicts.txt", []byte(sweep.Table())); err != nil {
		t.Logf("writing the verdict table: %v", err)
	}
}

// writeRawSweep puts what the sweep printed in the bundle, for the case where
// the sweep itself failed rather than the records.
//
// A sweep that answered for fewer records than it was asked about has no
// verdict table worth writing, so recordSweep would file an empty one and the
// run would be unreproducible. What the pod sent, including nothing at all, is
// the evidence. See F-011 in docs/findings.md.
func writeRawSweep(t *testing.T, f *framework.Framework, sweep framework.RecordSweep) {
	t.Helper()
	if err := f.WriteArtifact("record-sweep-raw.txt", []byte(sweep.Raw)); err != nil {
		t.Logf("writing the raw sweep output: %v", err)
	}
}

// serverRestartWatch is the first of the two readings behind "the server did
// not fall over under this", taken before a case does the thing it is about.
//
// It carries the error from that reading instead of acting on it, because the
// two halves are far apart: the reading is the first thing a case does and the
// assertion the last. A cluster whose NFS server is managed and has no pod here
// cannot answer the question at all (framework.ErrNoServerPods), and blocking
// at the top of the case would throw away everything else it asserts -- PROV-02's
// export IDs, PROV-11's writes across an expansion, DATA-10's invented names --
// on a cluster where those assertions are perfectly meaningful.
type serverRestartWatch struct {
	before int32
	err    error
}

// watchServerRestarts reads the server's container restart counts, to be
// compared by assertNoRestart against a second reading later.
func watchServerRestarts(ctx context.Context, f *framework.Framework) serverRestartWatch {
	before, err := framework.ServerRestartCount(ctx, f.C)
	return serverRestartWatch{before: before, err: err}
}

// assertNoRestart re-reads the counts and asserts that the server did not
// restart while the case was doing the thing it is about. The during argument
// names that operation, in a form that reads after "during", and lands in the
// failure message.
//
// The assertion is its own subtest so that an unanswerable question reports
// blocked by itself, leaving the verdict on the rest of the case intact. Same
// reason DATA-11 splits its halves: reported as one case, a blocked answer to
// half of it makes the half that ran and passed invisible.
func (w serverRestartWatch) assertNoRestart(ctx context.Context, t *testing.T, f *framework.Framework, during string) {
	t.Helper()
	t.Run("server-did-not-restart", func(t *testing.T) {
		f := f.SubTest(t)
		if w.err != nil {
			failOrBlock(t, w.err, "reading the server's restart count before %s", during)
		}
		after, err := framework.ServerRestartCount(ctx, f.C)
		if err != nil {
			failOrBlock(t, err, "re-reading the server's restart count after %s", during)
		}
		if after != w.before {
			t.Errorf("the server restarted %d time(s) during %s: %d container restarts across the "+
				"discovered server pods, was %d. The work a case does through the Kubernetes API must "+
				"not be able to take the server down", after-w.before, during, after, w.before)
		}
	})
}

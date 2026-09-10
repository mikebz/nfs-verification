package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/mikebz/nfs-verification/pkg/framework"
)

// Assertions here are calibrated to the protocol claim in Section 2.3 of the
// plan: NFSv4.1 semantics, close-to-open for regular files and byte-range locks
// for finer coordination. They are deliberately not tightened to POSIX.

// DATA-03: close-to-open across nodes. This is the guarantee the architecture
// actually makes, so it is asserted directly and nothing stronger is.
func TestDATA03_CloseToOpen(t *testing.T) {
	f := framework.New(t, "DATA-03", framework.GatePresubmit)
	ctx, cancel := caseCtx(t, 10*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustBoundRWXPVC(ctx, "data03")
	f.MustPod(ctx, toolsPod("writer", pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod("reader", pvc.Name, nodeB))

	path := fileIn("data03.txt")
	// The redirection closes the file, which is what makes the content visible
	// to a reader that opens it afterwards.
	f.MustShf(ctx, "writer", "echo close-to-open-payload > %s", framework.Quote(path))
	got := f.MustShf(ctx, "reader", "cat %s", framework.Quote(path))
	if got != "close-to-open-payload" {
		t.Errorf("reader on %s did not see what the writer on %s closed: got %q", nodeB, nodeA, got)
	}
}

// DATA-05: mutual exclusion across nodes. Locks are advisory, and they are
// visible across clients only because every client serializes through the one
// server. The byte-range half of this case lands with the locktool image.
func TestDATA05_LocksAcrossNodes(t *testing.T) {
	f := framework.New(t, "DATA-05", framework.GatePresubmit)
	requireCap(f, f.Caps.MultiNode, "cross-node locking needs two schedulable workers")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustBoundRWXPVC(ctx, "data05")
	f.MustPod(ctx, toolsPod("holder", pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod("contender", pvc.Name, nodeB))
	path := fileIn("data05.lock")

	holder, err := f.HoldFlock(ctx, "holder", path, "data05")
	if err != nil {
		t.Fatalf("acquiring the lock on %s: %v", nodeA, err)
	}
	granted, out, err := f.TryFlock(ctx, "contender", path)
	if err != nil {
		t.Fatalf("probing the lock from %s: %v", nodeB, err)
	}
	if granted {
		t.Fatalf("mutual exclusion broken: a pod on %s was granted a lock held by a pod on %s (%s)", nodeB, nodeA, out)
	}

	if err := holder.Release(ctx); err != nil {
		t.Fatalf("releasing the lock: %v", err)
	}
	// The second acquirer must succeed once the first lets go, within a bound
	// that has nothing to do with lease expiry: this is a clean release.
	if err := framework.Poll(ctx, framework.FastPoll, time.Minute, func(ctx context.Context) (bool, error) {
		granted, _, err := f.TryFlock(ctx, "contender", path)
		return granted, err
	}); err != nil {
		t.Errorf("lock was never grantable after a clean release: %v", err)
	}
	f.Logf("lock changed hands across nodes %s and %s under the one server (profile %s)",
		nodeA, nodeB, profile(f).Name)
}

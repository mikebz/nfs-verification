package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikebz/nfs-verification/pkg/framework"
)

// Assertions here are calibrated to the protocol claim in Section 2.3 of the
// plan: NFSv4.1 semantics, close-to-open for regular files and byte-range locks
// for finer coordination. They are deliberately not tightened to POSIX.

// DATA-03: close-to-open across nodes. This is the guarantee the architecture
// actually makes, so it is asserted directly and nothing stronger is.
func TestCloseToOpen(t *testing.T) {
	f := framework.New(t, "DATA-03")
	requireCap(t, f.Caps.MultiNode, "cross-node close-to-open needs two schedulable workers")
	ctx, cancel := caseCtx(t, 10*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "data03")
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
func TestLocksAcrossNodes(t *testing.T) {
	f := framework.New(t, "DATA-05")
	requireCap(t, f.Caps.MultiNode, "cross-node locking needs two schedulable workers")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "data05")
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
	t.Logf("lock changed hands across nodes %s and %s under the one server (profile %s)",
		nodeA, nodeB, profile(t).Name)
}

// DATA-01: N pods, N files, partitioned by file. Every checksum must match, and
// no writer's content may appear in another writer's file. Partitioned by file
// on purpose: this case is about the server keeping concurrent writers apart,
// not about coordination between them, which is DATA-02 and DATA-05.
func TestConcurrentWritersDistinctFiles(t *testing.T) {
	f := framework.New(t, "DATA-01")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	nodes, err := f.WorkerNodes(ctx)
	if err != nil {
		t.Fatalf("listing worker nodes: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatalf("no schedulable worker nodes")
	}
	const writers = 4
	pvc := f.MustRWXPVC(ctx, "data01")
	pods := make([]string, writers)
	for i := range pods {
		pods[i] = fmt.Sprintf("writer%d", i)
		// Round robin rather than left to the scheduler: a cross-contamination
		// failure has to name the node that produced it.
		f.MustPod(ctx, toolsPod(pods[i], pvc.Name, nodes[i%len(nodes)]))
	}
	t.Logf("%d writers across %d nodes: %v", writers, len(nodes), nodes)

	dir := fileIn("data01")
	f.MustShf(ctx, pods[0], "mkdir -p %s", framework.Quote(dir))

	paths := make([]string, writers)
	sums := make([]string, writers)
	errs := make([]error, writers)
	// Concurrently, not one after another: writers that take turns say nothing
	// about a server keeping simultaneous writers apart, which is the case.
	var wg sync.WaitGroup
	for i, pod := range pods {
		paths[i] = fmt.Sprintf("%s/file-%d.dat", dir, i)
		wg.Add(1)
		go func(i int, pod string) {
			defer wg.Done()
			// Distinct seeds, so that a file holding another writer's bytes
			// fails the comparison rather than passing by coincidence.
			sums[i], errs[i] = f.WriteFile(ctx, pod, paths[i], 1<<20, fmt.Sprintf("data01-writer-%d", i))
		}(i, pod)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d writing %s: %v", i, paths[i], err)
		}
	}

	// Each file is verified by a pod that did not write it, so the comparison
	// crosses the server instead of hitting the writer's own page cache.
	for i := range paths {
		reader := pods[(i+1)%writers]
		got, err := f.Sha256(ctx, reader, paths[i])
		if err != nil {
			t.Errorf("%s reading %s: %v", reader, paths[i], err)
			continue
		}
		if got != sums[i] {
			t.Errorf("%s read %s as %s, but its writer wrote %s", reader, paths[i], got, sums[i])
		}
	}
	// Distinct checksums are what rules out two writers landing on one file's
	// content; equal checksums with equal sizes would otherwise read as a pass.
	seen := map[string]int{}
	for i, sum := range sums {
		if prev, ok := seen[sum]; ok {
			t.Errorf("files %s and %s hold identical content (%s): writers were not kept apart",
				paths[prev], paths[i], sum)
		}
		seen[sum] = i
	}
	if got := f.MustShf(ctx, pods[0], "ls -1 %s | wc -l", framework.Quote(dir)); got != fmt.Sprint(writers) {
		t.Errorf("the share holds %s entries under %s, want %d: %s",
			got, dir, writers, f.MustShf(ctx, pods[0], "ls -l %s", framework.Quote(dir)))
	}
}

// DATA-04: the negative of DATA-03, and the reason it is worth having. A reader
// on another node may see nothing, or part of what a writer has produced, while
// the writer still holds the file open. That is the protocol behaving as
// specified, so this case asserts it is **not** a failure and records which of
// the two the deployment does. What it does assert is that the reader never
// sees bytes nobody wrote, and that the data is there once the writer closes.
func TestNoVisibilityBeforeClose(t *testing.T) {
	f := framework.New(t, "DATA-04")
	requireCap(t, f.Caps.MultiNode, "the cross-node visibility boundary needs two schedulable workers")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "data04")
	f.MustPod(ctx, toolsPod("writer", pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod("reader", pvc.Name, nodeB))

	path := fileIn("data04.txt")
	// Well under a page, so that a partial flush cannot produce a file whose
	// size is final while an earlier region still reads as zeros. This case is
	// about visibility, and a torn multi-page write would confuse it with
	// something else.
	const payload = "data04-written-but-not-yet-closed"

	w, err := f.HoldOpenWrite(ctx, "writer", path, payload, "data04")
	if err != nil {
		t.Fatalf("holding %s open on %s: %v", path, nodeA, err)
	}

	early := f.MustShf(ctx, "reader", "cat %s 2>/dev/null || true", framework.Quote(path))
	switch {
	case early == payload:
		t.Logf("the reader on %s saw the writer's data before close; this deployment flushes early, "+
			"which is permitted and is not what close-to-open promises", nodeB)
	case strings.HasPrefix(payload, early):
		t.Logf("the reader on %s saw %d of %d bytes before close, which is the documented boundary: "+
			"close-to-open promises nothing until the writer closes", nodeB, len(early), len(payload))
	default:
		// This is the only failure in the case, and it is a real one: bytes
		// nobody wrote are corruption, not a visibility boundary.
		t.Errorf("the reader on %s saw %q, which is not a prefix of what the writer produced (%q)",
			nodeB, early, payload)
	}

	if err := w.Close(ctx); err != nil {
		t.Fatalf("closing %s on %s: %v", path, nodeA, err)
	}
	// After the close, the guarantee applies. The bound is generous and the
	// delay is logged: an attribute cache holding a reader back for tens of
	// seconds after a close is worth seeing even when it passes.
	start := time.Now()
	var late string
	if err := framework.Poll(ctx, framework.PollInterval, time.Minute, func(ctx context.Context) (bool, error) {
		late = f.MustShf(ctx, "reader", "cat %s 2>/dev/null || true", framework.Quote(path))
		return late == payload, fmt.Errorf("reader on %s still sees %q", nodeB, late)
	}); err != nil {
		t.Fatalf("close-to-open did not hold after the writer closed: %v", err)
	}
	t.Logf("the reader on %s saw the closed file after %s", nodeB, time.Since(start).Round(time.Millisecond))
}

package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikebz/nfs-verification/pkg/chaos"
	"github.com/mikebz/nfs-verification/pkg/framework"
	"github.com/mikebz/nfs-verification/pkg/slo"
)

// Assertions here are calibrated to the protocol claim in Section 2.3 of the
// plan: NFSv4.1 semantics, close-to-open for regular files and byte-range locks
// for finer coordination. They are deliberately not tightened to POSIX.

// DATA-03: close-to-open across nodes. This is the guarantee the architecture
// actually makes, so it is asserted directly and nothing stronger is.
//
// Steps:
//  1. Pin a writer to one node and a reader to another, on one claim.
//  2. Write and close a file on the writer, using a redirect, since the close
//     is what makes the content visible.
//  3. Read it on the reader and compare.
func TestDataCloseToOpen(t *testing.T) {
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

// DATA-05: mutual exclusion across nodes, in both the shapes an application
// can ask for. Locks are advisory, and they are visible across clients only
// because every client serializes through the one server.
//
// The two halves are not the same test twice. A flock lock reaches the server
// as a lock over the whole range, so the first half exercises the wire
// operation without ever expressing a range. What only a byte range can show is
// two clients holding *different* parts of one file at the same time, and a
// whole-file lock silently standing in for a range would grant neither or both.
// That is the assertion the second half carries.
//
// Steps:
//  1. Pin a holder to one node and a contender to another, on one claim.
//  2. Take an exclusive whole-file lock in the holder and confirm it is held.
//  3. Probe from the contender: it must be refused.
//  4. Release the lock in the holder.
//  5. The contender must then be granted it, inside a bound that has nothing
//     to do with lease expiry, since this was a clean release.
//  6. Take disjoint ranges of one file from both pods: both must be granted.
//  7. Probe each pod on the other's range, and on a range overlapping it: both
//     must be refused, and the refusal must name the range actually held.
func TestDataLocksAcrossNodes(t *testing.T) {
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

	assertDisjointRangesAreIndependent(ctx, t, f, disjointRanges{
		holderPod: "holder", holderNode: nodeA,
		otherPod: "contender", otherNode: nodeB,
		claim: "data05", path: fileIn("data05.ranges"), id: "data05",
	})
}

// disjointRanges names the two clients a byte-range assertion runs between.
type disjointRanges struct {
	holderPod, holderNode string
	otherPod, otherNode   string
	claim, path, id       string
}

// The two ranges. Disjoint, adjacent, and at offsets no other case uses, so a
// /proc/locks line can be matched to the client that took it.
const (
	rangeALow  = 0
	rangeBLow  = 8192
	rangeWidth = 4096
)

// assertDisjointRangesAreIndependent is the byte-range assertion shared by
// DATA-05 and the extension to CHAOS-06: two clients hold different parts of
// one file at the same time, and neither can have the other's.
//
// It returns the two holders, still held, so a caller can injure the server
// underneath them and ask the same questions afterwards.
func assertDisjointRangesAreIndependent(ctx context.Context, t *testing.T, f *framework.Framework,
	d disjointRanges) (*framework.LockHolder, *framework.LockHolder) {
	t.Helper()
	pv, err := f.PVForClaim(ctx, d.claim)
	if err != nil {
		t.Fatalf("finding the volume behind claim %s: %v", d.claim, err)
	}
	requireServerSideLocking(ctx, t, f, pv.Name, framework.PosixLock, d.holderNode, d.otherNode)

	rangeA := framework.WriteRange(rangeALow, rangeWidth)
	rangeB := framework.WriteRange(rangeBLow, rangeWidth)

	holderA, err := f.HoldLock(ctx, d.holderPod, d.path, d.id+"ra", rangeA)
	if err != nil {
		t.Fatalf("taking %s on %s in %s: %v", rangeA, d.path, d.holderNode, err)
	}
	f.Defer(func(ctx context.Context) { _ = holderA.Release(ctx) })

	// The one a whole-file lock would get wrong. If this is refused, the first
	// lock covered more than it was asked for.
	holderB, err := f.HoldLock(ctx, d.otherPod, d.path, d.id+"rb", rangeB)
	if err != nil {
		t.Fatalf("taking %s on %s in %s while %s held %s on the same file: %v. Two clients holding "+
			"different parts of one file is the only thing a byte range adds over a whole-file lock, "+
			"and it is what this half exists to show", rangeB, d.path, d.otherNode, d.holderNode, rangeA, err)
	}
	f.Defer(func(ctx context.Context) { _ = holderB.Release(ctx) })
	t.Logf("%s on %s holds %s and %s on %s holds %s, on one file, at once",
		d.holderPod, d.holderNode, rangeA, d.otherPod, d.otherNode, rangeB)

	assertRangesExcludeEachOther(ctx, t, f, d, rangeA, rangeB)
	return holderA, holderB
}

// assertRangesExcludeEachOther probes each client on the other's range and on a
// range overlapping it. Both must be refused.
func assertRangesExcludeEachOther(ctx context.Context, t *testing.T, f *framework.Framework,
	d disjointRanges, rangeA, rangeB framework.LockRange) {
	t.Helper()
	probes := []struct {
		by, node string
		want     framework.LockRange
		held     framework.LockRange
		heldBy   string
	}{
		{d.otherPod, d.otherNode, rangeA, rangeA, d.holderPod},
		{d.holderPod, d.holderNode, rangeB, rangeB, d.otherPod},
		// Overlapping rather than identical: a lock that covered only its own
		// first byte would refuse the exact range and grant this one.
		{d.otherPod, d.otherNode, framework.WriteRange(rangeALow+rangeWidth/2, rangeWidth), rangeA, d.holderPod},
		{d.holderPod, d.holderNode, framework.WriteRange(rangeBLow+rangeWidth/2, rangeWidth), rangeB, d.otherPod},
	}
	for _, p := range probes {
		ans, err := f.TryLock(ctx, p.by, d.path, p.want)
		if err != nil {
			t.Fatalf("probing %s from %s on %s: %v", p.want, p.by, p.node, err)
		}
		if ans.Free {
			t.Errorf("%s on %s was granted %s, which overlaps %s held by %s. Two clients believing they "+
				"hold the same bytes is the failure byte-range locking exists to prevent",
				p.by, p.node, p.want, p.held, p.heldBy)
			continue
		}
		// The refusal must name the range actually held. A whole-file lock
		// standing in for a range would refuse with a range to end of file, and
		// the assertion above would pass while the case measured nothing.
		if c := ans.Conflict; c.Known && (c.Start != p.held.Start || c.Len != p.held.Len) {
			t.Errorf("%s on %s was refused %s by a lock over %s, but %s holds %s. A refusal naming a "+
				"different range means the lock covers more than it was asked for, which is a whole-file "+
				"lock wearing a range's arguments",
				p.by, p.node, p.want, c, p.heldBy, p.held)
		}
	}
	t.Logf("each client is refused on the other's range and on a range overlapping it, by the range " +
		"actually held")
}

// DATA-01: N pods, N files, partitioned by file. Every checksum must match, and
// no writer's content may appear in another writer's file. Partitioned by file
// on purpose: this case is about the server keeping concurrent writers apart,
// not about coordination between them, which is DATA-02 and DATA-05.
//
// Steps:
//  1. Put four pods on the available worker nodes, round robin, on one claim.
//  2. Have all four write a megabyte each, at the same time, to their own file
//     with their own seed.
//  3. Verify each file from a pod that did not write it, so the read crosses
//     the server rather than the writer's page cache.
//  4. Assert the four checksums are distinct, so two writers landing on one
//     content cannot read as a pass.
//  5. Assert the directory holds exactly four entries.
func TestDataConcurrentWritersDistinctFiles(t *testing.T) {
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
//
// Steps:
//  1. Pin a writer to one node and a reader to another, on one claim.
//  2. Write a short payload through a descriptor the writer holds open.
//  3. Read from the reader: empty, a prefix, or the whole payload are all
//     recorded as the documented boundary. Anything else is corruption.
//  4. Close the descriptor.
//  5. The reader must then see the whole payload, and the delay is logged.
func TestDataNoVisibilityBeforeClose(t *testing.T) {
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

// appendCaveat is attached to every DATA-02 failure. NFSv4.1 has no append
// operation: a client implements O_APPEND by writing at the offset it believes
// to be end of file, and with several clients appending at once that belief can
// be stale. An exact record count is therefore an implementation property, not
// a protocol guarantee, and a failure here belongs in the boundary discussion
// before it is filed against the server.
const appendCaveat = "\n\nNote before filing: NFSv4.1 has no append operation. A client implements O_APPEND " +
	"by writing at the offset it believes to be end of file, so concurrent appends from several clients " +
	"are an implementation property and not something the protocol promises (see gap 1 in docs/plan.md). " +
	"Route this to the boundary discussion, not to the server owner, unless records are torn rather than lost: " +
	"a torn record is corruption under any reading."

// DATA-02: N pods append to one file with O_APPEND. Every record must arrive
// whole, and the byte count must be exact. The second half of that is the
// assertion the plan flags as stronger than the protocol, so it fails with the
// caveat above rather than as a bare mismatch.
//
// Steps:
//  1. Put four pods on the available worker nodes, round robin, on one claim,
//     and truncate the shared file.
//  2. Have all four append fifty short records at the same time, each through
//     one descriptor held open for its whole loop.
//  3. Read the file from a pod that never wrote to it, on a node no appender
//     used where the cluster has one to spare.
//  4. Assert every line is a whole record, and that no record appears twice.
//     A torn record is corruption under any reading of the protocol.
//  5. Assert the line count is exactly what was written. This is the flagged
//     assertion, so a mismatch carries the note that routes it.
func TestDataConcurrentAppendToOneFile(t *testing.T) {
	f := framework.New(t, "DATA-02")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	nodes, err := f.WorkerNodes(ctx)
	if err != nil {
		t.Fatalf("listing worker nodes: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatalf("no schedulable worker nodes")
	}
	// Records are one short line each, well under a page, so a torn record
	// means the append path tore it and not that the record was too large to
	// write in one operation.
	const (
		appenders = 4
		records   = 50
	)
	pvc := f.MustRWXPVC(ctx, "data02")
	pods := make([]string, appenders)
	for i := range pods {
		pods[i] = fmt.Sprintf("appender%d", i)
		f.MustPod(ctx, toolsPod(pods[i], pvc.Name, nodes[i%len(nodes)]))
	}
	// A reader that never wrote, and where there is a spare node, one that no
	// appender ran on. Reading the file back from a pod that just appended to
	// it can be served out of that client's own cache without the server ever
	// being asked, which would make the count below say nothing about what the
	// server actually holds.
	readerNode := nodes[len(nodes)-1]
	if len(nodes) > appenders {
		readerNode = nodes[appenders%len(nodes)]
	}
	const reader = "reader"
	f.MustPod(ctx, toolsPod(reader, pvc.Name, readerNode))

	path := fileIn("data02.log")
	f.MustShf(ctx, pods[0], ": > %s", framework.Quote(path))

	// The redirect wraps the whole loop, so the file is opened once with
	// O_APPEND and every record is written through that one descriptor. That is
	// the shape of a real log appender, and it is the harder case: a client
	// that reopened per record would revalidate the size each time and lose the
	// property under test. Running the loop inside the pod rather than driving
	// each record over exec is what makes the appends concurrent at all.
	errs := make([]error, appenders)
	var wg sync.WaitGroup
	for i, pod := range pods {
		wg.Add(1)
		go func(i int, pod string) {
			defer wg.Done()
			_, errs[i] = f.C.MustSh(ctx, framework.Namespace, f.Name(pod), "main", fmt.Sprintf(
				`set -e; name=%s; { i=0; while [ $i -lt %d ]; do i=$((i+1)); `+
					`printf 'record-from-%%s-%%04d\n' "$name" "$i"; done; } >> %s; echo done`,
				framework.Quote(pod), records, framework.Quote(path)))
		}(i, pod)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("appender %d (%s): %v", i, pods[i], err)
		}
	}

	// Read from the pod that never wrote, so the read crosses the server.
	out := f.MustShf(ctx, reader, "cat %s", framework.Quote(path))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if out == "" {
		lines = nil
	}

	// Integrity first, because it is the assertion that holds under any reading
	// of the protocol: a record that arrived must have arrived whole.
	valid := map[string]int{}
	var torn []string
	for _, line := range lines {
		if appendRecord.MatchString(line) {
			valid[line]++
			continue
		}
		torn = append(torn, line)
	}
	if len(torn) > 0 {
		show := torn
		if len(show) > 5 {
			show = show[:5]
		}
		t.Errorf("%d of %d lines are not whole records, so concurrent appends interleaved within a record: %q",
			len(torn), len(lines), show)
	}
	for line, n := range valid {
		if n > 1 {
			t.Errorf("record %q appears %d times: an append was applied more than once", line, n)
		}
	}

	// Then the count, which is the part the plan flags.
	want := appenders * records
	if len(lines) != want {
		t.Errorf("the file holds %d lines, want %d from %d appenders writing %d records each; "+
			"%d whole, %d torn%s", len(lines), want, appenders, records, len(valid), len(torn), appendCaveat)
	}
}

// appendRecord matches a whole record. Anything else in the file is two writes
// that landed on top of each other.
var appendRecord = regexp.MustCompile(`^record-from-[a-z0-9-]+-\d{4}$`)

// DATA-06: a byte-range lock held by a pod that is force-deleted. The lock must
// become available to another client inside one lease period, and the case says
// which mechanism released it.
//
// The plan's expected result is "lock released within one lease period; new
// acquirer succeeds". Read literally against Kubernetes, that sentence has the
// wrong actor in it. The NFSv4 client is the *node*, not the pod: a Linux
// client establishes a single lease on each server it accesses, and every mount
// and every pod on that node shares it. So:
//
//   - A pod dies. Kubelet kills the container, its descriptors close, the
//     client sends LOCKU, and the lock is gone in about a second. No lease
//     expires, because the node never stopped renewing it, and only that pod's
//     locks move.
//   - A node dies. Nothing closes anything, and the locks of every pod on that
//     node stay held until the lease expires, then drop together.
//
// This case injects the first and asserts the bound that holds under both,
// because a case asserting expiry would fail on a healthy cluster and one
// asserting promptness would fail wherever kubelet was slow to kill, for
// reasons that have nothing to do with NFS. It classifies what it saw, so that
// "one application, back in a second" and "every pod on node X, for a minute"
// do not produce an identical pass. The node-loss side is CHAOS-03 and SEC-07.
//
// Force-deleting a mounted pod is the first half of F-001, so the case does not
// return until the node has released the mount, and teardown refuses to delete
// a claim whose unmount was never observed.
//
// Steps:
//  1. Pin a holder to one node and an acquirer to another, on one claim.
//  2. Read back both mounts and report blocked if either keeps byte-range
//     locks on the client, since cross-node exclusion would not then exist.
//  3. Take a write lock on a range in the holder.
//  4. Confirm the acquirer is refused on that range, so a case that was broken
//     from the start cannot pass, and record what the refusal named.
//  5. Force-delete the holder and wait for its node to release the mount, while
//     the acquirer retries the same range alongside.
//  6. Assert the range became available inside one lease period, and classify
//     the interval as a descriptor close or a lease expiry.
func TestDataLockReleasedAfterForcedPodLoss(t *testing.T) {
	f := framework.New(t, "DATA-06")
	requireCap(t, f.Caps.MultiNode, "re-acquiring a lock from another client needs two schedulable workers")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "data06")
	const (
		holder   = "holder"
		acquirer = "acquirer"
	)
	f.MustPod(ctx, toolsPod(holder, pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod(acquirer, pvc.Name, nodeB))

	pv, err := f.PVForClaim(ctx, "data06")
	if err != nil {
		t.Fatalf("finding the volume behind the claim: %v", err)
	}
	requireServerSideLocking(ctx, t, f, pv.Name, framework.PosixLock, nodeA, nodeB)

	path := fileIn("data06.lock")
	// A range rather than the whole file, because a range is what this phase
	// adds and what the protocol carries natively. A whole-file fallback would
	// exercise the same wire operation but would not say the range travelled.
	rng := framework.WriteRange(0, 4096)

	held, err := f.HoldLock(ctx, holder, path, "data06", rng)
	if err != nil {
		t.Fatalf("taking %s on %s in %s: %v", rng, path, nodeA, err)
	}
	// Registered before the case can fail, so a failing case does not leave a
	// lock held on the share by a pod that outlives it.
	f.Defer(func(ctx context.Context) { _ = held.Release(ctx) })

	refused, err := f.TryLock(ctx, acquirer, path, rng)
	if err != nil {
		t.Fatalf("probing %s from %s: %v", rng, nodeB, err)
	}
	if refused.Free {
		t.Fatalf("mutual exclusion broken before any fault: %s on %s was granted %s while %s on %s held it",
			acquirer, nodeB, rng, holder, nodeA)
	}
	t.Logf("%s on %s is refused %s, which is held by %s", acquirer, nodeB, rng, refused.Conflict)
	if err := f.RecordNodeLocks(ctx, "before-force-delete", nodeA, nodeB); err != nil {
		t.Logf("recording the client lock tables before the fault: %v", err)
	}

	bound := slo.LockReleaseBound(profile(t))
	// The release and the unmount are two consequences of one kill, so the
	// re-acquire runs alongside the unmount wait rather than after it. Waiting
	// for the unmount first would charge the lock measurement for kubelet's
	// unmount, and the case would fail for a reason that is not lock release.
	type grant struct {
		at  time.Time
		err error
	}
	// Waited out past the bound on purpose: a case that gives up at the SLO
	// reports "timed out" where it could report how long the release took, and
	// the second is what a defect report needs.
	waitFor := bound + 5*time.Minute
	grants := make(chan grant, 1)
	start := time.Now()
	go func() {
		err := framework.Poll(ctx, framework.PollInterval, waitFor, func(ctx context.Context) (bool, error) {
			ans, err := f.TryLock(ctx, acquirer, path, rng)
			if err != nil {
				return false, err
			}
			return ans.Free, fmt.Errorf("%s is still held by %s", rng, ans.Conflict)
		})
		grants <- grant{at: time.Now(), err: err}
	}()

	if err := f.ForceDeletePodAndAwaitUnmount(ctx, holder, "data06"); err != nil {
		t.Fatalf("force-deleting %s and waiting for %s to release the mount: %v", holder, nodeA, err)
	}
	t.Logf("%s released the mount after the force delete, so teardown may touch the claim", nodeA)

	g := <-grants
	if g.err != nil {
		t.Fatalf("%s never became available to %s on %s in the %s after the holder was force-deleted: %v. "+
			"A lock nothing can release is worse than a slow one: the application that needs the range "+
			"never gets it, and no client is left to let go",
			rng, acquirer, nodeB, waitFor, g.err)
	}
	interval := g.at.Sub(start)
	if err := f.RecordNodeLocks(ctx, "after-release", nodeB); err != nil {
		t.Logf("recording the client lock tables after the release: %v", err)
	}

	p := profile(t)
	if interval > bound {
		t.Errorf("%s took %s to become available after its holder was force-deleted, above the %s lease "+
			"on the %s profile. One lease is the bound because the lease is the only thing that releases "+
			"state nothing closed; past it, nothing else is coming",
			rng, interval.Round(time.Second), bound, p.Name)
	}
	if interval <= slo.PromptLockRelease {
		t.Logf("%s became available %s after the force delete, which is the descriptor-close path: kubelet "+
			"killed the container, its descriptors closed and the client sent LOCKU. No lease expired, and "+
			"only this pod's locks moved. An application losing a lock this way is back in a second",
			rng, interval.Round(time.Millisecond))
	} else {
		t.Logf("%s became available %s after the force delete, near the %s lease rather than promptly: the "+
			"node did not notice the pod had gone and the lease expired instead. On a cluster behaving this "+
			"way, losing a node takes every pod's locks on it out together, for a lease, not one "+
			"application's for a second", rng, interval.Round(time.Second), p.Lease)
	}
}

// DATA-07: one file opened O_DIRECT by two pods on two nodes. Direct I/O
// bypasses the page cache on both ends, so a read that returns the other pod's
// bytes crossed the server, and one that returns zeros did not.
//
// The image has to be able to do this and may not be able to. busybox dd does
// carry iflag=direct and oflag=direct, but both live behind the
// FEATURE_DD_IBS_OBS build option, so whether a given image has them is a
// property of that image's build. The case probes rather than assuming, and
// reports blocked naming -tools-image if the flag does not parse: an image
// without it is not a storage defect and must not read as one.
//
// Steps:
//  1. Pin two pods to two nodes on one claim.
//  2. Probe oflag=direct on the share, and report blocked if it is absent.
//  3. Each pod writes a 4KiB block of its own pattern, direct, at its own
//     offset in one file.
//  4. Each pod reads the other's block back, direct, and compares checksums.
//  5. Assert each block holds what its writer wrote, so neither a stale cache
//     nor a lost write can read as a pass.
func TestDataDirectIOFromTwoPods(t *testing.T) {
	f := framework.New(t, "DATA-07")
	requireCap(t, f.Caps.MultiNode, "contrasting two clients' direct I/O needs two schedulable workers")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "data07")
	const podA, podB = "directa", "directb"
	f.MustPod(ctx, toolsPod(podA, pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod(podB, pvc.Name, nodeB))

	if probe := f.ProbeDirectIO(ctx, podA, fileIn("data07.probe")); !probe.OK {
		blocked(t, "this tools image's dd cannot open a file with O_DIRECT, so there is no direct I/O to "+
			"contrast: %q. busybox builds it behind FEATURE_DD_IBS_OBS. Pass -tools-image naming an image "+
			"whose dd carries oflag=direct", probe.Output)
	}

	path := fileIn("data07.dat")
	// 4KiB blocks at disjoint offsets in one file. Disjoint on purpose: two
	// clients writing one region with no coordination would report a mismatch
	// that is the harness's fault, which is DATA-01's finding and not this one.
	const block = 4096
	writers := []struct {
		pod, node, seed string
		offset          int
	}{
		{podA, nodeA, "data07-a", 0},
		{podB, nodeB, "data07-b", 1},
	}
	sums := make([]string, len(writers))
	for i, w := range writers {
		sum, err := f.WriteDirect(ctx, w.pod, path, w.seed, block, w.offset)
		if err != nil {
			t.Fatalf("%s on %s writing block %d with O_DIRECT: %v", w.pod, w.node, w.offset, err)
		}
		sums[i] = sum
	}

	// Each block is read by the pod that did not write it. Direct on both ends,
	// so the comparison crosses the server rather than either page cache.
	for i, w := range writers {
		reader := writers[(i+1)%len(writers)]
		got, err := f.ReadDirect(ctx, reader.pod, path, block, w.offset)
		if err != nil {
			t.Errorf("%s on %s reading block %d with O_DIRECT: %v", reader.pod, reader.node, w.offset, err)
			continue
		}
		if got != sums[i] {
			t.Errorf("%s on %s read block %d as %s, but %s on %s wrote %s. Both ends bypassed the page "+
				"cache, so this is what the server holds: either the write never landed or the read "+
				"returned something else",
				reader.pod, reader.node, w.offset, got, w.pod, w.node, sums[i])
		}
	}
	t.Logf("two clients wrote and read back disjoint 4KiB blocks of one file with O_DIRECT on both ends")
}

// DATA-08: the same export mounted twice, once with noac, and the visibility
// that buys. This is the other side of DATA-04: there a reader on another node
// may see nothing before the writer closes, and that is the protocol behaving
// as specified. Here, with attribute caching off, the reader must see it.
//
// Mount options belong to the volume and Kubernetes has no per-pod override, so
// the two mounts are two volumes over one export: the dynamically provisioned
// claim gives the export, and a static clone PV points at the same server and
// path with noac.
//
// The option is read back from /proc/mounts on the reader's node before
// anything is asserted on it. Without that, a driver or a node that dropped the
// option would turn this into a second, slower DATA-04 that passes whenever the
// timing is kind. A dropped option fails the case and names the node.
//
// Steps:
//  1. Provision a claim and read the export out of the bound volume, reporting
//     blocked if its server and path cannot be read.
//  2. Create a clone PV over that export with noac, and a claim bound to it.
//  3. Pin a writer to one node on the dynamic claim and a reader to another on
//     the clone.
//  4. Assert the reader's mount actually carries noac.
//  5. Write through a descriptor the writer holds open, and assert the reader
//     sees the bytes before the writer closes.
func TestDataNoacVisibilityWithoutClose(t *testing.T) {
	f := framework.New(t, "DATA-08")
	requireCap(t, f.Caps.MultiNode, "contrasting two mounts of one export needs two schedulable workers")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "data08")
	const writer, reader = "writer", "noacreader"
	f.MustPod(ctx, toolsPod(writer, pvc.Name, nodeA))

	pv, err := f.PVForClaim(ctx, "data08")
	if err != nil {
		t.Fatalf("finding the volume behind the claim: %v", err)
	}
	source, err := framework.ExtractNFSSource(pv)
	if err != nil {
		blocked(t, "%v", err)
	}
	t.Logf("the dynamic claim is backed by %s; cloning it with noac", source)

	// vers=4.1 and hard to match what preflight pinned, and noac, which is the
	// whole point of the second mount.
	if _, _, err := f.CloneVolume(ctx, framework.CloneVolumeSpec{
		Name: "data08noac", Source: source, Options: []string{"vers=4.1", "hard", "noac"},
	}); err != nil {
		t.Fatalf("creating the noac clone over %s: %v", source, err)
	}
	f.MustPod(ctx, toolsPod(reader, "data08noac", nodeB))

	// Read back, not assumed. noac is what this case is measuring, and a mount
	// that does not carry it measures nothing.
	m, err := f.PodVolumeMount(ctx, nodeB, f.Name("data08noac"))
	if err != nil {
		t.Fatalf("reading the reader's mount on %s: %v", nodeB, err)
	}
	t.Logf("the reader's mount on %s carries %s", nodeB, m.Options)
	if !m.HasOption("noac") {
		t.Fatalf("the clone was created with noac and the mount on %s does not carry it (%s). Without "+
			"the option this case is a second, slower DATA-04 that passes whenever the timing is kind, "+
			"so it fails here rather than asserting on a mount it did not get",
			nodeB, m.Options)
	}

	path := fileIn("data08.txt")
	readerPath := "/mnt/share/data08.txt"
	// Well under a page, so a partial flush cannot produce a file whose size is
	// final while an earlier region still reads as zeros.
	const payload = "data08-visible-without-a-close"
	w, err := f.HoldOpenWrite(ctx, writer, path, payload, "data08")
	if err != nil {
		t.Fatalf("holding %s open on %s: %v", path, nodeA, err)
	}
	f.Defer(func(ctx context.Context) { _ = w.Close(ctx) })

	// The bound is short on purpose. noac turns off attribute caching, so the
	// reader revalidates on every access; a deployment that needs tens of
	// seconds here has not delivered what the option promises, even though the
	// bytes arrive eventually.
	var got string
	err = framework.Poll(ctx, framework.FastPoll, 30*time.Second, func(ctx context.Context) (bool, error) {
		got = f.MustShf(ctx, reader, "cat %s 2>/dev/null || true", framework.Quote(readerPath))
		return got == payload, fmt.Errorf("the noac reader on %s sees %q", nodeB, got)
	})
	if err != nil {
		t.Errorf("the reader on %s never saw the writer's bytes before the close, through a mount "+
			"carrying noac: it sees %q, want %q. That is the documented boundary on an ordinary mount "+
			"(DATA-04) and is what noac exists to remove: %v", nodeB, got, payload, err)
		return
	}
	t.Logf("the noac reader on %s saw the writer's bytes before the writer closed, which an ordinary "+
		"mount does not promise", nodeB)
}

// DATA-09: rename, unlink and re-create while another pod holds the file open.
// Two halves, asserting different things, because silly rename is a property of
// one client rather than of the protocol.
//
// The Linux client implements unlink of an open file by renaming it to
// .nfsXXXXXXXX and removing it when the last descriptor closes. That can only
// happen when the client doing the unlinking is the client holding the file
// open, and the client is the node. When another node unlinks it, the server
// removes it and the holder's next operation gets ESTALE. Both outcomes are
// correct, and a case that ran the cross-node shape while asserting the
// same-node expectation would file an NFS-conformant client as a defect.
//
// Steps:
//  1. Same node: pod A holds a descriptor; pod B on the same node unlinks the
//     file. Assert a .nfs* entry appears, that A's descriptor still reads what
//     was written, and that the entry is gone once A closes.
//  2. Cross node: pod A holds a descriptor; pod C on another node unlinks and
//     re-creates the name with different content. Record whether A sees the old
//     file or ESTALE, and fail only if A reads the new file's content, which
//     would mean a file handle was reused.
//  3. Rename, in both shapes: a descriptor must follow the file, not the name.
func TestDataSillyRenameAndOpenDescriptors(t *testing.T) {
	f := framework.New(t, "DATA-09")
	requireCap(t, f.Caps.MultiNode, "the cross-node half needs two schedulable workers")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "data09")
	const (
		holder   = "holder"
		sameNode = "samenode"
		otherOne = "othernode"
	)
	f.MustPod(ctx, toolsPod(holder, pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod(sameNode, pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod(otherOne, pvc.Name, nodeB))

	dir := fileIn("data09")
	f.MustShf(ctx, holder, "mkdir -p %s", framework.Quote(dir))

	t.Run("same-node-unlink-silly-renames", func(t *testing.T) {
		path := dir + "/same-node.dat"
		first, second := writeTwoChunks(ctx, t, f, holder, path)

		r, err := f.HoldOpenRead(ctx, holder, path, chunkBytes, "data09same")
		if err != nil {
			t.Fatalf("holding %s open on %s: %v", path, nodeA, err)
		}
		f.Defer(func(ctx context.Context) { _ = r.Close(ctx) })
		if got := mustReadChunk(ctx, t, r); got != first {
			t.Fatalf("the first read through the held descriptor returned %q, want %q", got, first)
		}

		f.MustShf(ctx, sameNode, "rm -f %s", framework.Quote(path))

		// The silly-rename entry is the client keeping the file alive for the
		// descriptor. Polled, because the rename is the client's own work.
		var silly []string
		if err := framework.Poll(ctx, framework.FastPoll, 30*time.Second, func(ctx context.Context) (bool, error) {
			var err error
			if silly, err = f.SillyRenames(ctx, holder, dir); err != nil {
				return false, err
			}
			return len(silly) > 0, fmt.Errorf("no .nfs* entry under %s yet", dir)
		}); err != nil {
			t.Errorf("a pod on %s unlinked a file another pod on the same node held open, and no .nfs* "+
				"entry appeared under %s. The Linux client renames rather than removing so the open "+
				"descriptor keeps working; without it the descriptor is the thing to check next: %v",
				nodeA, dir, err)
		} else {
			t.Logf("the client on %s kept the file as %v while the descriptor was open", nodeA, silly)
		}

		if got := mustReadChunk(ctx, t, r); got != second {
			t.Errorf("the read after the same-node unlink returned %q, want %q: the descriptor stopped "+
				"working, which is what silly rename exists to prevent", got, second)
		}

		if err := r.Close(ctx); err != nil {
			t.Fatalf("closing the descriptor: %v", err)
		}
		// The leftover check. A .nfs* file outliving its holder is a leak: it
		// consumes space nobody can find and nobody will remove.
		if err := framework.Poll(ctx, framework.PollInterval, time.Minute, func(ctx context.Context) (bool, error) {
			left, err := f.SillyRenames(ctx, holder, dir)
			if err != nil {
				return false, err
			}
			return len(left) == 0, fmt.Errorf("%v is still there", left)
		}); err != nil {
			t.Errorf("a .nfs* entry under %s outlived the descriptor that caused it: %v. That is a leak: "+
				"the space is held by a file with no name anyone will look for", dir, err)
		}
	})

	t.Run("cross-node-unlink-and-recreate", func(t *testing.T) {
		path := dir + "/cross-node.dat"
		first, second := writeTwoChunks(ctx, t, f, holder, path)

		r, err := f.HoldOpenRead(ctx, holder, path, chunkBytes, "data09cross")
		if err != nil {
			t.Fatalf("holding %s open on %s: %v", path, nodeA, err)
		}
		f.Defer(func(ctx context.Context) { _ = r.Close(ctx) })
		if got := mustReadChunk(ctx, t, r); got != first {
			t.Fatalf("the first read through the held descriptor returned %q, want %q", got, first)
		}

		// Another node unlinks and re-creates the name, with content the holder
		// must never see through its old descriptor.
		replacement := strings.Repeat("R", chunkBytes*2)
		f.MustShf(ctx, otherOne, "rm -f %s && printf %%s %s > %s",
			framework.Quote(path), framework.Quote(replacement), framework.Quote(path))

		got := mustReadChunk(ctx, t, r)
		switch {
		case got == second:
			t.Logf("after a pod on %s unlinked and re-created the name, the descriptor held on %s still "+
				"reads the old file. Both that and ESTALE are correct here: the unlinking client is not "+
				"the one holding it open, so there is no silly rename to protect it", nodeB, nodeA)
		case strings.HasPrefix(got, framework.ReadFailedPrefix):
			t.Logf("after a pod on %s unlinked the file, the descriptor held on %s failed, which is the "+
				"other correct answer and is how an application finds out: %s", nodeB, nodeA, got)
		case strings.HasPrefix(got, "R"):
			// The only failure in this half, and a serious one.
			t.Errorf("the descriptor held on %s read the *new* file's content (%q) after a pod on %s "+
				"unlinked and re-created the name. A descriptor must refer to the file it was opened on; "+
				"reading the replacement means a file handle was reused", nodeA, got, nodeB)
		default:
			t.Errorf("the descriptor held on %s returned %q, which is neither the old file, nor the new "+
				"one, nor an error", nodeA, got)
		}
	})

	t.Run("rename-does-not-move-a-descriptor", func(t *testing.T) {
		// A rename is not an unlink. The descriptor stays valid through it on
		// any client, and one that followed the name rather than the file would
		// be a failure in either shape.
		path := dir + "/renamed.dat"
		first, second := writeTwoChunks(ctx, t, f, holder, path)
		moved := dir + "/renamed-elsewhere.dat"

		r, err := f.HoldOpenRead(ctx, holder, path, chunkBytes, "data09rename")
		if err != nil {
			t.Fatalf("holding %s open on %s: %v", path, nodeA, err)
		}
		f.Defer(func(ctx context.Context) { _ = r.Close(ctx) })
		if got := mustReadChunk(ctx, t, r); got != first {
			t.Fatalf("the first read through the held descriptor returned %q, want %q", got, first)
		}

		f.MustShf(ctx, otherOne, "mv %s %s", framework.Quote(path), framework.Quote(moved))
		// A different file at the old name, so a descriptor following the name
		// fails the comparison rather than passing by coincidence.
		decoy := strings.Repeat("D", chunkBytes*2)
		f.MustShf(ctx, otherOne, "printf %%s %s > %s", framework.Quote(decoy), framework.Quote(path))

		if got := mustReadChunk(ctx, t, r); got != second {
			t.Errorf("after the file was renamed out from under it, the descriptor read %q, want %q. "+
				"A rename moves a name, not a file, and a descriptor that follows the name is reading "+
				"whatever happens to be there now", got, second)
		}
	})
}

// chunkBytes is how much of a file one read through a held descriptor returns.
// Small and printable, because the assertion is on identity rather than on
// volume, and the chunk travels back through an exec's stdout.
const chunkBytes = 16

// writeTwoChunks writes a file of two distinguishable chunks and returns them,
// so that a read after a rename or an unlink has a second chunk to be correct
// about rather than repeating the first.
func writeTwoChunks(ctx context.Context, t *testing.T, f *framework.Framework, pod, path string) (string, string) {
	t.Helper()
	first := strings.Repeat("A", chunkBytes)
	second := strings.Repeat("B", chunkBytes)
	f.MustShf(ctx, pod, "printf %%s %s > %s", framework.Quote(first+second), framework.Quote(path))
	return first, second
}

// mustReadChunk reads the next chunk through a held descriptor.
func mustReadChunk(ctx context.Context, t *testing.T, r *framework.OpenReader) string {
	t.Helper()
	got, err := r.Next(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("reading through the held descriptor: %v", err)
	}
	return got
}

// dirEntries is how many entries DATA-10 creates. Fixed, not configurable: it
// is a property of the measurement, and the plan states it. Section 3.2.
const dirEntries = 100000

// DATA-10: a large directory listed while another pod deletes from it. The
// listing may miss entries. It may never return a name that was never created.
//
// The defect this is derived from is a use-after-free on directory chunk reuse
// during READDIR under cache pressure. From a client that surfaces as a listing
// returning a name that is not in the directory: a fragment of a reused page
// decoded as an entry. It does not surface as a count being off, because a
// listing racing deletes is supposed to have a count that is off, so asserting
// one would fail on lawful behaviour.
//
// Steps:
//  1. Check free space and inodes before starting, and report blocked if the
//     export cannot hold the set. Failing on ENOSPC halfway through would point
//     at the server for a sizing problem.
//  2. Populate the directory from one pod, split across shells inside it.
//  3. Start a second pod deleting the back half.
//  4. List the directory from a third pod, streamed, while the deletes run.
//  5. Assert the listing completed, and that every name it returned is one the
//     populate step created.
//  6. Assert the server pod did not restart, and that no node logged an oops or
//     an NFS error over the window. The counts go in the bundle.
func TestDataLargeDirectoryReaddirUnderDeletes(t *testing.T) {
	f := framework.New(t, "DATA-10")
	requireCap(t, f.Caps.MultiNode, "listing from a pod that is not deleting needs two schedulable workers")
	ctx, cancel := caseCtx(t, 40*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "data10")
	const (
		populator = "populator"
		deleter   = "deleter"
		lister    = "lister"
	)
	f.MustPod(ctx, toolsPod(populator, pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod(deleter, pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod(lister, pvc.Name, nodeB))

	dir := fileIn("data10")
	// Checked before starting rather than discovered during. An hour of
	// population that dies on ENOSPC teaches nothing, and the failure would
	// name the server for what is a sizing problem.
	space, err := f.MountSpace(ctx, populator, mountPath)
	if err != nil {
		t.Fatalf("reading the export's free space: %v", err)
	}
	if space.InodesKnown && space.FreeInodes < int64(dirEntries) {
		blocked(t, "the export reports %d free inodes and this case needs %d. The claim's size is not "+
			"the limit here: an export may be directory-backed with no per-volume quota, and the real "+
			"limit is the backing filesystem's inode table", space.FreeInodes, dirEntries)
	}
	t.Logf("the export reports %d bytes free and %d free inodes (known %v) before %d entries",
		space.FreeBytes, space.FreeInodes, space.InodesKnown, dirEntries)

	restartsBefore := serverRestarts(ctx, t, f)
	kernelBefore := readKernelLog(ctx, t, f, nodeA, nodeB)

	created, err := f.PopulateDir(ctx, populator, dir, dirEntries, 16, 30*time.Minute)
	if err != nil {
		t.Fatalf("populating %s: %v", dir, err)
	}
	if created != dirEntries {
		t.Fatalf("the populate step created %d of %d entries, so the case would assert against entries "+
			"that were never there", created, dirEntries)
	}
	t.Logf("%d entries created under %s from %s on %s", created, dir, populator, nodeA)

	// The back half, so the listing is racing deletes over a region it has not
	// necessarily reached yet.
	del, err := f.StartDeletingEntries(ctx, deleter, dir, dirEntries/2+1, dirEntries, "data10")
	if err != nil {
		t.Fatalf("starting the deleter: %v", err)
	}

	listing, listErr := f.ListDirEntries(ctx, lister, dir, 20*time.Minute)
	listed, unknown := framework.ClassifyEntries(dir, created, listing)
	removed, delErr := del.Removed(ctx, 20*time.Minute)
	if delErr != nil {
		t.Errorf("the deleter never finished: %v", delErr)
	}

	census := framework.DirCensus{Created: created, Listed: listed, Deleted: removed, Unknown: unknown}
	if err := f.WriteArtifact("directory-census.txt", []byte(census.String()+"\n"+
		strings.Join(unknown, "\n")+"\n")); err != nil {
		t.Logf("writing the directory census: %v", err)
	}
	t.Logf("directory census: %s", census)

	if listErr != nil {
		t.Errorf("the listing of %s did not complete while %s was deleting from it: %v",
			dir, deleter, listErr)
	}
	// The assertion. A name nobody created is a directory chunk read after it
	// was reused.
	if len(unknown) > 0 {
		show := unknown
		if len(show) > 10 {
			show = show[:10]
		}
		t.Errorf("the listing returned %d names the populate step never created: %q. A listing racing "+
			"deletes is allowed to miss entries and is not allowed to invent one; a name that was never "+
			"there is a directory chunk decoded after it was reused, which is the defect this case exists "+
			"for", len(unknown), show)
	}
	// A count below what was created is lawful and is recorded, not asserted.
	if listed < created {
		t.Logf("the listing returned %d of %d entries, which is lawful: it was racing %d deletions",
			listed, created, removed)
	}

	if after := serverRestarts(ctx, t, f); after != restartsBefore {
		t.Errorf("the server pod restarted %d times during the listing (was %d): a readdir over a large "+
			"directory must not be able to take the server down", after-restartsBefore, restartsBefore)
	}
	assertNoNewKernelErrors(ctx, t, f, kernelBefore, nodeA, nodeB)
}

// DATA-11: a sparse file written, read back, and a hole punch that this mount
// cannot perform. What the case fails on is narrower than the plan's one-line
// row, and the row was amended to say so.
//
// Hole punching is unavailable twice over here. NFSv4.1 has no operation for
// it: ALLOCATE, DEALLOCATE and READ_PLUS arrived with NFSv4.2 in RFC 7862, and
// preflight pins vers=4.1. And the busybox fallocate applet parses -l and -o
// only, so on a stock image the flag is rejected by the tool whatever the mount
// underneath it is.
//
// Those two refusals are not the same answer and are not reported as one. An
// applet that cannot parse -p reports blocked, naming -tools-image, because
// nothing about the protocol has been learned. Only a -p that parses and is
// then refused is recorded as the protocol saying no. Without the split, every
// run on a busybox image would file "NFSv4.1 does not support hole punching" on
// evidence that is really "this image's applet has no -p".
//
// The 4.2 branch is written and unreachable until preflight accepts 4.2. It is
// written anyway: the alternative is a case that has to be rediscovered and
// rewritten on the day the mount changes.
//
// Steps:
//  1. Write a byte at the start of a file and another past a hole.
//  2. Assert the hole reads back as zeros and the far byte is at its offset.
//  3. Assert the logical size is the full length; record allocated blocks
//     without asserting, since how the backing filesystem stores a hole is not
//     an NFS property.
//  4. Probe whether the image's fallocate parses -p at all.
//  5. Attempt the punch. A refusal is recorded as unsupported. Success is
//     asserted: the region must read as zeros and the size must not change.
func TestDataSparseFileAndHolePunch(t *testing.T) {
	f := framework.New(t, "DATA-11")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodes, err := f.WorkerNodes(ctx)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("listing worker nodes: %v", err)
	}
	pvc := f.MustRWXPVC(ctx, "data11")
	const pod = "sparse"
	f.MustPod(ctx, toolsPod(pod, pvc.Name, nodes[0]))

	path := fileIn("data11.dat")
	// One block at the start, a hole, and one block past it. The offsets are
	// block-aligned so that a punch, where one is possible, has a whole block
	// to work on rather than a partial one the filesystem may only zero.
	const (
		block     = 4096
		holeBlock = 1
		farBlock  = 8
		totalSize = (farBlock + 1) * block
	)
	f.MustShf(ctx, pod, "rm -f %[1]s; dd if=/dev/zero of=%[1]s bs=%[2]d count=1 2>/dev/null; "+
		"yes data11-far | head -c %[2]d | dd of=%[1]s bs=%[2]d seek=%[3]d conv=notrunc 2>/dev/null",
		framework.Quote(path), block, farBlock)

	if got := f.MustShf(ctx, pod, "stat -c %%s %s", framework.Quote(path)); got != fmt.Sprint(totalSize) {
		t.Errorf("the sparse file reports a logical size of %s, want %d: a hole is part of the file's "+
			"length whether or not it is stored", got, totalSize)
	}
	// Recorded, not asserted. Whether the backing filesystem stores the hole
	// sparsely is a property of that filesystem, not of NFS.
	t.Logf("the sparse file reports %s allocated blocks for %d logical bytes",
		f.MustShf(ctx, pod, "stat -c %%b %s", framework.Quote(path)), totalSize)

	if nonZero := readNonZeroBytes(ctx, t, f, pod, path, block, holeBlock); nonZero != 0 {
		t.Errorf("the hole at block %d holds %d bytes that are not zero: a region nobody wrote must "+
			"read as zeros", holeBlock, nonZero)
	}
	if got := f.MustShf(ctx, pod, "dd if=%s bs=%d count=1 skip=%d 2>/dev/null | head -c 11",
		framework.Quote(path), block, farBlock); got != "data11-far" {
		t.Errorf("the byte written past the hole reads back as %q at block %d: a sparse write put the "+
			"data at the wrong offset", got, farBlock)
	}

	probe := f.ProbeHolePunch(ctx, pod)
	if probe.Missing {
		blocked(t, "this tools image's fallocate does not parse -p, so the punch was never requested and "+
			"nothing about the protocol has been learned: %q. The busybox applet parses -l and -o only. "+
			"Pass -tools-image naming an image whose fallocate carries -p", probe.Output)
	}

	punch := f.Sh(ctx, pod, fmt.Sprintf("fallocate -p -o %d -l %d %s 2>&1",
		holeBlock*block, block, framework.Quote(path)))
	if punch.Err != nil {
		// Rule 9: an operation the protocol does not define is recorded, not
		// failed. On a 4.1 mount this is the expected branch, and it is the
		// refusal of the operation rather than of the flag, because the probe
		// above established that -p parses.
		t.Logf("the hole punch was refused on this %s mount, which is the documented answer: NFSv4.1 "+
			"carries no DEALLOCATE, and ALLOCATE, DEALLOCATE and READ_PLUS arrived with NFSv4.2 in "+
			"RFC 7862. Recorded as unsupported, not failed: %s",
			f.Env.NFSVersion, strings.TrimSpace(punch.Combined()))
		return
	}

	// The punch reported success, so it is asserted. This is the branch that
	// becomes reachable the day preflight accepts 4.2, and the case does not
	// need rewriting for it.
	t.Logf("the hole punch succeeded on this %s mount", f.Env.NFSVersion)
	if nonZero := readNonZeroBytes(ctx, t, f, pod, path, block, holeBlock); nonZero != 0 {
		t.Errorf("fallocate -p reported success and the punched region still holds %d bytes that are "+
			"not zero. A punch that reports success and does not zero is worse than one that refuses: "+
			"an application told the data is gone can still read it", nonZero)
	}
	if got := f.MustShf(ctx, pod, "stat -c %%s %s", framework.Quote(path)); got != fmt.Sprint(totalSize) {
		t.Errorf("the file reports a logical size of %s after the punch, want %d: punching a hole "+
			"removes storage, not length", got, totalSize)
	}
}

// readNonZeroBytes returns how many bytes of one block are not zero, which is
// how a region nobody wrote is checked without moving a block through exec.
func readNonZeroBytes(ctx context.Context, t *testing.T, f *framework.Framework, pod, path string, block, index int) int {
	t.Helper()
	out := f.MustShf(ctx, pod, "dd if=%s bs=%d count=1 skip=%d 2>/dev/null | tr -d '\\000' | wc -c",
		framework.Quote(path), block, index)
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("counting the non-zero bytes of block %d: the pod said %q", index, out)
	}
	return n
}

// serverRestarts totals the restart counts of the NFS server pods, which is how
// a case says the server did not fall over under what it was doing. A cluster
// where the server was never discovered reports zero and the case's own
// assertion becomes a comparison of two zeros, which is why it is logged.
func serverRestarts(ctx context.Context, t *testing.T, f *framework.Framework) int32 {
	t.Helper()
	ns := f.Env.ServerNamespace()
	if ns == "" {
		t.Logf("the NFS server pods were never discovered, so a restart of one cannot be noticed here")
		return 0
	}
	pods, err := f.C.Kube.CoreV1().Pods(ns).List(ctx, framework.ListOptions(framework.Cfg().ServerSelector))
	if err != nil {
		t.Logf("reading the server pods' restart counts: %v", err)
		return 0
	}
	var total int32
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			total += cs.RestartCount
		}
	}
	return total
}

// kernelErrorMarkers are the lines worth failing a case over. A use-after-free
// that did not corrupt a listing still corrupts memory, and the kernel says so
// where no assertion on file content would see it.
var kernelErrorMarkers = []string{"oops", "kernel bug", "use-after-free", "kasan", "nfs: server"}

// readKernelLog captures each node's ring buffer, for comparison afterwards.
//
// A window, rather than the whole buffer, because these nodes have been running
// the rest of the suite: an NFS error logged by a chaos case an hour ago is not
// this case's finding. Comparing two reads is the honest way to get a window
// without parsing dmesg timestamps, whose format varies by node image.
func readKernelLog(ctx context.Context, t *testing.T, f *framework.Framework, nodes ...string) map[string]map[string]bool {
	t.Helper()
	seen := map[string]map[string]bool{}
	agent, err := framework.NodeAgent(ctx, f.C)
	if err != nil {
		t.Logf("the node agent is unavailable, so the kernel ring buffers were not read: %v", err)
		return seen
	}
	for _, node := range nodes {
		out, err := agent.Dmesg(ctx, node)
		if err != nil {
			t.Logf("reading dmesg on %s: %v", node, err)
			continue
		}
		lines := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			lines[strings.TrimSpace(line)] = true
		}
		seen[node] = lines
	}
	return seen
}

// assertNoNewKernelErrors fails on an oops or an NFS client error that was not
// already in the ring buffer when the case started.
//
// A node with no baseline is not silently passed: it is logged, because a check
// nobody performed must not read as a check that found nothing.
func assertNoNewKernelErrors(ctx context.Context, t *testing.T, f *framework.Framework,
	before map[string]map[string]bool, nodes ...string) {
	t.Helper()
	agent, err := framework.NodeAgent(ctx, f.C)
	if err != nil {
		t.Logf("the node agent is unavailable, so the kernel ring buffers were not compared: %v", err)
		return
	}
	for _, node := range nodes {
		baseline, ok := before[node]
		if !ok {
			t.Logf("no ring buffer was captured on %s before this case, so nothing about its kernel "+
				"is being asserted here", node)
			continue
		}
		out, err := agent.Dmesg(ctx, node)
		if err != nil {
			t.Logf("reading dmesg on %s: %v", node, err)
			continue
		}
		if err := f.WriteArtifact("dmesg-"+node+".txt", []byte(out)); err != nil {
			t.Logf("writing the ring buffer of %s: %v", node, err)
		}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || baseline[line] {
				continue
			}
			low := strings.ToLower(line)
			for _, marker := range kernelErrorMarkers {
				if strings.Contains(low, marker) {
					t.Errorf("the kernel on %s logged %q during this case, and it was not there before. "+
						"A directory chunk read after it was reused corrupts memory whether or not it "+
						"corrupted the listing, and that shows up here and nowhere else", node, line)
					break
				}
			}
		}
	}
}

// DATA-12: fsync and COMMIT durability. Write records with conv=fsync, SIGKILL
// the server under the load, and assert every record the server acknowledged
// before the fault is still there and still says what it said.
//
// It asserts durability and nothing else. CHAOS-01 owns the recovery number for
// this same fault, and two cases reporting it is two numbers to reconcile when
// they disagree. This waits for recovery because it has to read the share
// afterwards, and does not assert the SLO.
//
// It is a TestData case, not a TestChaos one, even though it kills the server.
// The suite sorts strictly by category, and the precedent is already in the
// tree: PROV-07 and PROV-08 take the server down under TestProv, OBS-02 and
// OBS-03 inject a failover under TestObs. After this case `make test-data`
// injects faults, which a reader of the Makefile should not have to infer.
//
// Steps:
//  1. Start a record workload that commits every record, and let it run.
//  2. SIGKILL the server process on its node.
//  3. Wait for I/O to resume, without asserting the recovery SLO.
//  4. Sweep every record committed before the fault, from a pod on the other
//     node, by content.
//  5. Fail on any verdict but correct: absent is data loss, short is a record
//     the server acknowledged and then truncated, wrong is corruption.
func TestDataFsyncDurabilityAcrossServerKill(t *testing.T) {
	f := framework.New(t, "DATA-12")
	requireCap(t, f.Caps.NodeAgent, "signalling the server process on a node needs the privileged node agent")
	ctx, cancel := caseCtx(t, 45*time.Minute)
	defer cancel()

	s := startChaosCase(ctx, t, f, "data12")

	faultAt, err := f.PodNow(ctx, s.writer)
	if err != nil {
		t.Fatalf("reading the writer's clock: %v", err)
	}
	before, err := s.load.Report(ctx)
	if err != nil {
		t.Fatalf("reading the workload log: %v", err)
	}
	committed := before.CommittedBefore(faultAt)
	if len(committed) == 0 {
		t.Fatalf("the workload committed nothing before the fault, so this case would assert over an " +
			"empty set and pass having checked nothing")
	}

	killed, err := chaos.KillServerProcess(ctx, f, s.target, "KILL")
	if err != nil {
		// Every path out means the fault was not injected. Asserting durability
		// across a crash that never happened would pass for the wrong reason.
		t.Skipf("blocked: %v", err)
	}
	t.Logf("SIGKILLed %d process(es) in %s on %s with %d records committed",
		killed, s.target.Pod, s.target.Node, len(committed))

	// Waited out, not asserted. CHAOS-01 owns this number for this fault.
	waitRecovered(ctx, t, s, faultAt)
	final, err := s.load.Stop(ctx)
	if err != nil {
		t.Fatalf("stopping the workload: %v", err)
	}
	if errs := final.Errors(); len(errs) > slo.MaxIOErrors {
		t.Errorf("the workload saw %d I/O errors across the kill, want %d: a hard NFSv4.1 mount is "+
			"specified to block and retry, not to return an error (first at index %d)",
			len(errs), slo.MaxIOErrors, errs[0].Index)
	}

	assertCommittedRecordsIntact(ctx, t, f, s.verifier, s.dir, committed)
}

// DATA-13: the negative of DATA-12, and the reason it is worth having. The
// records are written without an fsync, so nothing the server acknowledged is
// at stake and data that was never committed may be absent. That is never a
// failure. What still fails is a byte that differs at an offset that was
// written.
//
// Three things this case deliberately does not do.
//
// It does not assert that data was lost. It very often will not be: a SIGKILL
// of the server *process* does not drop the host page cache underneath it, so
// unstable writes the server had not yet committed to disk may well still be
// there when it restarts. The plan says "may be absent, asserted as acceptable,
// documented", and documented is the whole job.
//
// It does not call a short record corruption. Without an fsync the client is
// free to have flushed a prefix, and a prefix is lawful. A wrong byte inside
// the prefix is not, and that is the line the sweep draws.
//
// It does not measure recovery. CHAOS-01 owns that number.
//
// Steps:
//  1. Start a record workload with no fsync, and let it run.
//  2. SIGKILL the server process on its node.
//  3. Wait for I/O to resume.
//  4. Sweep every record the workload attempted, from a pod on the other node.
//  5. Record how many were absent or short, and fail only on wrong.
func TestDataDurabilityWithoutFsync(t *testing.T) {
	f := framework.New(t, "DATA-13")
	requireCap(t, f.Caps.NodeAgent, "signalling the server process on a node needs the privileged node agent")
	ctx, cancel := caseCtx(t, 45*time.Minute)
	defer cancel()

	s := startChaosCaseWith(ctx, t, f, "data13", framework.WriteLoadSpec{NoFsync: true})

	faultAt, err := f.PodNow(ctx, s.writer)
	if err != nil {
		t.Fatalf("reading the writer's clock: %v", err)
	}
	before, err := s.load.Report(ctx)
	if err != nil {
		t.Fatalf("reading the workload log: %v", err)
	}
	// Every record it tried, not the ones it logged as succeeding. Without an
	// fsync a logged success means only that the client accepted the write, so
	// a sweep restricted to that set would miss a record that reached the share
	// by a route nothing asserted on.
	attempted := before.Attempted()
	if len(attempted) == 0 {
		t.Fatalf("the workload attempted nothing before the fault, so this case would sweep an empty " +
			"set and pass having checked nothing")
	}

	killed, err := chaos.KillServerProcess(ctx, f, s.target, "KILL")
	if err != nil {
		t.Skipf("blocked: %v", err)
	}
	t.Logf("SIGKILLed %d process(es) in %s on %s with %d un-fsynced records attempted",
		killed, s.target.Pod, s.target.Node, len(attempted))

	waitRecovered(ctx, t, s, faultAt)
	if _, err := s.load.Stop(ctx); err != nil {
		t.Fatalf("stopping the workload: %v", err)
	}

	sweep, err := f.VerifyRecords(ctx, s.verifier, s.dir, attempted)
	if err != nil {
		t.Fatalf("sweeping the records from %s: %v", s.verifier, err)
	}
	recordSweep(t, f, sweep)
	if len(sweep.Unparsed) > 0 {
		t.Errorf("the sweep produced %d lines the harness could not read, so the verdicts below cannot "+
			"be trusted: %q", len(sweep.Unparsed), sweep.Unparsed[0])
	}

	// The only failure in this case. A byte that differs at an offset that was
	// written is corruption under every reading of the protocol, fsync or no
	// fsync, and it is why this case exists rather than being folded into a log
	// line of DATA-12's.
	if wrong, ok := sweep.FirstWrong(); ok {
		t.Errorf("%d of %d records hold a byte that differs from what was written, the first in record "+
			"%d at offset %d. Losing an un-fsynced write is lawful and is not what this reports: a "+
			"record that is present and says something else is corruption, and the absence of an fsync "+
			"does not licence it",
			sweep.Count(framework.VerdictWrong), len(attempted), wrong.Index, wrong.Offset)
	}

	// Documented, not asserted. This is the whole job of the case.
	absent, short := sweep.Count(framework.VerdictAbsent), sweep.Count(framework.VerdictShort)
	t.Logf("of %d records written without an fsync across a server process kill: %d correct, %d absent, "+
		"%d a correct prefix. All three are acceptable. A SIGKILL of the server process does not drop "+
		"the host page cache underneath it, so a run where nothing was lost is the ordinary result and "+
		"says nothing is wrong",
		len(attempted), sweep.Count(framework.VerdictCorrect), absent, short)
}

// The soak's shape. Every one of these is fixed rather than a flag: each is a
// property of the measurement and each is stated in Section 3.2 of the plan.
// The one a real run may argue with is the hour, and the design carries that as
// an open question rather than pre-emptively adding a flag for it.
const (
	soakPods        = 20
	soakRuntime     = time.Hour
	soakFilesPerPod = 4
	soakSizeRange   = "4k-1g"
	soakMaxFileSize = 1 << 30
	soakReadPercent = 70
)

// DATA-14: a mixed read/write soak across twenty pods for an hour, verified by
// checksum. The expected result is zero mismatches.
//
// Each pod owns a directory. Two pods writing one file with verification on
// would report mismatches that are the harness's fault rather than the
// storage's, which DATA-01 already established; cross-pod interference is
// DATA-01's and SCALE-06's job.
//
// verify_fatal stops a job at the mismatch, so the offending offset is in the
// output. Without it fio continues and the report is a count with no location.
//
// Throughput is recorded and asserted on by nobody. A performance bound here
// would be a SCALE case wearing a DATA number.
//
// This case has its own target, `make test-data-soak`. An hour does not fit
// inside `make test-data`, which is budgeted at under 45 minutes, and a target
// that cannot execute its own documented contents is worse than one that does
// less. Section 5.14 of the design has the alternatives that were weighed.
//
// Steps:
//  1. Skip without -fio-image. The case cannot run, and no image is assumed:
//     pulling one nobody named is a supply chain the operator did not agree to.
//  2. Size a claim from the job set and report blocked if the export cannot
//     hold it. An hour of soak that dies on ENOSPC at minute fifty is an hour
//     spent to learn nothing.
//  3. Start one fio job per pod, each in its own directory, spread round robin
//     across the worker nodes.
//  4. Wait the hour out and read each job's JSON report.
//  5. Assert no job ended with an error, which under verify_fatal is how a
//     checksum mismatch surfaces. Record the bytes moved.
func TestDataMixedSoak(t *testing.T) {
	f := framework.New(t, "DATA-14")
	image := framework.Cfg().FioImage
	if image == "" {
		t.Skip("no -fio-image was given, and this case needs fio. No image is assumed: pulling one " +
			"nobody named is a supply chain the operator did not agree to")
	}
	// The hour, plus room for twenty pods to schedule and pull an image, plus
	// the reports.
	ctx, cancel := caseCtx(t, soakRuntime+40*time.Minute)
	defer cancel()

	nodes, err := f.WorkerNodes(ctx)
	if err != nil {
		t.Fatalf("listing worker nodes: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatalf("no schedulable worker nodes")
	}

	// The worst case, not the average: fio picks each file's size at random
	// within the range, so a claim sized for the mean fills up on an unlucky
	// draw halfway through the hour.
	required := int64(soakPods) * int64(soakFilesPerPod) * int64(soakMaxFileSize)
	size := fmt.Sprintf("%dGi", (required>>30)+1)
	t.Logf("%d pods x %d files x up to %d bytes needs %d bytes at worst; claiming %s",
		soakPods, soakFilesPerPod, soakMaxFileSize, required, size)

	pvc, err := f.CreatePVC(ctx, framework.PVCSpec{Name: "data14", Size: size})
	if err != nil {
		t.Fatalf("creating the soak claim at %s: %v", size, err)
	}
	// The first pod both binds a WaitForFirstConsumer class and is where the
	// capacity is read from, since df inside a pod is what the workload sees.
	pods := make([]string, soakPods)
	for i := range pods {
		pods[i] = fmt.Sprintf("soak%d", i)
		f.MustPod(ctx, fioPod(pods[i], pvc.Name, nodes[i%len(nodes)], image))
	}
	t.Logf("%d soak pods across %d nodes on image %s", soakPods, len(nodes), image)

	space, err := f.MountSpace(ctx, pods[0], mountPath)
	if err != nil {
		t.Fatalf("reading the export's free space: %v", err)
	}
	if space.FreeBytes < required {
		blocked(t, "the export reports %d bytes free and this soak needs %d at worst. The claim's size "+
			"is not the limit here: an export may be directory-backed with no per-volume quota. An hour "+
			"of soak that dies on ENOSPC at minute fifty is an hour spent to learn nothing",
			space.FreeBytes, required)
	}

	runs := make([]*framework.FioRun, soakPods)
	for i, pod := range pods {
		// One directory per pod, named for the pod, so a mismatch names the
		// writer that produced it.
		run, err := f.StartFio(ctx, framework.FioSpec{
			Pod: pod, Dir: fmt.Sprintf("%s/data14/%s", mountPath, pod), ID: fmt.Sprintf("data14-%d", i),
			Runtime: soakRuntime, Files: soakFilesPerPod, SizeRange: soakSizeRange,
			ReadPercent: soakReadPercent,
		})
		if err != nil {
			t.Fatalf("starting the soak job in %s: %v", pod, err)
		}
		runs[i] = run
	}
	t.Logf("%d soak jobs running for %s at %d%% reads over files of %s",
		len(runs), soakRuntime, soakReadPercent, soakSizeRange)

	var totalRead, totalWritten int64
	failed := 0
	for i, run := range runs {
		// Each job's own wait, generously past the runtime: a job that is late
		// is a finding, and one that never reports is a worse one.
		report, err := run.Wait(ctx, soakRuntime+20*time.Minute)
		if err != nil {
			t.Errorf("the soak job in %s: %v", pods[i], err)
			continue
		}
		if raw, err := run.Raw(ctx); err == nil {
			if err := f.WriteArtifact("fio-"+pods[i]+".json", []byte(raw)); err != nil {
				t.Logf("writing the report of %s: %v", pods[i], err)
			}
		}
		read, written := report.Bytes()
		totalRead, totalWritten = totalRead+read, totalWritten+written
		for _, job := range report.Failed() {
			failed++
			t.Errorf("the soak job %q in %s ended with fio error %d. With verify_fatal=1 a crc32c "+
				"mismatch ends the job at the mismatch, so this is a checksum mismatch until the "+
				"report in the bundle says otherwise, and the expected result for this case is zero",
				job.Name, pods[i], job.Error)
		}
	}
	// Recorded, asserted on by nobody. A bound here would be a SCALE case
	// wearing a DATA number.
	t.Logf("the soak moved %d bytes read and %d written across %d pods in %s, with %d failed jobs",
		totalRead, totalWritten, soakPods, soakRuntime, failed)
	if totalRead == 0 && totalWritten == 0 {
		t.Errorf("the soak moved no bytes at all in %s, so whatever the jobs reported, nothing was "+
			"verified and this case has not exercised the data path", soakRuntime)
	}
}

// fioPod is a soak pod: the named fio image holding the mount open, driven
// through exec like every other client pod.
func fioPod(name, claim, node, image string) framework.PodSpec {
	spec := toolsPod(name, claim, node)
	spec.Image = image
	return spec
}

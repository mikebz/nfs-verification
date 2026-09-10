package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/mikebz/nfs-verification/pkg/framework"
)

// The identity the server records is the one the writing pod ran as. NFSv4
// carries owners as strings and maps them back through an idmapper on each
// client, so an id can survive on one node and read back as nobody on another.
// That is the failure this case exists to catch, and it is why ownership is
// read from two pods rather than one.

// testUID and testGID are arbitrary, and deliberately not a uid the image
// already knows: a name the container has never heard of is what exercises the
// numeric path rather than a lucky name match on both ends.
const (
	testUID = 1234
	testGID = 1234
	// nobody is the id an NFSv4 client substitutes when it cannot map an owner
	// string. Seeing it is the classic symptom, so it is named in the failure.
	nobodyUID = 65534
)

// SEC-01: uid and gid preservation across pods. A pod running as an ordinary
// user writes a file; another pod on another node reads the ownership back.
//
// Steps:
//  1. Pin a writer running as uid 1234 to one node, and a root reader to
//     another, on one claim.
//  2. From the reader, make a directory the ordinary user can write to. If
//     root cannot, report blocked: that is export configuration.
//  3. Write a file there as uid 1234. If that is refused, report blocked.
//  4. Read ownership on the writer's own client: a wrong id here means the
//     mapping broke on the way in, and no reader will fix it.
//  5. Read it again on the second client, and separate the two failures: wrong
//     on both is the server, wrong only on the reader is that node's idmapper.
func TestOwnershipPreservedAcrossPods(t *testing.T) {
	f := framework.New(t, "SEC-01")
	requireCap(t, f.Caps.MultiNode, "reading ownership from a second client needs two schedulable workers")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "sec01")
	// The writer runs as an ordinary user; the reader stays root so that it can
	// read ownership regardless of how the export treats the writer's id.
	f.MustPod(ctx, framework.PodSpec{
		Name: "writer", Node: nodeA, Mounts: mounts(pvc.Name),
		RunAsUser: framework.Int64(testUID), RunAsGroup: framework.Int64(testGID),
	})
	f.MustPod(ctx, toolsPod("reader", pvc.Name, nodeB))

	// The share's root belongs to whoever provisioned it, so the case makes a
	// directory the ordinary user can write to. Directory operations go to the
	// server, so the writer sees this without waiting on any cache.
	dir := fileIn("sec01")
	if r := f.Sh(ctx, "reader", "mkdir -p "+framework.Quote(dir)+" && chmod 0777 "+framework.Quote(dir)); r.Err != nil {
		t.Skipf("blocked on export configuration: root on %s cannot create a writable directory on the share (%s); "+
			"SEC-02 covers what the export does to root", nodeB, r.Combined())
	}
	// Best effort, and informative either way. A refusal here means root is
	// squashed, which is SEC-02's subject and does not stop this case: the
	// directory is already world-writable.
	if r := f.Sh(ctx, "reader", fmt.Sprintf("chown %d:%d %s", testUID, testGID, framework.Quote(dir))); r.Err != nil {
		t.Logf("root on %s could not chown on the share, so the export appears to squash root (%s); "+
			"this case continues through the world-writable directory", nodeB, r.Combined())
	}

	path := dir + "/owned.dat"
	if r := f.Sh(ctx, "writer", "echo sec01-payload > "+framework.Quote(path)); r.Err != nil {
		t.Skipf("blocked on export configuration: uid %d on %s cannot write to a 0777 directory on the share (%s); "+
			"the export is squashing or refusing ordinary users, which SEC-02 and SEC-05 cover",
			testUID, nodeA, r.Combined())
	}

	// The writer's own view first. If the id is already wrong here, the mapping
	// broke on the way in, and no reader is going to fix it.
	byWriter, err := f.StatOwner(ctx, "writer", path)
	if err != nil {
		t.Fatalf("reading ownership on %s: %v", nodeA, err)
	}
	if byWriter.UID != testUID || byWriter.GID != testGID {
		t.Errorf("the writer on %s wrote as %d:%d but its own client reports %s%s",
			nodeA, testUID, testGID, byWriter, nobodyNote(byWriter))
	}

	// Then the second client. A correct writer view with a wrong reader view is
	// a per-client idmapper problem, which routes to the node, not the server.
	byReader, err := f.StatOwner(ctx, "reader", path)
	if err != nil {
		t.Fatalf("reading ownership on %s: %v", nodeB, err)
	}
	switch {
	case byReader.UID == testUID && byReader.GID == testGID:
		t.Logf("ownership survived the crossing: %s on %s and %s on %s", byWriter, nodeA, byReader, nodeB)
	case byWriter.UID == testUID && byWriter.GID == testGID:
		t.Errorf("ownership is %s on the writing client %s but %s on the reading client %s%s: "+
			"the id survived the write and was lost on the read, which points at the reader's idmapper",
			byWriter, nodeA, byReader, nodeB, nobodyNote(byReader))
	default:
		t.Errorf("ownership is %s on %s, want %d:%d%s", byReader, nodeB, testUID, testGID, nobodyNote(byReader))
	}
}

// nobodyNote names the classic symptom when it is what happened, so that a
// failure report does not need a person to recognise 65534 on sight.
func nobodyNote(o framework.Owner) string {
	if o.UID == nobodyUID || o.GID == nobodyUID || o.User == "nobody" || o.Group == "nobody" {
		return " (mapped to nobody: the client could not resolve the owner string, " +
			"which is the reported failure on v4 clients with modern kernels)"
	}
	return ""
}

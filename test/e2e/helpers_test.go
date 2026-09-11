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

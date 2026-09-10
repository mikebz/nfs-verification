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

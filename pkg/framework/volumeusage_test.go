package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/mikebz/nfs-verification/pkg/slo"
)

// oneGiB is the capacity the readings below are of, so that the tolerance in
// play is a number a reader of this file can work out: two percent of it. A
// variable rather than a constant because the tolerance applied to it is a
// fraction, and Go will not fold that into an integer constant.
var oneGiB = int64(1) << 30

// TestCompareUsageVerdicts covers the three answers a comparison of two
// readings can give, one per row of the verdict table OBS-06 asserts against.
//
// The verdict is what decides whether a deployment is reported as unable to
// measure its own volumes, so each row is checked at the boundary rather than
// in the middle: a tolerance applied with the wrong comparison, or a freshness
// check that accepts a reading from before the workload wrote, both pass every
// test written comfortably inside the range.
//
// Steps:
//  1. Compare a fresh control plane reading inside the tolerance.
//  2. Compare a fresh reading outside it.
//  3. Compare a reading inside the tolerance whose sample predates the
//     workload's, which is two numbers agreeing about different moments.
//  4. Compare a reading sampled slightly before the workload's, within the
//     clock skew guard, which is the same moment read on two clocks.
func TestCompareUsageVerdicts(t *testing.T) {
	tolerance := int64(float64(oneGiB) * slo.VolumeUsageTolerance)
	podAt := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	pod := VolumeUsage{Source: SourcePodDF, Claim: "c", CapacityBytes: oneGiB, UsedBytes: 100 << 20, At: podAt}
	kubelet := func(used int64, at time.Time) VolumeUsage {
		return VolumeUsage{Source: SourceKubeletSummary, Claim: "c", CapacityBytes: oneGiB, UsedBytes: used, At: at}
	}

	for name, tc := range map[string]struct {
		kubelet VolumeUsage
		want    UsageVerdict
	}{
		"inside tolerance and fresh": {
			kubelet: kubelet(pod.UsedBytes+tolerance-1, podAt.Add(time.Second)),
			want:    UsageAgrees,
		},
		"reporting less, inside tolerance": {
			kubelet: kubelet(pod.UsedBytes-tolerance+1, podAt.Add(time.Second)),
			want:    UsageAgrees,
		},
		"outside tolerance, fresh": {
			kubelet: kubelet(pod.UsedBytes+tolerance+1, podAt.Add(time.Second)),
			want:    UsageDisagrees,
		},
		"reporting an almost empty volume, fresh": {
			kubelet: kubelet(0, podAt.Add(time.Second)),
			want:    UsageDisagrees,
		},
		"inside tolerance but sampled before the workload read": {
			kubelet: kubelet(pod.UsedBytes, podAt.Add(-slo.VolumeStatsPeriod)),
			want:    UsageStale,
		},
		"sampled within the clock skew guard": {
			kubelet: kubelet(pod.UsedBytes, podAt.Add(-slo.ClockSkewGuard+time.Second)),
			want:    UsageAgrees,
		},
	} {
		got := CompareUsage(pod, tc.kubelet)
		if got.Verdict != tc.want {
			t.Errorf("%s: verdict %s, want %s (delta %d bytes, tolerance %d, lag %s)",
				name, got.Verdict, tc.want, got.DeltaBytes, got.ToleranceBytes, got.Lag)
		}
		// The delta is recorded whatever the verdict, because a stale reading
		// that also disagrees is a different run from one that agrees, and the
		// table in the bundle is the only place that difference survives.
		if want := tc.kubelet.UsedBytes - pod.UsedBytes; got.DeltaBytes != want {
			t.Errorf("%s: delta %d bytes, want %d", name, got.DeltaBytes, want)
		}
	}
}

// TestMovementFloor states what counts as a reading having tracked a write.
//
// The floor exists to fail a gauge that ignores its subject, not to assert
// filesystem arithmetic, so what matters is that zero never clears it and that
// a reading which moved by roughly the bytes written always does.
//
// Steps:
//  1. Check the floor of a write is below the write and above nothing.
//  2. Check a frozen reading and a reading that moved by the full write.
func TestMovementFloor(t *testing.T) {
	written := slo.VolumeWriteBytes
	floor := MovementFloor(written)
	if floor <= 0 {
		t.Fatalf("a write of %d bytes has a movement floor of %d, which a reading that never moves clears",
			written, floor)
	}
	if floor >= written {
		t.Errorf("the movement floor of %d is not below the %d bytes written, so a filesystem accounting "+
			"for its own metadata would fail a case about the reading", floor, written)
	}
	before := VolumeUsage{UsedBytes: 4096}
	for name, tc := range map[string]struct {
		after VolumeUsage
		moved bool
	}{
		"frozen":             {after: VolumeUsage{UsedBytes: 4096}, moved: false},
		"moved a little":     {after: VolumeUsage{UsedBytes: 4096 + floor - 1}, moved: false},
		"moved by the bytes": {after: VolumeUsage{UsedBytes: 4096 + written}, moved: true},
	} {
		if got := UsedDelta(before, tc.after) >= floor; got != tc.moved {
			t.Errorf("%s: a delta of %d against a floor of %d read as moved=%v",
				name, UsedDelta(before, tc.after), floor, got)
		}
	}
}

// TestMatchesClaimCapacity is the quota check: whether the total a source
// reports describes the claim or the filesystem the claim is a directory on.
//
// An export with no per-volume quota reports its backing filesystem to every
// client, and both of OBS-06's sources then agree perfectly about a number that
// says nothing about this volume. That agreement is exactly what a passing case
// would hide, so this decides a failure and is checked at its edges.
//
// Steps:
//  1. Accept an exact match and a small difference, which is rounding.
//  2. Reject a total that is the backing filesystem rather than the claim.
//  3. Reject a claim with no capacity, which is a claim that never bound.
func TestMatchesClaimCapacity(t *testing.T) {
	claim := oneGiB
	within := int64(float64(claim) * slo.VolumeUsageTolerance)
	for name, tc := range map[string]struct {
		reported, claim int64
		want            bool
	}{
		"exact":                      {reported: claim, claim: claim, want: true},
		"rounded down by a kilobyte": {reported: claim - 1024, claim: claim, want: true},
		"at the tolerance":           {reported: claim + within, claim: claim, want: true},
		"past the tolerance":         {reported: claim + within + 1, claim: claim, want: false},
		"the backing filesystem":     {reported: 4 << 40, claim: claim, want: false},
		"nothing reported":           {reported: 0, claim: claim, want: false},
		"claim never bound":          {reported: claim, claim: 0, want: false},
	} {
		if got := MatchesClaimCapacity(tc.reported, tc.claim); got != tc.want {
			t.Errorf("%s: a reported total of %d against a claim of %d read as matching=%v",
				name, tc.reported, tc.claim, got)
		}
	}
}

// TestVolumeWriteIsMeasurable checks the two bounds OBS-06 rests on against
// each other, at the claim size the suite provisions.
//
// They are independent constants and either can be changed alone. A write below
// the tolerance would move a reading by less than the two sources are allowed
// to differ by, and the case would then be asserting that a number moved by an
// amount it cannot distinguish from noise. Nothing fails when that happens: the
// case keeps passing and stops meaning anything, which is why this is a test
// rather than a comment.
//
// Steps:
//  1. Work out the tolerance at the claim size the suite asks for.
//  2. Assert the bounded write is larger than it.
func TestVolumeWriteIsMeasurable(t *testing.T) {
	size, err := resource.ParseQuantity(Cfg().PVCSize)
	if err != nil {
		t.Fatalf("the configured claim size %q is not a quantity: %v", Cfg().PVCSize, err)
	}
	tolerance := int64(float64(size.Value()) * slo.VolumeUsageTolerance)
	if slo.VolumeWriteBytes <= tolerance {
		t.Errorf("OBS-06 writes %d bytes and the two sources may differ by %d on a %s claim, so the write "+
			"cannot be told apart from the tolerance and the movement assertion proves nothing",
			slo.VolumeWriteBytes, tolerance, Cfg().PVCSize)
	}
}

// TestUsageReportTable checks the bundle keeps what a reader needs.
//
// The table is written whether the case passed or not, and it is the only
// record of a run where the two sources differed but stayed inside tolerance. A
// table missing a timestamp or a row cannot be compared against the next run's.
//
// Steps:
//  1. Record two stages of comparisons.
//  2. Assert every source, both stages, the byte counts and the sample times
//     are all in the rendered table.
func TestUsageReportTable(t *testing.T) {
	at := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	pod := VolumeUsage{Source: SourcePodDF, Claim: "c", CapacityBytes: oneGiB, UsedBytes: 10240, At: at}
	kubelet := VolumeUsage{Source: SourceKubeletSummary, Claim: "c", CapacityBytes: oneGiB,
		UsedBytes: 10240, At: at.Add(time.Second)}

	var r UsageReport
	r.Record("before", CompareUsage(pod, kubelet))
	pod.UsedBytes += slo.VolumeWriteBytes
	kubelet.UsedBytes += slo.VolumeWriteBytes
	r.Record("after", CompareUsage(pod, kubelet))

	table := r.Table()
	for _, want := range []string{
		"before", "after", string(SourcePodDF), string(SourceKubeletSummary),
		"10240", "2026-09-11T10:00:00Z", "2026-09-11T10:00:01Z", string(UsageAgrees),
	} {
		if !strings.Contains(table, want) {
			t.Errorf("the usage table omits %q, so the run cannot be compared against another:\n%s", want, table)
		}
	}
}

// TestWriteBytesScriptReportsWhatLanded runs the write OBS-06 uses under a real
// shell, against a path with a space in it and a seed carrying quotes.
//
// Two things can go wrong here and neither shows up until a case is already
// running against a cluster. A quoting slip writes to the wrong path or lets
// the shell expand the seed, and a wrong format verb makes the script print
// something other than the size. The count it prints is what the movement
// assertion is measured against, so a script that prints the requested size
// instead of the written one would make that assertion circular.
//
// Steps:
//  1. Run the script under sh with a path holding a space.
//  2. Assert what it printed is the size the file actually has.
//  3. Assert the seed landed as bytes rather than being expanded.
func TestWriteBytesScriptReportsWhatLanded(t *testing.T) {
	sh := lookOrSkip(t, "sh", "yes", "head", "stat", "sync")
	target := filepath.Join(t.TempDir(), "a dir", "a file.bin")
	const seed = `seed with 'quotes' and $VARS`
	const size = 64 << 10

	out, err := exec.Command(sh, "-c", writeBytesScript(target, size, seed)).Output()
	if err != nil {
		t.Fatalf("running the write script: %v", err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("the script printed %q, which is not a byte count: %v", out, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("the script reported %d bytes written and left no file at %s: %v", n, target, err)
	}
	if n != info.Size() {
		t.Errorf("the script reported %d bytes and the file holds %d", n, info.Size())
	}
	if n != size {
		t.Errorf("the script wrote %d bytes of the %d asked for on a filesystem with room for them", n, size)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading back what was written: %v", err)
	}
	if !strings.Contains(string(body), seed) {
		t.Errorf("the seed did not survive quoting: the file starts %q", string(body[:min(len(body), 64)]))
	}
}

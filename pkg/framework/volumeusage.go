package framework

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mikebz/nfs-verification/pkg/slo"
)

// Two sources answer the question "how full is this volume", and OBS-06 is
// about whether they agree. The workload's own view comes from df inside a pod,
// which is what an application gets ENOSPC against. The control plane's view
// comes from the kubelet, which is what an operator's monitoring reads. A
// deployment where those two disagree is one where a dashboard says a volume is
// nearly empty while the workload has run out of room.

// UsageSource names who produced a reading. Every comparison is between two
// rows with the same claim and different sources.
type UsageSource string

const (
	// SourcePodDF is df inside the pod: what the workload sees, at the moment
	// it was asked.
	SourcePodDF UsageSource = "pod-df"
	// SourceKubeletSummary is the kubelet's stats summary: what the control
	// plane sees, as of whenever the kubelet last recomputed it.
	SourceKubeletSummary UsageSource = "kubelet-summary"
)

// VolumeUsage is one source's reading of one claim.
//
// Available is recorded rather than derived. On a filesystem with reserved
// blocks used plus available does not equal capacity, and deriving the third
// number from the other two would manufacture agreement between two sources
// that disagree.
type VolumeUsage struct {
	Source                                   UsageSource
	Claim                                    string
	CapacityBytes, UsedBytes, AvailableBytes int64
	// At is the kubelet's own sample time for a control plane reading, and the
	// workstation clock at the exec for a pod reading. The two come from
	// different clocks, which is why every comparison between them is guarded.
	At time.Time
}

func (u VolumeUsage) String() string {
	return fmt.Sprintf("%s: %s used %s of %s, %s available, at %s",
		u.Source, u.Claim, bytesOf(u.UsedBytes), bytesOf(u.CapacityBytes), bytesOf(u.AvailableBytes),
		u.At.UTC().Format(time.RFC3339))
}

// bytesOf renders a byte count both ways. The exact number is what a failure
// has to carry, and the rounded one is what makes it readable at a glance.
func bytesOf(n int64) string {
	const mib = 1 << 20
	if n < mib {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%d B (%.1f MiB)", n, float64(n)/mib)
}

// ClaimUsage reports what df inside a pod says about the mount at path.
//
// df rather than statfs, deliberately: this reading is the workload's view by
// definition, and df is the tool an operator would run in the same pod to
// check the same thing.
func (f *Framework) ClaimUsage(ctx context.Context, pod, path, claim string) (VolumeUsage, error) {
	// Taken before the exec, not after: the reading is at least this old, and a
	// timestamp taken afterwards would make a slow exec look like a fresher
	// reading than it is.
	at := time.Now()
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main",
		fmt.Sprintf("df -P -k %s", shellQuote(path)))
	if err != nil {
		return VolumeUsage{}, fmt.Errorf("reading what df says about %s in %s: %w", path, f.Name(pod), err)
	}
	row, err := parseDF(out)
	if err != nil {
		return VolumeUsage{}, err
	}
	return VolumeUsage{
		Source:         SourcePodDF,
		Claim:          f.Name(claim),
		CapacityBytes:  row.TotalBytes,
		UsedBytes:      row.UsedBytes,
		AvailableBytes: row.AvailBytes,
		At:             at,
	}, nil
}

// UsageVerdict is what a comparison of two readings of one claim says.
type UsageVerdict string

const (
	// UsageAgrees means the two sources are within tolerance of each other, on
	// a control plane reading fresh enough to be worth comparing.
	UsageAgrees UsageVerdict = "agrees"
	// UsageDisagrees means they are further apart than the tolerance allows.
	UsageDisagrees UsageVerdict = "disagrees"
	// UsageStale means the control plane's reading predates the workload's, so
	// it cannot reflect what the workload had already done. It is reported
	// separately from disagreement because two numbers that agree about
	// different moments have not been compared at all.
	UsageStale UsageVerdict = "stale"
)

// UsageComparison is two readings of one claim and what they say about each
// other.
type UsageComparison struct {
	Pod, Kubelet VolumeUsage
	// DeltaBytes is the control plane's used bytes minus the workload's.
	// Signed: which way a deployment is wrong is worth knowing, since a control
	// plane reporting less than the workload is the shape that hides a full
	// volume.
	DeltaBytes     int64
	ToleranceBytes int64
	// Lag is how far the control plane's sample time is behind the workload's
	// reading. Negative means the kubelet sampled afterwards, which is what the
	// case waits for.
	Lag     time.Duration
	Verdict UsageVerdict
}

// CompareUsage decides whether two readings of one claim agree, with the
// tolerance taken as a fraction of the smaller of what the claim was
// provisioned at and what the workload is told its filesystem holds.
//
// The smaller of the two, because they are not always the same number and the
// larger one is the wrong denominator. An export with no per-volume quota
// reports the filesystem behind it: measured on a real cluster, a 1 GiB claim
// on a 10 GiB backing volume produced a tolerance of 199 MiB, wider than the
// 128 MiB this case writes, so two sources could have disagreed by more than
// the whole workload and still agreed. On a multi-terabyte pool the tolerance
// would swallow anything. The claim is what an operator's threshold would be a
// fraction of, and where it is unknown the workload's view is all there is.
//
// Freshness is checked first: a stale reading that happens to be within
// tolerance has not been compared with anything, it has been compared with a
// moment before the case started.
func CompareUsage(pod, kubelet VolumeUsage, claimBytes int64) UsageComparison {
	c := UsageComparison{
		Pod:            pod,
		Kubelet:        kubelet,
		DeltaBytes:     kubelet.UsedBytes - pod.UsedBytes,
		ToleranceBytes: toleranceBytes(pod.CapacityBytes, claimBytes),
		Lag:            pod.At.Sub(kubelet.At),
	}
	switch {
	// The two ends are stamped by different clocks, the pod reading by the
	// workstation and the kubelet's by its node, so the same guard band that
	// narrows a grace window decides what counts as "afterwards" here.
	case c.Lag > slo.ClockSkewGuard:
		c.Verdict = UsageStale
	case abs64(c.DeltaBytes) > c.ToleranceBytes:
		c.Verdict = UsageDisagrees
	default:
		c.Verdict = UsageAgrees
	}
	return c
}

// toleranceBytes is the permitted disagreement, as a fraction of the smallest
// capacity in play. Either input may be zero, meaning unknown: a claim whose
// status carries no capacity, or a df that could not be read.
func toleranceBytes(workloadBytes, claimBytes int64) int64 {
	basis := workloadBytes
	if claimBytes > 0 && (basis <= 0 || claimBytes < basis) {
		basis = claimBytes
	}
	if basis <= 0 {
		return 0
	}
	return int64(float64(basis) * slo.VolumeUsageTolerance)
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func (c UsageComparison) String() string {
	return fmt.Sprintf("%s: the control plane is %s from the workload's %s (tolerance %s, "+
		"control plane sampled %s %s the workload's reading)",
		c.Verdict, bytesOf(abs64(c.DeltaBytes)), bytesOf(c.Pod.UsedBytes), bytesOf(c.ToleranceBytes),
		absDuration(c.Lag).Round(time.Second), behindOrAhead(c.Lag))
}

func behindOrAhead(lag time.Duration) string {
	if lag > 0 {
		return "before"
	}
	return "after"
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// UsedDelta is how much a source's used bytes moved between two readings.
func UsedDelta(before, after VolumeUsage) int64 { return after.UsedBytes - before.UsedBytes }

// MovementFloor is the smallest increase in used bytes that counts as a source
// having tracked a write of the given size.
//
// Not the written bytes themselves: a filesystem accounts for metadata too, and
// on a shared export another workload moves the number underneath the case in
// either direction. What is being asserted is that the reading responds to the
// workload at all, and a frozen gauge cannot clear this floor.
func MovementFloor(writtenBytes int64) int64 {
	return int64(float64(writtenBytes) * slo.VolumeUsageMovement)
}

// MatchesClaimCapacity reports whether a reported total describes the claim
// rather than something larger.
//
// An export that is a subdirectory of a filesystem with no per-volume quota
// reports that filesystem's size to every client of it. The numbers are real
// and two sources will agree on them perfectly; they are simply not a
// measurement of this claim, so no threshold on them says anything about it.
func MatchesClaimCapacity(reportedBytes, claimBytes int64) bool {
	if claimBytes <= 0 {
		return false
	}
	return abs64(reportedBytes-claimBytes) <= int64(float64(claimBytes)*slo.VolumeUsageTolerance)
}

// UsageReport is the table a case writes to its bundle.
//
// Written whether the case passed or not: a run where the two sources differed
// by 2% is a different run from one where they agreed exactly, and the pass
// looks identical without it.
type UsageReport struct{ stages []usageStage }

type usageStage struct {
	Label string
	Cmp   UsageComparison
}

// Record adds a labelled comparison.
func (r *UsageReport) Record(label string, c UsageComparison) {
	r.stages = append(r.stages, usageStage{Label: label, Cmp: c})
}

// Table renders every reading and every comparison, in the order they were
// taken.
func (r *UsageReport) Table() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-12s %-16s %14s %14s %14s  %s\n",
		"stage", "source", "capacity", "used", "available", "at")
	for _, s := range r.stages {
		for _, u := range []VolumeUsage{s.Cmp.Pod, s.Cmp.Kubelet} {
			fmt.Fprintf(&sb, "%-12s %-16s %14d %14d %14d  %s\n",
				s.Label, u.Source, u.CapacityBytes, u.UsedBytes, u.AvailableBytes,
				u.At.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&sb, "%-12s %s\n\n", s.Label, s.Cmp)
	}
	return sb.String()
}

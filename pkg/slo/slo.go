// Package slo holds every timing and correctness target the chaos cases assert
// against. Values live here and nowhere else, per Section 3.8 of the plan.
package slo

import (
	"fmt"
	"time"
)

// Profile is a pinned lease/grace configuration. Failover SLOs are meaningless
// without one, because grace is the dominant term in every recovery measurement.
type Profile struct {
	Name  string        `json:"name"`
	Lease time.Duration `json:"lease"`
	Grace time.Duration `json:"grace"`
}

// The two profiles the suite accepts. Any third value invalidates every timing
// assertion, so preflight fails rather than measuring against an unknown base.
var (
	// Tuned is the configuration the ratified SLOs were written against.
	Tuned = Profile{Name: "tuned", Lease: 20 * time.Second, Grace: 30 * time.Second}
	// Default documents what a customer who changes nothing experiences.
	Default = Profile{Name: "default", Lease: 60 * time.Second, Grace: 90 * time.Second}
)

// Profiles returns the accepted profiles in declaration order.
func Profiles() []Profile { return []Profile{Tuned, Default} }

// Match returns the profile matching the live lease and grace values.
func Match(lease, grace time.Duration) (Profile, bool) {
	for _, p := range Profiles() {
		if p.Lease == lease && p.Grace == grace {
			return p, true
		}
	}
	return Profile{}, false
}

// ByName returns a profile by its name.
func ByName(name string) (Profile, bool) {
	for _, p := range Profiles() {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// Event is a recovery-triggering event measured as time to first successful I/O.
type Event string

const (
	// EventServerRestart covers process kill and pod delete: the server comes
	// back in place, the backing volume never moves.
	EventServerRestart Event = "server-restart-in-place"
	// EventGracefulDrain covers cordon-and-drain reschedule.
	EventGracefulDrain Event = "server-reschedule-graceful-drain"
	// EventUngracefulNodeLoss covers hard node stop, including volume detach.
	EventUngracefulNodeLoss Event = "server-reschedule-ungraceful-node-loss"
)

// tunedTargets are the ratified p99 targets against the tuned profile.
var tunedTargets = map[Event]time.Duration{
	EventServerRestart:      60 * time.Second,
	EventGracefulDrain:      60 * time.Second,
	EventUngracefulNodeLoss: 90 * time.Second,
}

// Recovery returns the maximum permitted time to first successful I/O for an
// event under a profile. On the default profile the target is grace + 30s,
// because a 60s target below a 90s grace period fails with no defect present.
func Recovery(p Profile, e Event) (time.Duration, error) {
	switch p.Name {
	case Tuned.Name:
		d, ok := tunedTargets[e]
		if !ok {
			return 0, fmt.Errorf("slo: no target for event %q", e)
		}
		return d, nil
	case Default.Name:
		if _, ok := tunedTargets[e]; !ok {
			return 0, fmt.Errorf("slo: no target for event %q", e)
		}
		return p.Grace + 30*time.Second, nil
	default:
		return 0, fmt.Errorf("slo: unknown profile %q", p.Name)
	}
}

// Protocol invariants. These are not tunable: they come from NFSv4.1 semantics,
// not from a performance budget.
const (
	// MaxIOErrors is the permitted count of I/O errors on a hard mount across a
	// failover. Hard mounts block, they do not fail.
	MaxIOErrors = 0
	// MaxCommittedWritesLost is the permitted count of lost post-COMMIT writes.
	MaxCommittedWritesLost = 0
	// LockReclaimFraction is the required fraction of locks reclaimed.
	LockReclaimFraction = 1.0
	// MaxNewLocksDuringGrace is the permitted count of new locks granted to a
	// different client while the server is in grace.
	MaxNewLocksDuringGrace = 0
)

// GraceExitBound is the deadline for grace to end after it is entered.
// Convention: grace runs about two lease periods.
func GraceExitBound(p Profile) time.Duration { return 2 * p.Lease }

// LockReleaseBound is the longest a lock held by a client that vanished may
// stay held before another client can take the range.
//
// One lease period. The Linux NFSv4 client establishes a single lease on each
// server it accesses, shared by every mount and every pod on that node
// (Documentation/filesystems/nfs/client-identifier.rst), and that lease is the
// only thing that releases state nothing closed.
//
// It is an upper bound, never an expectation. A pod dying closes its
// descriptors and the lock goes in about a second with no lease expiring; a
// node dying closes nothing and every pod's locks on it wait out the lease. A
// case that asserted expiry would fail on a healthy cluster, and one that
// asserted promptness would fail wherever kubelet was slow to kill, for reasons
// that have nothing to do with NFS. The bound holds under both, which is why it
// is the bound the plan states.
func LockReleaseBound(p Profile) time.Duration { return p.Lease }

// ObservationMargin is how far past a bound a case keeps watching before it
// gives up.
//
// A case that stops at its own SLO reports "timed out" where it could report
// how long the thing actually took, and the second is what a defect report
// needs. It is a diagnostic allowance, never part of any assertion: the bound
// decides pass or fail, and this only decides how much is known about a
// failure.
//
// Named here rather than written as a literal in each case, so that it cannot
// drift out of step with the bounds beside it.
const ObservationMargin = 5 * time.Minute

// PromptLockRelease separates the two mechanisms that can release such a lock,
// so that a pass says which one was observed rather than only that the bound
// held. Below it the descriptors closed and the client sent LOCKU; near the
// lease the client never noticed and the lease expired.
//
// A run where every release is at the lease boundary is a different cluster
// from one where every release is prompt, and the pass looks identical without
// this. Deliberately loose: it classifies a log line, it gates nothing.
const PromptLockRelease = 5 * time.Second

// ClockSkewGuard narrows a window whose two ends were stamped by different
// clocks. A grace window is stamped by the kubelet on the server's node; a lock
// attempt is stamped by the client pod that made it, on another node. There is
// no way to put both on one clock, because grace happens on the server and the
// attempt has to come from a client.
//
// So the window is narrowed by this much at each end, and only an event
// unambiguously inside the narrowed window is reported as a violation. The
// direction is deliberate: a marginal violation missed costs one finding, and a
// lawful lock grant reported as a protocol violation costs a week.
const ClockSkewGuard = 5 * time.Second

// The bounds OBS-06 compares two readings of one volume against.
//
// Two samplers reading one quantity at two moments never produce equal numbers,
// so the case needs a stated tolerance rather than equality, and a statement of
// how fresh the slower sampler's answer has to be before it is worth comparing
// at all. Both live here rather than in the case, for the same reason every
// other bound does.
const (
	// VolumeStatsPeriod is how often the kubelet recomputes volume statistics
	// by default. It is the slower of the two samplers: `df` inside a pod
	// answers at the moment it is asked, and this one answers with whatever it
	// last computed.
	VolumeStatsPeriod = time.Minute

	// VolumeStatsFreshness is how long a case waits for a kubelet reading whose
	// own timestamp is later than the pod reading it will be compared with.
	// Three aggregation periods: enough that a kubelet on its documented
	// default is never the reason a case fails, short enough that a control
	// plane which has stopped recomputing usage is reported rather than waited
	// out.
	VolumeStatsFreshness = 3 * VolumeStatsPeriod

	// VolumeUsageTolerance is how far the two sources may disagree about used
	// bytes, as a fraction of the capacity the workload sees.
	//
	// Derived from the slower sampler's period: the kubelet's answer is stale
	// by up to VolumeStatsPeriod, so anything the workload changed inside that
	// window is legitimately missing from it. It is deliberately not tight. A
	// misread field, a source reporting a different filesystem, or a gauge that
	// ignores its subject are all wrong by orders of magnitude, and those are
	// the failures this case exists to catch.
	VolumeUsageTolerance = 0.02

	// VolumeWriteBytes is what OBS-06 writes to show the two sources move
	// together.
	//
	// An absolute count, not a fraction of the claim: a fraction is tens of
	// gigabytes on a multi-terabyte claim and below the tolerance on a small
	// one, so it would be neither bounded nor meaningful across deployments.
	// Nothing here approaches any capacity threshold; the threshold an alert
	// would sit on is the operator's.
	VolumeWriteBytes int64 = 128 << 20

	// VolumeUsageMovement is the fraction of the bytes actually written that
	// must show up as an increase in a source's used bytes.
	//
	// Not one: a filesystem accounts for metadata as well as data, and another
	// workload on a shared export moves the number underneath the case in
	// either direction. Half is far enough from zero that a frozen gauge cannot
	// satisfy it, which is the only thing being asserted.
	VolumeUsageMovement = 0.5
)

// SoakDegradationBound is the permitted throughput degradation trend over a
// sustained soak (SCALE-07).
const SoakDegradationBound = 0.10

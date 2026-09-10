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

// AlertSLO is the deadline for an availability alert to fire (OBS-01).
const AlertSLO = 5 * time.Minute

// SoakDegradationBound is the permitted throughput degradation trend over a
// sustained soak (SCALE-07).
const SoakDegradationBound = 0.10

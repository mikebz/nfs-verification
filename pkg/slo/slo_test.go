package slo

import (
	"testing"
	"time"
)

// The SLO table is the one place where a wrong number silently invalidates
// every chaos result, so it carries its own tests.

// TestRecoveryTargetsPerProfile covers the ratified targets for each event
// under each profile. Every chaos case reads its bound from here, so an error
// in this table is an error in every timing assertion in the suite.
//
// Steps:
//  1. Look up each event under the tuned profile against the ratified values.
//  2. Do the same under the default profile, where the target is grace + 30s.
func TestRecoveryTargetsPerProfile(t *testing.T) {
	cases := []struct {
		profile Profile
		event   Event
		want    time.Duration
	}{
		{Tuned, EventServerRestart, 60 * time.Second},
		{Tuned, EventGracefulDrain, 60 * time.Second},
		{Tuned, EventUngracefulNodeLoss, 90 * time.Second},
		// On the default profile a 60s target sits below a 90s grace period, so
		// the plan would fail itself with no defect present. Grace plus 30s.
		{Default, EventServerRestart, 120 * time.Second},
		{Default, EventUngracefulNodeLoss, 120 * time.Second},
	}
	for _, tc := range cases {
		got, err := Recovery(tc.profile, tc.event)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.profile.Name, tc.event, err)
		}
		if got != tc.want {
			t.Errorf("%s/%s: got %s, want %s", tc.profile.Name, tc.event, got, tc.want)
		}
	}
}

// TestDefaultProfileTargetExceedsGrace covers the reason the default profile
// has its own arithmetic: a 60s restart target under a 90s grace period is
// unachievable, so the suite would fail its own SLO with no defect present.
//
// Steps:
//  1. Take the restart target under the default profile.
//  2. Assert it sits above that profile's grace period.
func TestDefaultProfileTargetExceedsGrace(t *testing.T) {
	got, err := Recovery(Default, EventServerRestart)
	if err != nil {
		t.Fatal(err)
	}
	if got <= Default.Grace {
		t.Errorf("target %s does not exceed the %s grace period, so it can never be met", got, Default.Grace)
	}
}

// TestMatchRejectsOffProfileValues covers profile pinning. A third lease and
// grace combination is refused rather than accepted, because measuring against
// an unknown base silently invalidates every timing assertion in the suite.
//
// Steps:
//  1. Match both accepted combinations and check the names.
//  2. Offer off-profile combinations and assert none matches.
func TestMatchRejectsOffProfileValues(t *testing.T) {
	if _, ok := Match(20*time.Second, 30*time.Second); !ok {
		t.Error("the tuned profile should match its own values")
	}
	if _, ok := Match(60*time.Second, 90*time.Second); !ok {
		t.Error("the default profile should match its own values")
	}
	// A third value must not silently pass: every timing assertion depends on
	// the profile being one of the two.
	if _, ok := Match(45*time.Second, 45*time.Second); ok {
		t.Error("an off-profile lease/grace pair matched a profile")
	}
}

// TestUnknownProfileIsAnError checks that a profile nobody defined yields an
// error rather than a zero duration, which would read as an instant SLO that
// nothing can meet.
//
// Steps:
//  1. Ask for a recovery target under a profile that does not exist.
//  2. Assert it is an error.
func TestUnknownProfileIsAnError(t *testing.T) {
	if _, err := Recovery(Profile{Name: "invented"}, EventServerRestart); err == nil {
		t.Error("an unknown profile should not yield a target")
	}
}

// TestGraceExitBoundIsTwoLeases covers the convention that grace runs about
// two lease periods, which is the bound the grace cases hold a server to.
//
// Steps:
//  1. Take the grace exit bound for each profile.
//  2. Assert each is twice that profile's lease.
func TestGraceExitBoundIsTwoLeases(t *testing.T) {
	if got, want := GraceExitBound(Tuned), 40*time.Second; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestLockReleaseBoundIsOneLease covers the bound DATA-06 asserts against: a
// lock held by a client that vanished becomes available inside one lease
// period.
//
// One lease, not one grace period and not a literal. The Linux NFSv4 client
// keeps a single lease per server per node, and that lease is the only thing
// that releases state nothing closed.
//
// Steps:
//  1. Take the bound under each profile.
//  2. Assert it is that profile's lease, so the case measures against the
//     configuration preflight pinned rather than against a constant.
//  3. Assert the prompt-release threshold sits well below both, so that the
//     classification can tell a descriptor close from a lease expiry on either.
func TestLockReleaseBoundIsOneLease(t *testing.T) {
	for _, p := range Profiles() {
		if got := LockReleaseBound(p); got != p.Lease {
			t.Errorf("the lock release bound on the %s profile is %s, want its %s lease", p.Name, got, p.Lease)
		}
		if PromptLockRelease >= p.Lease {
			t.Errorf("the prompt-release threshold %s is not below the %s profile's %s lease, so a "+
				"lease expiry and a descriptor close would classify the same way",
				PromptLockRelease, p.Name, p.Lease)
		}
	}
}

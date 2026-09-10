package slo

import (
	"testing"
	"time"
)

// The SLO table is the one place where a wrong number silently invalidates
// every chaos result, so it carries its own tests.

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

func TestDefaultProfileTargetExceedsGrace(t *testing.T) {
	got, err := Recovery(Default, EventServerRestart)
	if err != nil {
		t.Fatal(err)
	}
	if got <= Default.Grace {
		t.Errorf("target %s does not exceed the %s grace period, so it can never be met", got, Default.Grace)
	}
}

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

func TestUnknownProfileIsAnError(t *testing.T) {
	if _, err := Recovery(Profile{Name: "invented"}, EventServerRestart); err == nil {
		t.Error("an unknown profile should not yield a target")
	}
}

func TestGraceExitBoundIsTwoLeases(t *testing.T) {
	if got, want := GraceExitBound(Tuned), 40*time.Second; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

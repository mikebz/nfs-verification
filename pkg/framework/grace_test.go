package framework

import (
	"strings"
	"testing"
	"time"
)

// Grace is the dominant term in every recovery number this suite reports, and
// the classifier below is the one part of reading it that depends on how an
// implementation words things. Getting it backwards does not fail loudly: it
// reports a healthy server as looping through grace, which is the diagnosis the
// triage runbook already calls the most common wrong one.

func stamp(sec int64) string { return time.Unix(sec, 0).UTC().Format(time.RFC3339Nano) }

// TestClassifyGraceWording covers the wordings servers use to announce grace,
// and the reason the rule is positional: the common exit wordings are the entry
// wording with a negation in front of them.
//
// Steps:
//  1. Classify entry wordings from several implementations.
//  2. Classify the exit wordings, including the ones that contain the entry
//     wording verbatim.
//  3. Assert a line about something else entirely is not classified at all.
func TestClassifyGraceWording(t *testing.T) {
	enters := []string{
		"NFS Server Now IN GRACE, duration 90",
		"nfsd: starting 90-second grace period",
		"Entering grace period",
		"grace_period :: server entered grace",
	}
	exits := []string{
		"NFS Server Now NOT IN GRACE",
		"nfsd: end of grace period",
		"grace period expired",
		"Lifting grace period, all clients reclaimed",
		"server is no longer in grace",
	}
	for _, line := range enters {
		if exit, ok := classifyGrace(line); !ok || exit {
			t.Errorf("%q was read as exit=%v (matched=%v), want an entry", line, exit, ok)
		}
	}
	for _, line := range exits {
		if exit, ok := classifyGrace(line); !ok || !exit {
			t.Errorf("%q was read as exit=%v (matched=%v), want an exit; an exit read as an entry "+
				"reports a healthy server as looping through grace", line, exit, ok)
		}
	}
	for _, line := range []string{"connection reset by peer", "reclaim complete for client 0x1"} {
		if _, ok := classifyGrace(line); ok {
			t.Errorf("%q was classified as a grace transition, but it says nothing about grace", line)
		}
	}
}

// TestGracePatternFlagsOverrideTheWordingRule covers the escape hatch for a
// server whose wording the rule does not cover. The flags replace the rule
// rather than adding to it: an operator who states the wording owns it, and
// falling back would classify lines their pattern deliberately left out.
//
// Steps:
//  1. Set both patterns and compile them.
//  2. Assert the stated wordings classify, in both directions.
//  3. Assert a line the built-in rule would have classified no longer does.
//  4. Assert one pattern without the other is refused, and that a pattern that
//     does not compile is refused too.
func TestGracePatternFlagsOverrideTheWordingRule(t *testing.T) {
	restore := *Cfg()
	t.Cleanup(func() { *Cfg() = restore })

	Cfg().GraceEnterPattern = "GRACE_START"
	Cfg().GraceExitPattern = "GRACE_STOP"
	if err := compileGracePatterns(); err != nil {
		t.Fatalf("compiling the stated wordings: %v", err)
	}
	if exit, ok := classifyGrace("grace: GRACE_START now"); !ok || exit {
		t.Errorf("the stated entry wording was read as exit=%v (matched=%v)", exit, ok)
	}
	if exit, ok := classifyGrace("grace: GRACE_STOP now"); !ok || !exit {
		t.Errorf("the stated exit wording was read as exit=%v (matched=%v)", exit, ok)
	}
	if _, ok := classifyGrace("NFS Server Now IN GRACE"); ok {
		t.Error("the built-in rule still ran alongside a stated wording, so lines the operator " +
			"deliberately left out are being classified anyway")
	}

	Cfg().GraceExitPattern = ""
	if err := compileGracePatterns(); err == nil {
		t.Error("one grace pattern without the other was accepted; every failover would then look " +
			"like a grace re-entry loop")
	}
	Cfg().GraceEnterPattern = "("
	Cfg().GraceExitPattern = "GRACE_STOP"
	if err := compileGracePatterns(); err == nil {
		t.Error("a pattern that does not compile was accepted, so the run would observe nothing quietly")
	}
}

// TestParseGraceLogKeepsWhatItCannotRead covers the stream as it arrives: an
// RFC 3339 timestamp from the container runtime, then the server's own line.
//
// Steps:
//  1. Parse a stream holding an entry, an exit, a line about grace in a wording
//     the rule does not cover, and a line with no timestamp at all.
//  2. Assert the signals carry the runtime's timestamp, not anything parsed out
//     of the server's text.
//  3. Assert the unreadable lines are kept rather than dropped, since a gap in
//     the classifier that presented as silence would be filed as a defect.
//  4. Assert lines before the window are dropped.
func TestParseGraceLogKeepsWhatItCannotRead(t *testing.T) {
	restore := *Cfg()
	t.Cleanup(func() { *Cfg() = restore })
	Cfg().GraceEnterPattern, Cfg().GraceExitPattern = "", ""
	if err := compileGracePatterns(); err != nil {
		t.Fatalf("resetting the wording flags: %v", err)
	}

	raw := stamp(1700000000) + " nfsd: starting 90-second grace period\n" +
		stamp(1700000090) + " nfsd: end of grace period\n" +
		stamp(1700000091) + " grace\n" +
		"no timestamp here, and it mentions grace\n" +
		stamp(1700000092) + " ordinary line about something else\n"

	obs := parseGraceLog(raw, time.Time{})
	if n := len(obs.Signals); n != 2 {
		t.Fatalf("parsed %d signals, want 2: %+v", n, obs.Signals)
	}
	if obs.Signals[0].Exit || !obs.Signals[0].At.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("first signal is %+v, want an entry at the runtime's timestamp", obs.Signals[0])
	}
	if !obs.Signals[1].Exit {
		t.Errorf("second signal is %+v, want an exit", obs.Signals[1])
	}
	// The bare "grace" line matches neither list, and the untimestamped line
	// cannot be placed against a fault. Both are reported.
	if n := len(obs.Unclassified); n != 2 {
		t.Errorf("kept %d unclassified lines, want 2: %v", n, obs.Unclassified)
	}

	// Anything before the window belongs to an earlier failover.
	if obs := parseGraceLog(raw, time.Unix(1700000001, 0)); len(obs.Entries()) != 0 {
		t.Errorf("an entry from before the window was counted: %+v", obs.Entries())
	}
}

// TestGraceWindowAndClockGuard covers the arithmetic every grace assertion
// rests on, including the case that has no window: grace entered and never
// observably left is the re-entry symptom, and it has to be representable
// rather than rounded off to a missing measurement.
//
// Steps:
//  1. Build an observation with an entry and a later exit, and assert the
//     window and its duration.
//  2. Assert a moment inside the window counts and one outside does not.
//  3. Assert a moment inside the window but within the guard band of a boundary
//     does not count, because the two ends came from different clocks.
//  4. Assert an entry with no exit yields no complete window, since that is the
//     re-entry symptom rather than a missing measurement.
func TestGraceWindowAndClockGuard(t *testing.T) {
	obs := GraceObservation{Signals: []GraceSignal{
		{At: time.Unix(1700000000, 0)},
		{At: time.Unix(1700000090, 0), Exit: true},
		{At: time.Unix(1700000300, 0)},
	}}
	w, ok := obs.Window()
	if !ok {
		t.Fatal("no complete window from an entry followed by an exit")
	}
	if got := w.Duration(); got != 90*time.Second {
		t.Errorf("window is %s, want 90s", got)
	}

	const guard = 5 * time.Second
	if !w.FirmlyContains(time.Unix(1700000045, 0), guard) {
		t.Error("a grant in the middle of grace was not counted, so the case would miss a violation")
	}
	if w.FirmlyContains(time.Unix(1700000200, 0), guard) {
		t.Error("a grant after grace ended was counted as inside it")
	}
	// Both ends of a window come from the server's node, the grant from a
	// client's. Near a boundary the two clocks cannot be told apart, and a
	// lawful grant reported as a protocol violation costs a week.
	if w.FirmlyContains(time.Unix(1700000087, 0), guard) {
		t.Error("a grant within the guard band of the exit was reported as unambiguously inside grace")
	}

	// The second entry has no exit after it: the re-entry symptom.
	tail := GraceObservation{Signals: obs.Signals[2:]}
	if _, ok := tail.Window(); ok {
		t.Error("an entry with no exit produced a complete window")
	}
	if n := len(obs.Entries()); n != 2 {
		t.Errorf("counted %d entries, want 2: the count is what tells a re-entry loop from an "+
			"ordinary grace period", n)
	}
}

// TestParseGraceLogSurvivesAnEnormousLine covers a stream holding one line too
// long for a buffered scanner. This is the failure that has no symptom: a
// scanner stops at such a line and the lines after it vanish with an error
// nobody reads, so a server that announced grace and then printed a stack dump
// would be reported as a server that never announced grace at all. That reading
// is the one this observer exists to prevent.
//
// Steps:
//  1. Build a stream with a grace entry, a line of two megabytes, then a grace
//     exit.
//  2. Parse it.
//  3. Assert both signals are there, so nothing after the long line was lost.
func TestParseGraceLogSurvivesAnEnormousLine(t *testing.T) {
	restore := *Cfg()
	t.Cleanup(func() { *Cfg() = restore })
	Cfg().GraceEnterPattern, Cfg().GraceExitPattern = "", ""
	if err := compileGracePatterns(); err != nil {
		t.Fatalf("resetting the wording flags: %v", err)
	}

	raw := stamp(1700000000) + " nfsd: starting 90-second grace period\n" +
		stamp(1700000001) + " " + strings.Repeat("x", 2*1024*1024) + "\n" +
		stamp(1700000090) + " nfsd: end of grace period\n"

	obs := parseGraceLog(raw, time.Time{})
	if n := len(obs.Signals); n != 2 {
		t.Fatalf("parsed %d signals across a two megabyte line, want 2: everything after that line "+
			"was dropped, which reads as a server that never announced grace", n)
	}
	if obs.Signals[0].Exit || !obs.Signals[1].Exit {
		t.Errorf("signals are %+v, want an entry then an exit", obs.Signals)
	}
}

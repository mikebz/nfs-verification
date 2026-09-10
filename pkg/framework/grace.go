package framework

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/slo"
)

// Grace is the interval after a restart in which the server accepts reclaims of
// state that existed before the crash and refuses everything new. It is the
// dominant term in every recovery number this suite reports, and a grace
// re-entry loop presents as a hung client in front of a healthy server, which
// the triage runbook calls the most common false diagnosis in this
// architecture. A suite that cannot see grace cannot tell the two apart.
//
// Grace is read from what the server publishes, never inferred from the fact
// that a client stalled: inferring it from a stall makes a case unable to tell
// grace from the failure that mimics it.
//
// The channel is the server's log stream through the Kubernetes API, which is
// the only one guaranteed present, timestamped by something other than the
// process under test, and readable without knowing anything about the server's
// deployment beyond which pods it runs. Metrics are the other channel the plan
// allows; nothing in the Kubernetes API states which port serves them or what
// the metric is called, so that stays open rather than being guessed at.

// GraceSignal is one observed transition.
type GraceSignal struct {
	// At is the timestamp the container runtime attached to the line, not a
	// time parsed out of the server's own wording. Log formats differ per
	// implementation and change between versions; runtime timestamps do not.
	At time.Time
	// Exit is false for a line announcing entry into grace and true for one
	// announcing the end of it.
	Exit bool
	// Line is the line itself, kept for the failure message.
	Line string
}

// GraceObservation is what the log stream said about grace over a window.
type GraceObservation struct {
	// Signals are the transitions, oldest first.
	Signals []GraceSignal
	// Unclassified holds lines that mention grace but could not be read as
	// either transition. A gap in the classifier that presented as "grace was
	// never observed" would be triaged as a server defect, so it is reported
	// instead.
	Unclassified []string
	// Sources names every stream that was read, including the ones that said
	// nothing, so a failure message can say where the suite looked.
	Sources []string
}

// Entries returns the signals announcing entry into grace, oldest first. Grace
// entered more than once for one failover is a re-entry loop, so the count
// matters as much as the first one's timestamp.
func (o GraceObservation) Entries() []GraceSignal {
	var out []GraceSignal
	for _, s := range o.Signals {
		if !s.Exit {
			out = append(out, s)
		}
	}
	return out
}

// Describe renders an observation for a log line or a failure message.
func (o GraceObservation) Describe() string {
	if len(o.Signals) == 0 {
		return fmt.Sprintf("no grace signal in %d server log stream(s): %s",
			len(o.Sources), strings.Join(o.Sources, ", "))
	}
	var parts []string
	for _, s := range o.Signals {
		word := "entered"
		if s.Exit {
			word = "left"
		}
		parts = append(parts, fmt.Sprintf("%s at %s (%q)", word, s.At.UTC().Format(time.RFC3339), s.Line))
	}
	return strings.Join(parts, " | ")
}

// GraceWindow is one grace period as observed: an entry and the exit that
// followed it. Entry and exit are kept as separate signals up to this point
// rather than as an interval, because an entry with no exit is the re-entry
// symptom and has to be representable.
type GraceWindow struct {
	Start, End time.Time
}

// Window returns the first entry and the first exit after it. The second return
// is false when grace was entered and never observably left, which is a finding
// rather than a missing measurement.
func (o GraceObservation) Window() (GraceWindow, bool) {
	var w GraceWindow
	started := false
	for _, s := range o.Signals {
		switch {
		case !s.Exit && !started:
			w.Start, started = s.At, true
		case s.Exit && started && s.At.After(w.Start):
			w.End = s.At
			return w, true
		}
	}
	return w, false
}

// Duration is how long grace lasted.
func (w GraceWindow) Duration() time.Duration { return w.End.Sub(w.Start) }

// FirmlyContains reports whether t is inside the window by more than guard at
// both ends. The two ends come from different clocks, so a moment near a
// boundary is not evidence of anything; see slo.ClockSkewGuard.
func (w GraceWindow) FirmlyContains(t time.Time, guard time.Duration) bool {
	return t.After(w.Start.Add(guard)) && t.Before(w.End.Add(-guard))
}

// String renders the window for a log line.
func (w GraceWindow) String() string {
	return fmt.Sprintf("%s to %s (%s)", w.Start.UTC().Format(time.RFC3339),
		w.End.UTC().Format(time.RFC3339), w.Duration().Round(time.Second))
}

// LogLine is one line of a container log stream. At is zero for a line that
// carried no runtime timestamp: nothing measures with such a line, since it
// cannot be placed against a fault, but one mentioning grace is still reported
// rather than dropped.
type LogLine struct {
	At   time.Time
	Text string
	// Source names the pod and container the line came from.
	Source string
}

// ServerLog reads the server pods' log streams from since onwards. The pods are
// resolved live: after a pod delete the stream that matters belongs to a pod
// that did not exist when the fault was injected.
//
// Where a container restarted in place, the previous container's stream is read
// too, since the pre-fault half of the story is there.
func ServerLog(ctx context.Context, c *Client, since time.Time) ([]LogLine, []string, error) {
	pods, err := ServerPods(ctx, c)
	if err != nil {
		return nil, nil, fmt.Errorf("discovering NFS server pods to read their logs: %w", err)
	}
	if len(pods) == 0 {
		return nil, nil, fmt.Errorf("no NFS server pods found, so there is no log stream to read")
	}
	// The caller's since is the workstation's clock and every line is stamped by
	// the server node's, so the cut is relaxed by the same guard band that
	// narrows a grace window. Dropping a real grace line because two clocks
	// disagree by a second would report a server that announced grace as one
	// that said nothing.
	cut := since
	if !cut.IsZero() {
		cut = cut.Add(-slo.ClockSkewGuard)
	}
	var lines []LogLine
	var sources []string
	for i := range pods {
		p := &pods[i]
		for _, ct := range p.Spec.Containers {
			for _, previous := range []bool{false, true} {
				raw, err := containerLog(ctx, c, p.Namespace, p.Name, ct.Name, previous, since)
				if err != nil {
					// A container with no predecessor has no previous stream,
					// and a pod that is still starting has no current one.
					// Neither is an error: the absence shows up as the signals
					// missing from the result.
					continue
				}
				where := p.Name + "/" + ct.Name
				if previous {
					where += " (previous)"
				}
				sources = append(sources, where)
				lines = append(lines, parseLogStream(raw, where, cut)...)
			}
		}
	}
	return lines, sources, nil
}

// FirstDated returns the earliest line carrying a timestamp. Read against a
// stream ServerLog already cut at a fault, it is the server's own answer to
// whether anything happened, which is what OBS-02 asks of it.
func FirstDated(lines []LogLine) (LogLine, bool) {
	best := LogLine{}
	found := false
	for _, l := range lines {
		if l.At.IsZero() {
			continue
		}
		if !found || l.At.Before(best.At) {
			best, found = l, true
		}
	}
	return best, found
}

// ObserveGrace reads the server pods' log streams from since onwards and
// classifies what they say about grace.
func ObserveGrace(ctx context.Context, c *Client, since time.Time) (GraceObservation, error) {
	lines, sources, err := ServerLog(ctx, c, since)
	if err != nil {
		return GraceObservation{}, err
	}
	obs := classifyGraceLines(lines)
	obs.Sources = sources
	return obs, nil
}

func sortSignals(s []GraceSignal) {
	sort.Slice(s, func(i, j int) bool { return s[i].At.Before(s[j].At) })
}

// containerLog streams one container's log with runtime timestamps.
func containerLog(ctx context.Context, c *Client, ns, pod, container string, previous bool, since time.Time) (string, error) {
	opts := &corev1.PodLogOptions{Container: container, Previous: previous, Timestamps: true}
	if !since.IsZero() {
		// A margin, because SinceTime is applied against the node's clock and
		// the caller's since came from the workstation's. Lines before the
		// window are dropped by timestamp below, where both sides are the
		// node's own.
		t := metav1.NewTime(since.Add(-logSinceMargin))
		opts.SinceTime = &t
	}
	rc, err := c.Kube.CoreV1().Pods(ns).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// logSinceMargin covers the difference between the workstation clock the caller
// passes and the node clock the API applies SinceTime against.
const logSinceMargin = 2 * time.Minute

// graceLine matches a line that says anything about grace at all. Everything
// after this is classification.
var graceLine = regexp.MustCompile(`(?i)grace`)

// exitWords and enterWords are the wordings servers use. The rule is
// positional: whichever word appears earliest in the line decides, and a tie
// goes to exit. That ordering is the whole point, because the common exit
// wordings are the entry wording with a negation in front of it ("NOT IN
// GRACE"), and a rule that tested for entry first would classify every exit as
// an entry and report a re-entry loop against a healthy server.
var (
	exitWords = []string{
		"not in grace", "no longer", "out of grace", "end", "exit", "over",
		"lift", "expire", "complete", "finish", "leav", "clear", "releas",
	}
	enterWords = []string{
		"in grace", "enter", "start", "begin", "init", "into grace",
	}
)

// classifyGrace decides what a line announces: exit is true for the end of
// grace, false for entry into it, and the second return is false for a line
// that announces neither. It is the one part of the observer that depends on
// how an implementation words things, which is why the grace pattern flags
// exist for a server it does not cover.
func classifyGrace(line string) (exit, ok bool) {
	if !graceLine.MatchString(line) {
		return false, false
	}
	if re := Cfg().graceExitRE; re != nil && re.MatchString(line) {
		return true, true
	}
	if re := Cfg().graceEnterRE; re != nil && re.MatchString(line) {
		return false, true
	}
	if Cfg().graceExitRE != nil || Cfg().graceEnterRE != nil {
		// An operator who states the wording owns it. Falling back to the
		// built-in rule here would classify lines their pattern deliberately
		// left out.
		return false, false
	}
	lower := strings.ToLower(line)
	exitAt := earliest(lower, exitWords)
	enterAt := earliest(lower, enterWords)
	switch {
	case exitAt < 0 && enterAt < 0:
		return false, false
	case enterAt < 0 || (exitAt >= 0 && exitAt <= enterAt):
		return true, true
	default:
		return false, true
	}
}

// earliest returns the index of the first of words to appear, or -1.
func earliest(lower string, words []string) int {
	best := -1
	for _, w := range words {
		if i := strings.Index(lower, w); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// parseLogStream splits a timestamped stream into lines. Lines older than since
// are dropped here, where the comparison is between two timestamps the node
// itself produced rather than between a node and the workstation.
//
// Split rather than scanned. The whole stream is already in memory by the time
// it arrives here, so a scanner buys nothing and brings a failure mode with it:
// it stops at a line longer than its buffer, and the lines after that one are
// dropped with no error anyone sees. A server that printed one enormous line
// would then look like a server that never announced grace, which is the
// reading this whole observer exists to prevent.
func parseLogStream(raw, source string, since time.Time) []LogLine {
	var out []LogLine
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		at, rest, ok := splitLogTimestamp(line)
		if !ok {
			out = append(out, LogLine{Text: line, Source: source})
			continue
		}
		if !since.IsZero() && at.Before(since) {
			continue
		}
		out = append(out, LogLine{At: at, Text: rest, Source: source})
	}
	return out
}

// classifyGraceLines turns a log stream into grace signals, keeping every line
// that mentions grace and matches nothing.
func classifyGraceLines(lines []LogLine) GraceObservation {
	var obs GraceObservation
	for _, l := range lines {
		// A line with no timestamp cannot be placed against a fault, and a
		// grace signal that cannot be placed is worse than none.
		exit, ok := classifyGrace(l.Text)
		if !ok || l.At.IsZero() {
			if graceLine.MatchString(l.Text) {
				obs.Unclassified = append(obs.Unclassified, l.Text)
			}
			continue
		}
		obs.Signals = append(obs.Signals, GraceSignal{At: l.At, Exit: exit, Line: strings.TrimSpace(l.Text)})
	}
	sortSignals(obs.Signals)
	return obs
}

// parseGraceLog is the whole path from a raw stream to grace signals, which is
// what the unit tests exercise without a cluster in front of them.
func parseGraceLog(raw string, since time.Time) GraceObservation {
	return classifyGraceLines(parseLogStream(raw, "", since))
}

// splitLogTimestamp peels off the RFC 3339 timestamp the container runtime puts
// in front of every line when Timestamps is set.
func splitLogTimestamp(line string) (time.Time, string, bool) {
	stamp, rest, found := strings.Cut(line, " ")
	if !found {
		return time.Time{}, "", false
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, "", false
	}
	return at, rest, true
}

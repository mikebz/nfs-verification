package framework

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Locks are advisory and are visible across nodes only because every client
// serializes through the one server. Nothing here asserts POSIX coherence: the
// guarantee under test is what NFSv4.1 specifies, not what a local filesystem
// provides.

// LockHolder is a lock held by a background process inside a pod.
type LockHolder struct {
	f     *Framework
	Pod   string
	Path  string
	ID    string
	state string
	run   string
}

// HoldFlock acquires an exclusive whole-file lock in a pod and keeps holding it
// until Release, or until the pod dies. It returns once the lock is confirmed
// held, so a caller never races its own fixture.
func (f *Framework) HoldFlock(ctx context.Context, pod, path, id string) (*LockHolder, error) {
	if err := CheckScriptID(id); err != nil {
		return nil, err
	}
	// Resolve the logical pod name once, here, so that State and Release cannot
	// drift from the pod the lock was taken in.
	l := &LockHolder{f: f, Pod: f.Name(pod), Path: path, ID: id,
		state: "/tmp/lock-" + id + ".state", run: "/tmp/lock-" + id + ".run"}
	script, err := RunScript("hold-flock.sh", id, path, l.run, l.state)
	if err != nil {
		return nil, err
	}
	if _, err := f.C.MustSh(ctx, Namespace, l.Pod, "main", script); err != nil {
		return nil, err
	}
	if err := Poll(ctx, FastPoll, 2*time.Minute, func(ctx context.Context) (bool, error) {
		s, err := l.State(ctx)
		if err != nil {
			return false, err
		}
		if s == "failed" {
			return false, fmt.Errorf("lock acquisition failed in %s", l.Pod)
		}
		return s == "held", nil
	}); err != nil {
		return nil, fmt.Errorf("holding lock %s in %s: %w", id, l.Pod, err)
	}
	return l, nil
}

// State returns the holder's state: held, released, failed, or empty while the
// acquisition is still blocked.
func (l *LockHolder) State(ctx context.Context) (string, error) {
	r := l.f.C.Sh(ctx, Namespace, l.Pod, "main", "cat "+shellQuote(l.state)+" 2>/dev/null || true")
	if r.Err != nil {
		return "", r.Err
	}
	return strings.TrimSpace(r.Stdout), nil
}

// Release drops the lock.
func (l *LockHolder) Release(ctx context.Context) error {
	_, err := l.f.C.MustSh(ctx, Namespace, l.Pod, "main", "rm -f "+shellQuote(l.run))
	return err
}

// Probe timings. One attempt per second, as for the workload: the windows
// these feed are thirty seconds and up, and a tighter loop would sharpen no
// assertion.
const (
	// lockWaitSeconds is how long one attempt waits for the lock itself.
	lockWaitSeconds = 2
	// lockBoundSeconds is the hard bound on one attempt, for a client that
	// blocks in the kernel rather than returning the server's retryable error.
	lockBoundSeconds = 20
)

// LockProbe is a background loop in a pod attempting a lock it has never held.
// It answers the question grace exists to make interesting: whether a server
// bars new state while it waits for old state to be reclaimed.
type LockProbe struct {
	f   *Framework
	Pod string
	log string
	run string
}

// StartLockProbe launches the probe and returns once it has made an attempt, so
// a fault injected afterwards lands on a probe that is demonstrably running.
//
// The probe reports in the same three-field format as the workload, so one
// parser serves both. In a probe report an OK record means the lock was granted
// at that moment, not that a write committed.
//
// The probe itself is scripts/lock-probe.sh.
func (f *Framework) StartLockProbe(ctx context.Context, pod, path, id string) (*LockProbe, error) {
	// Checked here as well as in RunScript, because the id becomes a filename
	// on the two lines below before RunScript ever sees it.
	if err := CheckScriptID(id); err != nil {
		return nil, err
	}
	p := &LockProbe{f: f, Pod: f.Name(pod),
		log: "/tmp/probe-" + id + ".log", run: "/tmp/probe-" + id + ".run"}
	script, err := RunScript("lock-probe.sh", id, path, p.run, p.log,
		strconv.Itoa(lockWaitSeconds), strconv.Itoa(lockBoundSeconds))
	if err != nil {
		return nil, err
	}
	if _, err := f.C.MustSh(ctx, Namespace, p.Pod, "main", script); err != nil {
		return nil, err
	}
	if err := Poll(ctx, FastPoll, 2*time.Minute, func(ctx context.Context) (bool, error) {
		rep, err := p.Report(ctx)
		if err != nil {
			return false, err
		}
		return len(rep.Records) > 0, fmt.Errorf("the lock probe in %s has not attempted anything yet", p.Pod)
	}); err != nil {
		return nil, fmt.Errorf("starting the lock probe in %s: %w", p.Pod, err)
	}
	return p, nil
}

// Report reads the probe log as it stands. Safe to call while it runs.
func (p *LockProbe) Report(ctx context.Context) (LoadReport, error) {
	out, err := p.f.C.MustSh(ctx, Namespace, p.Pod, "main", "cat "+shellQuote(p.log)+" 2>/dev/null || true")
	if err != nil {
		return LoadReport{}, err
	}
	return parseLoadLog(out), nil
}

// Stop ends the probe and returns its final log.
func (p *LockProbe) Stop(ctx context.Context) (LoadReport, error) {
	if _, err := p.f.C.MustSh(ctx, Namespace, p.Pod, "main", "rm -f "+shellQuote(p.run)); err != nil {
		return LoadReport{}, err
	}
	return p.Report(ctx)
}

// GrantsWithin returns the attempts granted inside a window, by the probe pod's
// clock. The window comes from the server's log stream, stamped by another
// node, so the caller narrows it by slo.ClockSkewGuard before asking.
func (r LoadReport) GrantsWithin(w GraceWindow, guard time.Duration) []LoadRecord {
	var out []LoadRecord
	for _, rec := range r.Records {
		if rec.OK && w.FirmlyContains(rec.At, guard) {
			out = append(out, rec)
		}
	}
	return out
}

// TryFlock attempts a non-blocking exclusive lock and reports whether it was
// granted. A refusal is the expected result in the mutual-exclusion cases, so
// this returns a bool rather than an error.
func (f *Framework) TryFlock(ctx context.Context, pod, path string) (bool, string, error) {
	r := f.Sh(ctx, pod, fmt.Sprintf(
		`exec 9>>%s; if flock -n -x 9; then echo GRANTED; flock -u 9; else echo REFUSED; fi`, shellQuote(path)))
	out := strings.TrimSpace(r.Stdout + r.Stderr)
	switch {
	case strings.Contains(out, "GRANTED"):
		return true, out, nil
	case strings.Contains(out, "REFUSED"):
		return false, out, nil
	}
	if r.Err != nil {
		return false, out, r.Err
	}
	return false, out, fmt.Errorf("unexpected flock output %q", out)
}

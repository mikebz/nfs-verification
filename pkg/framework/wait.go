package framework

import (
	"context"
	"fmt"
	"time"
)

// Common deadlines. Every wait in the suite takes one of these or an explicit
// SLO value, so that no case invents its own timeout.
const (
	PollInterval    = 2 * time.Second
	FastPoll        = 250 * time.Millisecond
	BindTimeout     = 120 * time.Second
	PodReadyTimeout = 5 * time.Minute
	DeleteTimeout   = 5 * time.Minute
	// PodTerminateTimeout bounds teardown's wait for pods to leave the API. It
	// is deliberately short: test pods have a 5 second grace period, so a pod
	// still present after this is a node that has stopped answering, and every
	// case paying five minutes for that exhausts the go test timeout. Both
	// halves of that are field findings: docs/findings.md F-001 for why a node
	// that stops answering is the dangerous state rather than a slow unmount,
	// and F-003 for a wrapper that wedged every terminating pod.
	PodTerminateTimeout = 90 * time.Second
	// ArtifactTimeout bounds one case's artifact collection.
	ArtifactTimeout = 60 * time.Second
	// EvidenceTimeout bounds the copy of the files a case named as its
	// evidence. Shorter than the bundle because it runs on every case rather
	// than only on failures, and because a file on a mount whose export is gone
	// never answers at all: the per-file bound is what keeps one of those from
	// spending this, and this is what keeps it from spending the cleanup.
	EvidenceTimeout = 30 * time.Second
	// ExpandTimeout bounds a volume expansion. Generous on purpose: expansion
	// is a control plane round trip through the driver, and on a shared server
	// it may be a quota change rather than a block resize.
	ExpandTimeout = 5 * time.Minute
	// SnapshotProbeTimeout bounds the wait for VolumeSnapshot ready status.
	SnapshotProbeTimeout = 15 * time.Second
	// ServerOutageObserveDuration is the window during which claim state is
	// observed while the server pod is known to be down.
	ServerOutageObserveDuration = 10 * time.Second
)

// Poll calls fn until it returns done, an error, or the deadline passes. The
// last error from fn is folded into the timeout message, because "timed out"
// with no cause is the least useful failure a suite can produce.
func Poll(ctx context.Context, interval, timeout time.Duration, fn func(context.Context) (bool, error)) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	var lastErr error
	timedOut := func() error {
		if lastErr != nil {
			return fmt.Errorf("timed out after %s: last error: %w", timeout, lastErr)
		}
		return fmt.Errorf("timed out after %s", timeout)
	}
	for {
		done, err := fn(ctx)
		if err != nil {
			lastErr = err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return timedOut()
		case <-ticker.C:
		}
	}
}

// Measure runs fn until it succeeds and reports how long that took. This is the
// primitive behind every recovery SLO assertion: time to first successful I/O.
func Measure(ctx context.Context, interval, timeout time.Duration, fn func(context.Context) error) (time.Duration, error) {
	start := time.Now()
	err := Poll(ctx, interval, timeout, func(ctx context.Context) (bool, error) {
		if err := fn(ctx); err != nil {
			return false, err
		}
		return true, nil
	})
	return time.Since(start), err
}

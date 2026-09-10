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

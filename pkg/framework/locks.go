package framework

import (
	"context"
	"fmt"
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
	// Resolve the logical pod name once, here, so that State and Release cannot
	// drift from the pod the lock was taken in.
	l := &LockHolder{f: f, Pod: f.Name(pod), Path: path, ID: id,
		state: "/tmp/lock-" + id + ".state", run: "/tmp/lock-" + id + ".run"}
	script := fmt.Sprintf(`
set -u
: > %[1]s
touch %[2]s
cat > /tmp/hold-%[3]s.sh <<'HOLDEOF'
exec 9>>"$1"
# Blocking acquire with no -w: busybox flock has no timeout flag, so the bound
# lives in the caller, which polls the state file.
if flock -x 9; then
  echo held > "$2"
  while [ -f "$3" ]; do sleep 1; done
  echo released > "$2"
else
  echo failed > "$2"
fi
HOLDEOF
setsid sh /tmp/hold-%[3]s.sh %[4]s %[1]s %[2]s >/dev/null 2>&1 </dev/null &
echo launched
`, shellQuote(l.state), shellQuote(l.run), id, shellQuote(path))
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

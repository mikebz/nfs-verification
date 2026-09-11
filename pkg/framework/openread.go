package framework

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// OpenReader is a file held open for reading by a background process inside a
// pod, which can read through that one descriptor on demand.
//
// It exists for the silly-rename case. The Linux client implements unlink of an
// open file by renaming it to .nfsXXXXXXXX and removing it when the last
// descriptor closes, and the only way to see that is to hold a descriptor from
// before the unlink and keep reading through it afterwards. Reopening the path
// is a different question with a different answer.
type OpenReader struct {
	f       *Framework
	Pod     string
	Path    string
	Chunk   int
	state   string
	run     string
	request string
	result  string
}

// ReadFailedPrefix marks a read the holder could not complete. The error text
// follows it, which is where ESTALE shows up.
const ReadFailedPrefix = "READ-FAILED"

// HoldOpenRead opens a file in a pod and keeps the descriptor until Close. It
// returns once the descriptor is open, so anything the case does next is racing
// the protocol rather than racing the harness.
//
// The reader itself is scripts/hold-open-read.sh.
func (f *Framework) HoldOpenRead(ctx context.Context, pod, path string, chunk int, id string) (*OpenReader, error) {
	if err := CheckScriptID(id); err != nil {
		return nil, err
	}
	r := &OpenReader{f: f, Pod: f.Name(pod), Path: path, Chunk: chunk,
		state: "/tmp/openr-" + id + ".state", run: "/tmp/openr-" + id + ".run",
		request: "/tmp/openr-" + id + ".req", result: "/tmp/openr-" + id + ".out"}
	script, err := RunScript("hold-open-read.sh", id, path, strconv.Itoa(chunk), r.run, r.state, r.request, r.result)
	if err != nil {
		return nil, err
	}
	if _, err := f.C.MustSh(ctx, Namespace, r.Pod, "main", script); err != nil {
		return nil, err
	}
	if err := Poll(ctx, FastPoll, time.Minute, func(ctx context.Context) (bool, error) {
		s, err := r.State(ctx)
		if err != nil {
			return false, err
		}
		return s == "open", fmt.Errorf("the reader in %s is %q, not open", r.Pod, s)
	}); err != nil {
		return nil, fmt.Errorf("holding %s open for reading in %s: %w", path, r.Pod, err)
	}
	return r, nil
}

// State returns open, closed, or empty while the descriptor is still being
// opened.
func (r *OpenReader) State(ctx context.Context) (string, error) {
	res := r.f.C.Sh(ctx, Namespace, r.Pod, "main", "cat "+shellQuote(r.state)+" 2>/dev/null || true")
	if res.Err != nil {
		return "", res.Err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Next reads one chunk through the held descriptor, from wherever the previous
// read left it.
//
// A failed read comes back as text beginning with ReadFailedPrefix rather than
// as an error, because a failure is one of the two correct answers in the
// cross-node half of the case: a descriptor to a file another client removed
// may legitimately return ESTALE.
func (r *OpenReader) Next(ctx context.Context, timeout time.Duration) (string, error) {
	if _, err := r.f.C.MustSh(ctx, Namespace, r.Pod, "main",
		fmt.Sprintf("rm -f %s; : > %s", shellQuote(r.result), shellQuote(r.request))); err != nil {
		return "", err
	}
	var out string
	err := Poll(ctx, FastPoll, timeout, func(ctx context.Context) (bool, error) {
		res := r.f.C.Sh(ctx, Namespace, r.Pod, "main",
			fmt.Sprintf("[ -f %s ] && cat %s", shellQuote(r.result), shellQuote(r.result)))
		if res.Err != nil {
			// A missing result file exits non-zero; that is the wait, not a
			// failure. Anything the reader actually said comes back below.
			return false, fmt.Errorf("the reader in %s has not answered yet", r.Pod)
		}
		out = res.Stdout
		return true, nil
	})
	if err != nil {
		return "", fmt.Errorf("reading a chunk through the held descriptor in %s: %w", r.Pod, err)
	}
	return out, nil
}

// Close closes the descriptor and waits for the close to have happened, which
// is the event a silly-rename entry is cleaned up by.
func (r *OpenReader) Close(ctx context.Context) error {
	if _, err := r.f.C.MustSh(ctx, Namespace, r.Pod, "main", "rm -f "+shellQuote(r.run)); err != nil {
		return err
	}
	return Poll(ctx, FastPoll, time.Minute, func(ctx context.Context) (bool, error) {
		s, err := r.State(ctx)
		if err != nil {
			return false, err
		}
		return s == "closed", fmt.Errorf("the reader in %s is %q, not closed", r.Pod, s)
	})
}

// SillyRenames returns the .nfsXXXXXXXX entries in a directory, which is how
// the Linux client keeps a file alive for a descriptor after its name is gone.
func (f *Framework) SillyRenames(ctx context.Context, pod, dir string) ([]string, error) {
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main",
		fmt.Sprintf("find %s -mindepth 1 -maxdepth 1 -name '.nfs*' 2>/dev/null || true", shellQuote(dir)))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

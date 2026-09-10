package framework

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// WriteFile writes size bytes of deterministic, seeded content at path and
// returns its sha256. Deterministic content matters: a checksum mismatch after
// a failover has to be reproducible to be filed.
func (f *Framework) WriteFile(ctx context.Context, pod, path string, sizeBytes int, seed string) (string, error) {
	script := fmt.Sprintf(
		`set -e; mkdir -p "$(dirname %[1]s)"; `+
			`yes %[3]s | head -c %[2]d > %[1]s; `+
			`sync; sha256sum %[1]s | cut -d' ' -f1`,
		shellQuote(path), sizeBytes, shellQuote(seed))
	return f.C.MustSh(ctx, Namespace, f.Name(pod), "main", script)
}

// Sha256 returns the checksum of a file as the pod sees it.
func (f *Framework) Sha256(ctx context.Context, pod, path string) (string, error) {
	return f.C.MustSh(ctx, Namespace, f.Name(pod), "main",
		fmt.Sprintf("sha256sum %s | cut -d' ' -f1", shellQuote(path)))
}

// Quote exposes shell quoting to test packages building their own scripts.
func Quote(s string) string { return shellQuote(s) }

// OpenWriter is a file written to through a descriptor a background process
// inside a pod keeps open. It exists for the negative close-to-open case: the
// boundary being documented is what another client may see *before* the writer
// closes, and an ordinary exec closes the file the moment it returns.
type OpenWriter struct {
	f     *Framework
	Pod   string
	Path  string
	state string
	run   string
}

// HoldOpenWrite writes payload at path and keeps the descriptor open until
// Close. It returns once the write has been issued, so a reader started
// afterwards is racing the protocol rather than racing the harness.
func (f *Framework) HoldOpenWrite(ctx context.Context, pod, path, payload, id string) (*OpenWriter, error) {
	if err := CheckScriptID(id); err != nil {
		return nil, err
	}
	// The pod name is resolved once, here, so State and Close cannot address a
	// different pod from the one holding the descriptor.
	w := &OpenWriter{f: f, Pod: f.Name(pod), Path: path,
		state: "/tmp/openw-" + id + ".state", run: "/tmp/openw-" + id + ".run"}
	if _, err := f.C.MustSh(ctx, Namespace, w.Pod, "main",
		openWriteScript(w.state, w.run, id, path, payload)); err != nil {
		return nil, err
	}
	if err := Poll(ctx, FastPoll, time.Minute, func(ctx context.Context) (bool, error) {
		s, err := w.State(ctx)
		if err != nil {
			return false, err
		}
		return s == "open", fmt.Errorf("writer in %s is %q, not open", w.Pod, s)
	}); err != nil {
		return nil, fmt.Errorf("holding %s open in %s: %w", path, w.Pod, err)
	}
	return w, nil
}

// openWriteScript renders the background writer. State and run files live on
// the pod's own filesystem, never on the share: a case that reads its own
// progress through the filesystem under test cannot tell a harness stall from a
// storage stall. The writer is detached with setsid so that it outlives the
// exec that launched it, which is the whole point of the helper.
func openWriteScript(state, run, id, path, payload string) string {
	// Every value reaching the shell is quoted, the derived script path
	// included. Ids are written by cases rather than by users, but a helper
	// that is safe only because of who calls it is one refactor away from not
	// being safe at all.
	return fmt.Sprintf(`
set -u
: > %[1]s
touch %[2]s
cat > %[3]s <<'OPENEOF'
exec 8> "$1"
printf '%%s' "$2" >&8
echo open > "$4"
while [ -f "$3" ]; do sleep 1; done
exec 8>&-
echo closed > "$4"
OPENEOF
setsid sh %[3]s %[4]s %[5]s %[2]s %[1]s >/dev/null 2>&1 </dev/null &
echo launched
`, shellQuote(state), shellQuote(run), shellQuote("/tmp/openw-"+id+".sh"), shellQuote(path), shellQuote(payload))
}

// State returns open, closed, or empty while the write is still in flight.
func (w *OpenWriter) State(ctx context.Context) (string, error) {
	r := w.f.C.Sh(ctx, Namespace, w.Pod, "main", "cat "+shellQuote(w.state)+" 2>/dev/null || true")
	if r.Err != nil {
		return "", r.Err
	}
	return strings.TrimSpace(r.Stdout), nil
}

// Close closes the descriptor and waits for the close to have happened, which
// is the event the close-to-open guarantee hangs off.
func (w *OpenWriter) Close(ctx context.Context) error {
	if _, err := w.f.C.MustSh(ctx, Namespace, w.Pod, "main", "rm -f "+shellQuote(w.run)); err != nil {
		return err
	}
	return Poll(ctx, FastPoll, time.Minute, func(ctx context.Context) (bool, error) {
		s, err := w.State(ctx)
		if err != nil {
			return false, err
		}
		return s == "closed", fmt.Errorf("writer in %s is %q, not closed", w.Pod, s)
	})
}

// Owner is file ownership as one client sees it. Both forms are carried: the
// numeric ids are the assertion, and the names are what makes a failure
// readable, since the classic one reads back as nobody.
type Owner struct {
	UID, GID    int
	User, Group string
}

func (o Owner) String() string { return fmt.Sprintf("%d(%s):%d(%s)", o.UID, o.User, o.GID, o.Group) }

// StatOwner reports the ownership of a path as a given pod sees it. Which pod
// asks matters: an id that is correct on the writer and wrong on the reader is
// a client-side mapping problem, not a server-side one.
func (f *Framework) StatOwner(ctx context.Context, pod, path string) (Owner, error) {
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main",
		fmt.Sprintf("stat -c '%%u %%g %%U %%G' %s", shellQuote(path)))
	if err != nil {
		return Owner{}, err
	}
	return parseOwner(out)
}

func parseOwner(out string) (Owner, error) {
	fields := strings.Fields(out)
	if len(fields) != 4 {
		return Owner{}, fmt.Errorf("unexpected stat output %q", out)
	}
	uid, err := strconv.Atoi(fields[0])
	if err != nil {
		return Owner{}, fmt.Errorf("unexpected uid in stat output %q: %w", out, err)
	}
	gid, err := strconv.Atoi(fields[1])
	if err != nil {
		return Owner{}, fmt.Errorf("unexpected gid in stat output %q: %w", out, err)
	}
	return Owner{UID: uid, GID: gid, User: fields[2], Group: fields[3]}, nil
}

// Capacity is what df reports for a mount, in bytes.
type Capacity struct{ TotalBytes, AvailBytes int64 }

// MountCapacity reports capacity as the workload sees it, which is the only
// view that matters to the expansion and capacity cases: the control plane's
// number is a claim, and this is whether the claim reached the application.
//
// It asks statfs, through `stat -f`, and falls back to df. statfs is preferred
// because its output is three integers and nothing else: no header, no device
// column, no server address, nothing that moves when an export is renamed or a
// tool changes its layout. df is kept as a fallback for an image whose stat was
// built without -f, which is the one thing statfs cannot survive.
func (f *Framework) MountCapacity(ctx context.Context, pod, path string) (Capacity, error) {
	if r := f.Sh(ctx, pod, fmt.Sprintf("stat -f -c '%%b %%a %%S' %s", shellQuote(path))); r.Err == nil {
		c, err := parseStatFS(r.Stdout)
		if err == nil {
			return c, nil
		}
	}
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main",
		fmt.Sprintf("df -P -k %s", shellQuote(path)))
	if err != nil {
		return Capacity{}, fmt.Errorf("neither stat -f nor df could read the capacity of %s: %w", path, err)
	}
	return parseDF(out)
}

// parseStatFS reads `stat -f -c '%b %a %S'`: total data blocks, blocks
// available to an ordinary user, and the fundamental block size.
func parseStatFS(out string) (Capacity, error) {
	fields := strings.Fields(out)
	if len(fields) != 3 {
		return Capacity{}, fmt.Errorf("unexpected statfs output %q", out)
	}
	nums := make([]int64, 3)
	for i, f := range fields {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return Capacity{}, fmt.Errorf("unexpected statfs output %q: %w", out, err)
		}
		nums[i] = n
	}
	blocks, avail, size := nums[0], nums[1], nums[2]
	// A zero block size means stat printed something that is not a block size,
	// which on an image without -f is the literal format string. Reporting a
	// capacity of zero would read as a full volume.
	if size <= 0 {
		return Capacity{}, fmt.Errorf("statfs reported a block size of %d in %q", size, out)
	}
	return Capacity{TotalBytes: blocks * size, AvailBytes: avail * size}, nil
}

// parseDF reads the POSIX df format: header, then one line per filesystem with
// 1K blocks in field 2 and available blocks in field 4.
func parseDF(out string) (Capacity, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return Capacity{}, fmt.Errorf("no filesystem line in df output %q", out)
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return Capacity{}, fmt.Errorf("unexpected df line %q", lines[len(lines)-1])
	}
	total, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return Capacity{}, fmt.Errorf("unexpected block count in df line %q: %w", lines[len(lines)-1], err)
	}
	avail, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return Capacity{}, fmt.Errorf("unexpected available count in df line %q: %w", lines[len(lines)-1], err)
	}
	return Capacity{TotalBytes: total * 1024, AvailBytes: avail * 1024}, nil
}

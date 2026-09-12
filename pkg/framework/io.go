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

// WriteBytes writes size bytes at path and reports how many actually landed.
//
// The count comes from the filesystem rather than from the request. `head -c`
// on a volume with less room than that writes what fits and exits without
// complaint, so a case that compared a usage reading against the size it asked
// for would be comparing it against bytes nobody wrote.
func (f *Framework) WriteBytes(ctx context.Context, pod, path string, sizeBytes int64, seed string) (int64, error) {
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main", writeBytesScript(path, sizeBytes, seed))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("asking %s how large %s ended up: the pod said %q", f.Name(pod), path, out)
	}
	return n, nil
}

// writeBytesScript is what WriteBytes runs in the pod. Separate from the call
// so that a quoting slip fails under a shell on a workstation rather than
// inside a case that was measuring something else.
func writeBytesScript(path string, sizeBytes int64, seed string) string {
	return fmt.Sprintf(
		`set -e; mkdir -p "$(dirname %[1]s)"; `+
			`yes %[3]s | head -c %[2]d > %[1]s; `+
			`sync; stat -c %%s %[1]s`,
		shellQuote(path), sizeBytes, shellQuote(seed))
}

// Sha256 returns the checksum of a file as the pod sees it.
func (f *Framework) Sha256(ctx context.Context, pod, path string) (string, error) {
	return f.C.MustSh(ctx, Namespace, f.Name(pod), "main",
		fmt.Sprintf("sha256sum %s | cut -d' ' -f1", shellQuote(path)))
}

// FileIdentity returns a file's device and inode as a pod sees them.
//
// It is how a lock line in /proc/locks is matched to the file a case locked.
// The table covers every file on every filesystem on the node, so neither a
// byte range nor an inode number alone is identity: the same offsets in another
// file, or the same inode number on another device, would satisfy an assertion
// that compared only one of them.
//
// A device that cannot be read is not an error. stat's device field is the one
// part of this a stripped-down image might not print, and an identity with only
// an inode still rules out almost everything; the caller reports that it is
// matching on less.
func (f *Framework) FileIdentity(ctx context.Context, pod, path string) (FileID, error) {
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main",
		fmt.Sprintf("stat -c '%%i %%d' %s", shellQuote(path)))
	if err != nil {
		return FileID{}, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return FileID{}, fmt.Errorf("stat reported nothing for %s", path)
	}
	inode, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return FileID{}, fmt.Errorf("stat reported %q as the inode of %s, which is not a number", out, path)
	}
	id := FileID{Inode: inode}
	if len(fields) > 1 {
		if dev, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
			id.Device = DeviceFromStatDev(dev)
		}
	}
	return id, nil
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
//
// The writer itself is scripts/hold-open-write.sh.
func (f *Framework) HoldOpenWrite(ctx context.Context, pod, path, payload, id string) (*OpenWriter, error) {
	// The pod name is resolved once, here, so State and Close cannot address a
	// different pod from the one holding the descriptor.
	w := &OpenWriter{f: f, Pod: f.Name(pod), Path: path,
		state: "/tmp/openw-" + id + ".state", run: "/tmp/openw-" + id + ".run"}
	script, err := RunScript("hold-open-write.sh", id, path, payload, w.run, w.state)
	if err != nil {
		return nil, err
	}
	if _, err := f.C.MustSh(ctx, Namespace, w.Pod, "main", script); err != nil {
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
		fmt.Sprintf("stat -c '%%u|%%g|%%U|%%G' %s", shellQuote(path)))
	if err != nil {
		return Owner{}, err
	}
	return parseOwner(out)
}

// parseOwner reads the delimited form of `stat -c '%u|%g|%U|%G'`. The delimiter
// is not decoration: an environment resolving names through LDAP, Active
// Directory or an NSS module hands back names with spaces in them, and
// splitting an owner like "Domain Users" on whitespace turns a correct
// ownership into a parse failure.
func parseOwner(out string) (Owner, error) {
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) != 4 {
		return Owner{}, fmt.Errorf("unexpected stat output %q", out)
	}
	uid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil {
		return Owner{}, fmt.Errorf("unexpected uid in stat output %q: %w", out, err)
	}
	gid, err := strconv.Atoi(strings.TrimSpace(fields[1]))
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
	row, err := parseDF(out)
	if err != nil {
		return Capacity{}, err
	}
	return Capacity{TotalBytes: row.TotalBytes, AvailBytes: row.AvailBytes}, nil
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

// dfRow is one filesystem line of `df -P -k`, in bytes.
//
// Used is carried rather than derived from the other two. A filesystem with
// reserved blocks reports a used count that does not equal capacity minus
// available, and a case comparing two sources of one quantity must not invent
// agreement by computing one of the numbers it is comparing.
type dfRow struct{ TotalBytes, UsedBytes, AvailBytes int64 }

// parseDF reads the POSIX df format: header, then one line per filesystem with
// 1K blocks in field 2, used blocks in field 3 and available blocks in field 4.
func parseDF(out string) (dfRow, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return dfRow{}, fmt.Errorf("no filesystem line in df output %q", out)
	}
	line := lines[len(lines)-1]
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return dfRow{}, fmt.Errorf("unexpected df line %q", line)
	}
	names := []string{"", "block count", "used count", "available count"}
	nums := make([]int64, 4)
	for i := 1; i < 4; i++ {
		n, err := strconv.ParseInt(fields[i], 10, 64)
		if err != nil {
			return dfRow{}, fmt.Errorf("unexpected %s in df line %q: %w", names[i], line, err)
		}
		nums[i] = n
	}
	return dfRow{TotalBytes: nums[1] * 1024, UsedBytes: nums[2] * 1024, AvailBytes: nums[3] * 1024}, nil
}

// Direct I/O bypasses the page cache on both ends, which is the only way a
// case can say that what it read came from the server rather than from the
// client that wrote it. A read that returns zeros never crossed.
//
// Both helpers assume the flags parse. Probe with ProbeDirectIO first: an image
// whose dd was built without FEATURE_DD_IBS_OBS reports blocked rather than
// failing, because a missing flag is a fact about the image and not a defect in
// the storage.

// WriteDirect writes one block of deterministic content at a block offset with
// O_DIRECT, and returns the block's sha256.
//
// The block is built on the pod's own filesystem and copied into place, because
// a direct write needs a whole aligned block from a file rather than a stream.
// conv=notrunc so that a pod writing a later block does not remove an earlier
// one written by another pod.
func (f *Framework) WriteDirect(ctx context.Context, pod, path, seed string, block, offset int) (string, error) {
	script := fmt.Sprintf(
		`set -e; mkdir -p "$(dirname %[1]s)"; `+
			`yes %[2]s | head -c %[3]d > /tmp/nfsv-direct-block; `+
			`dd if=/tmp/nfsv-direct-block of=%[1]s bs=%[3]d count=1 seek=%[4]d oflag=direct conv=notrunc 2>/dev/null; `+
			`sha256sum /tmp/nfsv-direct-block | cut -d' ' -f1`,
		shellQuote(path), shellQuote(seed), block, offset)
	return f.C.MustSh(ctx, Namespace, f.Name(pod), "main", script)
}

// CountNonZeroBytes reads one block at a block offset and returns how many of
// its bytes are not zero.
//
// The read lands in a file before it is counted, for the reason ReadDirect does
// the same: a POSIX pipeline reports its last command's status, so piping a
// failed read into a counter yields a confident zero, and "the hole read back
// as zeros" would be indistinguishable from "the hole could not be read".
func (f *Framework) CountNonZeroBytes(ctx context.Context, pod, path string, block, offset int) (int, error) {
	const scratch = "/tmp/nfsv-hole-read"
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main", fmt.Sprintf(
		`set -e; dd if=%s of=%s bs=%d count=1 skip=%d 2>/dev/null; tr -d '\000' < %s | wc -c`,
		shellQuote(path), shellQuote(scratch), block, offset, shellQuote(scratch)))
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("counting the non-zero bytes of block %d of %s: the pod said %q", offset, path, out)
	}
	return n, nil
}

// ReadDirect reads one block at a block offset with O_DIRECT and returns its
// sha256, as that pod sees it.
//
// The read lands in a file on the pod's own filesystem before it is hashed,
// rather than being piped straight into sha256sum. A POSIX pipeline reports the
// status of its last command, so a failed direct read would be hashed as an
// empty stream and come back as a valid-looking checksum of nothing. The case
// would then report a data mismatch where the truth is that the read failed,
// which is a different defect filed against a different owner.
func (f *Framework) ReadDirect(ctx context.Context, pod, path string, block, offset int) (string, error) {
	const scratch = "/tmp/nfsv-direct-read"
	script := fmt.Sprintf(
		`set -e; dd if=%s of=%s bs=%d count=1 skip=%d iflag=direct 2>/dev/null; `+
			`sha256sum %s | cut -d' ' -f1`,
		shellQuote(path), shellQuote(scratch), block, offset, shellQuote(scratch))
	return f.C.MustSh(ctx, Namespace, f.Name(pod), "main", script)
}

package framework

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// locktool is a byte-range lock tool this repository builds and streams into an
// existing pod. Nothing on a stock image can take an fcntl byte-range lock:
// flock(1) calls flock(2), which has no range argument, and util-linux ships no
// byte-range lock command. cmd/locktool has the candidates that were checked.
//
// It is never built into an image. A binary this repository owns, gated on a
// registry an operator must populate, would mean the byte-range cases never run
// anywhere, and a case that is skipped everywhere is a case that does not exist.
// So it is compiled per node architecture by `make locktool`, streamed in over
// pods/exec with stdin attached, and verified by checksum on the far side.

// LockToolPath is where the binary lands inside a pod. On the container
// filesystem, never on the share: putting the tool that tests the filesystem on
// the filesystem under test is the same mistake as logging progress there.
const LockToolPath = "/tmp/locktool"

// LockKind is the application API a lock is taken with. The distinction matters
// because the mount options that make locking local are per-kind: one mount can
// send byte-range locks to the server while keeping flock locks on the client.
type LockKind string

const (
	// FlockLock is flock(2): whole file, released when the last descriptor of
	// the open file description closes.
	FlockLock LockKind = "flock"
	// PosixLock is fcntl(2) F_SETLK: a byte range, released when any descriptor
	// to the file is closed by the process holding it.
	PosixLock LockKind = "fcntl byte-range"
)

// LockRange is a range of a file and the kind of lock asked for on it.
type LockRange struct {
	Start int64
	// Len of 0 means "to end of file", which is how a whole-file fcntl lock is
	// expressed. It is not the same as an empty range.
	Len  int64
	Mode string // read or write
}

// WriteRange returns an exclusive lock on a range.
func WriteRange(start, length int64) LockRange {
	return LockRange{Start: start, Len: length, Mode: "write"}
}

// ReadRange returns a shared lock on a range.
func ReadRange(start, length int64) LockRange {
	return LockRange{Start: start, Len: length, Mode: "read"}
}

func (r LockRange) String() string {
	if r.Len == 0 {
		return fmt.Sprintf("%s [%d..EOF)", r.Mode, r.Start)
	}
	return fmt.Sprintf("%s [%d..%d)", r.Mode, r.Start, r.Start+r.Len)
}

// args renders the range as locktool takes it.
func (r LockRange) args() []string {
	return []string{strconv.FormatInt(r.Start, 10), strconv.FormatInt(r.Len, 10), r.Mode}
}

// LockAnswer is one line of locktool output, parsed.
type LockAnswer struct {
	// Free is true for GRANTED from try and FREE from getlk: in both the range
	// was available at that moment.
	Free bool
	// Conflict describes the lock in the way. Known is false when the tool
	// reported a refusal it could no longer attribute, which happens when the
	// conflicting lock cleared between the acquire and the query.
	Conflict LockConflict
	Raw      string
}

// LockConflict is the lock standing in the way of a request.
//
// There is no pid, deliberately. NFSv4.1 does not carry one: a denied LOCK or
// LOCKT gives the conflicting offset, length and type plus an opaque lock
// owner, and nothing that identifies a process on another node. Which pod on
// which node is expected to hold a range is something the harness knows because
// it put it there, so the case carries that and its failure message names it.
type LockConflict struct {
	Known bool
	Mode  string // read or write
	Start int64
	Len   int64
}

func (c LockConflict) String() string {
	if !c.Known {
		return "an unnamed lock that had already cleared when the holder was asked for"
	}
	return LockRange{Start: c.Start, Len: c.Len, Mode: c.Mode}.String()
}

// ParseLockAnswer reads one line of locktool output. A pure function over the
// tool's four shapes, so that a parser change fails on a workstation rather
// than inside a case measuring something else.
func ParseLockAnswer(out string) (LockAnswer, error) {
	line := strings.TrimSpace(out)
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return LockAnswer{}, fmt.Errorf("locktool printed nothing")
	}
	ans := LockAnswer{Raw: line}
	switch fields[0] {
	case "GRANTED", "FREE":
		if len(fields) != 1 {
			return LockAnswer{}, fmt.Errorf("locktool printed %q, which carries fields %s does not take", line, fields[0])
		}
		ans.Free = true
		return ans, nil
	case "REFUSED", "HELD":
	default:
		return LockAnswer{}, fmt.Errorf("locktool printed %q, which is none of GRANTED, REFUSED, FREE or HELD", line)
	}
	// A bare REFUSED is the one shape with no conflict to name.
	if len(fields) == 1 && fields[0] == "REFUSED" {
		return ans, nil
	}
	c, err := parseConflict(fields[1:])
	if err != nil {
		return LockAnswer{}, fmt.Errorf("locktool printed %q: %w", line, err)
	}
	ans.Conflict = c
	return ans, nil
}

// parseConflict reads the type, start and len fields of a refusal.
func parseConflict(fields []string) (LockConflict, error) {
	c := LockConflict{Known: true}
	var seen int
	for _, f := range fields {
		key, val, ok := strings.Cut(f, "=")
		if !ok {
			return LockConflict{}, fmt.Errorf("field %q is not key=value", f)
		}
		var err error
		switch key {
		case "type":
			switch val {
			case "r":
				c.Mode = "read"
			case "w":
				c.Mode = "write"
			default:
				return LockConflict{}, fmt.Errorf("type %q is neither r nor w", val)
			}
		case "start":
			c.Start, err = strconv.ParseInt(val, 10, 64)
		case "len":
			c.Len, err = strconv.ParseInt(val, 10, 64)
		default:
			return LockConflict{}, fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			return LockConflict{}, fmt.Errorf("field %q: %w", f, err)
		}
		seen++
	}
	if seen != 3 {
		return LockConflict{}, fmt.Errorf("want type, start and len, got %d fields", seen)
	}
	return c, nil
}

// LockToolBinary returns the path `make locktool` builds for an architecture.
func LockToolBinary(arch string) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", fmt.Errorf("locating the repository root for the locktool build: %w", err)
	}
	return filepath.Join(root, "bin", "locktool-linux-"+arch), nil
}

// NodeArch reports the architecture a node runs, which is what decides which
// locktool binary it can execute. Read from the node rather than assumed: a
// cluster with mixed nodes is ordinary, and a binary for the wrong one fails
// with an exec format error that says nothing about why.
func (f *Framework) NodeArch(ctx context.Context, node string) (string, error) {
	n, err := f.C.Kube.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if n.Status.NodeInfo.Architecture == "" {
		return "", fmt.Errorf("node %s reports no architecture", node)
	}
	return n.Status.NodeInfo.Architecture, nil
}

// EnsureLockTool puts the binary in a pod and proves it arrived intact. Once
// per pod, not once per call: the copy is a megabyte and a half over an exec
// stream, and every lock operation would otherwise pay for it.
func (f *Framework) EnsureLockTool(ctx context.Context, pod string) error {
	name := f.Name(pod)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, done := f.lockTool[name]; done {
		return err
	}
	err := f.installLockTool(ctx, name)
	if f.lockTool == nil {
		f.lockTool = map[string]error{}
	}
	f.lockTool[name] = err
	return err
}

// installLockTool streams the binary in and verifies it by checksum. A silently
// wrong binary is worse than a missing one: a truncated stream fails here,
// naming the pod, rather than as an unexplained exec error inside an assertion.
func (f *Framework) installLockTool(ctx context.Context, pod string) error {
	p, err := f.C.Kube.CoreV1().Pods(Namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading pod %s to find its node: %w", pod, err)
	}
	if p.Spec.NodeName == "" {
		return fmt.Errorf("pod %s is not scheduled, so its architecture is unknown", pod)
	}
	arch, err := f.NodeArch(ctx, p.Spec.NodeName)
	if err != nil {
		return fmt.Errorf("reading the architecture of node %s: %w", p.Spec.NodeName, err)
	}
	path, err := LockToolBinary(arch)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no locktool built for %s, which is what node %s runs: %w. "+
			"Run `make locktool`, which builds one per architecture into bin/",
			arch, p.Spec.NodeName, err)
	}
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	// Written to a temporary name and moved into place, so a stream that died
	// halfway never leaves an executable at the path a case will run.
	part := LockToolPath + ".part"
	install := fmt.Sprintf("set -e; cat > %s; chmod +x %s; mv %s %s",
		shellQuote(part), shellQuote(part), shellQuote(part), shellQuote(LockToolPath))
	r := f.C.ExecStdin(ctx, Namespace, pod, "main", bytes.NewReader(body), "sh", "-c", install)
	if r.Err != nil {
		return fmt.Errorf("streaming locktool into %s: %w: %s", pod, r.Err, r.Combined())
	}
	got, err := f.C.MustSh(ctx, Namespace, pod, "main",
		fmt.Sprintf("sha256sum %s | cut -d' ' -f1", shellQuote(LockToolPath)))
	if err != nil {
		return fmt.Errorf("checksumming locktool in %s: %w", pod, err)
	}
	if got != want {
		return fmt.Errorf("locktool arrived in %s as %s, want %s: the exec stream did not deliver "+
			"the whole binary, and running it would fail in a way that says nothing about why", pod, got, want)
	}
	return nil
}

// lockToolCmd builds the shell for one locktool invocation.
func lockToolCmd(sub, path string, r LockRange, extra ...string) string {
	args := append([]string{sub, path}, r.args()...)
	args = append(args, extra...)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return shellQuote(LockToolPath) + " " + strings.Join(quoted, " ")
}

// TryLock attempts a byte-range lock and reports the answer. A refusal is the
// expected result in half the cases that call this, so it is an answer rather
// than an error, and the range and type of the lock in the way come back with
// it.
func (f *Framework) TryLock(ctx context.Context, pod, path string, r LockRange) (LockAnswer, error) {
	return f.runLockTool(ctx, pod, "try", path, r)
}

// GetLock asks who holds a range without taking it.
//
// This is why the tool has both. F_SETLK answers the same question by
// acquiring, which changes the state every later attempt observes; a case
// asserting that a range is *still* held by its original holder cannot use a
// probe that takes the lock to find out.
func (f *Framework) GetLock(ctx context.Context, pod, path string, r LockRange) (LockAnswer, error) {
	return f.runLockTool(ctx, pod, "getlk", path, r)
}

func (f *Framework) runLockTool(ctx context.Context, pod, sub, path string, r LockRange) (LockAnswer, error) {
	if err := f.EnsureLockTool(ctx, pod); err != nil {
		return LockAnswer{}, err
	}
	res := f.Sh(ctx, pod, lockToolCmd(sub, path, r))
	// Exit 1 is a refusal and exit 0 a grant; both are answers. Anything else
	// is the tool failing, and the output says which.
	if res.Err != nil && res.ExitCode != 1 {
		return LockAnswer{}, fmt.Errorf("locktool %s in %s on %s: %w: %s", sub, f.Name(pod), path, res.Err, res.Combined())
	}
	ans, err := ParseLockAnswer(res.Stdout)
	if err != nil {
		return LockAnswer{}, fmt.Errorf("locktool %s in %s on %s: %w (stderr %q)",
			sub, f.Name(pod), path, err, strings.TrimSpace(res.Stderr))
	}
	return ans, nil
}

// HoldLock takes a byte-range lock in a pod and keeps holding it until Release,
// or until the pod dies. It returns once the lock is confirmed held, so a
// caller never races its own fixture.
//
// The holder never blocks in the kernel. locktool polls F_SETLK rather than
// using F_SETLKW, because a blocking acquire cannot notice its run-file being
// removed: a holder that is refused would sit in the kernel with Release unable
// to stop it, and on a hard mount that process can be unkillable, which leaves
// the pod Terminating and turns teardown into the F-001-adjacent path it exists
// to avoid.
func (f *Framework) HoldLock(ctx context.Context, pod, path, id string, r LockRange) (*LockHolder, error) {
	if err := CheckScriptID(id); err != nil {
		return nil, err
	}
	if err := f.EnsureLockTool(ctx, pod); err != nil {
		return nil, err
	}
	l := &LockHolder{f: f, Pod: f.Name(pod), Path: path, ID: id, Kind: PosixLock, Range: r,
		state: "/tmp/lock-" + id + ".state", run: "/tmp/lock-" + id + ".run"}
	if _, err := f.C.MustSh(ctx, Namespace, l.Pod, "main", lockToolCmd("hold", path, r, l.run, l.state)); err != nil {
		return nil, err
	}
	if err := l.awaitHeld(ctx, 2*time.Minute); err != nil {
		return nil, err
	}
	return l, nil
}

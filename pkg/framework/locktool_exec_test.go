package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The tool itself, run for real. Everything above tests the parser; this tests
// that what the parser reads is true.
//
// It matters because the whole phase rests on one claim: that two clients can
// hold disjoint ranges of one file and are refused on each other's. A tool that
// quietly took a whole-file lock would satisfy every parser test here and make
// DATA-05's byte-range half assert nothing, on a real cluster, silently.
//
// This says the tool is correct against a local filesystem. It says nothing
// about NFS, which is what the cases on a cluster are for.

// buildLockTool compiles the tool for this machine and returns its path.
func buildLockTool(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("locktool takes fcntl byte-range locks through the Linux system call; this is %s", runtime.GOOS)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build locktool with")
	}
	root, err := repoRoot()
	if err != nil {
		t.Skipf("cannot find the repository root: %v", err)
	}
	out := filepath.Join(t.TempDir(), "locktool")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/locktool")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building locktool: %v\n%s", err, b)
	}
	return out
}

// runLockTool runs one invocation and returns the parsed answer.
func runLockTool(t *testing.T, tool string, args ...string) LockAnswer {
	t.Helper()
	out, err := exec.Command(tool, args...).Output()
	// Exit 1 is a refusal and exit 0 a grant. Both are answers.
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			t.Fatalf("locktool %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	ans, perr := ParseLockAnswer(string(out))
	if perr != nil {
		t.Fatalf("locktool %s printed %q: %v", strings.Join(args, " "), out, perr)
	}
	return ans
}

// TestLockToolHoldsAndRefusesByRange is the assertion the phase rests on: a
// lock covers the range it was asked for and no more.
//
// Steps:
//  1. Hold an exclusive lock on the first 4096 bytes.
//  2. Assert an overlapping request is refused, and that the refusal names the
//     range and type actually held rather than a whole-file lock.
//  3. Assert a disjoint request is granted, which a whole-file fallback would
//     refuse.
//  4. Assert getlk reports the holder without taking anything, by asking twice
//     and getting the same answer.
//  5. Remove the run file and assert the range becomes available.
func TestLockToolHoldsAndRefusesByRange(t *testing.T) {
	tool := buildLockTool(t)
	base := t.TempDir()
	// A path with a space in it, because paths come from cases and travel
	// through shell quoting on the way to a pod.
	target := filepath.Join(base, "a locked file")
	run := filepath.Join(base, "hold.run")
	state := filepath.Join(base, "hold.state")

	out, err := exec.Command(tool, "hold", target, "0", "4096", "write", run, state).CombinedOutput()
	if err != nil {
		t.Fatalf("launching the holder: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "launched" {
		t.Fatalf("the holder printed %q, want launched", got)
	}
	defer os.Remove(run)
	waitForState(t, state, "held")

	if ans := runLockTool(t, tool, "try", target, "0", "4096", "write"); ans.Free {
		t.Error("an overlapping request was granted while the range was held")
	} else if !ans.Conflict.Known || ans.Conflict.Start != 0 || ans.Conflict.Len != 4096 || ans.Conflict.Mode != "write" {
		t.Errorf("the refusal named %s, want the range actually held", ans.Conflict)
	}
	if ans := runLockTool(t, tool, "try", target, "2048", "4096", "write"); ans.Free {
		t.Error("a partially overlapping request was granted")
	}
	// The one a whole-file lock would get wrong, and the only thing a byte
	// range adds over flock.
	if ans := runLockTool(t, tool, "try", target, "4096", "4096", "write"); !ans.Free {
		t.Errorf("a disjoint range was refused (%s): the lock covers more than it was asked for, "+
			"which is a whole-file lock wearing a range's arguments", ans.Conflict)
	}

	// getlk asks without taking, so asking twice must answer the same way. A
	// probe that acquired would report the range free on the second call once
	// the holder let go, and held by itself in between.
	first := runLockTool(t, tool, "getlk", target, "0", "4096", "write")
	second := runLockTool(t, tool, "getlk", target, "0", "4096", "write")
	if first.Free || second.Free {
		t.Errorf("getlk reported the held range free: %+v then %+v", first, second)
	}
	if first.Conflict != second.Conflict {
		t.Errorf("getlk changed the state it was reading: %s then %s", first.Conflict, second.Conflict)
	}

	if err := os.Remove(run); err != nil {
		t.Fatalf("removing the run file: %v", err)
	}
	waitForState(t, state, "released")
	if ans := runLockTool(t, tool, "try", target, "0", "4096", "write"); !ans.Free {
		t.Errorf("the range was still refused after the holder released it: %s", ans.Conflict)
	}
}

// TestLockToolReadLocksShare covers the other half of the lock type: shared
// locks coexist and an exclusive one does not join them. A tool that treated
// every lock as exclusive would make a read-lock case fail against a correct
// server.
//
// Steps:
//  1. Hold a shared lock on a range.
//  2. Assert a second shared lock on the same range is granted, since shared
//     locks coexist by definition.
//  3. Assert an exclusive lock over that range is refused.
func TestLockToolReadLocksShare(t *testing.T) {
	tool := buildLockTool(t)
	base := t.TempDir()
	target := filepath.Join(base, "shared")
	run := filepath.Join(base, "hold.run")
	state := filepath.Join(base, "hold.state")

	if out, err := exec.Command(tool, "hold", target, "0", "1024", "read", run, state).CombinedOutput(); err != nil {
		t.Fatalf("launching the reader: %v\n%s", err, out)
	}
	defer os.Remove(run)
	waitForState(t, state, "held")

	if ans := runLockTool(t, tool, "try", target, "0", "1024", "read"); !ans.Free {
		t.Errorf("a second shared lock on the same range was refused: %s", ans.Conflict)
	}
	if ans := runLockTool(t, tool, "try", target, "0", "1024", "write"); ans.Free {
		t.Error("an exclusive lock was granted over a range already held shared")
	}
}

// TestLockToolWaitsRatherThanBlocking covers the state a blocking acquire could
// not report. locktool polls F_SETLK instead of sitting in F_SETLKW, because a
// blocked acquire cannot notice its run file being removed, and on a hard mount
// the process can be unkillable, which leaves the pod Terminating.
//
// Steps:
//  1. Hold a range.
//  2. Start a second holder on the same range.
//  3. Assert it reports waiting rather than held, and is still a live process.
//  4. Remove its run file and assert it stops, which a blocked acquire could not.
func TestLockToolWaitsRatherThanBlocking(t *testing.T) {
	tool := buildLockTool(t)
	base := t.TempDir()
	target := filepath.Join(base, "contended")
	firstRun, firstState := filepath.Join(base, "a.run"), filepath.Join(base, "a.state")
	secondRun, secondState := filepath.Join(base, "b.run"), filepath.Join(base, "b.state")

	if out, err := exec.Command(tool, "hold", target, "0", "512", "write", firstRun, firstState).CombinedOutput(); err != nil {
		t.Fatalf("launching the first holder: %v\n%s", err, out)
	}
	defer os.Remove(firstRun)
	waitForState(t, firstState, "held")

	if out, err := exec.Command(tool, "hold", target, "0", "512", "write", secondRun, secondState).CombinedOutput(); err != nil {
		t.Fatalf("launching the second holder: %v\n%s", err, out)
	}
	defer os.Remove(secondRun)
	// Long enough for several acquire attempts, each a second apart.
	time.Sleep(2500 * time.Millisecond)
	if got := readState(t, secondState); got != "waiting" {
		t.Fatalf("the refused holder reports %q, want waiting: a holder that reported held would have "+
			"taken a lock someone else has, and one that reported failed would hide a live contender", got)
	}

	if err := os.Remove(secondRun); err != nil {
		t.Fatalf("removing the second run file: %v", err)
	}
	waitForState(t, secondState, "released")

	// And the first holder still has it, so stopping the contender took nothing
	// away from the client that was granted the range.
	if got := readState(t, firstState); got != "held" {
		t.Errorf("the original holder reports %q after the contender gave up, want held", got)
	}
}

// readState reads a holder's state file as it stands, for the checks that want
// the current value rather than a value to wait for.
func readState(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

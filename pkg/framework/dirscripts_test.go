package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The scripts behind the large-directory and silly-rename cases, run under a
// real shell. Each of them fails silently rather than loudly if it is wrong: a
// populate step that created half of what it reported would make the case
// assert against entries that were never there, and a reader that reopened the
// path instead of using its held descriptor would answer the wrong question
// while looking correct.

// TestPopulateDirCountsTheDirectoryNotTheLoops runs the populate script and
// checks that what it reports is what is on disk.
//
// Steps:
//  1. Populate a directory across several shards.
//  2. Assert the reported count matches the entries actually created.
//  3. Assert the names are the ones the classifier expects, with no gaps, so a
//     sharded loop that skipped an index cannot pass.
func TestPopulateDirCountsTheDirectoryNotTheLoops(t *testing.T) {
	sh := lookOrSkip(t, "sh", "find")
	// A path with a space in it, because it comes from a case and travels
	// through shell quoting on the way to the pod.
	dir := filepath.Join(t.TempDir(), "a big directory")

	out, err := exec.Command(sh, materializeScript(t, "populate-dir.sh"), dir, "50", "4").CombinedOutput()
	if err != nil {
		t.Fatalf("populating: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "50" {
		t.Fatalf("the populate step reported %q, want 50", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the directory back: %v", err)
	}
	if len(entries) != 50 {
		t.Fatalf("the directory holds %d entries, want 50", len(entries))
	}
	// Every index present exactly once. Sharded loops are where an off-by-one
	// hides, and a missing index would later read as a listing that lawfully
	// missed an entry.
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	for i := 1; i <= 50; i++ {
		name := "e-" + strconv.Itoa(i)
		if !seen[name] {
			t.Errorf("%s was never created, so the shards do not cover the range", name)
		}
	}
}

// TestDeleteEntriesRemovesItsRangeAndNothingElse covers the deleter. A range
// that leaked past its end would delete entries the listing is still expected
// to be able to return.
//
// Steps:
//  1. Populate a directory.
//  2. Delete a range from the middle of it.
//  3. Assert the reported count, that the range is gone, and that the entries
//     on either side of it are untouched.
func TestDeleteEntriesRemovesItsRangeAndNothingElse(t *testing.T) {
	sh := lookOrSkip(t, "sh", "setsid", "find")
	dir := filepath.Join(t.TempDir(), "entries")
	state := filepath.Join(t.TempDir(), "del.state")

	if out, err := exec.Command(sh, materializeScript(t, "populate-dir.sh"), dir, "20", "2").CombinedOutput(); err != nil {
		t.Fatalf("populating: %v\n%s", err, out)
	}
	if out, err := exec.Command(sh, materializeScript(t, "delete-entries.sh"), dir, "5", "10", state).CombinedOutput(); err != nil {
		t.Fatalf("launching the deleter: %v\n%s", err, out)
	}
	if got := waitForAnyState(t, state); got != "6" {
		t.Errorf("the deleter reported %q entries removed, want 6", got)
	}
	for i := 5; i <= 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "e-"+strconv.Itoa(i))); err == nil {
			t.Errorf("e-%d survived a delete that reported it removed", i)
		}
	}
	for _, i := range []int{4, 11, 20} {
		if _, err := os.Stat(filepath.Join(dir, "e-"+strconv.Itoa(i))); err != nil {
			t.Errorf("e-%d was removed, and it is outside the range the deleter was given", i)
		}
	}
}

// TestHoldOpenReadKeepsItsDescriptorAcrossAnUnlink is the check the silly-rename
// case cannot make for itself on a local filesystem, and the one that says the
// reader is reading through the descriptor rather than reopening the path.
//
// On a local filesystem an unlinked file stays readable through an open
// descriptor and its name is gone immediately; on NFS the client renames it to
// .nfsXXXXXXXX instead. Both rest on the same thing: the descriptor opened
// before the unlink keeps working. A reader that reopened by path would fail
// here, where the name really is gone.
//
// Steps:
//  1. Write a file of two distinct chunks and hold it open.
//  2. Read the first chunk.
//  3. Unlink the file.
//  4. Read the second chunk through the same descriptor and assert it is what
//     was written, from the offset the first read left.
//  5. Close, and assert the reader reports it.
func TestHoldOpenReadKeepsItsDescriptorAcrossAnUnlink(t *testing.T) {
	sh := lookOrSkip(t, "sh", "setsid", "dd")
	base := t.TempDir()
	target := filepath.Join(base, "held open")
	run := filepath.Join(base, "r.run")
	state := filepath.Join(base, "r.state")
	request := filepath.Join(base, "r.req")
	result := filepath.Join(base, "r.out")

	const first, second = "AAAAAAAA", "BBBBBBBB"
	if err := os.WriteFile(target, []byte(first+second), 0o644); err != nil {
		t.Fatalf("writing the target: %v", err)
	}
	out, err := exec.Command(sh, materializeScript(t, "hold-open-read.sh"),
		target, "8", run, state, request, result).CombinedOutput()
	if err != nil {
		t.Fatalf("launching the reader: %v\n%s", err, out)
	}
	defer os.Remove(run)
	waitForState(t, state, "open")

	if got := requestChunk(t, request, result); got != first {
		t.Fatalf("the first read returned %q, want %q", got, first)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("unlinking the target: %v", err)
	}
	if got := requestChunk(t, request, result); got != second {
		t.Errorf("the read after the unlink returned %q, want %q: the descriptor either stopped working "+
			"or the reader reopened the path, and the second would answer a different question from the "+
			"one the case asks", got, second)
	}

	if err := os.Remove(run); err != nil {
		t.Fatalf("signalling the close: %v", err)
	}
	waitForState(t, state, "closed")
}

// requestChunk asks the held reader for one chunk and returns it.
func requestChunk(t *testing.T, request, result string) string {
	t.Helper()
	_ = os.Remove(result)
	if err := os.WriteFile(request, nil, 0o644); err != nil {
		t.Fatalf("asking for a chunk: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(result); err == nil {
			return string(b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the reader did not answer a chunk request in 30s")
	return ""
}

// waitForAnyState polls a state file until it holds anything at all.
func waitForAnyState(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				return s
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the worker never wrote its state file")
	return ""
}

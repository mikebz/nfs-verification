package framework

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The workload is the measuring instrument behind every chaos assertion, so its
// log format and its shell are tested here. An instrument that miscounts is
// worse than no instrument: it produces confident, wrong findings.

func at(sec int64) time.Time { return time.Unix(sec, 0) }

// TestParseLoadLog covers the log the chaos assertions are computed from: the
// error count, the set of committed writes, the first success after a fault,
// and the gap that a blocked client leaves behind.
//
// Steps:
//  1. Parse a log holding successes, a failure and unreadable lines.
//  2. Assert unreadable lines are kept rather than dropped.
//  3. Assert the committed set, and the subset committed before a given time.
//  4. Assert a failed attempt does not count as a recovery.
//  5. Assert the longest gap is the outage, not the polling interval.
func TestParseLoadLog(t *testing.T) {
	rep := parseLoadLog(`OK 1 1700000000
OK 2 1700000001
ERR 3 1700000002
OK 4 1700000090

garbage line
OK five 1700000091
`)
	if n := len(rep.Records); n != 4 {
		t.Fatalf("parsed %d records, want 4: %+v", n, rep.Records)
	}
	if len(rep.Unparsed) != 2 {
		t.Errorf("unreadable lines were dropped rather than kept: %v", rep.Unparsed)
	}
	if errs := rep.Errors(); len(errs) != 1 || errs[0].Index != 3 {
		t.Errorf("errors are %+v, want just index 3", errs)
	}
	if got, want := rep.Committed(), []int{1, 2, 4}; !equalInts(got, want) {
		t.Errorf("committed indices are %v, want %v", got, want)
	}
	// The set a fault at t may not lose is what was acknowledged at or before t.
	if got, want := rep.CommittedBefore(at(1700000001)), []int{1, 2}; !equalInts(got, want) {
		t.Errorf("committed before the fault: %v, want %v", got, want)
	}
	// The failed attempt at index 3 must not count as a recovery.
	rec, ok := rep.FirstSuccessAfter(at(1700000002))
	if !ok || rec.Index != 4 {
		t.Errorf("first success after the fault is %+v (found=%v), want index 4", rec, ok)
	}
	if got := rep.LongestGap(); got != 88*time.Second {
		t.Errorf("longest gap is %s, want 88s: that gap is what a blocked client looks like", got)
	}
}

// TestLoadReportOnAnEmptyLog covers the case that would be worst to get wrong:
// a workload that wrote nothing must not report an instant recovery.
//
// Steps:
//  1. Parse an empty log.
//  2. Assert no records, no errors, no success and no gap.
func TestLoadReportOnAnEmptyLog(t *testing.T) {
	rep := parseLoadLog("")
	if len(rep.Records) != 0 || len(rep.Errors()) != 0 || len(rep.Committed()) != 0 {
		t.Errorf("an empty log did not parse as empty: %+v", rep)
	}
	if _, ok := rep.FirstSuccessAfter(at(0)); ok {
		t.Error("an empty log reported a successful write, which would read as an instant recovery")
	}
	if got := rep.LongestGap(); got != 0 {
		t.Errorf("longest gap on an empty log is %s, want 0", got)
	}
}

// TestWriteLoadScriptRuns runs the rendered workload under a real shell. It
// says the script is correct, not that NFS behaves: what the chaos cases assert
// still needs a cluster.
// TestWriteLoadScriptRuns runs the rendered workload under a real shell, in a
// path with a space in it. It says the script is correct, not that NFS behaves:
// what the chaos cases assert still needs a cluster.
//
// Steps:
//  1. Launch the workload against a local directory.
//  2. Wait for it to commit two writes, then stop it.
//  3. Assert it reported no errors and nothing unreadable against local disk.
//  4. Assert every write it logged as committed is a real 4KiB file, since a
//     log claiming durability the workload never achieved is worse than none.
func TestWriteLoadScriptRuns(t *testing.T) {
	sh := lookOrSkip(t, "sh", "setsid", "dd", "date")
	base := t.TempDir()
	log := filepath.Join(base, "load.log")
	run := filepath.Join(base, "load.run")
	// A directory with a space in it, because the path comes from a case and
	// goes through shell quoting on the way to the pod.
	dir := filepath.Join(base, "records here")

	cmd := exec.Command(sh, "-c", writeLoadScript(log, run, "unit", dir))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launching the workload: %v\n%s", err, out)
	}
	// Two records is enough to prove the loop iterates; the workload writes one
	// per second by design.
	deadline := time.Now().Add(30 * time.Second)
	var rep LoadReport
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(log)
		if err == nil {
			rep = parseLoadLog(string(b))
			if len(rep.Committed()) >= 2 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := os.Remove(run); err != nil {
		t.Fatalf("stopping the workload: %v", err)
	}
	if len(rep.Committed()) < 2 {
		t.Fatalf("the workload committed %d writes in 30s, want at least 2: %+v", len(rep.Committed()), rep)
	}
	if len(rep.Errors()) != 0 || len(rep.Unparsed) != 0 {
		t.Errorf("the workload reported failures against a local disk: errors=%+v unparsed=%v",
			rep.Errors(), rep.Unparsed)
	}
	// Every logged success must be a real 4KiB record on disk, or the log is
	// claiming durability the workload never achieved.
	for _, i := range rep.Committed() {
		info, err := os.Stat(filepath.Join(dir, fmt.Sprintf("rec-%d", i)))
		if err != nil {
			t.Errorf("record %d was logged as committed but is not there: %v", i, err)
			continue
		}
		if info.Size() != 4096 {
			t.Errorf("record %d is %d bytes, want 4096", i, info.Size())
		}
	}
}

// TestMissingRecordsScript checks the record sweep under a real shell, with a
// present record, an absent one and an empty one. An empty record counts as
// missing: a zero-length file is not a committed 4KiB write.
// TestMissingRecordsScript covers the sweep that checks committed writes
// survived a failover, under a real shell and in a path with a space in it.
//
// Steps:
//  1. Lay down one full record, no second record, and an empty third.
//  2. Sweep all three.
//  3. Assert the absent and the empty are both reported: a zero-length file is
//     not a committed 4KiB write.
func TestMissingRecordsScript(t *testing.T) {
	sh := lookOrSkip(t, "sh")
	dir := filepath.Join(t.TempDir(), "records here")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("preparing the record directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rec-1"), make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("writing a record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rec-3"), nil, 0o644); err != nil {
		t.Fatalf("writing an empty record: %v", err)
	}
	cmd := exec.Command(sh, "-c", missingRecordsScript(dir, []int{1, 2, 3}))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running the record sweep: %v", err)
	}
	if got, want := strings.Fields(string(out)), []string{"2", "3"}; !equalStrings(got, want) {
		t.Errorf("the sweep reported %v missing, want %v", got, want)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Ints(a)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

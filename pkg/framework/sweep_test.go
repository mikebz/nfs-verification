package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestParseRecordSweep covers the four verdicts and the asymmetry the
// durability pair rests on: one case fails on anything but correct, the other
// only on wrong.
//
// A parser that collapsed short into wrong would fail the negative case on
// lawful behaviour, and one that collapsed wrong into short would let
// corruption pass. Both are the kind of mistake that produces a confident,
// wrong finding.
//
// Steps:
//  1. Parse a sweep holding each verdict, including a record longer than a
//     record should be.
//  2. Check the counts, the index lists and the first wrong byte.
//  3. Assert unreadable lines are kept rather than dropped, since a sweep the
//     harness cannot read must not report no corruption.
func TestParseRecordSweep(t *testing.T) {
	const out = `correct 1 4096
correct 2 4096
absent 3 0
short 4 1024
wrong 5 101
wrong 6 4097
not a verdict at all
wrong 7
maybe 8 0
correct nine 4096
`
	s := ParseRecordSweep(out)
	if got, want := len(s.Results), 6; got != want {
		t.Fatalf("parsed %d results, want %d: %+v", got, want, s.Results)
	}
	for verdict, want := range map[RecordVerdict]int{
		VerdictCorrect: 2, VerdictAbsent: 1, VerdictShort: 1, VerdictWrong: 2,
	} {
		if got := s.Count(verdict); got != want {
			t.Errorf("counted %d %s, want %d", got, verdict, want)
		}
	}
	if got := s.Indices(VerdictWrong); len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Errorf("the wrong records are %v, want [5 6]", got)
	}
	// The first wrong byte, by record index, is what a filed defect needs.
	wrong, ok := s.FirstWrong()
	if !ok || wrong.Index != 5 || wrong.Offset != 101 {
		t.Errorf("the first wrong byte is record %d offset %d (found %v), want record 5 offset 101",
			wrong.Index, wrong.Offset, ok)
	}
	// Four unreadable lines: a line that is not three fields, one with two
	// fields, one with a verdict nobody defines, and one with a non-numeric
	// index. Kept, because a sweep the harness cannot read is a harness defect
	// and reporting no corruption would hide it.
	if got := len(s.Unparsed); got != 4 {
		t.Errorf("kept %d unreadable lines, want 4: %q", got, s.Unparsed)
	}
	if !strings.Contains(s.String(), "first wrong byte in record 5 at offset 101") {
		t.Errorf("the summary does not name the first wrong byte: %s", s)
	}
}

// TestRecordSweepIsEmptyWithoutResults covers the empty sweep.
//
// Steps:
//  1. Parse no output at all.
//  2. Assert it holds no results and no unreadable lines.
//  3. Assert it reports no wrong record, and that its summary names no offset.
//     A sweep over an empty set must say nothing rather than fabricate a
//     corruption finding out of a zero value.
func TestRecordSweepIsEmptyWithoutResults(t *testing.T) {
	s := ParseRecordSweep("")
	if len(s.Results) != 0 || len(s.Unparsed) != 0 {
		t.Fatalf("an empty sweep parsed as %+v", s)
	}
	if _, ok := s.FirstWrong(); ok {
		t.Error("an empty sweep reported a wrong record")
	}
	if strings.Contains(s.String(), "wrong byte") {
		t.Errorf("an empty sweep's summary names a wrong byte: %s", s)
	}
}

// TestRecordSweepTableListsEveryRecord covers the artifact.
//
// Steps:
//  1. Render the table of a sweep holding a correct record, a wrong one and an
//     unreadable line.
//  2. Assert all three appear. The bundle is what a failure is filed with, and
//     a table naming only the failures cannot say how large the set was that
//     they came from, nor that part of it could not be read.
func TestRecordSweepTableListsEveryRecord(t *testing.T) {
	table := ParseRecordSweep("correct 1 4096\nwrong 2 17\nunreadable line here\n").Table()
	for _, want := range []string{"rec-1\tcorrect", "rec-2\twrong\t17", "unreadable\tunreadable line here"} {
		if !strings.Contains(table, want) {
			t.Errorf("the verdict table does not hold %q:\n%s", want, table)
		}
	}
}

// TestVerifyRecordsScriptAnswersForEveryIndex holds the real script to the rule
// the short-answer error rests on: one line per index, on every path.
//
// This is the failure with no symptom. VerifyRecords reads a short answer as
// the sweep never having run, and says so in a message that sends the reader
// looking at the exec rather than at the data. If some path through the script
// could print nothing for a record, that message would be a confident lie, and
// a real lost record would be filed as a harness problem. See F-011.
//
// Steps:
//  1. Sweep a directory that does not exist at all.
//  2. Sweep a directory that exists and is empty.
//  3. Sweep a directory holding a mixture of correct, short, wrong and missing
//     records, plus a record that cannot be read.
//  4. Assert each run printed exactly one line per index, and that every line
//     landed somewhere in the sweep rather than being dropped.
func TestVerifyRecordsScriptAnswersForEveryIndex(t *testing.T) {
	sh := lookOrSkip(t, "sh", "cmp", "yes")
	script := materializeScript(t, "verify-records.sh")
	const size = 4096
	pattern := func(index, n int) []byte {
		unit := []byte("rec-" + strconv.Itoa(index) + "\n")
		out := make([]byte, 0, n+len(unit))
		for len(out) < n {
			out = append(out, unit...)
		}
		return out[:n]
	}

	missing := filepath.Join(t.TempDir(), "never-created")
	empty := t.TempDir()
	mixed := t.TempDir()
	for _, r := range []struct {
		index int
		body  []byte
		mode  os.FileMode
	}{
		{1, pattern(1, size), 0o644},   // correct
		{2, pattern(2, 1024), 0o644},   // short
		{4, []byte("not this"), 0o644}, // wrong
		{5, pattern(5, size), 0o000},   // present but unreadable
	} {
		if err := os.WriteFile(filepath.Join(mixed, "rec-"+strconv.Itoa(r.index)), r.body, r.mode); err != nil {
			t.Fatalf("writing rec-%d: %v", r.index, err)
		}
	}
	// rec-3 is never written, so the mixed set also covers absent.

	indices := []string{"1", "2", "3", "4", "5"}
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"missing directory", missing},
		{"empty directory", empty},
		{"mixed records", mixed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := filepath.Join(t.TempDir(), "expected")
			args := append([]string{script, tc.dir, strconv.Itoa(size), scratch}, indices...)
			out, err := exec.Command(sh, args...).Output()
			if err != nil {
				t.Fatalf("running the record sweep: %v", err)
			}
			got := len(strings.Split(strings.TrimRight(string(out), "\n"), "\n"))
			if strings.TrimSpace(string(out)) == "" {
				got = 0
			}
			if got != len(indices) {
				t.Fatalf("the sweep printed %d lines for %d indices, and VerifyRecords reads anything "+
					"short of one line each as the sweep not having run:\n%s", got, len(indices), out)
			}
			sweep := ParseRecordSweep(string(out))
			if n := len(sweep.Results) + len(sweep.Unparsed); n != len(indices) {
				t.Errorf("the sweep accounted for %d of %d indices: %s", n, len(indices), out)
			}
		})
	}
}

// TestShortSweepErrorNamesWhatItSaw covers the message a short sweep fails
// with.
//
// The message exists because the original one named a count and nothing else,
// and a count cannot tell a reader whether the pod said nothing or the harness
// lost what it said. Both streams and their lengths go in, since an empty
// stdout next to an empty stderr is itself the evidence. See F-011.
//
// Steps:
//  1. Build the error for a sweep that answered for none of fourteen records
//     from an exec that reported success with nothing on either stream.
//  2. Assert it names the pod, the directory, both counts and both stream
//     lengths, and points at the finding.
//  3. Assert a long stream is clipped rather than pasted whole into a test log.
func TestShortSweepErrorNamesWhatItSaw(t *testing.T) {
	indices := make([]int, 14)
	for i := range indices {
		indices[i] = i
	}
	err := shortSweepError("nfsv-chaos-06-run-verifier", "/mnt/share/chaos-06",
		indices, ParseRecordSweep(""), ExecResult{})
	for _, want := range []string{
		"nfsv-chaos-06-run-verifier", "/mnt/share/chaos-06",
		"14 records", "answered for 0 of them",
		"stdout was 0 bytes", "stderr 0 bytes", "F-011",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the short-sweep error does not mention %q:\n%v", want, err)
		}
	}

	long := strings.Repeat("x", 5000)
	clipped := shortSweepError("pod", "/dir", indices, ParseRecordSweep(""),
		ExecResult{Stderr: long}).Error()
	if len(clipped) > 2000 {
		t.Errorf("the short-sweep error is %d characters long, so a noisy stream buries the message "+
			"it was written to deliver", len(clipped))
	}
	if !strings.Contains(clipped, "stderr 5000 bytes") {
		t.Errorf("the clipped error no longer says how much stderr there was:\n%s", clipped)
	}
}

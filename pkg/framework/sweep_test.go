package framework

import (
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

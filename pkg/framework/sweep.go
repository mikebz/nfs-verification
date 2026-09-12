package framework

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The durability pair needs a verdict finer than present or absent. "Is the
// file non-empty" is enough for a failover case asserting that nothing was
// lost; it is not enough where a truncated record is an acceptable outcome and
// a wrong byte at a correct offset is corruption. Collapsing those two into one
// answer is what makes a negative durability case either useless or wrong.
//
// Four verdicts, exhaustive over observed length, and the asymmetry between
// which of them fail in which case is the whole content of the pair.

// RecordVerdict is one record's outcome.
type RecordVerdict string

const (
	// VerdictCorrect is the full length, every byte as written.
	VerdictCorrect RecordVerdict = "correct"
	// VerdictAbsent is no file, or a file of zero length.
	VerdictAbsent RecordVerdict = "absent"
	// VerdictShort is a correct prefix, less than the full length. Lawful
	// wherever no fsync was issued: the client is free to have flushed a
	// prefix, and a prefix is not corruption.
	VerdictShort RecordVerdict = "short"
	// VerdictWrong is a byte that differs at an offset that was written, or a
	// record longer than it should be. A record that grew holds a byte nobody
	// wrote, and no reading of the protocol admits that.
	VerdictWrong RecordVerdict = "wrong"
)

// RecordResult is one record's verdict and where it went wrong.
type RecordResult struct {
	Index   int
	Verdict RecordVerdict
	// Offset is the first differing byte for a wrong record, counting from 1 as
	// cmp reports it, or -1 where the tool said nothing readable. For the other
	// verdicts it is the observed length.
	Offset int64
}

// RecordSweep is the whole set's verdicts.
type RecordSweep struct {
	Results []RecordResult
	// Unparsed lines, kept rather than dropped: a sweep the harness cannot read
	// is a harness defect, and silently reporting no corruption would hide it.
	Unparsed []string
	// Raw is the text the sweep was parsed from, kept so a sweep that answered
	// for fewer records than it was asked about can be filed with what the pod
	// actually printed. A verdict table holding nothing is not evidence of
	// anything, and that is the one shape where the raw output is all there is.
	// See F-011 in docs/findings.md.
	Raw string
}

// Count returns how many records came back with a verdict.
func (s RecordSweep) Count(v RecordVerdict) int {
	n := 0
	for _, r := range s.Results {
		if r.Verdict == v {
			n++
		}
	}
	return n
}

// Indices returns the records with a verdict, in order.
func (s RecordSweep) Indices(v RecordVerdict) []int {
	var out []int
	for _, r := range s.Results {
		if r.Verdict == v {
			out = append(out, r.Index)
		}
	}
	sort.Ints(out)
	return out
}

// FirstWrong returns the lowest-numbered corrupt record, which is what a filed
// defect needs: an index and an offset someone can go and look at.
func (s RecordSweep) FirstWrong() (RecordResult, bool) {
	best := RecordResult{}
	found := false
	for _, r := range s.Results {
		if r.Verdict != VerdictWrong {
			continue
		}
		if !found || r.Index < best.Index {
			best, found = r, true
		}
	}
	return best, found
}

// String renders the verdict table for the artifact bundle and the log line.
func (s RecordSweep) String() string {
	out := fmt.Sprintf("%d records: %d correct, %d absent, %d short, %d wrong",
		len(s.Results), s.Count(VerdictCorrect), s.Count(VerdictAbsent),
		s.Count(VerdictShort), s.Count(VerdictWrong))
	if w, ok := s.FirstWrong(); ok {
		out += fmt.Sprintf("; first wrong byte in record %d at offset %d", w.Index, w.Offset)
	}
	if len(s.Unparsed) > 0 {
		out += fmt.Sprintf("; %d unreadable lines", len(s.Unparsed))
	}
	return out
}

// Table renders one line per record, for the bundle.
func (s RecordSweep) Table() string {
	var sb strings.Builder
	sb.WriteString(s.String() + "\n\n")
	for _, r := range s.Results {
		fmt.Fprintf(&sb, "rec-%d\t%s\t%d\n", r.Index, r.Verdict, r.Offset)
	}
	for _, line := range s.Unparsed {
		fmt.Fprintf(&sb, "unreadable\t%s\n", line)
	}
	return sb.String()
}

// ParseRecordSweep reads the sweep's output: one "<verdict> <index> <offset>"
// per record. A pure function, because it is read from a mount that is still
// recovering and a parser that fails there fails where nothing can be debugged.
func ParseRecordSweep(out string) RecordSweep {
	s := RecordSweep{Raw: out}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			s.Unparsed = append(s.Unparsed, line)
			continue
		}
		verdict := RecordVerdict(fields[0])
		switch verdict {
		case VerdictCorrect, VerdictAbsent, VerdictShort, VerdictWrong:
		default:
			s.Unparsed = append(s.Unparsed, line)
			continue
		}
		index, err1 := strconv.Atoi(fields[1])
		offset, err2 := strconv.ParseInt(fields[2], 10, 64)
		if err1 != nil || err2 != nil {
			s.Unparsed = append(s.Unparsed, line)
			continue
		}
		s.Results = append(s.Results, RecordResult{Index: index, Verdict: verdict, Offset: offset})
	}
	return s
}

// VerifyRecords sweeps a record set from a pod and returns a verdict each.
//
// Read from a pod other than the writer wherever a case can arrange it: a
// different client crosses the server rather than the writer's own page cache,
// and the writer's cache is exactly what a durability question is trying to
// see past.
//
// One exec for the whole set. A round trip per record would take longer than
// the outage being measured, and would be running while the mount is still
// recovering.
func (f *Framework) VerifyRecords(ctx context.Context, pod, dir string, indices []int) (RecordSweep, error) {
	if len(indices) == 0 {
		return RecordSweep{}, nil
	}
	args := make([]string, 0, len(indices)+3)
	// The scratch path is on the pod's own filesystem, never on the share: the
	// expected bytes must not be written through the thing under test.
	args = append(args, dir, strconv.Itoa(RecordBytes), "/tmp/nfsv-sweep-expected")
	for _, i := range indices {
		args = append(args, strconv.Itoa(i))
	}
	script, err := RunScript("verify-records.sh", "sweep", args...)
	if err != nil {
		return RecordSweep{}, err
	}
	// Sh rather than MustSh: MustSh keeps stderr only when the exec failed, and
	// the failure this has to explain is an exec that succeeded and said
	// nothing. See F-011 in docs/findings.md.
	res := f.C.Sh(ctx, Namespace, f.Name(pod), "main", script)
	if res.Err != nil {
		return RecordSweep{}, fmt.Errorf("sweeping records in %s/%s: %w: %s",
			Namespace, f.Name(pod), res.Err, res.Combined())
	}
	// Untrimmed on purpose: the parser skips blank lines by itself, and the raw
	// text it keeps is only evidence if it is what the pod actually sent.
	sweep := ParseRecordSweep(res.Stdout)
	if len(sweep.Results)+len(sweep.Unparsed) != len(indices) {
		return sweep, shortSweepError(f.Name(pod), dir, indices, sweep, res)
	}
	return sweep, nil
}

// shortSweepError reports a sweep that did not answer for every record it was
// asked about, and says what the pod actually printed.
//
// verify-records.sh prints exactly one line per index on every path it can
// take, a directory that is not there included, and
// TestVerifyRecordsScriptAnswersForEveryIndex holds it to that. So a short
// answer is not a verdict about the records: either the script did not run or
// its output did not reach the harness. That distinction is the whole reason
// this message exists, because the caller's next assertion is data loss, and a
// sweep that answered for nothing must never be read as one that found nothing.
//
// The counts and both streams go in the message because the one occurrence so
// far was an exec that reported success with an empty stdout, where the old
// message named a number and nothing else. See F-011 in docs/findings.md.
func shortSweepError(pod, dir string, indices []int, s RecordSweep, r ExecResult) error {
	return fmt.Errorf("the sweep of %d records under %s in pod %s answered for %d of them, so what it "+
		"did say cannot stand for the set. The script prints one line per index on every path, so this "+
		"is the sweep failing to run or its output being lost rather than a verdict about the data: the "+
		"exec reported no error, stdout was %d bytes (%q) and stderr %d bytes (%q). See F-011 in "+
		"docs/findings.md",
		len(indices), dir, pod, len(s.Results)+len(s.Unparsed),
		len(r.Stdout), clipStream(r.Stdout), len(r.Stderr), clipStream(r.Stderr))
}

// clipStream shortens a captured stream for an error message, keeping the head,
// where the first thing that went wrong will be.
func clipStream(s string) string {
	const max = 400
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

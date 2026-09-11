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
	var s RecordSweep
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
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main", script)
	if err != nil {
		return RecordSweep{}, err
	}
	sweep := ParseRecordSweep(out)
	if len(sweep.Results)+len(sweep.Unparsed) != len(indices) {
		return sweep, fmt.Errorf("the sweep answered for %d of %d records, so what it did say cannot "+
			"stand for the set", len(sweep.Results)+len(sweep.Unparsed), len(indices))
	}
	return sweep, nil
}

package framework

import (
	"strings"
	"testing"
)

// TestParseFioReport covers the soak's only assertion. Under verify_fatal a
// crc32c mismatch ends the job with a non-zero error, so this parser is what
// stands between a detected corruption and an hour reported as clean.
//
// Steps:
//  1. Parse a report holding one clean job and one that ended with an error.
//  2. Assert the failing job is the one returned, and the byte totals add up.
//  3. Assert a report with no JSON, unreadable JSON, or no jobs is an error,
//     because each of those would otherwise read as "no failing jobs".
func TestParseFioReport(t *testing.T) {
	const out = `fio: this build has no zlib support
{
  "fio version": "fio-3.36",
  "jobs": [
    {"jobname": "soak", "error": 0,
     "read": {"io_bytes": 1048576, "bw_bytes": 2048},
     "write": {"io_bytes": 524288, "bw_bytes": 1024}},
    {"jobname": "soak", "error": 84,
     "read": {"io_bytes": 4096, "bw_bytes": 8},
     "write": {"io_bytes": 0, "bw_bytes": 0}}
  ]
}`
	r, err := ParseFioReport(out)
	if err != nil {
		t.Fatalf("reading the report: %v", err)
	}
	if r.Version != "fio-3.36" || len(r.Jobs) != 2 {
		t.Fatalf("parsed %+v", r)
	}
	failed := r.Failed()
	if len(failed) != 1 || failed[0].Error != 84 {
		t.Errorf("the failing jobs are %+v, want the one that ended with error 84", failed)
	}
	read, written := r.Bytes()
	if read != 1048576+4096 || written != 524288 {
		t.Errorf("the totals are %d read and %d written", read, written)
	}

	// Each of these would otherwise read as an hour with nothing wrong, which
	// is the one answer the parser must never invent.
	for _, bad := range []string{
		"",
		"fio: failed to open the job file",
		`{"fio version": "fio-3.36"}`,
		`{"fio version": "fio-3.36", "jobs": []}`,
		`{"fio version": "fio-3.36", "jobs": [`,
	} {
		if _, err := ParseFioReport(bad); err == nil {
			t.Errorf("parsed %q without an error, and a report the harness cannot read must not stand "+
				"for a soak with no failing jobs", bad)
		}
	}
}

// TestFioReportStringNamesFailures covers the line a run logs. A summary that
// did not say how many jobs failed would make a clean hour and a corrupt one
// look alike in a scrollback.
//
// Steps:
//  1. Render the summary of a report holding one job that ended with an error.
//  2. Assert it carries the fio version, the job count and the failed count,
//     which are the three fields that separate a clean hour from a corrupt one
//     in a scrollback.
func TestFioReportStringNamesFailures(t *testing.T) {
	r := FioReport{Version: "fio-3.36", Jobs: []FioJob{{Name: "soak", Error: 84}}}
	got := r.String()
	for _, want := range []string{"fio-3.36", "1 jobs", "1 failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary does not hold %q: %s", want, got)
		}
	}
}

package framework

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// fio is the one tool in this suite the repository does not own, and it arrives
// as an image the operator names. That is right for fio and wrong for locktool:
// fio's packaging is a decision of whoever runs the suite, and a soak case that
// skips costs nothing on the days nobody has an image to run it with.
//
// The harness consumes only what it asserts on. Throughput is recorded in the
// bundle and asserted on by nobody: the soak's expected result is zero checksum
// mismatches, and a performance bound here would be a SCALE case wearing a DATA
// number.

// FioDirection is one direction's totals from an fio job.
type FioDirection struct {
	IOBytes int64 `json:"io_bytes"`
	BWBytes int64 `json:"bw_bytes"`
}

// FioJob is the part of one job's JSON result the harness reads.
type FioJob struct {
	Name string `json:"jobname"`
	// Error is fio's exit reason for this job. With verify_fatal=1 a checksum
	// mismatch ends the job here, which is why this is the assertion rather
	// than a count buried in a summary.
	Error int          `json:"error"`
	Read  FioDirection `json:"read"`
	Write FioDirection `json:"write"`
}

// FioReport is one fio run's JSON output.
type FioReport struct {
	Version string   `json:"fio version"`
	Jobs    []FioJob `json:"jobs"`
}

// Failed returns the jobs fio ended with an error, which under verify_fatal is
// how a checksum mismatch surfaces.
func (r FioReport) Failed() []FioJob {
	var out []FioJob
	for _, j := range r.Jobs {
		if j.Error != 0 {
			out = append(out, j)
		}
	}
	return out
}

// Bytes returns the total read and written across every job, for the record.
func (r FioReport) Bytes() (read, written int64) {
	for _, j := range r.Jobs {
		read += j.Read.IOBytes
		written += j.Write.IOBytes
	}
	return read, written
}

func (r FioReport) String() string {
	read, written := r.Bytes()
	return fmt.Sprintf("fio %s: %d jobs, %d failed, %d bytes read, %d written",
		r.Version, len(r.Jobs), len(r.Failed()), read, written)
}

// ParseFioReport reads fio's JSON output.
//
// The scan for the opening brace is not defensive dressing: fio writes warnings
// ahead of the document on some builds, and a report that failed to parse would
// otherwise be indistinguishable from a run with no failing jobs, which is
// exactly the answer this must never invent.
func ParseFioReport(out string) (FioReport, error) {
	start := strings.Index(out, "{")
	if start < 0 {
		return FioReport{}, fmt.Errorf("fio printed no JSON document: %q", truncate(out, 500))
	}
	var r FioReport
	if err := json.Unmarshal([]byte(out[start:]), &r); err != nil {
		return FioReport{}, fmt.Errorf("reading fio's JSON report: %w: %q", err, truncate(out[start:], 500))
	}
	if len(r.Jobs) == 0 {
		return r, fmt.Errorf("fio's report holds no jobs, so it says nothing about whether the soak " +
			"verified anything")
	}
	return r, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// FioSpec is one soak job: one pod, one directory it owns alone.
type FioSpec struct {
	Pod string
	Dir string
	ID  string
	// Runtime is how long the job runs.
	Runtime time.Duration
	// Files is how many files the job spreads itself over.
	Files int
	// SizeRange is fio's filesize argument, such as "4k-1g".
	SizeRange string
	// ReadPercent is the share of the mix that is reads.
	ReadPercent int
}

// FioRun is a running soak job in a pod.
type FioRun struct {
	f     *Framework
	Pod   string
	Dir   string
	out   string
	state string
}

// StartFio launches one soak job in the background and returns once it is
// running. Background because the run is an hour: an exec stream held open for
// that long is a connection to lose, and losing it would lose the result.
func (f *Framework) StartFio(ctx context.Context, spec FioSpec) (*FioRun, error) {
	if err := CheckScriptID(spec.ID); err != nil {
		return nil, err
	}
	r := &FioRun{f: f, Pod: f.Name(spec.Pod), Dir: spec.Dir,
		out: "/tmp/fio-" + spec.ID + ".json", state: "/tmp/fio-" + spec.ID + ".state"}
	script, err := RunScript("fio-soak.sh", spec.ID, spec.Dir,
		strconv.Itoa(int(spec.Runtime.Seconds())), strconv.Itoa(spec.Files),
		spec.SizeRange, strconv.Itoa(spec.ReadPercent), r.out, r.state)
	if err != nil {
		return nil, err
	}
	if _, err := f.C.MustSh(ctx, Namespace, r.Pod, "main", script); err != nil {
		return nil, err
	}
	return r, nil
}

// Wait blocks until the job finishes and returns its report.
//
// A non-zero exit from fio is not an error here: under verify_fatal a checksum
// mismatch is exactly that, and it is the finding rather than a reason to stop
// reading the report.
func (r *FioRun) Wait(ctx context.Context, timeout time.Duration) (FioReport, error) {
	var status int
	err := Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		out, err := r.f.C.MustSh(ctx, Namespace, r.Pod, "main",
			"cat "+shellQuote(r.state)+" 2>/dev/null || true")
		if err != nil {
			return false, err
		}
		fields := strings.Fields(out)
		if len(fields) != 2 || fields[0] != "done" {
			return false, fmt.Errorf("the soak job in %s has not finished", r.Pod)
		}
		status, err = strconv.Atoi(fields[1])
		if err != nil {
			return false, fmt.Errorf("the soak job in %s reported %q rather than an exit status", r.Pod, out)
		}
		return true, nil
	})
	if err != nil {
		return FioReport{}, fmt.Errorf("waiting for the soak job in %s: %w", r.Pod, err)
	}
	out, err := r.f.C.MustSh(ctx, Namespace, r.Pod, "main",
		"cat "+shellQuote(r.out)+" 2>/dev/null || true")
	if err != nil {
		return FioReport{}, err
	}
	report, err := ParseFioReport(out)
	if err != nil {
		return FioReport{}, fmt.Errorf("the soak job in %s exited %d and left no readable report: %w",
			r.Pod, status, err)
	}
	return report, nil
}

// Raw returns fio's JSON output verbatim, for the bundle.
func (r *FioRun) Raw(ctx context.Context) (string, error) {
	return r.f.C.MustSh(ctx, Namespace, r.Pod, "main", "cat "+shellQuote(r.out)+" 2>/dev/null || true")
}

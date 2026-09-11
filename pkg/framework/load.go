package framework

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Active I/O is what makes a chaos case mean anything: a failover with no
// workload running proves only that the server came back. The load here writes
// one small record per second and fsyncs it, so that every logged success is a
// committed write the server acknowledged, and records the outcome and the time
// of each attempt on the pod's own filesystem.
//
// One write per second is deliberate. The measurements this feeds are a 60 to
// 120 second recovery bound at one second of resolution, and a tighter loop
// would fill the share with records without sharpening a single assertion.

// WriteLoad is a running background workload in a client pod.
type WriteLoad struct {
	f   *Framework
	Pod string
	// Dir is where the records land on the share.
	Dir string
	log string
	run string
}

// LoadRecord is one attempt: whether it committed, and when it finished. The
// time comes from the pod's clock, because the assertion compares it against a
// fault injected at a moment read from that same clock.
type LoadRecord struct {
	Index int
	OK    bool
	At    time.Time
}

// LoadReport is the parsed log of a workload.
type LoadReport struct {
	Records []LoadRecord
	// Unparsed lines, kept rather than dropped: a log the harness cannot read
	// is a harness defect, and silently reporting zero errors would hide it.
	Unparsed []string
}

// RecordBytes is the length of one record the workload writes. Not
// configurable: it is a property of the measurement, and the sweep that reads
// these back computes every expected byte from it.
const RecordBytes = 4096

// WriteLoadSpec is how a case asks for a workload.
type WriteLoadSpec struct {
	Pod string
	Dir string
	ID  string
	// NoFsync leaves each record to the client rather than committing it.
	//
	// Only the negative durability case sets this, and it changes what a logged
	// success means: with the fsync a success is a write the server
	// acknowledged, and without it a success is only a write the client
	// accepted. A case that asserted post-COMMIT durability against this
	// workload would be asserting a guarantee nothing gave it.
	NoFsync bool
}

// StartWriteLoad launches a workload that commits every record, which is what
// every case except the negative durability one wants.
func (f *Framework) StartWriteLoad(ctx context.Context, pod, dir, id string) (*WriteLoad, error) {
	return f.StartWriteLoadSpec(ctx, WriteLoadSpec{Pod: pod, Dir: dir, ID: id})
}

// StartWriteLoadSpec launches the workload and returns once its first write has
// committed, so a fault injected afterwards lands on a workload that is
// demonstrably running rather than one that may not have started.
//
// The workload itself is scripts/write-load.sh.
func (f *Framework) StartWriteLoadSpec(ctx context.Context, spec WriteLoadSpec) (*WriteLoad, error) {
	pod, dir, id := spec.Pod, spec.Dir, spec.ID
	fsync := "fsync"
	if spec.NoFsync {
		fsync = "nofsync"
	}
	w := &WriteLoad{f: f, Pod: f.Name(pod), Dir: dir,
		log: "/tmp/load-" + id + ".log", run: "/tmp/load-" + id + ".run"}
	script, err := RunScript("write-load.sh", id, dir, w.run, w.log,
		strconv.Itoa(RecordBytes), fsync)
	if err != nil {
		return nil, err
	}
	if _, err := f.C.MustSh(ctx, Namespace, w.Pod, "main", script); err != nil {
		return nil, err
	}
	if err := Poll(ctx, FastPoll, 2*time.Minute, func(ctx context.Context) (bool, error) {
		rep, err := w.Report(ctx)
		if err != nil {
			return false, err
		}
		return len(rep.Records) > 0, fmt.Errorf("workload in %s has not written anything yet", w.Pod)
	}); err != nil {
		return nil, fmt.Errorf("starting the workload in %s: %w", w.Pod, err)
	}
	return w, nil
}

// Report reads the log as it stands. Safe to call while the workload runs,
// which is how a case waits for I/O to resume after a fault.
func (w *WriteLoad) Report(ctx context.Context) (LoadReport, error) {
	out, err := w.f.C.MustSh(ctx, Namespace, w.Pod, "main", "cat "+shellQuote(w.log)+" 2>/dev/null || true")
	if err != nil {
		return LoadReport{}, err
	}
	return parseLoadLog(out), nil
}

// Stop ends the workload and returns its final log.
func (w *WriteLoad) Stop(ctx context.Context) (LoadReport, error) {
	if _, err := w.f.C.MustSh(ctx, Namespace, w.Pod, "main", "rm -f "+shellQuote(w.run)); err != nil {
		return LoadReport{}, err
	}
	return w.Report(ctx)
}

func parseLoadLog(out string) LoadReport {
	var rep LoadReport
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || (fields[0] != "OK" && fields[0] != "ERR") {
			rep.Unparsed = append(rep.Unparsed, line)
			continue
		}
		idx, err1 := strconv.Atoi(fields[1])
		secs, err2 := strconv.ParseInt(fields[2], 10, 64)
		if err1 != nil || err2 != nil {
			rep.Unparsed = append(rep.Unparsed, line)
			continue
		}
		rep.Records = append(rep.Records, LoadRecord{Index: idx, OK: fields[0] == "OK", At: time.Unix(secs, 0)})
	}
	return rep
}

// Errors returns the failed attempts. On a hard NFSv4.1 mount this must be
// empty across a failover: the client blocks, it does not return EIO.
func (r LoadReport) Errors() []LoadRecord {
	var out []LoadRecord
	for _, rec := range r.Records {
		if !rec.OK {
			out = append(out, rec)
		}
	}
	return out
}

// Committed returns the indices of every write the server acknowledged, in
// order. These are the writes a failover may not lose.
func (r LoadReport) Committed() []int {
	var out []int
	for _, rec := range r.Records {
		if rec.OK {
			out = append(out, rec.Index)
		}
	}
	sort.Ints(out)
	return out
}

// Attempted returns the indices of every write the workload tried, in order.
//
// This is the set the negative durability case sweeps. Without an fsync a
// logged success means only that the client accepted the write, so the
// committed set is not the set that may be on the share, and a sweep restricted
// to it would miss a record that arrived by a route nobody asserted on.
func (r LoadReport) Attempted() []int {
	var out []int
	for _, rec := range r.Records {
		out = append(out, rec.Index)
	}
	sort.Ints(out)
	return out
}

// CommittedBefore returns the indices acknowledged at or before t, which is the
// set a fault injected at t may not lose.
func (r LoadReport) CommittedBefore(t time.Time) []int {
	var out []int
	for _, rec := range r.Records {
		if rec.OK && !rec.At.After(t) {
			out = append(out, rec.Index)
		}
	}
	sort.Ints(out)
	return out
}

// FirstSuccessAfter returns the first committed write at or after t. This is
// the measurement behind every recovery SLO in the plan: time to first
// successful I/O, taken from the workload itself rather than from a probe the
// harness starts once it notices something happened.
func (r LoadReport) FirstSuccessAfter(t time.Time) (LoadRecord, bool) {
	best := LoadRecord{}
	found := false
	for _, rec := range r.Records {
		if !rec.OK || rec.At.Before(t) {
			continue
		}
		if !found || rec.At.Before(best.At) {
			best, found = rec, true
		}
	}
	return best, found
}

// LongestGap returns the largest interval between consecutive attempts, which
// is what a blocked client looks like from outside: no errors, no progress.
func (r LoadReport) LongestGap() time.Duration {
	var longest time.Duration
	for i := 1; i < len(r.Records); i++ {
		if d := r.Records[i].At.Sub(r.Records[i-1].At); d > longest {
			longest = d
		}
	}
	return longest
}

// PodNow reads a pod's clock. Recovery is measured between a fault and a write
// logged inside the pod, so both ends of the measurement are taken from the
// same clock rather than trusting the workstation and the node to agree.
func (f *Framework) PodNow(ctx context.Context, pod string) (time.Time, error) {
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main", "date +%s")
	if err != nil {
		return time.Time{}, err
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("unexpected clock reading %q from %s: %w", out, pod, err)
	}
	return time.Unix(secs, 0), nil
}

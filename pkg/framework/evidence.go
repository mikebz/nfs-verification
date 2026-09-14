package framework

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A bundle is only as good as what it holds, and the two things a case argues
// from hardest are the two teardown destroys before anyone reads them: the
// workload's record stream, which lives on the writer pod's own filesystem so
// that the measurement never crosses the filesystem under test, and the file on
// the share a data case is making a claim about. Both go with the pod and the
// claim, so a recovery number or a missing-record count has nothing behind it
// by the time someone asks. F-016 is the example: DATA-02's finding is about
// which records went missing from one file, and the file was gone, so the
// evidence had to be squeezed into the failure message, which can only say what
// the case thought to ask.
//
// A case therefore names what it argues from, and the harness copies it out
// while the pods are still alive, whether the case passed or failed. A passing
// case is not the boring case here: CHAOS-05 passes with five recovery
// measurements in it, and F-017 is a run whose numbers had to be re-derived
// after the fact.
//
// Three rules keep that from becoming a problem of its own:
//
//   - One named file per registration, and nothing walks the share. DATA-10
//     creates a hundred thousand entries, and a rule that collected a directory
//     would try to bring all of them home.
//   - A byte cap, and a capture that hit it says so and says how large the file
//     really was. A bundle holding the first megabyte of a larger file while
//     looking like a whole file is worse than one holding nothing.
//   - Its own clock, per file, so that one file nobody can read does not cost
//     the bundle every other one.
//
// # What the clock cannot do
//
// A read of a file on a hard NFSv4.1 mount whose export has gone does not fail.
// It blocks in uninterruptible I/O, and cancelling the exec stream from here
// ends the harness's wait without ending the pod's read: no timeout available
// to a Kubernetes client can interrupt a process in that state. So a capture
// taken after a fault can leave a blocked reader behind in the pod.
//
// That is survivable, and it is survivable by design rather than by luck. The
// pod then does not terminate inside PodTerminateTimeout, and teardown already
// treats a pod that will not leave the API as the hazard it is: it keeps that
// pod's claim instead of destroying an export under a live mount, which is the
// sequence F-001 is about. The cost of a blocked capture is therefore a leaked
// claim, recoverable by hand, and never a wedged node. collectEvidence says so
// in the error it returns, so the run reports the trade rather than leaving it
// to be discovered.

// EvidenceMaxBytes is the most of any one file the bundle keeps. The things
// cases name are a record-per-second log and a file of short records, both far
// under this; the cap is here so that a case naming something unexpectedly
// large truncates loudly instead of pulling a share onto the workstation.
const EvidenceMaxBytes = 1 << 20

// evidenceReadTimeout bounds one file's copy, per file for the same reason
// nodeInspectTimeout is per node. It covers the read and the size query
// together: a truncated file must not cost twice what a whole one costs.
const evidenceReadTimeout = 10 * time.Second

// evidenceManifest names the index of what was captured, which is the half a
// reader needs in order to trust the other files.
const evidenceManifest = "evidence.txt"

// evidenceItem is one registered file, with the pod name already resolved: the
// case that registered it may be gone by the time it is read.
type evidenceItem struct {
	pod  string
	path string
	name string
}

// KeepPodFile registers a file inside a pod to be copied into this case's
// bundle before teardown, whether the case passes or fails. It names one
// regular file; there is no directory form, deliberately.
//
// name is the filename it lands under in artifacts/<run-id>/<CASE-ID>/, and
// path is the file inside the pod, which may be on the share or on the pod's
// own filesystem. Registering the same name twice for the same file is
// harmless; registering it for a different file is an error rather than a
// silent overwrite of the evidence registered first.
//
// Register as soon as the file exists rather than at the end of the case. A
// case that fails in the middle is the one whose evidence matters most, and it
// never reaches its last line. Nothing is read at registration, so registering
// a file that is still being written is fine: what lands in the bundle is
// whatever the file held at teardown, which for a workload still running may
// end mid-record.
func (f *Framework) KeepPodFile(pod, path, name string) error {
	if err := checkEvidenceName(name); err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("evidence %q names no path in pod %s", name, pod)
	}
	resolved := f.Name(pod)
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	for _, e := range f.state.evidence {
		if e.name != name {
			continue
		}
		if e.pod == resolved && e.path == path {
			return nil
		}
		return fmt.Errorf("evidence %q is already registered for %s:%s, so registering %s:%s under that "+
			"name would overwrite it in the bundle; give one of them a different name",
			name, e.pod, e.path, resolved, path)
	}
	f.state.evidence = append(f.state.evidence, evidenceItem{pod: resolved, path: path, name: name})
	return nil
}

// checkEvidenceName rejects anything that is not a plain filename, and the
// manifest's own name. The name becomes a path under the bundle directory and
// the id of the script that fetches the file, so a separator or a leading dot
// in it writes somewhere nobody asked for, and a capture landing on the
// manifest would leave the bundle with no account of what is in it.
//
// A helper that builds a name from a value a case chose validates it here
// before it acts, not after: a registration refused halfway through a case
// costs the evidence of everything that follows it.
func checkEvidenceName(name string) error {
	if err := CheckScriptID(name); err != nil {
		return fmt.Errorf("%q is not usable as an evidence name, since it becomes a filename in the "+
			"bundle: %w", name, err)
	}
	if name == evidenceManifest {
		return fmt.Errorf("%q is the name of the manifest that says what the bundle holds; choose another", name)
	}
	return nil
}

// registeredEvidence copies the registration list.
func (f *Framework) registeredEvidence() []evidenceItem {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	return append([]evidenceItem(nil), f.state.evidence...)
}

// collectEvidence copies every registered file into the bundle and writes the
// manifest saying what arrived, what was truncated and what could not be read.
//
// Called from teardown for every case, passed or failed, before anything is
// deleted. It never fails a case: a file the harness could not fetch is a gap
// in the harness, and reporting it as a red case would file a collection defect
// against the storage system.
func (f *Framework) collectEvidence(ctx context.Context) error {
	items := f.registeredEvidence()
	if len(items) == 0 {
		return nil
	}
	dir := CaseDir(f.CaseID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rows := make([]evidenceRow, 0, len(items))
	for _, it := range items {
		rows = append(rows, f.captureEvidence(ctx, dir, it))
	}
	var errs []string
	if err := os.WriteFile(filepath.Join(dir, evidenceManifest), []byte(renderEvidence(rows)), 0o644); err != nil {
		errs = append(errs, fmt.Sprintf("%s: %v", evidenceManifest, err))
	}
	for _, r := range rows {
		if r.Problem != "" {
			errs = append(errs, fmt.Sprintf("%s (%s:%s): %s", r.Name, r.Pod, r.Path, r.Problem))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("evidence collection had gaps: %s. A read that did not answer may still be "+
			"running in the pod, because a hard NFSv4.1 mount whose export has gone blocks "+
			"uninterruptibly and no client-side timeout ends that; the pod will then not terminate, and "+
			"teardown keeps its claim rather than destroying an export under a live mount "+
			"(docs/findings.md F-001)", strings.Join(errs, "; "))
	}
	if f.T != nil {
		f.T.Logf("evidence written to %s", dir)
	}
	return nil
}

// captureEvidence copies one file, on its own clock.
func (f *Framework) captureEvidence(ctx context.Context, dir string, it evidenceItem) evidenceRow {
	row := evidenceRow{Name: it.name, Pod: it.pod, Path: it.path}
	dest := filepath.Join(dir, it.name)
	if err := clearStaleEvidence(dest); err != nil {
		row.Problem = err.Error()
		return row
	}
	// One byte past the cap, so that a file exactly at the cap is reported
	// complete and one byte over it is reported truncated.
	script, err := RunScript("read-evidence.sh", it.name, it.path, strconv.Itoa(EvidenceMaxBytes+1))
	if err != nil {
		row.Problem = fmt.Sprintf("the read could not be built: %v", err)
		return row
	}
	readCtx, cancel := context.WithTimeout(ctx, evidenceReadTimeout)
	defer cancel()

	data, truncated, problem := classifyCapture(f.C.Exec(readCtx, Namespace, it.pod, "main", "sh", "-c", script))
	row.Truncated, row.Problem = truncated, problem
	if truncated {
		// The same deadline, not a fresh one: a file that hit the cap must not
		// be able to spend twice the per-file bound and starve the files after
		// it. A size the pod does not answer in what is left costs the manifest
		// a number, which is the cheap half of this.
		row.PodSize = f.evidenceSize(readCtx, it)
	}
	if len(data) == 0 && problem != "" {
		return row
	}
	row.Bytes = len(data)
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		row.Problem = fmt.Sprintf("read from the pod but not written: %v", err)
		row.Bytes = 0
	}
	return row
}

// clearStaleEvidence removes whatever is already at a capture's destination.
//
// A run that reuses a run id is an ordinary thing to do: -run-id exists for it,
// and triage step 1 is to run one case again. Without this, the previous run's
// file stays in the case directory next to a manifest saying this run captured
// nothing, which is the worst of the failure modes available here. It does not
// look like a gap; it looks like evidence, and it is evidence of a different
// run. A destination that cannot be cleared fails the capture rather than
// letting the file be passed off as this run's.
func clearStaleEvidence(dest string) error {
	if err := os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("an earlier file at %s could not be removed, so nothing was captured rather "+
			"than risk reporting it as this run's: %v", dest, err)
	}
	return nil
}

// classifyCapture decides what one read produced: the bytes worth keeping,
// whether the file was longer than the cap, and what to say about it.
//
// Separate from the exec so it can be tested without a cluster, because every
// mistake available here is silent. Keeping one byte too many turns a whole
// file into a truncated one; discarding what a failed read returned throws away
// the only bytes a wedged mount will ever give up; and reporting a problem the
// bundle does not carry sends triage looking for a file that is not there.
func classifyCapture(res ExecResult) (data []byte, truncated bool, problem string) {
	data = []byte(res.Stdout)
	if len(data) > EvidenceMaxBytes {
		data, truncated = data[:EvidenceMaxBytes], true
	}
	if res.Err != nil {
		// Whatever arrived before it stopped is kept anyway. A read that dies
		// partway through is what a file on a mount whose export has gone looks
		// like, and the lines that did come back are the ones nearest whatever
		// happened. The manifest says the file is short and why, so nobody
		// mistakes it for the whole thing.
		problem = fmt.Sprintf("unreadable within %s: %v: %s",
			evidenceReadTimeout, res.Err, strings.TrimSpace(res.Stderr))
	}
	return data, truncated, problem
}

// evidenceSize asks the pod how large the file really is. Only a truncated
// capture needs it, so the common path costs no second exec, and a pod that
// will not answer costs the manifest a number rather than the capture.
//
// One command with no control flow, so it stays at the call site rather than
// becoming a script, the same as the stat and df one-liners in io.go.
func (f *Framework) evidenceSize(ctx context.Context, it evidenceItem) string {
	out, err := f.C.MustSh(ctx, Namespace, it.pod, "main", "stat -c %s "+shellQuote(it.path))
	if err != nil {
		return ""
	}
	out = strings.TrimSpace(out)
	if _, err := strconv.ParseInt(out, 10, 64); err != nil {
		return ""
	}
	return out
}

// evidenceRow is what the manifest says about one registered file.
type evidenceRow struct {
	Name  string
	Pod   string
	Path  string
	Bytes int
	// Truncated means the file was larger than the cap, and PodSize is how
	// large, or empty when the pod would not say.
	Truncated bool
	PodSize   string
	// Problem is why the capture is not the whole file, empty when it is.
	// Bytes says whether anything arrived in spite of it.
	Problem string
}

// renderEvidence writes the manifest: where each file came from, how much of it
// arrived, and what was left out. Separate from the files themselves because a
// truncated capture and a whole one look identical from the bytes.
func renderEvidence(rows []evidenceRow) string {
	var sb strings.Builder
	sb.WriteString("# files this case named as its evidence, copied out before teardown\n")
	fmt.Fprintf(&sb, "# one line per file: name, source, bytes in the bundle, what was left out (cap %d bytes)\n",
		EvidenceMaxBytes)
	for _, r := range rows {
		status := "complete"
		switch {
		case r.Problem != "" && r.Bytes > 0:
			status = "PARTIAL, this is what arrived before the read stopped: " + r.Problem
		case r.Problem != "":
			status = "NOT CAPTURED: " + r.Problem
		case r.Truncated && r.PodSize != "":
			status = fmt.Sprintf("TRUNCATED at the cap; the file is %s bytes", r.PodSize)
		case r.Truncated:
			status = "TRUNCATED at the cap; the pod did not answer how large the file is"
		}
		fmt.Fprintf(&sb, "%s\t%s:%s\t%d bytes\t%s\n", r.Name, r.Pod, r.Path, r.Bytes, status)
	}
	return sb.String()
}

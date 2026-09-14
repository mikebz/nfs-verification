package framework

import (
	"context"
	"fmt"
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
// measurements in it, and F-017 is a run whose numbers needed re-deriving
// afterwards.
//
// Three rules keep that from becoming a problem of its own:
//
//   - One named file per registration, and nothing walks the share. DATA-10
//     creates a hundred thousand entries, and a rule that collected a directory
//     would try to bring all of them home.
//   - A byte cap, and a capture that hit it says so and says how large the file
//     really was. A bundle holding the first megabyte of a larger file while
//     looking like a whole file is worse than one holding nothing.
//   - Its own clock, per file. Reading a file on a hard NFSv4.1 mount whose
//     export is gone blocks forever and this runs during teardown, so one
//     unreachable file must not cost the bundle every other one.

// EvidenceMaxBytes is the most of any one file the bundle keeps. The things
// cases name are a record-per-second log and a file of short records, both far
// under this; the cap is here so that a case naming something unexpectedly
// large truncates loudly instead of pulling a share onto the workstation.
const EvidenceMaxBytes = 1 << 20

// evidenceReadTimeout bounds one file's copy, per file for the same reason
// nodeInspectTimeout is per node.
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
// manifest's own name. The name becomes a path under the bundle directory, so a
// separator or a leading dot in it writes somewhere nobody asked for, and a
// capture landing on the manifest would leave the bundle with no account of
// what is in it.
func checkEvidenceName(name string) error {
	if !scriptID.MatchString(name) {
		return fmt.Errorf("%q is not usable as an evidence name: it must start with a letter or digit and "+
			"hold only letters, digits, dot, dash and underscore, since it becomes a filename in the bundle", name)
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
		return fmt.Errorf("evidence collection had gaps: %s", strings.Join(errs, "; "))
	}
	if f.T != nil {
		f.T.Logf("evidence written to %s", dir)
	}
	return nil
}

// captureEvidence copies one file, on its own clock.
func (f *Framework) captureEvidence(ctx context.Context, dir string, it evidenceItem) evidenceRow {
	row := evidenceRow{Name: it.name, Pod: it.pod, Path: it.path}
	readCtx, cancel := context.WithTimeout(ctx, evidenceReadTimeout)
	defer cancel()

	// One byte past the cap, so that a file exactly at the cap is reported
	// complete and one byte over it is reported truncated.
	res := f.C.Exec(readCtx, Namespace, it.pod, "main", "sh", "-c", evidenceReadCmd(it.path, EvidenceMaxBytes+1))
	data := []byte(res.Stdout)
	if res.Err != nil {
		row.Problem = fmt.Sprintf("unreadable within %s: %v: %s",
			evidenceReadTimeout, res.Err, strings.TrimSpace(res.Stderr))
		// Whatever arrived before it stopped is kept anyway. A read that dies
		// partway through is what a file on a mount whose export has gone looks
		// like, and the lines that did come back are the ones nearest whatever
		// happened. The manifest says the file is short and why, so nobody
		// mistakes it for the whole thing.
		if len(data) == 0 {
			return row
		}
	}
	if len(data) > EvidenceMaxBytes {
		data = data[:EvidenceMaxBytes]
		row.Truncated = true
		row.PodSize = f.evidenceSize(ctx, it)
	}
	row.Bytes = len(data)
	if err := os.WriteFile(filepath.Join(dir, it.name), data, 0o644); err != nil {
		row.Problem = fmt.Sprintf("read from the pod but not written: %v", err)
		row.Bytes = 0
	}
	return row
}

// evidenceSize asks the pod how large the file really is. Only a truncated
// capture needs it, so the common path costs no second exec, and a pod that
// will not answer costs the manifest a number rather than the capture.
func (f *Framework) evidenceSize(ctx context.Context, it evidenceItem) string {
	sizeCtx, cancel := context.WithTimeout(ctx, evidenceReadTimeout)
	defer cancel()
	out, err := f.C.MustSh(sizeCtx, Namespace, it.pod, "main", "stat -c %s "+shellQuote(it.path))
	if err != nil {
		return ""
	}
	out = strings.TrimSpace(out)
	if _, err := strconv.ParseInt(out, 10, 64); err != nil {
		return ""
	}
	return out
}

// evidenceReadCmd is the shell that reads one file out of a pod: its bytes on
// stdout and nothing else, and a non-zero exit with a reason on stderr when the
// path is not a regular file.
//
// The directory case is refused here rather than at registration because what a
// path names is only knowable inside the pod, and `head` on a directory is an
// error on some implementations and an empty success on others. An empty
// success would put a zero-byte file in the bundle and call it the evidence.
func evidenceReadCmd(path string, limit int) string {
	quoted := shellQuote(path)
	return fmt.Sprintf("if [ ! -f %s ]; then echo 'not a regular file' >&2; exit 3; fi; head -c %d %s",
		quoted, limit, quoted)
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

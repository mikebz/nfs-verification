package framework

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The evidence registry decides what survives a case, and every way it can go
// wrong is quiet: a name that escapes the bundle directory, a second
// registration that overwrites the first, a read that returns success and no
// bytes for a directory, a truncated capture that looks like a whole file, or a
// previous run's file left sitting where this run's should be. None of those
// fails a run, because collection is deliberately not allowed to fail a case.
// They are found here or on the day someone opens the bundle and argues from
// the wrong file.

// TestKeepPodFileRejectsUnusableNames covers the names that would write outside
// the bundle or over the manifest that describes it.
//
// Steps:
//  1. Register each bad name against a fixture.
//  2. Assert every one is refused, and that nothing was recorded.
func TestKeepPodFileRejectsUnusableNames(t *testing.T) {
	for _, name := range []string{
		"", "..", "../escape.log", "sub/dir.log", ".hidden", "has space.log",
		"quote'.log", "$(whoami).log", strings.Repeat("x", 65),
		// The manifest itself: a capture landing on it would leave the bundle
		// with no account of what the rest of it is.
		evidenceManifest,
	} {
		f := &Framework{CaseID: "DATA-02", state: &caseState{}}
		if err := f.KeepPodFile("reader", "/mnt/data/f", name); err == nil {
			t.Errorf("%q was accepted as an evidence name; it becomes a filename in the bundle", name)
		}
		if got := len(f.registeredEvidence()); got != 0 {
			t.Errorf("%q was refused but %d registrations were recorded", name, got)
		}
	}
}

// TestKeepPodFileRejectsAPathlessRegistration covers the registration that
// would produce an empty file in the bundle and no sign of why.
//
// Steps:
//  1. Register a name against a path that is only whitespace.
//  2. Assert it is refused.
func TestKeepPodFileRejectsAPathlessRegistration(t *testing.T) {
	f := &Framework{CaseID: "DATA-02", state: &caseState{}}
	if err := f.KeepPodFile("reader", "   ", "data02.log"); err == nil {
		t.Fatal("a registration with no path was accepted")
	}
}

// TestKeepPodFileRefusesToOverwriteAnEarlierRegistration is the one that
// matters most. Two files registered under one name means the bundle holds one
// of them, and nothing says which: the case would argue from evidence that is
// not there.
//
// Steps:
//  1. Register a file.
//  2. Register the same pod and path under the same name again, and assert it
//     is accepted, since a helper called twice is not an error.
//  3. Register a different path under that name, and assert it is refused and
//     that the first registration survived.
func TestKeepPodFileRefusesToOverwriteAnEarlierRegistration(t *testing.T) {
	f := &Framework{CaseID: "CHAOS-05", state: &caseState{}}
	if err := f.KeepPodFile("writer", "/tmp/load-chaos05.log", "load-chaos05.log"); err != nil {
		t.Fatalf("registering the workload log: %v", err)
	}
	if err := f.KeepPodFile("writer", "/tmp/load-chaos05.log", "load-chaos05.log"); err != nil {
		t.Errorf("re-registering the same file was refused: %v", err)
	}
	err := f.KeepPodFile("verifier", "/tmp/other.log", "load-chaos05.log")
	if err == nil {
		t.Fatal("a second file was accepted under a name already registered, so one would silently win")
	}
	if !strings.Contains(err.Error(), "/tmp/load-chaos05.log") {
		t.Errorf("the refusal does not name the registration it protected: %v", err)
	}
	items := f.registeredEvidence()
	if len(items) != 1 || items[0].path != "/tmp/load-chaos05.log" {
		t.Fatalf("registrations are %+v, want only the first", items)
	}
}

// TestKeepPodFileResolvesTheCaseName checks that a logical pod name is resolved
// at registration. Collection happens in teardown, where nothing is left to
// turn "writer" into the object that actually holds the file.
//
// Steps:
//  1. Register a file against the logical pod name a case would use.
//  2. Assert the registration holds the prefixed object name instead.
func TestKeepPodFileResolvesTheCaseName(t *testing.T) {
	f := &Framework{CaseID: "CHAOS-05", state: &caseState{}}
	if err := f.KeepPodFile("writer", "/tmp/load.log", "load.log"); err != nil {
		t.Fatalf("registering: %v", err)
	}
	items := f.registeredEvidence()
	if len(items) != 1 {
		t.Fatalf("%d registrations, want 1", len(items))
	}
	if want := f.Name("writer"); items[0].pod != want {
		t.Errorf("the registration names pod %q, want the resolved %q", items[0].pod, want)
	}
}

// TestWorkloadLogNameIsKeepable checks the name the workload builds for its own
// log against the rule that will judge it.
//
// The workload validates this before it launches, because the name is longer
// than the id it is built from: an id of 60 characters passes CheckScriptID and
// produces a 69-character evidence name. Finding that out after the workload is
// already writing would stop a running measurement to complain about a
// filename.
//
// Steps:
//  1. Build the evidence name from a plausible case id and assert it is
//     keepable.
//  2. Build it from an id that is itself valid but too long to survive the
//     suffix, and assert the name is refused.
func TestWorkloadLogNameIsKeepable(t *testing.T) {
	if err := checkEvidenceName("load-" + "chaos05" + ".log"); err != nil {
		t.Errorf("an ordinary case id produced an unkeepable log name: %v", err)
	}
	long := strings.Repeat("a", 60)
	if err := CheckScriptID(long); err != nil {
		t.Fatalf("the premise of this test is wrong: %q is not a valid script id: %v", long, err)
	}
	if err := checkEvidenceName("load-" + long + ".log"); err == nil {
		t.Error("a 69-character evidence name was accepted, so the workload would fail after launching")
	}
}

// TestReadEvidenceScriptRunsUnderARealShell runs the script that fetches a file
// out of a pod, because every failure mode it has looks like success: a quoting
// slip reads the wrong path, and a directory or a missing file that exits zero
// puts an empty file in the bundle and calls it the evidence.
//
// Steps:
//  1. Read a small file and assert the bytes come back exactly, from a path
//     with a space and a quote in it.
//  2. Read a file larger than the limit and assert exactly limit bytes arrive,
//     which is what the caller reads truncation off.
//  3. Point it at a directory and at a missing path, and assert each exits
//     non-zero with something on stderr rather than returning no bytes.
func TestReadEvidenceScriptRunsUnderARealShell(t *testing.T) {
	sh := lookOrSkip(t, "sh", "head")
	script := materializeScript(t, "read-evidence.sh")
	dir := t.TempDir()

	small := filepath.Join(dir, "an odd' name.log")
	body := "OK 1 1757000000\nOK 2 1757000001\n"
	if err := os.WriteFile(small, []byte(body), 0o644); err != nil {
		t.Fatalf("writing the small file: %v", err)
	}
	out, err := exec.Command(sh, script, small, "64").Output()
	if err != nil {
		t.Fatalf("reading the small file: %v", err)
	}
	if string(out) != body {
		t.Errorf("read %q, want %q", out, body)
	}

	big := filepath.Join(dir, "big.log")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 500)), 0o644); err != nil {
		t.Fatalf("writing the large file: %v", err)
	}
	out, err = exec.Command(sh, script, big, "100").Output()
	if err != nil {
		t.Fatalf("reading the large file: %v", err)
	}
	if len(out) != 100 {
		t.Errorf("read %d bytes with a limit of 100; the caller reads truncation off this length", len(out))
	}

	for _, tc := range []struct{ what, path string }{
		{"a directory", dir},
		{"a missing file", filepath.Join(dir, "never-written.log")},
	} {
		cmd := exec.Command(sh, script, tc.path, "64")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil {
			t.Errorf("%s was read as if it were evidence, returning %d bytes", tc.what, len(out))
		}
		if strings.TrimSpace(stderr.String()) == "" {
			t.Errorf("%s failed without saying why, so the manifest would have nothing to record", tc.what)
		}
	}
}

// TestClassifyCaptureKeepsTheRightBytes covers the decision between a whole
// file, a truncated one and a read that died holding something.
//
// The caller asks for one byte past the cap so that these can be told apart,
// and this is where that extra byte is spent. Keeping it would make every
// file at the cap look truncated; dropping the bytes a failed read returned
// would throw away the only ones a wedged mount will ever give up.
//
// Steps:
//  1. Classify a short read, a read exactly at the cap, and a read one byte
//     over it, and assert only the last is truncated.
//  2. Classify a failed read that returned bytes, and assert the bytes are kept
//     and a problem is recorded.
//  3. Classify a failed read that returned nothing, and assert the problem
//     carries what the pod put on stderr.
func TestClassifyCaptureKeepsTheRightBytes(t *testing.T) {
	data, truncated, problem := classifyCapture(ExecResult{Stdout: "four"})
	if string(data) != "four" || truncated || problem != "" {
		t.Errorf("a short read gave (%q, %v, %q), want the bytes, not truncated, no problem", data, truncated, problem)
	}

	atCap := strings.Repeat("a", EvidenceMaxBytes)
	data, truncated, _ = classifyCapture(ExecResult{Stdout: atCap})
	if len(data) != EvidenceMaxBytes || truncated {
		t.Errorf("a file exactly at the cap gave %d bytes, truncated=%v; it is whole", len(data), truncated)
	}

	data, truncated, _ = classifyCapture(ExecResult{Stdout: atCap + "a"})
	if len(data) != EvidenceMaxBytes || !truncated {
		t.Errorf("a file one byte over the cap gave %d bytes, truncated=%v", len(data), truncated)
	}

	data, _, problem = classifyCapture(ExecResult{Stdout: "OK 1 100\nOK 2 101\n", Err: errors.New("stream closed")})
	if string(data) != "OK 1 100\nOK 2 101\n" {
		t.Errorf("a read that died partway discarded the %d bytes it had; those are the ones nearest the fault", len(data))
	}
	if problem == "" {
		t.Error("a read that died partway reported no problem, so the bundle would look complete")
	}

	data, _, problem = classifyCapture(ExecResult{Stderr: "not a regular file\n", Err: errors.New("exit 3")})
	if len(data) != 0 {
		t.Errorf("a failed read with no output produced %d bytes", len(data))
	}
	if !strings.Contains(problem, "not a regular file") {
		t.Errorf("the problem does not carry what the pod said: %q", problem)
	}
}

// TestClearStaleEvidenceRemovesAnEarlierRunsFile covers the stale artifact.
// Reusing a run id is ordinary -- `-run-id` exists for it, and triage step 1 is
// to run one case again -- and without this the previous run's file sits in the
// case directory next to a manifest saying this run captured nothing. That does
// not look like a gap, it looks like evidence, which is the worst failure this
// code has available to it.
//
// Steps:
//  1. Clear a destination that holds an earlier run's file, and assert it is
//     gone and no error is reported.
//  2. Clear a destination that holds nothing, and assert that is not an error.
//  3. Clear a destination something else occupies and cannot be removed, and
//     assert the error names the path, so the capture records why rather than
//     writing over it.
func TestClearStaleEvidenceRemovesAnEarlierRunsFile(t *testing.T) {
	dir := t.TempDir()

	dest := filepath.Join(dir, "data02.log")
	if err := os.WriteFile(dest, []byte("records from a run that is not this one\n"), 0o644); err != nil {
		t.Fatalf("seeding the earlier file: %v", err)
	}
	if err := clearStaleEvidence(dest); err != nil {
		t.Fatalf("clearing an earlier run's file: %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the earlier run's file survived (%v), so triage would read it as this run's evidence", err)
	}

	if err := clearStaleEvidence(filepath.Join(dir, "never-captured.log")); err != nil {
		t.Errorf("clearing a destination that holds nothing reported an error: %v", err)
	}

	occupied := filepath.Join(dir, "occupied.log")
	if err := os.MkdirAll(filepath.Join(occupied, "child"), 0o755); err != nil {
		t.Fatalf("seeding the occupied destination: %v", err)
	}
	err := clearStaleEvidence(occupied)
	if err == nil {
		t.Fatal("a destination that could not be cleared reported success")
	}
	if !strings.Contains(err.Error(), occupied) {
		t.Errorf("the error does not name the path it could not clear: %v", err)
	}
}

// TestEvidenceManifestNamesWhatWasLeftOut covers the manifest. A truncated
// capture, a partial one and a whole one are identical from the bytes, so a
// reader who cannot tell them apart will argue from a file that is missing its
// end.
//
// Steps:
//  1. Render a complete capture, a truncated one with a known size, a
//     truncated one whose size the pod would not give, a read that stopped
//     partway with bytes in hand, and one that produced nothing.
//  2. Assert each line says which it is, and that the cap is stated once.
func TestEvidenceManifestNamesWhatWasLeftOut(t *testing.T) {
	got := renderEvidence([]evidenceRow{
		{Name: "load-chaos05.log", Pod: "nfsv-chaos-05-x-writer", Path: "/tmp/load-chaos05.log", Bytes: 412},
		{Name: "data02.log", Pod: "nfsv-data-02-x-reader", Path: "/mnt/data/data02.log",
			Bytes: EvidenceMaxBytes, Truncated: true, PodSize: "4194304"},
		{Name: "wide.log", Pod: "nfsv-data-02-x-reader", Path: "/mnt/data/wide.log",
			Bytes: EvidenceMaxBytes, Truncated: true},
		{Name: "wedged.log", Pod: "nfsv-chaos-03-x-writer", Path: "/mnt/data/wedged.log",
			Bytes: 900, Problem: "unreadable within 10s: context deadline exceeded"},
		{Name: "gone.log", Pod: "nfsv-chaos-03-x-writer", Path: "/mnt/data/gone.log",
			Problem: "unreadable within 10s: timed out"},
	})
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("the manifest has %d lines, want two of preamble and five of rows:\n%s", len(lines), got)
	}
	for _, want := range []string{
		"cap 1048576 bytes",
		"load-chaos05.log\tnfsv-chaos-05-x-writer:/tmp/load-chaos05.log\t412 bytes\tcomplete",
		"TRUNCATED at the cap; the file is 4194304 bytes",
		"TRUNCATED at the cap; the pod did not answer how large the file is",
		"900 bytes\tPARTIAL, this is what arrived before the read stopped: unreadable within 10s: context deadline exceeded",
		"0 bytes\tNOT CAPTURED: unreadable within 10s: timed out",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the manifest does not say %q:\n%s", want, got)
		}
	}
}

package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The evidence registry decides what survives a case, and every way it can go
// wrong is quiet: a name that escapes the bundle directory, a second
// registration that overwrites the first, a read command that returns success
// and no bytes for a directory, or a truncated capture that looks like a whole
// file. None of those fails a run. They are found here or on the day someone
// opens the bundle and finds the wrong thing in it.

// TestKeepPodFileRejectsUnusableNames covers the names that would write outside
// the bundle or collide with something already in it.
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

// TestEvidenceReadCommandRunsUnderARealShell runs the generated command against
// real files, because every failure mode it has looks like success: a quoting
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
func TestEvidenceReadCommandRunsUnderARealShell(t *testing.T) {
	sh := lookOrSkip(t, "sh", "head")
	dir := t.TempDir()

	small := filepath.Join(dir, "an odd' name.log")
	body := "OK 1 1757000000\nOK 2 1757000001\n"
	if err := os.WriteFile(small, []byte(body), 0o644); err != nil {
		t.Fatalf("writing the small file: %v", err)
	}
	out, err := exec.Command(sh, "-c", evidenceReadCmd(small, 64)).Output()
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
	out, err = exec.Command(sh, "-c", evidenceReadCmd(big, 100)).Output()
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
		cmd := exec.Command(sh, "-c", evidenceReadCmd(tc.path, 64))
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

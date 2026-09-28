package framework

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mikebz/nfs-verification/pkg/env"
)

// TestClaimCaseDirRefusesReuse exists because a reused run id fails silently
// otherwise. Two executions of one case in one directory read as one bundle:
// the second's evidence beside the first's failure, a node file from the first
// describing a node the second never touched (#71). Nothing fails and nothing
// says so, so this is found here or by someone arguing from the wrong run's
// dmesg.
//
// Steps:
//  1. Point the artifacts directory and run id at a temp dir, with no run
//     directory yet, and claim a case: it must succeed and create both levels.
//  2. Claim a different case under the same run id: it must succeed, because
//     splitting one run across several invocations by case is supported.
//  3. Claim the first case again: it must fail, name the directory and the run
//     id so the reader knows what to change, and leave the earlier
//     execution's files untouched.
func TestClaimCaseDirRefusesReuse(t *testing.T) {
	originalDir, originalRun := cfg.ArtifactsDir, cfg.RunID
	defer func() { cfg.ArtifactsDir, cfg.RunID = originalDir, originalRun }()
	cfg.ArtifactsDir = filepath.Join(t.TempDir(), "artifacts")
	cfg.RunID = "w2-data12-20260928"

	if err := claimCaseDir("DATA-12"); err != nil {
		t.Fatalf("the first execution of DATA-12 was refused: %v", err)
	}
	if info, err := os.Stat(CaseDir("DATA-12")); err != nil || !info.IsDir() {
		t.Fatalf("the claim returned success but %s is not a directory (%v)", CaseDir("DATA-12"), err)
	}

	if err := claimCaseDir("DATA-13"); err != nil {
		t.Errorf("a different case under the same run id was refused, so a run cannot be split by case: %v", err)
	}

	earlier := filepath.Join(CaseDir("DATA-12"), "evidence.txt")
	if err := os.WriteFile(earlier, []byte("the first execution's manifest\n"), 0o644); err != nil {
		t.Fatalf("seeding the first execution's evidence: %v", err)
	}
	err := claimCaseDir("DATA-12")
	if err == nil {
		t.Fatal("a second execution of DATA-12 under one run id was allowed, so its bundle would mix with the first's")
	}
	for _, want := range []string{CaseDir("DATA-12"), cfg.RunID, "RUN_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so the reader cannot tell what to change: %v", want, err)
		}
	}
	if got, readErr := os.ReadFile(earlier); readErr != nil || string(got) != "the first execution's manifest\n" {
		t.Errorf("the refusal disturbed the first execution's files: %q, %v", got, readErr)
	}
}

// TestClaimCaseDirReportsObstruction covers the refusals that are not reuse.
// Something other than a directory where the run or case directory should be
// was put there by hand, since the harness makes neither files nor symlinks
// there, and no execution owns it. Reporting it as "already run" would send the
// reader off to choose a new run id when the fix is to remove it.
//
// Steps:
//  1. Under a fresh artifacts directory, plant one obstruction: a regular file
//     at the run directory, a regular file at the case directory, or a symlink
//     to a real directory at the case directory.
//  2. Claim the case, and assert it fails, names the path, does not use the
//     already-run wording and does not wrap ErrExist, which is what a caller
//     checking for reuse would test.
func TestClaimCaseDirReportsObstruction(t *testing.T) {
	file := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("not a directory\n"), 0o644); err != nil {
			t.Fatalf("seeding the obstruction: %v", err)
		}
	}
	symlinkToDir := func(t *testing.T, path string) {
		t.Helper()
		target := t.TempDir()
		if err := os.Symlink(target, path); err != nil {
			t.Fatalf("seeding the obstruction: %v", err)
		}
	}
	for _, tc := range []struct {
		what    string
		blocked func() string
		plant   func(*testing.T, string)
	}{
		{"a file at the run directory", RunDir, file},
		{"a file at the case directory", func() string { return CaseDir("PROV-01") }, file},
		{"a symlink at the case directory", func() string { return CaseDir("PROV-01") }, symlinkToDir},
	} {
		t.Run(tc.what, func(t *testing.T) {
			originalDir, originalRun := cfg.ArtifactsDir, cfg.RunID
			defer func() { cfg.ArtifactsDir, cfg.RunID = originalDir, originalRun }()
			cfg.ArtifactsDir = t.TempDir()
			cfg.RunID = "blocked-run"

			path := tc.blocked()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("preparing the parent of %s: %v", path, err)
			}
			tc.plant(t, path)

			err := claimCaseDir("PROV-01")
			if err == nil {
				t.Fatalf("a claim succeeded with %s", tc.what)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the error does not name the path in the way: %v", err)
			}
			if strings.Contains(err.Error(), "already run") || strings.Contains(err.Error(), "RUN_ID") {
				t.Errorf("an obstruction was reported as a reused run id: %v", err)
			}
			if errors.Is(err, fs.ErrExist) {
				t.Errorf("the error wraps ErrExist, so a caller checking for reuse would misread it: %v", err)
			}
		})
	}
}

// TestWriteRunEnvironmentWritesOnce exists because the run-level record is
// what every passing case under a run id is attributed by, and a later
// preflight under a reused id used to overwrite it before anything refused the
// case. Nothing failed: the run was quietly re-attributed, possibly to another
// cluster (#71).
//
// Steps:
//  1. Write a record for context A under a fresh run id, and assert it lands.
//  2. Write a newer record for context A, as a refreshed preflight would, and
//     assert it succeeds and the first record is left exactly as it was.
//  3. Write a record for context B, and assert it fails, names both contexts
//     and RUN_ID, and still leaves the first record as it was.
func TestWriteRunEnvironmentWritesOnce(t *testing.T) {
	originalDir, originalRun := cfg.ArtifactsDir, cfg.RunID
	defer func() { cfg.ArtifactsDir, cfg.RunID = originalDir, originalRun }()
	cfg.ArtifactsDir = t.TempDir()
	cfg.RunID = "w1-split-20260928"
	path := filepath.Join(RunDir(), "environment.json")
	read := func() string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the run's record at %s: %v", path, err)
		}
		return string(b)
	}

	if err := WriteRunEnvironment(&env.Environment{Context: "gke-w1", KubernetesVersion: "v1.37.0"}); err != nil {
		t.Fatalf("the first record under a run id was refused: %v", err)
	}
	first := read()

	if err := WriteRunEnvironment(&env.Environment{Context: "gke-w1", KubernetesVersion: "v1.38.0"}); err != nil {
		t.Errorf("a refreshed preflight against the same cluster failed, so a split run could not continue: %v", err)
	}
	if got := read(); got != first {
		t.Errorf("a later preflight against the same cluster rewrote the record the run's cases rest on:\n%s", got)
	}

	err := WriteRunEnvironment(&env.Environment{Context: "gke-w2", KubernetesVersion: "v1.37.0"})
	if err == nil {
		t.Fatal("a preflight against a second cluster under one run id was accepted")
	}
	for _, want := range []string{path, "gke-w1", "gke-w2", "RUN_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so the reader cannot tell what to change: %v", want, err)
		}
	}
	if got := read(); got != first {
		t.Errorf("the refused preflight rewrote the record anyway:\n%s", got)
	}
}

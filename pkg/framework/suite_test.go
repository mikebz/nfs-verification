package framework

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaimCaseDirRefusesASecondExecution exists because a reused run id fails
// silently otherwise. Two executions of one case in one directory read as one
// bundle: the second's evidence beside the first's failure, a node file from
// the first describing a node the second never touched (#71). Nothing fails
// and nothing says so, so this is found here or by someone arguing from the
// wrong run's dmesg.
//
// Steps:
//  1. Point the artifacts directory and run id at a temp dir, with no run
//     directory yet, and claim a case: it must succeed and create both levels.
//  2. Claim a different case under the same run id: it must succeed, because
//     splitting one run across several invocations by case is supported.
//  3. Claim the first case again: it must fail, name the directory and the run
//     id so the reader knows what to change, and leave the earlier
//     execution's files untouched.
func TestClaimCaseDirRefusesASecondExecution(t *testing.T) {
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

// TestClaimCaseDirReportsAnUnusableRunDir covers the other refusal. A run
// directory that cannot be created must not read as "already ran": that
// message sends the reader off to choose a new run id when the problem is the
// artifacts path.
//
// Steps:
//  1. Put a regular file where the run directory should be.
//  2. Claim a case, and assert it fails without the already-ran wording.
func TestClaimCaseDirReportsAnUnusableRunDir(t *testing.T) {
	originalDir, originalRun := cfg.ArtifactsDir, cfg.RunID
	defer func() { cfg.ArtifactsDir, cfg.RunID = originalDir, originalRun }()
	cfg.ArtifactsDir = t.TempDir()
	cfg.RunID = "blocked-run"
	if err := os.WriteFile(RunDir(), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("seeding the obstruction: %v", err)
	}

	err := claimCaseDir("PROV-01")
	if err == nil {
		t.Fatal("a claim under a run directory that is a regular file succeeded")
	}
	if strings.Contains(err.Error(), "already run") {
		t.Errorf("an unusable run directory was reported as a reused run id: %v", err)
	}
	if errors.Is(err, fs.ErrExist) {
		t.Errorf("the error wraps ErrExist, so a caller checking for reuse would misread it: %v", err)
	}
}

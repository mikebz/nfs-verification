package framework

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mikebz/nfs-verification/pkg/env"
)

// Suite-wide state, set once by TestMain after preflight and read by every
// fixture. Kept here rather than in the preflight package so that framework
// depends on nothing above it.
var (
	suiteClient *Client
	suiteEnv    *env.Environment
	suiteCaps   Capabilities
	suiteErr    = errors.New("framework.SetSuite was never called")
)

// SetSuite publishes the preflight result to the fixtures.
func SetSuite(c *Client, e *env.Environment, caps Capabilities) {
	suiteClient, suiteEnv, suiteCaps, suiteErr = c, e, caps, nil
}

// SuiteEnv returns the discovered environment, or nil before setup.
func SuiteEnv() *env.Environment { return suiteEnv }

// SuiteCaps returns the discovered capabilities.
func SuiteCaps() Capabilities { return suiteCaps }

// PreflightCache is where a passing preflight is remembered, one file per
// kubeconfig context. Preflight takes minutes on a cold cluster and its answers
// do not change between runs against the same cluster, so a run reuses it
// rather than repeating the probe.
func PreflightCache(context string) string {
	name := context
	if name == "" {
		name = "default"
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	return filepath.Join(Cfg().ArtifactsDir, "preflight-"+name+".json")
}

// RunDir is artifacts/<run-id>.
func RunDir() string { return filepath.Join(Cfg().ArtifactsDir, Cfg().RunID) }

// CaseDir is artifacts/<run-id>/<case-id>.
func CaseDir(caseID string) string { return filepath.Join(RunDir(), caseID) }

// claimCaseDir creates artifacts/<run-id>/<case-id> for one execution of a
// case, and refuses when it already exists: one directory holds one execution.
//
// A run id that has already run this case would otherwise put two executions
// in one bundle with nothing to tell them apart. Evidence of the second lands
// beside the failure bundle of the first, and a node file from the first stays
// behind describing a node the second never touched (#71). That has happened:
// two sessions ran DATA-12 and DATA-13 under one run id, and the second's
// evidence replaced the first's without a word. Refusing costs a retry a new
// run id; mixing costs every reader the ability to trust the bundle.
//
// Mkdir rather than a check followed by MkdirAll, so the check and the claim are
// one step, and two executions started together cannot both win it.
func claimCaseDir(caseID string) error {
	if err := os.MkdirAll(RunDir(), 0o755); err != nil {
		return fmt.Errorf("creating the run directory: %w", err)
	}
	dir := CaseDir(caseID)
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists: run id %q has already run %s, and a second execution "+
				"would mix its bundle with the first's; run it again under a new RUN_ID", dir, Cfg().RunID, caseID)
		}
		return fmt.Errorf("creating the case directory: %w", err)
	}
	return nil
}

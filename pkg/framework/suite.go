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
// one step, and two executions started together cannot both win it. Only a real
// directory at the path is an earlier execution: anything else there, a symlink
// included, was put by hand, since the harness makes none. No run owns it, and
// telling the reader to change RUN_ID would send them to the wrong fix.
func claimCaseDir(caseID string) error {
	if err := os.MkdirAll(RunDir(), 0o755); err != nil {
		return fmt.Errorf("creating the run directory: %w", err)
	}
	dir := CaseDir(caseID)
	err := os.Mkdir(dir, 0o755)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("creating the case directory: %w", err)
	}
	if info, statErr := os.Lstat(dir); statErr != nil || !info.IsDir() {
		return fmt.Errorf("%s exists but is not a directory, so no execution of %s owns it; remove it "+
			"and run again", dir, caseID)
	}
	return fmt.Errorf("%s already exists: run id %q has already run %s, and a second execution "+
		"would mix its bundle with the first's; run it again under a new RUN_ID", dir, Cfg().RunID, caseID)
}

// WriteRunEnvironment records e as artifacts/<run-id>/environment.json, the
// description of the cluster every case under the run id ran against. The first
// invocation under a run id writes it, a preflight or a suite start from a
// cached result alike, and nothing rewrites it afterwards.
//
// A later invocation under the same run id finds the record already there,
// whether it ran a fresh preflight or reused a cached one, and:
//
//   - against the same kubeconfig context, it leaves the record alone. The
//     cases already in the run rest on it, and an invocation refused by
//     claimCaseDir must not have changed anything on its way to the refusal.
//   - against another context, it fails. A run id describes one cluster, and
//     overwriting would attribute every earlier case to a cluster it never ran
//     on; keeping the old record would do the same to every later case.
//
// Before this, every fresh preflight overwrote the record, so a reused run id
// could re-attribute a whole run without a word (#71).
func WriteRunEnvironment(e *env.Environment) error {
	path := filepath.Join(RunDir(), "environment.json")
	err := e.Create(path)
	if err == nil || !errors.Is(err, fs.ErrExist) {
		return err
	}
	prior, loadErr := env.Load(path)
	if loadErr != nil {
		return fmt.Errorf("%s already exists and cannot be read, so whether run id %q describes this "+
			"cluster is unknown: %w", path, Cfg().RunID, loadErr)
	}
	if prior.Context != e.Context {
		return fmt.Errorf("%s says run id %q ran against context %q, and this preflight ran against %q; "+
			"a run id describes one cluster, so run this one under a new RUN_ID",
			path, Cfg().RunID, prior.Context, e.Context)
	}
	return nil
}

package framework

import (
	"errors"
	"path/filepath"

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

// RunDir is artifacts/<run-id>.
func RunDir() string { return filepath.Join(Cfg().ArtifactsDir, Cfg().RunID) }

// CaseDir is artifacts/<run-id>/<case-id>.
func CaseDir(caseID string) string { return filepath.Join(RunDir(), caseID) }

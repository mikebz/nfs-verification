package framework

import (
	"errors"
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

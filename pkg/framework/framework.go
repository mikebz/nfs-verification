package framework

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/env"
)

// TestingT is the part of *testing.T the fixture uses. Preflight builds a
// fixture with no test attached, so the dependency is an interface rather than
// the concrete type.
type TestingT interface {
	Helper()
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
	Failed() bool
	Cleanup(func())
	Name() string
}

// Framework is the per-test fixture: the shared clients, the environment record
// discovered at preflight, and a name prefix that keeps one case's objects
// apart from another's. It creates no namespace: everything the suite makes
// lands in the configured namespace, which defaults to default.
type Framework struct {
	T    TestingT
	C    *Client
	Env  *env.Environment
	Caps Capabilities

	Namespace string
	CaseID    string

	cleanups []func(context.Context)
	faults   []FaultEvent
}

// New creates the fixture for one case. caseID is the plan's ID, for example
// "DATA-05"; it prefixes every object the case creates, lands in labels, and
// names the artifact bundle, so a stray object can be traced back to the case
// and the run that made it.
func New(t *testing.T, caseID string, gate Gate) *Framework {
	t.Helper()
	RequireGate(t, gate)
	if suiteErr != nil {
		t.Fatalf("suite not initialized: %v", suiteErr)
	}
	f := &Framework{
		T: t, C: suiteClient, Env: suiteEnv, Caps: suiteCaps,
		CaseID:    caseID,
		Namespace: Cfg().Namespace,
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), DeleteTimeout)
		defer cancel()
		if f.T != nil && f.T.Failed() {
			// Artifacts before teardown: deleted pods tell no stories.
			if err := f.CollectArtifacts(ctx); err != nil {
				t.Logf("collecting artifacts: %v", err)
			}
		}
		for i := len(f.cleanups) - 1; i >= 0; i-- {
			f.cleanups[i](ctx)
		}
		if Cfg().KeepObjects {
			t.Logf("keeping the objects of %s in namespace %s for triage (selector %s)",
				f.CaseID, f.Namespace, f.Selector())
			return
		}
		if err := f.DeleteCaseObjects(ctx); err != nil {
			t.Logf("cleaning up after %s: %v", f.CaseID, err)
		}
	})
	return f
}

// Defer registers a cleanup that runs before the case's objects are deleted.
func (f *Framework) Defer(fn func(context.Context)) { f.cleanups = append(f.cleanups, fn) }

// Labels are stamped on everything the fixture creates.
func (f *Framework) Labels() map[string]string {
	return map[string]string{
		"nfs-verification/run":  Cfg().RunID,
		"nfs-verification/case": strings.ToLower(f.CaseID),
	}
}

// Name prefixes a logical object name with the case and run, so that two cases
// in one namespace cannot collide and a leftover object says where it came
// from. It is idempotent, so passing an already-prefixed name is harmless.
func (f *Framework) Name(logical string) string {
	prefix := f.prefix()
	if strings.HasPrefix(logical, prefix) {
		return logical
	}
	return prefix + logical
}

func (f *Framework) prefix() string {
	// The whole run id, not a suffix of it: two runs at the same time of day on
	// different days would otherwise collide on a leftover object.
	return fmt.Sprintf("nfsv-%s-%s-",
		strings.ToLower(strings.ReplaceAll(f.CaseID, "_", "-")), strings.ToLower(Cfg().RunID))
}

// Selector matches everything this case created.
func (f *Framework) Selector() string {
	return fmt.Sprintf("nfs-verification/run=%s,nfs-verification/case=%s",
		Cfg().RunID, strings.ToLower(f.CaseID))
}

// DeleteCaseObjects removes what the case made. Pods go first and are waited
// out: deleting a claim while a pod still mounts it is PROV-03's assertion, not
// a teardown strategy.
func (f *Framework) DeleteCaseObjects(ctx context.Context) error {
	pods := f.C.Kube.CoreV1().Pods(f.Namespace)
	if err := pods.DeleteCollection(ctx, DeleteNow(), ListOptions(f.Selector())); err != nil {
		return fmt.Errorf("deleting pods: %w", err)
	}
	if err := Poll(ctx, PollInterval, DeleteTimeout, func(ctx context.Context) (bool, error) {
		list, err := pods.List(ctx, ListOptions(f.Selector()))
		if err != nil {
			return false, err
		}
		return len(list.Items) == 0, fmt.Errorf("%d pods still terminating", len(list.Items))
	}); err != nil {
		return fmt.Errorf("waiting for pods to go away: %w", err)
	}
	if err := f.C.Kube.CoreV1().PersistentVolumeClaims(f.Namespace).
		DeleteCollection(ctx, metav1.DeleteOptions{}, ListOptions(f.Selector())); err != nil {
		return fmt.Errorf("deleting claims: %w", err)
	}
	return nil
}

// Logf logs through the test, prefixed with the case ID. Preflight fixtures
// have no test attached and log to stderr instead.
func (f *Framework) Logf(format string, args ...any) {
	if f.T == nil {
		log.Printf("[%s] "+format, append([]any{f.CaseID}, args...)...)
		return
	}
	f.T.Helper()
	f.T.Logf("[%s] "+format, append([]any{f.CaseID}, args...)...)
}

// Skipf skips a case for a capability reason. Cases skip by capability, never
// by platform name.
func (f *Framework) Skipf(format string, args ...any) {
	f.T.Helper()
	f.T.Skipf("[%s] "+format, append([]any{f.CaseID}, args...)...)
}

// RequireGate skips a case that is not part of the selected gate.
func RequireGate(t *testing.T, want Gate) {
	t.Helper()
	if !Cfg().Gate.Includes(want) {
		t.Skipf("case gate %s not selected by -gate=%s", want, Cfg().Gate)
	}
}

// faults is the injected-fault timeline for this case.
func (f *Framework) Faults() []FaultEvent { return f.faults }

// NewSystem builds a fixture with no test attached, for preflight and for
// tooling. The caller owns teardown via the returned close function.
func NewSystem(c *Client, e *env.Environment, caps Capabilities, caseID string) (*Framework, func(context.Context)) {
	f := &Framework{C: c, Env: e, Caps: caps, CaseID: caseID, Namespace: Cfg().Namespace}
	closeFn := func(ctx context.Context) {
		for i := len(f.cleanups) - 1; i >= 0; i-- {
			f.cleanups[i](ctx)
		}
		if Cfg().KeepObjects {
			return
		}
		_ = f.DeleteCaseObjects(ctx)
	}
	return f, closeFn
}

package framework

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// Framework is the per-test fixture: one namespace, the shared clients, and the
// environment record discovered at preflight.
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

// New creates a namespace-scoped fixture for one case. caseID is the plan's ID,
// for example "DATA-05"; it lands in labels and in the artifact bundle so a
// failure can be traced back to the case that produced it.
func New(t *testing.T, caseID string, gate Gate) *Framework {
	t.Helper()
	RequireGate(t, gate)
	if suiteErr != nil {
		t.Fatalf("suite not initialized: %v", suiteErr)
	}
	f := &Framework{T: t, C: suiteClient, Env: suiteEnv, Caps: suiteCaps, CaseID: caseID}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ns, err := f.createNamespace(ctx)
	if err != nil {
		t.Fatalf("creating namespace for %s: %v", caseID, err)
	}
	f.Namespace = ns

	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), DeleteTimeout)
		defer ccancel()
		if f.T != nil && f.T.Failed() {
			// Artifacts before teardown: a deleted namespace tells no stories.
			if err := f.CollectArtifacts(cctx); err != nil {
				t.Logf("collecting artifacts: %v", err)
			}
		}
		for i := len(f.cleanups) - 1; i >= 0; i-- {
			f.cleanups[i](cctx)
		}
		if Cfg().KeepNamespaces {
			t.Logf("keeping namespace %s for triage", f.Namespace)
			return
		}
		if err := IgnoreNotFound(f.C.Kube.CoreV1().Namespaces().Delete(cctx, f.Namespace, metav1.DeleteOptions{})); err != nil {
			t.Logf("deleting namespace %s: %v", f.Namespace, err)
		}
	})
	return f
}

// Defer registers a cleanup that runs before the namespace is deleted.
func (f *Framework) Defer(fn func(context.Context)) { f.cleanups = append(f.cleanups, fn) }

// Labels are stamped on everything the fixture creates.
func (f *Framework) Labels() map[string]string {
	return map[string]string{
		"nfs-verification/run":  Cfg().RunID,
		"nfs-verification/case": strings.ToLower(f.CaseID),
	}
}

func (f *Framework) createNamespace(ctx context.Context) (string, error) {
	base := fmt.Sprintf("nfsv-%s-", strings.ToLower(strings.ReplaceAll(f.CaseID, "_", "-")))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: base,
		Labels:       f.Labels(),
	}}
	created, err := f.C.Kube.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return created.Name, nil
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
func NewSystem(ctx context.Context, c *Client, e *env.Environment, caps Capabilities, caseID string) (*Framework, func(context.Context), error) {
	f := &Framework{C: c, Env: e, Caps: caps, CaseID: caseID}
	ns, err := f.createNamespace(ctx)
	if err != nil {
		return nil, nil, err
	}
	f.Namespace = ns
	closeFn := func(ctx context.Context) {
		for i := len(f.cleanups) - 1; i >= 0; i-- {
			f.cleanups[i](ctx)
		}
		if Cfg().KeepNamespaces {
			return
		}
		_ = IgnoreNotFound(c.Kube.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}))
	}
	return f, closeFn, nil
}

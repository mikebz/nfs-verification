package framework

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/env"
)

// Namespace is where the suite puts everything it creates. Fixed for now: the
// cases need one namespace, not a namespace each, and the node agent is
// privileged, so a cluster that enforces a restricted Pod Security level on
// this namespace cannot run the suite as it stands.
const Namespace = "default"

// Framework is the per-test fixture: the shared clients, the environment record
// discovered at preflight, and a name prefix that keeps one case's objects
// apart from another's. It creates no namespace; everything lands in Namespace.
//
// Build one with New, NewSystem or SubTest. The accumulated state behind it is
// a pointer those three set, so a Framework assembled field by field is not
// usable.
type Framework struct {
	// T is nil for the fixture preflight uses, which has no test attached.
	T    *testing.T
	C    *Client
	Env  *env.Environment
	Caps Capabilities

	CaseID string

	// state is everything the case accumulates, held behind a pointer so that a
	// fixture bound to a subtest shares it. A copy with its own cleanup list
	// would silently drop the cleanups a subtest registered.
	state *caseState
}

// caseState is one case's accumulated state, shared by every fixture that
// belongs to it.
type caseState struct {
	cleanups []func(context.Context)
	faults   []FaultEvent

	// mu guards the two maps below. Cases drive several pods at once, so the
	// locktool install and the unproven-claim record are both reachable from
	// more than one goroutine.
	mu sync.Mutex
	// lockTool remembers which pods already carry the binary, and whether the
	// copy succeeded, so the result is one install per pod and not one per call.
	lockTool map[string]error
	// unproven records claims whose mount was never observed to go away, keyed
	// by claim name and holding the reason. Teardown refuses to delete these.
	unproven map[string]string
}

// SubTest returns a fixture bound to a subtest's *testing.T.
//
// A case with halves that are gated differently runs them under t.Run, and a
// helper that fails fatally inside one must stop that subtest. Without this it
// would call FailNow on the parent from the subtest's goroutine, which fails
// the case by way of a runtime.Goexit the testing package reports as a panic,
// several lines away from what actually went wrong.
//
// Everything but the *testing.T is shared, deliberately: objects a subtest
// creates are the case's objects, and the case owns their lifetime.
func (f *Framework) SubTest(t *testing.T) *Framework {
	sub := *f
	sub.T = t
	return &sub
}

// MarkClaimUnproven records that a claim may still be mounted by a node,
// because the pod that mounted it left the API before kubelet unmounted.
// Teardown keeps such a claim rather than destroying an export under a live
// mount; see docs/findings.md F-001.
func (f *Framework) MarkClaimUnproven(claim, why string) {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	if f.state.unproven == nil {
		f.state.unproven = map[string]string{}
	}
	f.state.unproven[claim] = why
}

// ClearClaimUnproven records an observed unmount. Only an observation clears
// the mark: a wait that timed out leaves it set, which is what makes teardown
// safe for a case that failed between the force delete and the wait.
func (f *Framework) ClearClaimUnproven(claim string) {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	delete(f.state.unproven, claim)
}

// unprovenClaims returns a copy of the record.
func (f *Framework) unprovenClaims() map[string]string {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	out := make(map[string]string, len(f.state.unproven))
	for k, v := range f.state.unproven {
		out[k] = v
	}
	return out
}

// New creates the fixture for one case. caseID is the plan's ID, for example
// "DATA-05"; it prefixes every object the case creates, lands in labels, and
// names the artifact bundle, so a stray object can be traced back to the case
// and the run that made it.
func New(t *testing.T, caseID string) *Framework {
	t.Helper()
	if suiteErr != nil {
		t.Fatalf("suite not initialized: %v", suiteErr)
	}
	f := &Framework{T: t, C: suiteClient, Env: suiteEnv, Caps: suiteCaps, CaseID: caseID, state: &caseState{}}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), DeleteTimeout)
		defer cancel()
		if t.Failed() {
			// Artifacts before teardown: deleted pods tell no stories. On its
			// own clock, so that a sick node cannot spend the whole budget here
			// and leave nothing for cleanup.
			artifactCtx, artifactCancel := context.WithTimeout(ctx, ArtifactTimeout)
			if err := f.CollectArtifacts(artifactCtx); err != nil {
				t.Logf("collecting artifacts: %v", err)
			}
			artifactCancel()
		}
		for i := len(f.state.cleanups) - 1; i >= 0; i-- {
			f.state.cleanups[i](ctx)
		}
		if Cfg().KeepObjects {
			t.Logf("keeping the objects of %s in namespace %s for triage (selector %s)",
				f.CaseID, Namespace, f.Selector())
			return
		}
		if err := f.DeleteCaseObjects(ctx); err != nil {
			t.Logf("cleaning up after %s: %v", f.CaseID, err)
		}
	})
	return f
}

// Defer registers a cleanup that runs before the case's objects are deleted.
// Registered against the case, not against whichever subtest happened to call
// it, because the objects belong to the case.
func (f *Framework) Defer(fn func(context.Context)) {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	f.state.cleanups = append(f.state.cleanups, fn)
}

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
//
// The logical name is lowercased, because Kubernetes object names are RFC 1123
// and a capital letter is rejected at creation. That normalization has a
// consequence worth stating: two logical names differing only in case are the
// same object. A case comparing two clients must not name them "rootA" and
// "roota", or it will compare a client with itself and pass vacuously.
//
// Lowercasing is all this does. Anything else RFC 1123 forbids is caught by
// CheckObjectName where the name becomes an object, rather than being silently
// repaired into a name the caller did not ask for.
func (f *Framework) Name(logical string) string {
	logical = strings.ToLower(logical)
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

// DeleteCaseObjects removes what the case made. Pods go first, gracefully, and
// are waited out before any claim is touched.
//
// Graceful is not a nicety here. A force delete removes the pod from the API
// before kubelet has unmounted, the claim then goes, the provisioner destroys
// the export, and the node is left retrying RPCs against an export that no
// longer exists. On a hard NFSv4.1 mount that retry loop is uninterruptible: it
// wedges kubelet's volume manager and takes the node out of service. See
// docs/findings.md.
func (f *Framework) DeleteCaseObjects(ctx context.Context) error {
	pods := f.C.Kube.CoreV1().Pods(Namespace)
	if err := pods.DeleteCollection(ctx, metav1.DeleteOptions{}, ListOptions(f.Selector())); err != nil {
		return fmt.Errorf("deleting pods: %w", err)
	}
	var stuck []corev1.Pod
	waitErr := Poll(ctx, PollInterval, PodTerminateTimeout, func(ctx context.Context) (bool, error) {
		list, err := pods.List(ctx, ListOptions(f.Selector()))
		if err != nil {
			return false, err
		}
		stuck = list.Items
		return len(stuck) == 0, fmt.Errorf("%d pods still terminating", len(stuck))
	})
	unproven := f.unprovenClaims()
	if waitErr == nil && len(unproven) == 0 {
		if err := f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace).
			DeleteCollection(ctx, metav1.DeleteOptions{}, ListOptions(f.Selector())); err != nil {
			return fmt.Errorf("deleting claims: %w", err)
		}
		return nil
	}

	// Two ways a claim can still be mounted by a node. A pod that outlives the
	// wait means a node stopped answering. A claim still marked unproven means
	// a case force-deleted the pod holding it and the unmount was never
	// observed, which teardown cannot see for itself because the pod is no
	// longer in the API.
	//
	// Either way the claim must stay: deleting one destroys an export under a
	// live mount, which is how a sick node becomes an unusable node
	// (docs/findings.md F-001). Everything neither reason covers is safe to
	// remove, so the run leaks as little as it can and says what it left and why.
	held := f.claimsHeldBy(stuck)
	for name := range unproven {
		held[name] = true
	}
	kept, deleted, delErr := f.deleteUnheldClaims(ctx, held)
	var reasons []string
	if len(stuck) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d pods did not terminate within %s (%s)",
			len(stuck), PodTerminateTimeout, describePods(stuck)))
	}
	for name, why := range unproven {
		reasons = append(reasons, fmt.Sprintf("claim %s has an unproven unmount: %s", name, why))
	}
	sort.Strings(reasons)
	msg := fmt.Sprintf("%s; deleted %d claims, kept %d still mounted (%s)",
		strings.Join(reasons, "; "), deleted, len(kept), strings.Join(kept, ", "))
	if delErr != nil {
		return fmt.Errorf("%s: %w", msg, delErr)
	}
	return fmt.Errorf("%s: clean these up by hand once the node recovers", msg)
}

// claimsHeldBy returns the claims the given pods still mount.
func (f *Framework) claimsHeldBy(pods []corev1.Pod) map[string]bool {
	held := map[string]bool{}
	for i := range pods {
		for _, v := range pods[i].Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				held[v.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
	return held
}

// deleteUnheldClaims removes this case's claims except the ones named in held.
func (f *Framework) deleteUnheldClaims(ctx context.Context, held map[string]bool) (kept []string, deleted int, err error) {
	claims := f.C.Kube.CoreV1().PersistentVolumeClaims(Namespace)
	list, err := claims.List(ctx, ListOptions(f.Selector()))
	if err != nil {
		return nil, 0, fmt.Errorf("listing claims: %w", err)
	}
	for i := range list.Items {
		name := list.Items[i].Name
		if held[name] {
			kept = append(kept, name)
			continue
		}
		if err := IgnoreNotFound(claims.Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
			return kept, deleted, fmt.Errorf("deleting claim %s: %w", name, err)
		}
		deleted++
	}
	return kept, deleted, nil
}

// describePods names pods and the nodes holding them, which is what an operator
// needs to know to go and look.
func describePods(pods []corev1.Pod) string {
	var parts []string
	for i := range pods {
		parts = append(parts, pods[i].Name+" on "+pods[i].Spec.NodeName)
	}
	return strings.Join(parts, ", ")
}

// NewSystem builds a fixture with no test attached, for preflight and for
// tooling. The caller owns teardown via the returned close function.
func NewSystem(c *Client, e *env.Environment, caps Capabilities, caseID string) (*Framework, func(context.Context)) {
	f := &Framework{C: c, Env: e, Caps: caps, CaseID: caseID, state: &caseState{}}
	closeFn := func(ctx context.Context) {
		for i := len(f.state.cleanups) - 1; i >= 0; i-- {
			f.state.cleanups[i](ctx)
		}
		if Cfg().KeepObjects {
			return
		}
		_ = f.DeleteCaseObjects(ctx)
	}
	return f, closeFn
}

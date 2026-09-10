package e2e

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/framework"
)

// mountFailureReasons are the reasons kubelet and the attach-detach controller
// use when a volume cannot be made ready. Any of them satisfies OBS-04; what
// the case will not accept is silence.
var mountFailureReasons = map[string]bool{
	"FailedMount":        true,
	"FailedAttachVolume": true,
}

// OBS-04: a mount that fails must reach the operator as a Kubernetes Event with
// a reason they can act on. A failure an operator can only find by reading
// kubelet logs on the node is, in practice, a silent failure, and the pod being
// stuck in ContainerCreating says nothing about why.
//
// The failure is manufactured with a volume pointing at an export that does not
// exist, on an address RFC 5737 reserves for documentation. The real export and
// the real server are not touched, so this case cannot disturb anything else in
// the run.
//
// Steps:
//  1. Create a PV pointing at an unroutable export and a claim bound to it by
//     name, so it can bind to nothing else.
//  2. Create a pod on it, without waiting for Ready, since it never will be.
//  3. Wait for a Warning event with a mount failure reason.
//  4. Assert the message names what could not be mounted; a message that omits
//     the cause is logged rather than failed, since the wording is kubelet's.
//  5. Assert no container reported Ready with a volume that never mounted.
//  6. Delete the pod and wait for it to leave the API before teardown reaches
//     the claim.
func TestMountFailureSurfacesAsEvent(t *testing.T) {
	f := framework.New(t, "OBS-04")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	pv, claim, err := f.CreateBrokenNFSVolume(ctx, framework.BrokenNFSSpec{Name: "obs04"})
	if err != nil {
		t.Skipf("blocked: this cluster does not allow a static NFS PersistentVolume, so a mount failure "+
			"cannot be manufactured without disturbing the real export: %v", err)
	}
	t.Logf("claim %s is bound to %s, which points at %s:%s and can never mount",
		claim.Name, pv.Name, framework.UnroutableServer, pv.Spec.NFS.Path)

	// The pod is created without waiting for Ready: it never will be. Waiting
	// is what the Event assertion is for.
	spec := toolsPod("stuck", claim.Name, "")
	obj, err := f.PodBuilder(spec)
	if err != nil {
		t.Fatalf("building the pod: %v", err)
	}
	if _, err := f.C.Kube.CoreV1().Pods(framework.Namespace).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the pod: %v", err)
	}

	// Generous, and for a reason: mount.nfs retries before it gives up, and
	// kubelet has its own operation timeout in front of that. The case is
	// asserting that the report arrives at all, not how fast.
	const reportWithin = 8 * time.Minute
	ev, err := f.WaitPodEvent(ctx, spec.Name, reportWithin, func(e corev1.Event) bool {
		return e.Type == corev1.EventTypeWarning && mountFailureReasons[e.Reason]
	})
	if err != nil {
		t.Fatalf("no mount failure was reported on the pod within %s, so a volume that can never mount "+
			"is invisible to anyone who is not reading kubelet logs on the node: %v", reportWithin, err)
	}
	t.Logf("mount failure surfaced as %s/%s: %s", ev.Type, ev.Reason, strings.TrimSpace(ev.Message))

	// Actionable, not merely present. The message has to name what failed, or
	// an operator with fifty pods learns nothing from it.
	msg := ev.Message
	names := []string{pv.Name, claim.Name, spec.Name, "vol0"}
	if !containsAny(msg, names) {
		t.Errorf("the mount failure event names none of %v, so it does not say what could not be mounted: %q",
			names, msg)
	}
	// And it should say something about the failure itself rather than only
	// that something timed out. This is a warning rather than a failure: the
	// wording is kubelet's, and a cluster that reports the volume without the
	// cause is still far better than silence.
	if !containsAny(strings.ToLower(msg), []string{"mount", "attach", "nfs", "timed out", "timeout", "connection"}) {
		t.Logf("the event names the volume but not the failure, which makes triage slower than it should be: %q", msg)
	}

	// The pod must not have been reported Ready with a volume that never
	// mounted, which would be the worse failure: a lie rather than silence.
	pod, err := f.C.Kube.CoreV1().Pods(framework.Namespace).Get(ctx, f.Name(spec.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading the pod: %v", err)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Ready {
			t.Errorf("container %s reports Ready while its volume has never mounted", cs.Name)
		}
	}

	// Delete the pod before the fixture tears the claim down. Nothing ever
	// mounted here, so there is no export to pull out from under a live mount,
	// but the ordering rule from F-001 is worth keeping unconditional.
	if err := f.DeletePod(ctx, spec.Name); err != nil {
		t.Fatalf("deleting the pod: %v", err)
	}
	if err := f.WaitPodGone(ctx, spec.Name, framework.DeleteTimeout); err != nil {
		t.Errorf("the pod did not leave the API after its volume failed to mount: %v", err)
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

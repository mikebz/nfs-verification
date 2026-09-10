package e2e

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/framework"
)

// PROV-01: dynamic provision, bind, mount, write, delete. The PV must be
// removed, not merely unbound: an unreclaimed backing volume is a leak that
// surfaces weeks later as a quota failure with no obvious cause.
func TestProvisionMountWriteDelete(t *testing.T) {
	f := framework.New(t, "PROV-01")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	pvc := f.MustRWXPVC(ctx, "prov01")
	// The pod comes before the bind check: a class that binds on first consumer
	// has nothing to bind to until something is scheduled.
	pod := f.MustPod(ctx, toolsPod("writer", pvc.Name, ""))
	if _, err := f.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout); err != nil {
		t.Fatalf("claim did not bind once a pod consumed it: %v", err)
	}
	pv, err := f.PVForClaim(ctx, pvc.Name)
	if err != nil {
		t.Fatalf("resolving the bound PV: %v", err)
	}
	t.Logf("claim %s bound to %s on StorageClass %s", pvc.Name, pv.Name, f.Env.StorageClass)
	want, err := f.WriteFile(ctx, pod.Name, fileIn("prov01.dat"), 1<<20, "prov01")
	if err != nil {
		t.Fatalf("writing to the share: %v", err)
	}
	got, err := f.Sha256(ctx, pod.Name, fileIn("prov01.dat"))
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if got != want {
		t.Fatalf("checksum mismatch on read back: got %s want %s", got, want)
	}

	// Graceful, and waited out: the claim below must not outlive the mount.
	if err := f.DeletePod(ctx, pod.Name); err != nil {
		t.Fatalf("deleting the pod: %v", err)
	}
	if err := f.WaitPodGone(ctx, pod.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("pod did not go away: %v", err)
	}
	if err := f.C.Kube.CoreV1().PersistentVolumeClaims(framework.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting the claim: %v", err)
	}
	if err := f.WaitPVCGone(ctx, pvc.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("claim was not removed: %v", err)
	}

	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Skipf("reclaim policy on %s is %s, so PV removal is not expected here; PROV-06 covers Retain",
			pv.Name, pv.Spec.PersistentVolumeReclaimPolicy)
	}
	if err := f.WaitPVGone(ctx, pv.Name, framework.DeleteTimeout); err != nil {
		t.Errorf("backing volume was not reclaimed: %v", err)
	}
}

// PROV-03: delete a claim a pod still mounts. The claim must stay Terminating
// until the mount is gone, and the pod must keep working while it does.
//
// This is the case that stands between an ordinary `kubectl delete pvc` and
// F-001 in docs/findings.md: without the protection, the export is destroyed
// under a live hard NFSv4.1 mount and the client node retries forever.
func TestDeleteClaimUnderLiveMount(t *testing.T) {
	f := framework.New(t, "PROV-03")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	pvc := f.MustRWXPVC(ctx, "prov03")
	pod := f.MustPod(ctx, toolsPod("holder", pvc.Name, ""))
	if _, err := f.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout); err != nil {
		t.Fatalf("claim did not bind once a pod consumed it: %v", err)
	}
	want, err := f.WriteFile(ctx, pod.Name, fileIn("prov03.dat"), 1<<20, "prov03")
	if err != nil {
		t.Fatalf("writing to the share: %v", err)
	}

	if err := f.DeletePVC(ctx, pvc.Name); err != nil {
		t.Fatalf("deleting the claim: %v", err)
	}

	// Sustained, not sampled once: the assertion is that the claim survives for
	// as long as the mount does, and a single Get right after the delete would
	// pass even if the object vanished a second later.
	const observe = 20 * time.Second
	deadline := time.Now().Add(observe)
	for time.Now().Before(deadline) {
		live, err := f.GetPVC(ctx, pvc.Name)
		if err != nil {
			t.Fatalf("claim %s was removed while %s still mounted it: %v", pvc.Name, pod.Name, err)
		}
		if live.DeletionTimestamp == nil {
			t.Fatalf("claim %s has no deletion timestamp, so the delete never reached the API", pvc.Name)
		}
		if !hasFinalizer(live.Finalizers, "kubernetes.io/pvc-protection") {
			t.Errorf("claim %s is Terminating without the pvc-protection finalizer (%v); "+
				"nothing is holding the export open for the live mount", pvc.Name, live.Finalizers)
			break
		}
		time.Sleep(2 * time.Second)
	}

	// The protection is only worth having if the mount still works underneath
	// it, so the case reads through it rather than trusting the object state.
	got, err := f.Sha256(ctx, pod.Name, fileIn("prov03.dat"))
	if err != nil {
		t.Fatalf("reading the share while its claim is Terminating: %v", err)
	}
	if got != want {
		t.Errorf("checksum changed while the claim was Terminating: got %s want %s", got, want)
	}
	if _, err := f.WriteFile(ctx, pod.Name, fileIn("prov03-after.dat"), 1<<16, "prov03-after"); err != nil {
		t.Errorf("writing to the share while its claim is Terminating: %v", err)
	}

	// Graceful, so kubelet unmounts before the pod leaves the API. See F-001.
	if err := f.DeletePod(ctx, pod.Name); err != nil {
		t.Fatalf("deleting the pod: %v", err)
	}
	if err := f.WaitPodGone(ctx, pod.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("pod did not go away: %v", err)
	}
	if err := f.WaitPVCGone(ctx, pvc.Name, framework.DeleteTimeout); err != nil {
		t.Errorf("claim stayed Terminating after the last mount was gone: %v", err)
	}
}

func hasFinalizer(finalizers []string, want string) bool {
	for _, f := range finalizers {
		if f == want {
			return true
		}
	}
	return false
}

// PROV-04: volume expansion. Capacity grows, existing data survives, and the
// workload keeps its mount throughout. A class that does not advertise
// expansion must reject the request rather than accept a resize it will never
// perform, which leaves the claim wedged in Resizing with nothing to fix it.
func TestVolumeExpansion(t *testing.T) {
	f := framework.New(t, "PROV-04")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	pvc := f.MustRWXPVC(ctx, "prov04")
	pod := f.MustPod(ctx, toolsPod("writer", pvc.Name, ""))
	bound, err := f.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout)
	if err != nil {
		t.Fatalf("claim did not bind once a pod consumed it: %v", err)
	}
	before := bound.Spec.Resources.Requests[corev1.ResourceStorage]
	grown := before.DeepCopy()
	grown.Add(resource.MustParse("1Gi"))

	if !f.Caps.CanExpand {
		// Not a skip. The plan asks for a clean rejection here, and a silent
		// acceptance is the defect: a resize the driver cannot perform leaves
		// the claim in Resizing with no way back.
		if err := f.ExpandPVC(ctx, pvc.Name, grown.String()); err == nil {
			t.Fatalf("StorageClass %s does not advertise expansion, yet the API accepted a resize from %s to %s",
				f.Env.StorageClass, before.String(), grown.String())
		} else {
			t.Logf("expansion is unsupported on StorageClass %s and was rejected cleanly: %v",
				f.Env.StorageClass, err)
		}
		live, err := f.GetPVC(ctx, pvc.Name)
		if err != nil {
			t.Fatalf("re-reading the claim: %v", err)
		}
		if got := live.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(before) != 0 {
			t.Errorf("a rejected resize still changed the request: %s, was %s", got.String(), before.String())
		}
		return
	}

	want, err := f.WriteFile(ctx, pod.Name, fileIn("prov04.dat"), 1<<20, "prov04")
	if err != nil {
		t.Fatalf("writing to the share: %v", err)
	}
	seenBefore, err := f.MountCapacity(ctx, pod.Name, mountPath)
	if err != nil {
		t.Fatalf("reading capacity from the pod: %v", err)
	}
	restartsBefore, err := f.PodRestarts(ctx, pod.Name)
	if err != nil {
		t.Fatalf("reading the pod's restart count: %v", err)
	}

	if err := f.ExpandPVC(ctx, pvc.Name, grown.String()); err != nil {
		t.Fatalf("requesting expansion from %s to %s: %v", before.String(), grown.String(), err)
	}
	capacity, err := f.WaitPVCCapacity(ctx, pvc.Name, grown.String(), framework.ExpandTimeout)
	if err != nil {
		t.Fatalf("claim capacity did not reach %s: %v", grown.String(), err)
	}
	t.Logf("claim %s expanded from %s to %s", pvc.Name, before.String(), capacity.String())

	// Existing data intact, and the mount still the same mount: an expansion
	// that silently remounts the share is not the operation that was asked for.
	got, err := f.Sha256(ctx, pod.Name, fileIn("prov04.dat"))
	if err != nil {
		t.Fatalf("reading back after expansion: %v", err)
	}
	if got != want {
		t.Errorf("data written before expansion did not survive it: got %s want %s", got, want)
	}
	if _, err := f.WriteFile(ctx, pod.Name, fileIn("prov04-after.dat"), 1<<20, "prov04-after"); err != nil {
		t.Errorf("writing after expansion: %v", err)
	}
	restartsAfter, err := f.PodRestarts(ctx, pod.Name)
	if err != nil {
		t.Fatalf("re-reading the pod's restart count: %v", err)
	}
	if restartsAfter != restartsBefore {
		t.Errorf("the client restarted during expansion: %d restarts, was %d", restartsAfter, restartsBefore)
	}

	seenAfter, err := f.MountCapacity(ctx, pod.Name, mountPath)
	if err != nil {
		t.Fatalf("reading capacity from the pod after expansion: %v", err)
	}
	switch {
	case seenAfter.TotalBytes > seenBefore.TotalBytes:
		t.Logf("the workload sees the new capacity: df reports %d bytes, was %d",
			seenAfter.TotalBytes, seenBefore.TotalBytes)
	case seenAfter.TotalBytes == seenBefore.TotalBytes:
		// Not a failure, and not ignored either. A directory-backed export with
		// no per-volume quota reports the whole backing filesystem to every
		// client, so df cannot move. Which of the two shapes is in play is
		// PROV-11's question, and it needs the fan-out preflight records.
		t.Logf("df still reports %d bytes after expansion; this export does not appear to enforce "+
			"per-volume capacity, so the control plane grew and the client's view could not. "+
			"PROV-11 decides which assertion applies from the server fan-out",
			seenAfter.TotalBytes)
	default:
		t.Errorf("the share shrank across an expansion: df reports %d bytes, was %d",
			seenAfter.TotalBytes, seenBefore.TotalBytes)
	}
}

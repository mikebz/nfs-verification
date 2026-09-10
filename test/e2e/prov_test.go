package e2e

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

	if err := f.DeletePodNow(ctx, pod.Name); err != nil {
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

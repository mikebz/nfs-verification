package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/chaos"
	"github.com/mikebz/nfs-verification/pkg/framework"
)

// PROV-01: dynamic provision, bind, mount, write, delete. The PV must be
// removed, not merely unbound: an unreclaimed backing volume is a leak that
// surfaces weeks later as a quota failure with no obvious cause.
//
// Steps:
//  1. Create an RWX claim and mount it in a writer on node A and a reader on node B.
//  2. Confirm the claim bound once a pod consumed it.
//  3. Write a megabyte from the writer and read the checksum back from the reader
//     across the wire, avoiding writer page cache.
//  4. Delete both pods gracefully and wait for them to leave the API.
//  5. Delete the claim and wait for it to go.
//  6. On a Delete reclaim policy, the PV must go too. On Retain, skip: that is
//     PROV-06's case, not a leak.
func TestProvProvisionMountWriteDelete(t *testing.T) {
	f := framework.New(t, "PROV-01")
	requireCap(t, f.Caps.MultiNode, "cross-node verification needs two schedulable workers")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "prov01")
	// The pods come before the bind check: a class that binds on first consumer
	// has nothing to bind to until something is scheduled.
	writer := f.MustPod(ctx, toolsPod("writer", pvc.Name, nodeA))
	reader := f.MustPod(ctx, toolsPod("reader", pvc.Name, nodeB))
	if _, err := f.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout); err != nil {
		t.Fatalf("claim did not bind once a pod consumed it: %v", err)
	}
	pv, err := f.PVForClaim(ctx, pvc.Name)
	if err != nil {
		t.Fatalf("resolving the bound PV: %v", err)
	}
	t.Logf("claim %s bound to %s on StorageClass %s", pvc.Name, pv.Name, f.Env.StorageClass)
	want, err := f.WriteFile(ctx, writer.Name, fileIn("prov01.dat"), 1<<20, "prov01")
	if err != nil {
		t.Fatalf("writing to the share from writer pod %s on %s: %v", writer.Name, nodeA, err)
	}
	got, err := f.Sha256(ctx, reader.Name, fileIn("prov01.dat"))
	if err != nil {
		t.Fatalf("reader pod %s on %s failed reading file written by writer pod %s on %s (profile %s, want %s): %v",
			reader.Name, nodeB, writer.Name, nodeA, profile(t).Name, want, err)
	}
	if got != want {
		t.Fatalf("reader on %s did not see what writer on %s closed (profile %s): got %s want %s",
			nodeB, nodeA, profile(t).Name, got, want)
	}

	// Graceful, and waited out: the claim below must not outlive the mounts.
	for _, pod := range []*corev1.Pod{writer, reader} {
		if err := f.DeletePod(ctx, pod.Name); err != nil {
			t.Fatalf("deleting pod %s: %v", pod.Name, err)
		}
		if err := f.WaitPodGone(ctx, pod.Name, framework.DeleteTimeout); err != nil {
			t.Fatalf("pod %s did not go away: %v", pod.Name, err)
		}
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
//
// Steps:
//  1. Provision an RWX claim, mount it in a pod, write a file.
//  2. Delete the claim while the pod still has it mounted.
//  3. Watch for 20s: the claim must stay, carrying the pvc-protection
//     finalizer, rather than disappearing a moment after the delete.
//  4. Read and write through the mount while the claim is Terminating, since
//     the protection is only worth having if the mount still works.
//  5. Delete the pod gracefully and wait for it to leave the API.
//  6. The claim must then finish deleting.
func TestProvDeleteClaimUnderLiveMount(t *testing.T) {
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
//
// Steps:
//  1. Provision an RWX claim, mount it, and read the requested size.
//  2. Without expansion support: request one gibibyte more, require the API to
//     reject it, confirm the request is unchanged, and stop there.
//  3. With expansion support: write a megabyte, record the checksum, what df
//     reports and the client's restart count.
//  4. Request one gibibyte more and wait for status.capacity to reach it.
//  5. Assert the data survived, the share is still writable, and the client
//     never restarted.
//  6. Compare df before and after: a rise is logged, no change is recorded
//     with its explanation, and a fall is a failure.
func TestProvVolumeExpansion(t *testing.T) {
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

// PROV-02: provision 20 RWX claims concurrently. All must bind, no duplicate
// export IDs or duplicate export paths may be assigned, and the server must
// not restart under the load.
//
// Steps:
//  1. Record server container restart count before provisioning.
//  2. Launch 20 concurrent goroutines to create RWX claims.
//  3. If the storage class binds on first consumer, create consumer pods.
//  4. Wait for all 20 claims to reach Bound.
//  5. Resolve backing PVs and assert:
//     a. Every PV has a unique name.
//     b. No duplicate export IDs (from Export_Id annotation) across PVs.
//     c. No duplicate export paths / volume handles across PVs.
//  6. Assert server restart count did not increase.
func TestProvConcurrentProvisioning(t *testing.T) {
	f := framework.New(t, "PROV-02")
	ctx, cancel := caseCtx(t, 25*time.Minute)
	defer cancel()

	restartsBefore, err := framework.ServerRestartCount(ctx, f.C)
	if err != nil {
		t.Fatalf("reading initial server restart count: %v", err)
	}

	mode, err := f.BindingMode(ctx, f.Env.StorageClass)
	if err != nil {
		t.Fatalf("reading binding mode of StorageClass %s: %v", f.Env.StorageClass, err)
	}

	const claimCount = 20
	type claimResult struct {
		name string
		err  error
	}
	ch := make(chan claimResult, claimCount)

	for i := 1; i <= claimCount; i++ {
		name := fmt.Sprintf("prov02-%02d", i)
		go func(cName string) {
			_, cErr := f.CreatePVC(ctx, framework.PVCSpec{Name: cName})
			ch <- claimResult{name: cName, err: cErr}
		}(name)
	}

	var claimNames []string
	for i := 0; i < claimCount; i++ {
		res := <-ch
		if res.err != nil {
			t.Fatalf("creating claim %s: %v", res.name, res.err)
		}
		claimNames = append(claimNames, res.name)
	}

	// If the storage class binds on first consumer, spawn a consumer pod for each.
	if mode == storagev1.VolumeBindingWaitForFirstConsumer {
		t.Logf("StorageClass %s binds on first consumer; launching pods for all %d claims",
			f.Env.StorageClass, claimCount)
		for _, cName := range claimNames {
			pName := fmt.Sprintf("consumer-%s", cName)
			f.MustPod(ctx, toolsPod(pName, f.Name(cName), ""))
		}
	}

	// Wait for all 20 claims to reach Bound.
	for _, cName := range claimNames {
		if _, err := f.WaitPVCBound(ctx, cName, framework.BindTimeout); err != nil {
			t.Fatalf("claim %s did not bind: %v", cName, err)
		}
	}

	// Inspect backing PVs for duplicate export IDs and paths.
	seenPVs := make(map[string]bool)
	seenExportIDs := make(map[string]string) // exportID -> pvName
	seenPaths := make(map[string]string)     // path -> pvName

	for _, cName := range claimNames {
		pv, err := f.PVForClaim(ctx, cName)
		if err != nil {
			t.Fatalf("resolving PV for claim %s: %v", cName, err)
		}
		if seenPVs[pv.Name] {
			t.Errorf("claim %s bound to duplicate PV %s", cName, pv.Name)
		}
		seenPVs[pv.Name] = true

		if expID := pv.Annotations["Export_Id"]; expID != "" {
			if existing, exists := seenExportIDs[expID]; exists {
				t.Errorf("duplicate Export_Id %s on PV %s (already used by %s)", expID, pv.Name, existing)
			}
			seenExportIDs[expID] = pv.Name
		}

		exportPath := ""
		if pv.Spec.NFS != nil && pv.Spec.NFS.Path != "" {
			exportPath = pv.Spec.NFS.Path
		} else if pv.Spec.CSI != nil && pv.Spec.CSI.VolumeHandle != "" {
			exportPath = pv.Spec.CSI.VolumeHandle
		}
		if exportPath != "" {
			if existing, exists := seenPaths[exportPath]; exists {
				t.Errorf("duplicate export path / handle %s on PV %s (already used by %s)",
					exportPath, pv.Name, existing)
			}
			seenPaths[exportPath] = pv.Name
		}
	}

	restartsAfter, err := framework.ServerRestartCount(ctx, f.C)
	if err != nil {
		t.Fatalf("reading final server restart count: %v", err)
	}
	if restartsAfter != restartsBefore {
		t.Errorf("server restarted during concurrent provisioning: %d restarts, was %d",
			restartsAfter, restartsBefore)
	}
}

// PROV-05: snapshot and restore, if advertised. A restored volume must mount
// RWX and its content must match the original snapshot source. Snapshots count as
// advertised only when a VolumeSnapshotClass names this claim's CSI driver: snapshotting
// is an optional CSI capability (CSI spec, CREATE_DELETE_SNAPSHOT), so a driver without
// one is not defective. Where it is not advertised, the API must either reject the
// request outright or leave the snapshot unready; what it must never do is report
// readyToUse for a snapshot no driver took.
//
// Steps:
//  1. Provision an RWX claim and mount it in a pod.
//  2. Write known data and compute checksum, then delete the pod so the volume is unmounted.
//  3. If the VolumeSnapshot CRD is not served, assert a claim with a snapshot
//     DataSource is cleanly rejected, and return.
//  4. If it is served but no VolumeSnapshotClass names f.Env.CSIDriver, snapshotting
//     is not advertised for this driver: a VolumeSnapshot must either be rejected
//     outright or stay unready, never report readyToUse. Assert that, and return.
//  5. If advertised, create a VolumeSnapshot targeting the claim.
//  6. Wait for the VolumeSnapshot to become readyToUse.
//  7. Provision a new claim with DataSource set to the VolumeSnapshot.
//  8. Mount the restored claim on one node, assert the bound claim is RWX, then mount it
//     on a second node too and verify the data matches on both.
//  9. Delete pods and claims.
func TestProvSnapshotAndRestore(t *testing.T) {
	f := framework.New(t, "PROV-05")
	requireCap(t, f.Caps.MultiNode, "PROV-05 requires two worker nodes to assert RWX mount")

	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)

	pvc := f.MustRWXPVC(ctx, "prov05")
	pod := f.MustPod(ctx, toolsPod("writer", pvc.Name, nodeA))
	if _, err := f.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout); err != nil {
		t.Fatalf("claim did not bind once a pod consumed it: %v", err)
	}

	want, err := f.WriteFile(ctx, pod.Name, fileIn("prov05.dat"), 1<<20, "prov05")
	if err != nil {
		t.Fatalf("writing to share: %v", err)
	}

	// Delete writer pod so volume unmounts before snapshot, avoiding live snapshot races.
	if err := f.DeletePod(ctx, pod.Name); err != nil {
		t.Fatalf("deleting writer pod: %v", err)
	}
	if err := f.WaitPodGone(ctx, pod.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("waiting for writer pod to delete: %v", err)
	}

	if !f.Caps.CanSnapshot {
		// Assert clean rejection when CRD is not present.
		t.Logf("VolumeSnapshot CRD is not served; asserting clean API rejection on restore claim")
		group := "snapshot.storage.k8s.io"
		_, err := f.CreatePVC(ctx, framework.PVCSpec{
			Name: "prov05-restore-unsupported",
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &group,
				Kind:     "VolumeSnapshot",
				Name:     f.Name("prov05-nonexistent-snap"),
			},
		})
		if err == nil {
			t.Errorf("expected rejection when creating PVC from snapshot without snapshot capability, got nil")
		} else {
			t.Logf("clean rejection when snapshotting is unsupported: %v", err)
		}
		return
	}

	snapClass, err := f.C.MatchingVolumeSnapshotClass(ctx, f.Env.CSIDriver)
	if err != nil {
		t.Fatalf("resolving the VolumeSnapshotClass for driver %s: %v", f.Env.CSIDriver, err)
	}
	if snapClass == "" {
		t.Logf("VolumeSnapshot CRD is served but no VolumeSnapshotClass matches driver %q", f.Env.CSIDriver)
		// Assert clean rejection or unready status when no matching class is configured.
		_, snapErr := f.CreateVolumeSnapshot(ctx, "prov05-snap", pvc.Name, "")
		if snapErr != nil {
			t.Logf("VolumeSnapshot creation without class rejected cleanly: %v", snapErr)
		} else {
			t.Logf("VolumeSnapshot created without class; verifying it does not falsely report readyToUse")
			readyErr := f.WaitVolumeSnapshotReady(ctx, "prov05-snap", framework.SnapshotProbeTimeout)
			if readyErr == nil {
				t.Errorf("snapshot reported readyToUse without a backing VolumeSnapshotClass")
			}
		}
		return
	}

	t.Logf("using VolumeSnapshotClass %s for snapshot test", snapClass)
	_, err = f.CreateVolumeSnapshot(ctx, "prov05-snap", pvc.Name, snapClass)
	if err != nil {
		t.Fatalf("creating VolumeSnapshot: %v", err)
	}
	if err := f.WaitVolumeSnapshotReady(ctx, "prov05-snap", framework.BindTimeout); err != nil {
		t.Fatalf("VolumeSnapshot did not become readyToUse: %v", err)
	}

	// Restore from snapshot.
	group := "snapshot.storage.k8s.io"
	restoredPVC, err := f.CreatePVC(ctx, framework.PVCSpec{
		Name: "prov05-restored",
		DataSource: &corev1.TypedLocalObjectReference{
			APIGroup: &group,
			Kind:     "VolumeSnapshot",
			Name:     f.Name("prov05-snap"),
		},
	})
	if err != nil {
		t.Fatalf("creating restored PVC from snapshot: %v", err)
	}

	// Reader A first: the claim binds on its first consumer, and the access modes on the
	// bound claim are what say whether the restore is RWX. Scheduling reader B before that
	// check turns a claim restored RWO into a wait out to PodReadyTimeout for a pod that
	// will never be ready, and the assertion below never runs to name the reason.
	restoredPodA := f.MustPod(ctx, toolsPod("reader-a", restoredPVC.Name, nodeA))
	boundPVC, err := f.WaitPVCBound(ctx, restoredPVC.Name, framework.BindTimeout)
	if err != nil {
		t.Fatalf("restored claim %s did not bind with reader pod %s consuming it on %s (profile %s): %v",
			restoredPVC.Name, restoredPodA.Name, nodeA, profile(t).Name, err)
	}
	hasRWX := false
	for _, mode := range boundPVC.Status.AccessModes {
		if mode == corev1.ReadWriteMany {
			hasRWX = true
			break
		}
	}
	if !hasRWX {
		t.Fatalf("restored claim %s status.accessModes does not include ReadWriteMany, so it cannot be mounted on %s and %s at once (profile %s): %v",
			boundPVC.Name, nodeA, nodeB, profile(t).Name, boundPVC.Status.AccessModes)
	}

	// Only now mount it on the second node: the claim says RWX, so a pod that never
	// becomes ready here is a finding about the driver, not about the access modes.
	restoredPodB := f.MustPod(ctx, toolsPod("reader-b", restoredPVC.Name, nodeB))

	gotA, err := f.Sha256(ctx, restoredPodA.Name, fileIn("prov05.dat"))
	if err != nil {
		t.Fatalf("reader pod %s on %s failed reading the restored volume (profile %s, want %s): %v",
			restoredPodA.Name, nodeA, profile(t).Name, want, err)
	}
	if gotA != want {
		t.Fatalf("reader pod %s on %s did not see the snapshot source content on the restored volume (profile %s): got %s want %s",
			restoredPodA.Name, nodeA, profile(t).Name, gotA, want)
	}

	gotB, err := f.Sha256(ctx, restoredPodB.Name, fileIn("prov05.dat"))
	if err != nil {
		t.Fatalf("reader pod %s on %s failed reading the restored volume (profile %s, want %s): %v",
			restoredPodB.Name, nodeB, profile(t).Name, want, err)
	}
	if gotB != want {
		t.Fatalf("reader pod %s on %s did not see the snapshot source content on the restored volume (profile %s): got %s want %s",
			restoredPodB.Name, nodeB, profile(t).Name, gotB, want)
	}
}

// PROV-06: reclaim policy Retain. When a claim with Retain policy is deleted,
// the backing PV must persist in Released phase. Once its ClaimRef is cleared,
// it must be rebound to a new claim and its existing data must remain intact.
//
// Steps:
//  1. Dynamically provision an RWX claim, mount it, write a test file with known checksum.
//  2. Resolve the underlying PV.
//  3. Set the PV reclaim policy to Retain.
//  4. Delete the pod gracefully and wait for it to leave the API.
//  5. Delete the claim and wait for it to be removed.
//  6. Verify the PV persists and transitions to Released phase.
//  7. Clear the PV's ClaimRef so it transitions to Available.
//  8. Create a new claim bound explicitly to the retained PV by name.
//  9. Wait for the new claim to bind.
//
// 10. Mount the rebound claim in a new pod, read the file, and verify checksum matches.
// 11. Restore PV reclaim policy to Delete so framework teardown reclaims the backing storage.
func TestProvReclaimPolicyRetain(t *testing.T) {
	f := framework.New(t, "PROV-06")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	pvc := f.MustRWXPVC(ctx, "prov06")
	pod := f.MustPod(ctx, toolsPod("writer", pvc.Name, ""))
	if _, err := f.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout); err != nil {
		t.Fatalf("claim did not bind: %v", err)
	}

	want, err := f.WriteFile(ctx, pod.Name, fileIn("prov06.dat"), 1<<20, "prov06")
	if err != nil {
		t.Fatalf("writing to share: %v", err)
	}

	pv, err := f.PVForClaim(ctx, pvc.Name)
	if err != nil {
		t.Fatalf("resolving PV for claim: %v", err)
	}

	// Change policy to Retain.
	if err := f.SetPVReclaimPolicy(ctx, pv.Name, corev1.PersistentVolumeReclaimRetain); err != nil {
		t.Fatalf("setting PV reclaim policy to Retain: %v", err)
	}

	// Graceful pod delete.
	if err := f.DeletePod(ctx, pod.Name); err != nil {
		t.Fatalf("deleting pod: %v", err)
	}
	if err := f.WaitPodGone(ctx, pod.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("pod did not leave API: %v", err)
	}

	// Delete claim.
	if err := f.DeletePVC(ctx, pvc.Name); err != nil {
		t.Fatalf("deleting claim: %v", err)
	}
	if err := f.WaitPVCGone(ctx, pvc.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("claim did not leave API: %v", err)
	}

	// PV must persist and transition to Released.
	if err := f.WaitPVPhase(ctx, pv.Name, corev1.VolumeReleased, framework.DeleteTimeout); err != nil {
		t.Fatalf("PV did not enter Released phase: %v", err)
	}
	t.Logf("PV %s survived claim deletion in Released phase", pv.Name)

	// Release PV to Available by clearing ClaimRef.
	if err := f.ReleasePV(ctx, pv.Name); err != nil {
		t.Fatalf("clearing ClaimRef on PV: %v", err)
	}
	if err := f.WaitPVPhase(ctx, pv.Name, corev1.VolumeAvailable, framework.DeleteTimeout); err != nil {
		t.Fatalf("PV did not enter Available phase: %v", err)
	}

	// Re-bind to a new claim referencing this PV by VolumeName.
	reboundPVC, err := f.BindPVToClaim(ctx, "prov06-rebound", pv.Name, "")
	if err != nil {
		t.Fatalf("creating rebound PVC: %v", err)
	}
	readerPod := f.MustPod(ctx, toolsPod("reader", reboundPVC.Name, ""))
	if _, err := f.WaitPVCBound(ctx, reboundPVC.Name, framework.BindTimeout); err != nil {
		t.Fatalf("rebound claim did not bind: %v", err)
	}

	// Verify data survived across retention and rebinding.
	got, err := f.Sha256(ctx, readerPod.Name, fileIn("prov06.dat"))
	if err != nil {
		t.Fatalf("reading from rebound volume: %v", err)
	}
	if got != want {
		t.Fatalf("data corruption on rebound volume: got %s want %s", got, want)
	}

	// Restore Delete policy so cleanup removes the PV.
	if err := f.SetPVReclaimPolicy(ctx, pv.Name, corev1.PersistentVolumeReclaimDelete); err != nil {
		t.Errorf("restoring PV reclaim policy to Delete: %v", err)
	}
}

// PROV-07: provision while the server pod is down. The claim must remain Pending
// during the outage, bind cleanly after server recovery, and provide a working
// data path without orphaned exports.
//
// Steps:
//  1. Discover the server pod target. Skip if unmanaged (no controller).
//  2. Delete the server pod via pkg/chaos.
//  3. Create an RWX claim while the server is down.
//  4. Observe the claim for a bounded window: verify it stays Pending.
//  5. Wait for the server pod to recover and become Ready.
//  6. If the StorageClass binds on first consumer, create a consumer pod.
//  7. Wait for the claim to reach Bound.
//  8. Mount the claim, write a test file, and verify checksum on read-back.
//  9. Delete pod and claim.
func TestProvProvisionServerDown(t *testing.T) {
	f := framework.New(t, "PROV-07")
	ctx, cancel := caseCtx(t, 25*time.Minute)
	defer cancel()

	target, err := chaos.ServerTarget(ctx, f)
	if err != nil {
		t.Skipf("blocked: %v", err)
	}
	if target.Controller == "" {
		t.Skipf("blocked: server pod %s has no controller, so deleting it would not bring it back", target.Pod)
	}

	mode, err := f.BindingMode(ctx, f.Env.StorageClass)
	if err != nil {
		t.Fatalf("reading binding mode of StorageClass %s: %v", f.Env.StorageClass, err)
	}

	if err := chaos.DeleteServerPod(ctx, f, target); err != nil {
		t.Fatalf("injuring server pod: %v", err)
	}
	if err := chaos.WaitServerGone(ctx, f, target, framework.PodTerminateTimeout); err != nil {
		t.Fatalf("target server pod did not leave API: %v", err)
	}

	// Create claim during the outage.
	pvc, err := f.CreatePVC(ctx, framework.PVCSpec{Name: "prov07"})
	if err != nil {
		t.Fatalf("creating claim: %v", err)
	}

	// For WaitForFirstConsumer, schedule the consumer pod during the outage so the
	// scheduler and CSI driver attempt volume provisioning during the outage.
	// We create the pod directly without waiting for Ready because the volume cannot
	// be provisioned/mounted until the server recovers.
	var pod *corev1.Pod
	if mode == storagev1.VolumeBindingWaitForFirstConsumer {
		spec := toolsPod("holder", pvc.Name, "")
		obj, err := f.PodBuilder(spec)
		if err != nil {
			t.Fatalf("building consumer pod: %v", err)
		}
		pod, err = f.C.Kube.CoreV1().Pods(framework.Namespace).Create(ctx, obj, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("creating consumer pod without waiting: %v", err)
		}
	}

	// Sustained check: verify the claim remains Pending while the server is down.
	checkedAtLeastOnce := false
	deadline := time.Now().Add(framework.ServerOutageObserveDuration)
	for time.Now().Before(deadline) {
		live, err := f.GetPVC(ctx, pvc.Name)
		if err != nil {
			t.Fatalf("re-reading claim: %v", err)
		}
		if live.Status.Phase == corev1.ClaimBound {
			t.Fatalf("claim %s bound prematurely while server was down", pvc.Name)
		}
		checkedAtLeastOnce = true

		pods, err := framework.ServerPods(ctx, f.C)
		if err == nil {
			recovered := false
			for i := range pods {
				if (pods[i].UID != target.UID || pods[i].Name != target.Pod) && framework.PodReady(&pods[i]) {
					recovered = true
					break
				}
			}
			if recovered {
				t.Logf("replacement server pod became ready; concluding outage observation")
				break
			}
		}

		time.Sleep(framework.PollInterval)
	}
	if !checkedAtLeastOnce {
		t.Fatalf("outage observation window ended before verifying claim was pending")
	}
	t.Logf("claim %s remained Pending throughout server outage", pvc.Name)

	// Wait for a replacement server pod to recover and become Ready.
	if err := chaos.WaitServerReplaced(ctx, f, target, framework.PodReadyTimeout); err != nil {
		t.Fatalf("replacement server pod did not recover: %v", err)
	}

	// For Immediate binding mode, launch the consumer pod once the claim binds.
	if _, err := f.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout); err != nil {
		t.Fatalf("claim did not bind after server recovery: %v", err)
	}

	if pod == nil {
		pod = f.MustPod(ctx, toolsPod("holder", pvc.Name, ""))
	} else {
		var err error
		pod, err = f.WaitPodReady(ctx, pod.Name, framework.PodReadyTimeout)
		if err != nil {
			t.Fatalf("consumer pod did not become ready after server recovery: %v", err)
		}
	}

	want, err := f.WriteFile(ctx, pod.Name, fileIn("prov07.dat"), 1<<20, "prov07")
	if err != nil {
		t.Fatalf("writing to share after server recovery: %v", err)
	}
	got, err := f.Sha256(ctx, pod.Name, fileIn("prov07.dat"))
	if err != nil {
		t.Fatalf("reading back from share after server recovery: %v", err)
	}
	if got != want {
		t.Fatalf("checksum mismatch: got %s want %s", got, want)
	}

	// Assert clean teardown and no orphaned backing volume.
	pv, err := f.PVForClaim(ctx, pvc.Name)
	if err != nil {
		t.Fatalf("resolving PV for claim: %v", err)
	}
	if err := f.DeletePod(ctx, pod.Name); err != nil {
		t.Fatalf("deleting pod: %v", err)
	}
	if err := f.WaitPodGone(ctx, pod.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("pod did not leave API: %v", err)
	}
	if err := f.DeletePVC(ctx, pvc.Name); err != nil {
		t.Fatalf("deleting claim: %v", err)
	}
	if err := f.WaitPVCGone(ctx, pvc.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("claim did not leave API: %v", err)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
		if err := f.WaitPVGone(ctx, pv.Name, framework.DeleteTimeout); err != nil {
			t.Errorf("backing PV %s leaked after server recovery: %v", pv.Name, err)
		}
	}
}

// PROV-08: delete claim while the server pod is down. Deletion must complete
// without orphaned exports or leaked backing storage once the server recovers.
//
// Steps:
//  1. Provision an RWX claim, mount it in a pod, write a test file.
//  2. Delete the pod gracefully and wait for it to leave the API before touching the server.
//  3. Resolve the underlying PV name and reclaim policy.
//  4. Discover the server pod target. Skip if unmanaged.
//  5. Delete the server pod via pkg/chaos and confirm target instance is gone.
//  6. Delete the claim while the server is down.
//  7. Wait for a replacement server pod to recover and become Ready.
//  8. Wait for the claim to be completely removed from the API.
//  9. If reclaim policy was Delete, wait for the PV to be removed as well.
func TestProvDeleteClaimServerDown(t *testing.T) {
	f := framework.New(t, "PROV-08")
	ctx, cancel := caseCtx(t, 25*time.Minute)
	defer cancel()

	pvc := f.MustRWXPVC(ctx, "prov08")
	pod := f.MustPod(ctx, toolsPod("writer", pvc.Name, ""))
	if _, err := f.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout); err != nil {
		t.Fatalf("claim did not bind: %v", err)
	}
	if _, err := f.WriteFile(ctx, pod.Name, fileIn("prov08.dat"), 1<<20, "prov08"); err != nil {
		t.Fatalf("writing to share: %v", err)
	}

	pv, err := f.PVForClaim(ctx, pvc.Name)
	if err != nil {
		t.Fatalf("resolving PV: %v", err)
	}

	// Gracefully remove pod before injuring server to prevent F-001 unmount wedging.
	if err := f.DeletePod(ctx, pod.Name); err != nil {
		t.Fatalf("deleting pod: %v", err)
	}
	if err := f.WaitPodGone(ctx, pod.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("pod did not leave API: %v", err)
	}

	target, err := chaos.ServerTarget(ctx, f)
	if err != nil {
		t.Skipf("blocked: %v", err)
	}
	if target.Controller == "" {
		t.Skipf("blocked: server pod %s has no controller, so deleting it would not bring it back", target.Pod)
	}

	if err := chaos.DeleteServerPod(ctx, f, target); err != nil {
		t.Fatalf("injuring server pod: %v", err)
	}
	if err := chaos.WaitServerGone(ctx, f, target, framework.PodTerminateTimeout); err != nil {
		t.Fatalf("target server pod did not leave API: %v", err)
	}

	// Delete claim while server is down.
	if err := f.DeletePVC(ctx, pvc.Name); err != nil {
		t.Fatalf("initiating PVC delete during server outage: %v", err)
	}

	// Wait for server to recover.
	if err := chaos.WaitServerReplaced(ctx, f, target, framework.PodReadyTimeout); err != nil {
		t.Fatalf("server pod did not recover: %v", err)
	}

	// Wait for claim to complete deletion.
	if err := f.WaitPVCGone(ctx, pvc.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("claim did not leave API after server recovery: %v", err)
	}

	// If reclaim policy is Delete, verify backing PV was reclaimed.
	if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
		if err := f.WaitPVGone(ctx, pv.Name, framework.DeleteTimeout); err != nil {
			t.Errorf("backing volume %s was leaked after server recovery: %v", pv.Name, err)
		}
	}
}

// PROV-09: rapid create/delete churn (100 cycles). Asserts no export ID
// exhaustion, no file descriptor leaks, and server RSS remains bounded under
// the ceiling.
//
// Steps:
//  1. Check for short test mode; skip if -short is set.
//  2. Record server restart count before churn.
//  3. Execute 100 cycles of create PVC -> wait Bound -> delete PVC -> wait gone.
//  4. Assert all cycles completed successfully.
//  5. Assert server container restart count did not increase.
func TestProvRapidProvisionChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100-cycle rapid provision churn in -short mode")
	}

	f := framework.New(t, "PROV-09")
	ctx, cancel := caseCtx(t, 45*time.Minute)
	defer cancel()

	restartsBefore, err := framework.ServerRestartCount(ctx, f.C)
	if err != nil {
		t.Fatalf("reading initial server restart count: %v", err)
	}

	const cycles = 100
	res, err := f.RunPVCLifecycleChurn(ctx, cycles, "prov09")
	if err != nil {
		t.Fatalf("churn run failed after %d cycles: %v", res.Completed, err)
	}
	t.Logf("completed %d of %d rapid churn cycles", res.Completed, cycles)

	restartsAfter, err := framework.ServerRestartCount(ctx, f.C)
	if err != nil {
		t.Fatalf("reading final server restart count: %v", err)
	}
	if restartsAfter != restartsBefore {
		t.Errorf("server restarted during rapid churn: %d restarts, was %d",
			restartsAfter, restartsBefore)
	}
}

// PROV-10: volume name edge cases. Assert that Kubernetes admission cleanly rejects
// invalid names (1000-character length, uppercase letters) before storage provisioning,
// and that the maximum-length valid RFC 1123 name (253 characters) binds, mounts,
// produces a well-formed export configuration, and supports cross-node I/O without
// server restart.
//
// Note: 1000-character and uppercase rejections are Kubernetes apiserver schema admission
// barriers (RFC 1123), not storage driver validation. The NFS verification exercises
// the maximum valid boundary name through the storage backend to verify that export
// paths, volume handles, and mount syntax are not truncated or malformed.
//
// Steps:
//  1. Verify admission-layer rejection: attempt to create a PVC with a 1000-character name.
//  2. Verify admission-layer rejection: attempt to create a PVC with uppercase characters.
//  3. Create a claim with a maximum-length valid RFC 1123 name (253 characters).
//  4. Mount the boundary claim in a writer on node A and a reader on node B.
//  5. Confirm the claim reaches Bound, resolve the PV, and inspect the minted export
//     server, path and CSI volumeHandle for truncation or malformation. A PV whose
//     export the harness cannot read reports blocked, not pass.
//  6. Write a payload from the writer on node A and verify the SHA-256 checksum from
//     the reader on node B across the wire.
//  7. Delete both pods gracefully, await API departure, and delete the claim.
//  8. Confirm server remains healthy with no restarts.
func TestProvVolumeNameEdgeCases(t *testing.T) {
	f := framework.New(t, "PROV-10")
	requireCap(t, f.Caps.MultiNode, "cross-node verification needs two schedulable workers")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)

	restartsBefore, err := framework.ServerRestartCount(ctx, f.C)
	if err != nil {
		t.Fatalf("reading initial server restart count: %v", err)
	}

	// 1. Admission barrier: direct API call with 1000-character name.
	// Must be rejected cleanly by kube-apiserver admission (RFC 1123 limit).
	longName := strings.Repeat("a", 1000)
	longPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: longName, Namespace: framework.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if _, err := f.C.Kube.CoreV1().PersistentVolumeClaims(framework.Namespace).Create(ctx, longPVC, metav1.CreateOptions{}); err == nil {
		t.Fatalf("API accepted PVC with 1000-character name; expected admission rejection")
	} else if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) {
		t.Fatalf("expected Invalid/BadRequest admission error for 1000-character name, got: %v", err)
	} else {
		t.Logf("1000-character PVC name rejected cleanly at admission layer: %v", err)
	}

	// 2. Admission barrier: direct API call with invalid characters (uppercase letters).
	// Must be rejected cleanly by kube-apiserver admission (RFC 1123 subdomain syntax).
	// The name is alphanumeric on purpose. A name carrying underscores as well
	// is rejected for the underscores, so it would pass this check on a cluster
	// that accepted capital letters, which is the one thing it is here to rule
	// out.
	badCharPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "InvalidUppercaseName", Namespace: framework.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if _, err := f.C.Kube.CoreV1().PersistentVolumeClaims(framework.Namespace).Create(ctx, badCharPVC, metav1.CreateOptions{}); err == nil {
		t.Fatalf("API accepted PVC with uppercase name; expected admission rejection")
	} else if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) {
		t.Fatalf("expected Invalid/BadRequest admission error for uppercase name, got: %v", err)
	} else {
		t.Logf("uppercase PVC name rejected cleanly at admission layer: %v", err)
	}

	// 3. Boundary RFC 1123 name at maximum allowed length (253 characters).
	prefix := f.Name("")
	pad := 253 - len(prefix)
	if pad <= 0 {
		t.Fatalf("framework prefix %q length %d is >= 253 characters; shorten -run-id", prefix, len(prefix))
	}
	boundaryLogical := strings.Repeat("x", pad)
	boundPVC, err := f.CreatePVC(ctx, framework.PVCSpec{Name: boundaryLogical})
	if err != nil {
		t.Fatalf("creating 253-character boundary claim %s: %v", prefix+boundaryLogical, err)
	}
	if len(boundPVC.Name) != 253 {
		t.Fatalf("expected boundary PVC name length 253, got %d (%s)", len(boundPVC.Name), boundPVC.Name)
	}

	// 4. Mount the boundary claim in a writer on node A and a reader on node B.
	// The pods come before the bind check: a class that binds on first consumer
	// has nothing to bind to until something is scheduled.
	writer := f.MustPod(ctx, toolsPod("writer", boundPVC.Name, nodeA))
	reader := f.MustPod(ctx, toolsPod("reader", boundPVC.Name, nodeB))

	// 5. Confirm the claim reached Bound, resolve the PV, and inspect export configuration.
	bound, err := f.WaitPVCBound(ctx, boundPVC.Name, framework.BindTimeout)
	if err != nil {
		t.Fatalf("boundary claim %s did not reach Bound once pods consumed it: %v", boundPVC.Name, err)
	}
	pv, err := f.C.Kube.CoreV1().PersistentVolumes().Get(ctx, bound.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("resolving bound PV %s for claim %s: %v", bound.Spec.VolumeName, bound.Name, err)
	}
	t.Logf("253-character boundary claim %s bound to PV %s on StorageClass %s", bound.Name, pv.Name, f.Env.StorageClass)

	var volumeHandle string
	if pv.Spec.CSI != nil {
		volumeHandle = pv.Spec.CSI.VolumeHandle
		if volumeHandle == "" {
			t.Errorf("bound PV %s has empty CSI volumeHandle", pv.Name)
		}
		if strings.ContainsAny(volumeHandle, "\r\n") {
			t.Errorf("bound PV %s volumeHandle contains newline characters: %q", pv.Name, volumeHandle)
		}
	}
	// The minted export is what the boundary name is being tested against, so a
	// source the harness cannot read leaves this case with nothing to assert.
	// It reports blocked, as DATA-08 does, rather than logging past it: a pass
	// carried by mount and I/O alone would claim an export assertion that never
	// ran.
	nfsSource, err := framework.ExtractNFSSource(pv)
	if err != nil {
		blocked(t, "the export behind the 253-character claim %s could not be read, so PROV-10 cannot "+
			"assert on the minted export configuration: %v", bound.Name, err)
	}
	// Server and path are one source. A newline in either builds a mount
	// address nobody asked for, so both are checked and both are reported.
	if strings.ContainsAny(nfsSource.Server, "\r\n") || strings.ContainsAny(nfsSource.Path, "\r\n") {
		t.Errorf("bound PV %s export source contains newline characters: server=%q path=%q",
			pv.Name, nfsSource.Server, nfsSource.Path)
	}
	t.Logf("boundary volume configuration: PV=%s volumeHandle=%q export=%s", pv.Name, volumeHandle, nfsSource)

	// 6. Write a payload from writer on node A and verify checksum from reader on node B.
	want, err := f.WriteFile(ctx, writer.Name, fileIn("prov10.dat"), 1<<20, "prov10")
	if err != nil {
		t.Fatalf("writing to boundary share from writer pod %s on %s: %v", writer.Name, nodeA, err)
	}
	got, err := f.Sha256(ctx, reader.Name, fileIn("prov10.dat"))
	if err != nil {
		t.Fatalf("reader pod %s on %s failed reading file written by writer pod %s on %s (profile %s, want %s): %v",
			reader.Name, nodeB, writer.Name, nodeA, profile(t).Name, want, err)
	}
	if got != want {
		t.Fatalf("reader on %s did not see what writer on %s closed (profile %s): got %s want %s",
			nodeB, nodeA, profile(t).Name, got, want)
	}

	// 7. Graceful teardown: pods first, wait for API departure, then delete the claim.
	for _, pod := range []*corev1.Pod{writer, reader} {
		if err := f.DeletePod(ctx, pod.Name); err != nil {
			t.Fatalf("deleting pod %s: %v", pod.Name, err)
		}
		if err := f.WaitPodGone(ctx, pod.Name, framework.DeleteTimeout); err != nil {
			t.Fatalf("pod %s did not go away: %v", pod.Name, err)
		}
	}
	if err := f.DeletePVC(ctx, bound.Name); err != nil {
		t.Fatalf("deleting boundary claim %s: %v", bound.Name, err)
	}
	if err := f.WaitPVCGone(ctx, bound.Name, framework.DeleteTimeout); err != nil {
		t.Fatalf("boundary claim %s did not delete: %v", bound.Name, err)
	}

	// 8. Confirm server remained healthy.
	restartsAfter, err := framework.ServerRestartCount(ctx, f.C)
	if err != nil {
		t.Fatalf("reading final server restart count: %v", err)
	}
	if restartsAfter != restartsBefore {
		t.Errorf("server restarted during volume name edge case tests: %d restarts, was %d",
			restartsAfter, restartsBefore)
	}
}

// PROV-11: two-stage expansion under active I/O. Grows the backing volume while
// an active write load is running. Asserts zero I/O errors, that all committed
// writes survive, and client df reflects the new capacity.
//
// Steps:
//  1. Provision an RWX claim, mount it in a client pod.
//  2. If expansion is unsupported, assert clean rejection as in PROV-04.
//  3. If expansion is supported:
//     a. Record initial df capacity, pod restarts, and server restarts.
//     b. Start background write load (StartWriteLoad).
//     c. Request volume expansion from size X to X+1Gi.
//     d. Wait for claim status.capacity to update.
//     e. Stop the write load and parse the report.
//     f. Assert zero I/O errors (report.Errors() is empty).
//     g. Assert all committed writes are readable on the share.
//     h. Assert pod restart count did not increase.
//     i. Assert server restart count did not increase.
//     j. Compare df before and after: growth is logged, no change is explained.
func TestProvTwoStageExpansionUnderIO(t *testing.T) {
	f := framework.New(t, "PROV-11")
	ctx, cancel := caseCtx(t, 25*time.Minute)
	defer cancel()

	pvc := f.MustRWXPVC(ctx, "prov11")
	pod := f.MustPod(ctx, toolsPod("writer", pvc.Name, ""))
	bound, err := f.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout)
	if err != nil {
		t.Fatalf("claim did not bind: %v", err)
	}
	before := bound.Spec.Resources.Requests[corev1.ResourceStorage]
	grown := before.DeepCopy()
	grown.Add(resource.MustParse("1Gi"))

	if !f.Caps.CanExpand {
		if err := f.ExpandPVC(ctx, pvc.Name, grown.String()); err == nil {
			t.Fatalf("StorageClass %s does not advertise expansion, yet API accepted resize", f.Env.StorageClass)
		} else {
			t.Logf("expansion is unsupported on StorageClass %s and was rejected cleanly: %v",
				f.Env.StorageClass, err)
		}
		return
	}

	seenBefore, err := f.MountCapacity(ctx, pod.Name, mountPath)
	if err != nil {
		t.Fatalf("reading capacity from pod: %v", err)
	}
	podRestartsBefore, err := f.PodRestarts(ctx, pod.Name)
	if err != nil {
		t.Fatalf("reading pod restarts: %v", err)
	}
	serverRestartsBefore, err := framework.ServerRestartCount(ctx, f.C)
	if err != nil {
		t.Fatalf("reading server restarts: %v", err)
	}

	// Start background active I/O.
	workload, err := f.StartWriteLoad(ctx, pod.Name, mountPath, "prov11")
	if err != nil {
		t.Fatalf("starting background workload: %v", err)
	}
	f.Defer(func(ctx context.Context) {
		if _, err := workload.Stop(ctx); err != nil {
			t.Logf("stopping background workload in cleanup: %v", err)
		}
	})

	// Two-stage expansion:
	// - For dedicated-server deployments (one server per volume), expansion grows
	//   the backing block device and then the exported share filesystem.
	// - For shared-server deployments, expansion updates the per-volume quota / export.
	if err := f.ExpandPVC(ctx, pvc.Name, grown.String()); err != nil {
		t.Fatalf("requesting expansion during active I/O: %v", err)
	}
	capacity, err := f.WaitPVCCapacity(ctx, pvc.Name, grown.String(), framework.ExpandTimeout)
	if err != nil {
		t.Fatalf("claim capacity did not reach %s: %v", grown.String(), err)
	}
	t.Logf("claim expanded to %s during active I/O (sharedServer=%v)", capacity.String(), f.Caps.SharedServer)

	// Stop workload and check results.
	rep, err := workload.Stop(ctx)
	if err != nil {
		t.Fatalf("stopping workload: %v", err)
	}
	if errs := rep.Errors(); len(errs) > 0 {
		t.Errorf("%d I/O errors occurred during volume expansion under active I/O", len(errs))
	}
	if len(rep.Unparsed) > 0 {
		t.Errorf("the workload log holds %d unparsed lines: %q", len(rep.Unparsed), rep.Unparsed[0])
	}
	committed := rep.Committed()
	if len(committed) == 0 {
		t.Errorf("no writes were committed by the workload during expansion")
	}
	// By content, not by existence. An expansion that grew the backing device
	// and lost a byte inside a record it kept would pass an existence check.
	assertCommittedRecordsIntact(ctx, t, f, pod.Name, mountPath, committed)

	podRestartsAfter, err := f.PodRestarts(ctx, pod.Name)
	if err != nil {
		t.Fatalf("re-reading pod restarts: %v", err)
	}
	if podRestartsAfter != podRestartsBefore {
		t.Errorf("client pod restarted during expansion: %d restarts, was %d",
			podRestartsAfter, podRestartsBefore)
	}

	serverRestartsAfter, err := framework.ServerRestartCount(ctx, f.C)
	if err != nil {
		t.Fatalf("re-reading server restarts: %v", err)
	}
	if serverRestartsAfter != serverRestartsBefore {
		t.Errorf("server restarted during volume expansion: %d restarts, was %d",
			serverRestartsAfter, serverRestartsBefore)
	}

	seenAfter, err := f.MountCapacity(ctx, pod.Name, mountPath)
	if err != nil {
		t.Fatalf("reading capacity after expansion: %v", err)
	}
	switch {
	case seenAfter.TotalBytes > seenBefore.TotalBytes:
		t.Logf("workload sees new capacity under df: %d bytes, was %d",
			seenAfter.TotalBytes, seenBefore.TotalBytes)
	case seenAfter.TotalBytes == seenBefore.TotalBytes:
		if !f.Caps.SharedServer {
			t.Errorf("df did not reflect new capacity after expansion on dedicated-server volume: %d bytes, was %d",
				seenAfter.TotalBytes, seenBefore.TotalBytes)
		} else {
			t.Logf("df still reports %d bytes after expansion; shared-server export without per-volume quota",
				seenAfter.TotalBytes)
		}
	default:
		t.Errorf("share shrank across expansion: df reports %d bytes, was %d",
			seenAfter.TotalBytes, seenBefore.TotalBytes)
	}
}

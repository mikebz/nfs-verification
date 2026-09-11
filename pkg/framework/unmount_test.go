package framework

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// testFixture builds a fixture over a fake clientset. Fields are not assembled
// by hand: New, NewSystem and SubTest are what set the shared state behind a
// Framework, and a literal would nil-panic several calls deep.
func testFixture(kube kubernetes.Interface) *Framework {
	f, _ := NewSystem(&Client{Kube: kube}, nil, Capabilities{}, "unit")
	return f
}

// csiDriver builds a CSIDriver object declaring whether it attaches.
func csiDriver(name string, attach bool) *storagev1.CSIDriver {
	return &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       storagev1.CSIDriverSpec{AttachRequired: &attach},
	}
}

// Force-deleting a mounted pod is the first half of F-001: the pod leaves the
// API before kubelet unmounts, and anything that then deletes the claim
// destroys an export a node is still mounting. Everything here is about the
// observation that goes between those two steps, and each of these failures
// costs a node rather than a test.

// TestCSIUniqueVolumeName covers the name kubelet actually records, which is
// not the CSI volume handle.
//
// A wait comparing a bare handle against node.status.volumesInUse matches
// nothing, reports the volume gone on its first poll, and is therefore F-001
// with extra steps: it would look like a wait and do nothing.
//
// Steps:
//  1. Build the name from a driver and a handle.
//  2. Assert the shape kubelet uses, driver and handle joined by "^" under the
//     csi plugin prefix.
//  3. Assert the raw handle is not that name.
//  4. Assert a name that cannot be built is an error, never a usable string.
func TestCSIUniqueVolumeName(t *testing.T) {
	const (
		driver = "nfs.csi.k8s.io"
		handle = "10.0.0.5#export#pvc-123##"
	)
	got, err := CSIUniqueVolumeName(driver, handle)
	if err != nil {
		t.Fatalf("building the unique volume name: %v", err)
	}
	if want := "kubernetes.io/csi/" + driver + "^" + handle; got != want {
		t.Errorf("built %q, want %q", got, want)
	}
	if got == handle {
		t.Errorf("the unique name equals the raw handle, so a wait comparing handles would match "+
			"an entry it should not: %q", got)
	}
	if !strings.Contains(got, "^") {
		t.Errorf("the unique name %q does not join driver and handle with the separator kubelet uses", got)
	}
	for _, bad := range [][2]string{{"", handle}, {driver, ""}, {"", ""}} {
		if name, err := CSIUniqueVolumeName(bad[0], bad[1]); err == nil {
			t.Errorf("driver %q and handle %q produced %q; a name that cannot be built must be an "+
				"error, because the alternative is treating an unmatchable name as proof of an unmount",
				bad[0], bad[1], name)
		}
	}
}

// TestAwaitVolumeNotInUseFailsWhileStillMounted covers the direction that
// matters: a volume still in use at the deadline is an error, never a done.
//
// Steps:
//  1. Offer a node that never stops reporting the volume in use.
//  2. Wait with a short deadline.
//  3. Assert the wait failed, and that the message names the node, the volume
//     and the finding that explains why the claim is being kept.
func TestAwaitVolumeNotInUseFailsWhileStillMounted(t *testing.T) {
	const (
		node   = "worker-1"
		unique = "kubernetes.io/csi/nfs.csi.k8s.io^pvc-123"
	)
	kube := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Status:     corev1.NodeStatus{VolumesInUse: []corev1.UniqueVolumeName{unique}},
	})
	f := testFixture(kube)

	err := f.awaitVolumeNotInUse(context.Background(), node, unique, 2*time.Second)
	if err == nil {
		t.Fatal("the wait reported the volume released while the node still listed it in volumesInUse")
	}
	for _, want := range []string{node, unique, "F-001"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}
}

// TestAwaitVolumeNotInUseSucceedsOnceReleased is the other direction, so that
// the check above is not passing because the wait never succeeds at all.
//
// Steps:
//  1. Offer a node whose volumesInUse is empty, which is what a finished
//     unmount looks like on a driver that attaches.
//  2. Assert the wait returns without error, so that the check above is failing
//     on the state it names rather than because this wait never succeeds at all.
func TestAwaitVolumeNotInUseSucceedsOnceReleased(t *testing.T) {
	const node = "worker-1"
	kube := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}})
	f := testFixture(kube)

	if err := f.awaitVolumeNotInUse(context.Background(), node,
		"kubernetes.io/csi/nfs.csi.k8s.io^pvc-123", 5*time.Second); err != nil {
		t.Fatalf("the wait never finished against a node holding nothing: %v", err)
	}
}

// TestAwaitUnmountIgnoresVolumesInUseWithoutAnAttachingDriver covers the branch
// choice. node.status.volumesInUse is a list of *attachable* volumes in use, and
// NFS CSI drivers generally set attachRequired false, so on such a driver an
// empty list is indistinguishable from a finished unmount.
//
// Steps:
//  1. Offer a PV on a driver whose CSIDriver object sets attachRequired false.
//  2. Ask whether a unique volume name applies to it.
//  3. Assert it does not, so the wait falls back to reading the node.
//  4. Do the same for a driver that does attach, and for an in-tree spec.nfs
//     volume with no CSIDriver object at all.
func TestAwaitUnmountIgnoresVolumesInUseWithoutAnAttachingDriver(t *testing.T) {
	csiPV := func(driver, handle string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-123"},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: handle}}},
		}
	}
	inTreePV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-123"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			NFS: &corev1.NFSVolumeSource{Server: "10.0.0.5", Path: "/export/pvc-123"}}},
	}
	f := testFixture(fake.NewSimpleClientset(
		csiDriver("nfs.csi.k8s.io", false),
		csiDriver("blocks.csi.example.com", true),
	))
	ctx := context.Background()

	if _, ok := f.uniqueVolumeName(ctx, csiPV("nfs.csi.k8s.io", "h1")); ok {
		t.Error("volumesInUse was chosen for a driver that does not attach, where an empty list " +
			"means nothing and would read as a finished unmount")
	}
	if _, ok := f.uniqueVolumeName(ctx, csiPV("absent.csi.example.com", "h1")); ok {
		t.Error("volumesInUse was chosen for a driver with no CSIDriver object")
	}
	if _, ok := f.uniqueVolumeName(ctx, csiPV("blocks.csi.example.com", "")); ok {
		t.Error("volumesInUse was chosen for a volume whose unique name cannot be built; a name the " +
			"harness cannot construct must mean 'not proven gone', never 'gone'")
	}
	if _, ok := f.uniqueVolumeName(ctx, inTreePV); ok {
		t.Error("volumesInUse was chosen for an in-tree spec.nfs volume, which is not a CSI volume at all")
	}
	name, ok := f.uniqueVolumeName(ctx, csiPV("blocks.csi.example.com", "h1"))
	if !ok {
		t.Fatal("volumesInUse was not chosen for a driver that does attach, so the wait pays for an " +
			"exec into a privileged pod where a Node GET would do")
	}
	if name != "kubernetes.io/csi/blocks.csi.example.com^h1" {
		t.Errorf("chose the name %q", name)
	}
}

// TestDeleteCaseObjectsKeepsAnUnprovenClaim is the teardown half of the rule.
// Registering the unmount wait is not enough: Defer callbacks return nothing,
// and a force-deleted pod is not in the API, so teardown has nothing to notice.
//
// Steps:
//  1. Mark one of two claims unproven, as a force delete does.
//  2. Run teardown with no pods left, which is what a force delete produces.
//  3. Assert the unproven claim is still there and the other one is gone.
//  4. Assert the error names the claim and why it was kept.
func TestDeleteCaseObjectsKeepsAnUnprovenClaim(t *testing.T) {
	claim := func(name string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: Namespace,
			Labels: map[string]string{"nfs-verification/run": "unit", "nfs-verification/case": "data-06"},
		}}
	}
	kube := fake.NewSimpleClientset(claim("kept"), claim("removable"))
	f := testFixture(kube)
	f.CaseID = "DATA-06"
	// The selector is built from the run id, so it has to match the labels
	// above. Restored, because the configuration is process-wide.
	previous := Cfg().RunID
	Cfg().RunID = "unit"
	t.Cleanup(func() { Cfg().RunID = previous })
	f.MarkClaimUnproven("kept", "pod holder was force-deleted while mounting it on worker-1")

	err := f.DeleteCaseObjects(context.Background())
	if err == nil {
		t.Fatal("teardown reported success while a claim's unmount was never observed; deleting it " +
			"destroys the export under a live mount, which is F-001")
	}
	for _, want := range []string{"kept", "force-deleted", "worker-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}
	claims := kube.CoreV1().PersistentVolumeClaims(Namespace)
	if _, err := claims.Get(context.Background(), "kept", metav1.GetOptions{}); err != nil {
		t.Errorf("the claim with an unproven unmount was deleted anyway: %v", err)
	}
	if _, err := claims.Get(context.Background(), "removable", metav1.GetOptions{}); err == nil {
		t.Error("a claim nothing was holding was kept; the run should leak as little as it can")
	}

	// An observed unmount clears the mark, and teardown stops objecting. Only
	// the error is checked here: past the guard, teardown deletes claims with
	// DeleteCollection, which the fake clientset accepts and does not perform,
	// so asserting the object is gone would be asserting against the double.
	f.ClearClaimUnproven("kept")
	if err := f.DeleteCaseObjects(context.Background()); err != nil {
		t.Errorf("teardown still refused after the unmount was observed: %v", err)
	}
}

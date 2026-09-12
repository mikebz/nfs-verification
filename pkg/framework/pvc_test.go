package framework

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// TestCheckObjectNameEdgeCases covers boundary conditions on Kubernetes object names:
// empty names, 1000-character names, boundary RFC 1123 lengths, and forbidden characters.
//
// Steps:
//  1. Check empty name rejection.
//  2. Check 1000-character name rejection (>253 limit).
//  3. Check 253-character valid name acceptance.
//  4. Check invalid character rejection (uppercase, underscore, special characters).
func TestCheckObjectNameEdgeCases(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "empty", input: "", wantErr: true},
		{name: "over-limit-1000", input: strings.Repeat("a", 1000), wantErr: true},
		{name: "over-limit-254", input: strings.Repeat("a", 254), wantErr: true},
		{name: "exact-253-limit", input: strings.Repeat("a", 253), wantErr: false},
		{name: "valid-standard", input: "prov10-volume-test", wantErr: false},
		{name: "invalid-uppercase", input: "PROV10-UPPERCASE", wantErr: true},
		{name: "invalid-underscore", input: "prov10_invalid_underscore", wantErr: true},
		{name: "invalid-symbol", input: "prov10$invalid", wantErr: true},
		{name: "starts-with-dash", input: "-invalid-dash", wantErr: true},
		{name: "ends-with-dash", input: "invalid-dash-", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckObjectName("claim", tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("CheckObjectName(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

// TestPVCSpecWithDataSource covers building claim specifications that restore
// from VolumeSnapshot references.
//
// Steps:
//  1. Create a PVCSpec with a VolumeSnapshot DataSource.
//  2. Verify the DataSource fields retain the apiGroup, kind, and name.
func TestPVCSpecWithDataSource(t *testing.T) {
	group := "snapshot.storage.k8s.io"
	spec := PVCSpec{
		Name: "restore-claim",
		Size: "5Gi",
		DataSource: &corev1.TypedLocalObjectReference{
			APIGroup: &group,
			Kind:     "VolumeSnapshot",
			Name:     "source-snap",
		},
	}

	if spec.DataSource == nil {
		t.Fatal("DataSource is nil")
	}
	if *spec.DataSource.APIGroup != group {
		t.Errorf("APIGroup = %s, want %s", *spec.DataSource.APIGroup, group)
	}
	if spec.DataSource.Kind != "VolumeSnapshot" {
		t.Errorf("Kind = %s, want VolumeSnapshot", spec.DataSource.Kind)
	}
	if spec.DataSource.Name != "source-snap" {
		t.Errorf("Name = %s, want source-snap", spec.DataSource.Name)
	}
}

// TestVolumeSnapshotUnstructuredStructure asserts that the unstructured representation
// produced for VolumeSnapshot conforms to snapshot.storage.k8s.io/v1 schema.
//
// Steps:
//  1. Construct unstructured VolumeSnapshot.
//  2. Verify GroupVersionKind, name, namespace, and PVC source reference.
func TestVolumeSnapshotUnstructuredStructure(t *testing.T) {
	snapName := "test-snapshot"
	pvcName := "test-pvc"
	className := "nfs-snapclass"

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshot",
			"metadata": map[string]any{
				"name":      snapName,
				"namespace": Namespace,
			},
			"spec": map[string]any{
				"volumeSnapshotClassName": className,
				"source": map[string]any{
					"persistentVolumeClaimName": pvcName,
				},
			},
		},
	}

	if obj.GetAPIVersion() != "snapshot.storage.k8s.io/v1" {
		t.Errorf("apiVersion = %s, want snapshot.storage.k8s.io/v1", obj.GetAPIVersion())
	}
	if obj.GetKind() != "VolumeSnapshot" {
		t.Errorf("kind = %s, want VolumeSnapshot", obj.GetKind())
	}
	spec, ok := obj.Object["spec"].(map[string]any)
	if !ok {
		t.Fatal("missing spec in unstructured object")
	}
	if spec["volumeSnapshotClassName"] != className {
		t.Errorf("volumeSnapshotClassName = %v, want %s", spec["volumeSnapshotClassName"], className)
	}
	src, ok := spec["source"].(map[string]any)
	if !ok || src["persistentVolumeClaimName"] != pvcName {
		t.Errorf("source PVC = %v, want %s", src["persistentVolumeClaimName"], pvcName)
	}
}

// TestRunPVCLifecycleChurnContextCancel asserts that the churn runner terminates
// cleanly when the caller's context is canceled.
//
// Steps:
//  1. Create a canceled context.
//  2. Call RunPVCLifecycleChurn.
//  3. Assert it returns immediately with context.Canceled.
func TestRunPVCLifecycleChurnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := &Framework{CaseID: "PROV-09"}
	res, err := f.RunPVCLifecycleChurn(ctx, 10, "churn-test")
	if err == nil {
		t.Error("expected error on canceled context, got nil")
	}
	if res.Completed != 0 {
		t.Errorf("completed cycles = %d, want 0", res.Completed)
	}
}

// TestMatchingVolumeSnapshotClass asserts that MatchingVolumeSnapshotClass filters
// classes by the driver attribute and ignores classes belonging to other drivers.
//
// Getting this wrong is silent: an unrelated class read as a match sends PROV-05 into a
// snapshot the driver will never take, and a malformed class read as absent reports a
// cluster that does advertise snapshots as one that does not.
//
// Steps:
//  1. Create fake dynamic client with VolumeSnapshotClasses for driver A and driver B.
//  2. Query for driver A; assert driver A class is returned.
//  3. Query for driver C (non-existent); assert empty string is returned.
func TestMatchingVolumeSnapshotClass(t *testing.T) {
	scheme := runtime.NewScheme()
	classA := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshotClass",
			"metadata": map[string]any{
				"name": "snapclass-a",
			},
			"driver": "driver-a.csi.k8s.io",
		},
	}
	classB := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshotClass",
			"metadata": map[string]any{
				"name": "snapclass-b",
			},
			"driver": "driver-b.csi.k8s.io",
		},
	}

	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{
			VolumeSnapshotClassGVR: "VolumeSnapshotClassList",
		},
		classA, classB,
	)
	c := &Client{Dynamic: dyn}

	matchA, err := c.MatchingVolumeSnapshotClass(context.Background(), "driver-a.csi.k8s.io")
	if err != nil {
		t.Fatalf("unexpected error finding class for driver A: %v", err)
	}
	if matchA != "snapclass-a" {
		t.Errorf("got %q, want snapclass-a", matchA)
	}

	matchNone, err := c.MatchingVolumeSnapshotClass(context.Background(), "driver-c.csi.k8s.io")
	if err != nil {
		t.Fatalf("unexpected error finding class for driver C: %v", err)
	}
	if matchNone != "" {
		t.Errorf("got %q, want empty string", matchNone)
	}
}

// TestMatchingVolumeSnapshotClassMalformed asserts how MatchingVolumeSnapshotClass reads
// a class whose driver field is not the string the API promises: a class with no driver
// at all is simply not a match, while one whose driver is of the wrong type is an error
// naming the object, not a silent non-match that would report snapshotting as unadvertised.
//
// Steps:
//  1. List a single class with no driver field; assert a clean non-match.
//  2. List a single class whose driver is a map; assert an error that names the class.
func TestMatchingVolumeSnapshotClassMalformed(t *testing.T) {
	class := func(name string, driver any) *unstructured.Unstructured {
		obj := map[string]any{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshotClass",
			"metadata": map[string]any{
				"name": name,
			},
		}
		if driver != nil {
			obj["driver"] = driver
		}
		return &unstructured.Unstructured{Object: obj}
	}
	// One object per client: the fake tracker gives no ordering guarantee, and a
	// match found before the malformed object would not exercise it.
	client := func(obj *unstructured.Unstructured) *Client {
		return &Client{Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(),
			map[schema.GroupVersionResource]string{
				VolumeSnapshotClassGVR: "VolumeSnapshotClassList",
			},
			obj,
		)}
	}

	noDriver := client(class("snapclass-no-driver", nil))
	match, err := noDriver.MatchingVolumeSnapshotClass(context.Background(), "driver-a.csi.k8s.io")
	if err != nil {
		t.Fatalf("class with no driver field reported an error, want a clean non-match: %v", err)
	}
	if match != "" {
		t.Errorf("got %q, want empty string", match)
	}

	badDriver := client(class("snapclass-bad-driver", map[string]any{"name": "driver-a.csi.k8s.io"}))
	if _, err := badDriver.MatchingVolumeSnapshotClass(context.Background(), "driver-a.csi.k8s.io"); err == nil {
		t.Fatalf("malformed driver field reported as a clean non-match, want an error")
	} else if !strings.Contains(err.Error(), "snapclass-bad-driver") {
		t.Errorf("error does not name the malformed class: %v", err)
	}
}

// TestDescribeResizeConditions covers the diagnosis attached to an expansion
// that never completed.
//
// It exists because the interesting branch had become unreachable and nothing
// said so. The helper listed every condition on the claim, and Kubernetes posts
// Unused=False on every claim a pod references, so "nothing acted on the
// request" could never be reached on a mounted claim -- which is every claim
// PROV-04 and PROV-11 expand. The run that found it printed
// "[Unused=False(A pod is currently referencing this PVC)]" where the F-004
// diagnosis belonged. See F-012.
//
// Steps:
//  1. Describe a claim carrying only the unrelated condition the cluster posts.
//  2. Describe a claim with a real resize condition, and one with both.
//  3. Describe a claim with no conditions at all.
//  4. Assert the diagnosis appears whenever no resize condition does, that an
//     unrelated condition is named but kept out of the verdict, and that a real
//     resize condition is reported on its own.
func TestDescribeResizeConditions(t *testing.T) {
	// Verbatim from run e2e-full-20260911, PROV-04 and PROV-11 on
	// Kubernetes v1.37.0-gke.2941000.
	unused := corev1.PersistentVolumeClaimCondition{
		Type:    corev1.PersistentVolumeClaimConditionType("Unused"),
		Status:  corev1.ConditionFalse,
		Message: "A pod is currently referencing this PVC",
	}
	resizing := corev1.PersistentVolumeClaimCondition{
		Type:    corev1.PersistentVolumeClaimResizing,
		Status:  corev1.ConditionTrue,
		Message: "waiting for the controller to expand the volume",
	}
	pending := corev1.PersistentVolumeClaimCondition{
		Type:    corev1.PersistentVolumeClaimFileSystemResizePending,
		Status:  corev1.ConditionTrue,
		Message: "waiting for a pod to start to finish file system resize",
	}
	const diagnosis = "nothing acted on the request"

	claim := func(cs ...corev1.PersistentVolumeClaimCondition) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			Status: corev1.PersistentVolumeClaimStatus{Conditions: cs},
		}
	}

	for _, tc := range []struct {
		name       string
		pvc        *corev1.PersistentVolumeClaim
		wantDiag   bool
		wantHas    []string
		wantHasNot []string
	}{{
		name:     "only an unrelated condition",
		pvc:      claim(unused),
		wantDiag: true,
		// Named, so a reader knows the claim was not condition-free, but the
		// message must not read as though Unused explained the timeout.
		wantHas:    []string{"Unused"},
		wantHasNot: []string{"A pod is currently referencing this PVC"},
	}, {
		name:       "no conditions at all",
		pvc:        claim(),
		wantDiag:   true,
		wantHasNot: []string{"does carry"},
	}, {
		name:       "a real resize condition",
		pvc:        claim(resizing),
		wantHas:    []string{"Resizing=True", "waiting for the controller"},
		wantHasNot: []string{diagnosis},
	}, {
		name:       "a resize condition alongside the unrelated one",
		pvc:        claim(unused, pending),
		wantHas:    []string{"FileSystemResizePending=True"},
		wantHasNot: []string{diagnosis, "Unused"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := describeResizeConditions(tc.pvc)
			if tc.wantDiag && !strings.Contains(got, diagnosis) {
				t.Errorf("no resize condition was posted, so the F-004 diagnosis should appear: %s", got)
			}
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("the description does not mention %q: %s", want, got)
				}
			}
			for _, unwanted := range tc.wantHasNot {
				if strings.Contains(got, unwanted) {
					t.Errorf("the description should not mention %q: %s", unwanted, got)
				}
			}
		})
	}
}

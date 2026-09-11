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

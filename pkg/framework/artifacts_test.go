package framework

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestWriteLogsPropagatesError covers error propagation in writeLogs when target
// path creation or streaming fails.
//
// Steps:
//  1. Create a fake Kubernetes clientset.
//  2. Invoke writeLogs with an invalid output directory path.
//  3. Assert that writeLogs returns the file creation error.
func TestWriteLogsPropagatesError(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	fw := &Framework{
		C: &Client{Kube: fakeClient},
	}

	// Use a non-existent directory path without creating it, causing os.Create to fail.
	invalidDir := filepath.Join(t.TempDir(), "nonexistent", "subdir")
	err := fw.writeLogs(context.Background(), invalidDir, "default", "pod1", "c1", false)
	if err == nil {
		t.Error("writeLogs returned nil error for invalid target directory, want error")
	}
	if !os.IsNotExist(err) {
		t.Errorf("writeLogs returned error %v, want os.ErrNotExist", err)
	}
}

// TestDumpPodsSurvivesLogErrors covers dumpPods executing across containers
// even when individual container log requests return errors.
//
// Steps:
//  1. Create a fake Kubernetes clientset with a single test pod.
//  2. Invoke dumpPods.
//  3. Assert that dumpPods completes successfully without failing overall pod metadata collection.
func TestDumpPodsSurvivesLogErrors(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "c1"},
			},
		},
	}
	fakeClient := fake.NewSimpleClientset(pod)
	fw := &Framework{
		C: &Client{Kube: fakeClient},
	}

	dir := t.TempDir()
	err := fw.dumpPods(context.Background(), dir, "default", "app=test", "client")
	if err != nil {
		t.Fatalf("dumpPods returned unexpected error: %v", err)
	}
}

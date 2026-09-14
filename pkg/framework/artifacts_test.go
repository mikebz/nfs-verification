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

// TestDumpPodsErrorPropagated verifies that dumpPods returns an error when
// writing a pod manifest fails.
//
// Steps:
//  1. Create a fake Kubernetes clientset containing a pod.
//  2. Create a Framework instance with the fake clientset.
//  3. Create a directory collision where writing the JSON file fails.
//  4. Assert that dumpPods returns an error rather than ignoring the failure.
func TestDumpPodsErrorPropagated(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
	}
	kube := fake.NewSimpleClientset(pod)
	f := &Framework{
		C: &Client{Kube: kube},
	}

	dir := t.TempDir()
	sub := filepath.Join(dir, "client-pods")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("failed to create sub dir: %v", err)
	}
	// Create a directory where the pod file should be written, causing WriteFile to return an error.
	podFilePath := filepath.Join(sub, "test-pod.json")
	if err := os.MkdirAll(podFilePath, 0o755); err != nil {
		t.Fatalf("failed to create directory in place of pod file: %v", err)
	}

	err := f.dumpPods(context.Background(), dir, "default", "app=test", "client")
	if err == nil {
		t.Fatal("expected dumpPods to fail when os.WriteFile returns an error, but got nil")
	}
}

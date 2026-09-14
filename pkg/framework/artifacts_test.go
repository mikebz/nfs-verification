package framework

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

type errReader struct {
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	return 0, e.err
}

func (e *errReader) Close() error {
	return nil
}

type errTransport struct {
	err error
}

func (t *errTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       &errReader{err: t.err},
		Header:     make(http.Header),
	}, nil
}

// TestWriteLogsPropagatesIOCopyError covers writeLogs propagating stream read
// errors encountered during io.Copy.
//
// Steps:
//  1. Construct a Kubernetes client backed by a transport returning an erroring stream.
//  2. Invoke writeLogs.
//  3. Assert that writeLogs captures and returns the io.Copy stream error.
func TestWriteLogsPropagatesIOCopyError(t *testing.T) {
	streamErr := errors.New("simulated io.Copy stream failure")
	restCfg := &rest.Config{
		Host:      "http://localhost",
		Transport: &errTransport{err: streamErr},
	}
	kubeClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("building kube client: %v", err)
	}
	fw := &Framework{
		C: &Client{Kube: kubeClient},
	}

	dir := t.TempDir()
	err = fw.writeLogs(context.Background(), dir, "default", "pod1", "c1", false)
	if err == nil {
		t.Fatal("writeLogs returned nil error for failing stream copy, want error")
	}
	if !strings.Contains(err.Error(), "simulated io.Copy stream failure") {
		t.Errorf("writeLogs returned error %v, want simulated io.Copy stream failure", err)
	}
}

// TestWriteLogsPropagatesFileCreateError covers writeLogs propagating errors when
// target output file creation fails.
//
// Steps:
//  1. Create a fake Kubernetes clientset.
//  2. Invoke writeLogs with an invalid output directory path.
//  3. Assert that writeLogs returns the file creation error.
func TestWriteLogsPropagatesFileCreateError(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	fw := &Framework{
		C: &Client{Kube: fakeClient},
	}

	invalidDir := filepath.Join(t.TempDir(), "nonexistent", "subdir")
	err := fw.writeLogs(context.Background(), invalidDir, "default", "pod1", "c1", false)
	if err == nil {
		t.Error("writeLogs returned nil error for invalid target directory, want error")
	}
	if !os.IsNotExist(err) {
		t.Errorf("writeLogs returned error %v, want os.ErrNotExist", err)
	}
}

// TestDumpPodsCollectsLogErrors covers dumpPods propagating log collection failures.
//
// Steps:
//  1. Create a fake Kubernetes clientset with a pod.
//  2. Create a conflicting directory blocking log file creation.
//  3. Invoke dumpPods.
//  4. Assert that dumpPods reports errors from writeLogs rather than ignoring them.
func TestDumpPodsCollectsLogErrors(t *testing.T) {
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
	sub := filepath.Join(dir, "client-pods")
	// Pre-create a directory with the log filename so os.Create inside writeLogs fails.
	if err := os.MkdirAll(filepath.Join(sub, "test-pod-c1.log"), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	err := fw.dumpPods(context.Background(), dir, "default", "app=test", "client")
	if err == nil {
		t.Fatal("dumpPods returned nil error when writeLogs fails, want error")
	}
	if !strings.Contains(err.Error(), "dumping pods in default") {
		t.Errorf("dumpPods error = %v, want error containing 'dumping pods in default'", err)
	}
}

package framework

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MountSpec attaches a claim into a pod at a path.
type MountSpec struct {
	Claim    string
	Path     string
	ReadOnly bool
	SubPath  string
}

// PodSpec is the subset of the pod API the suite needs. Anything not here is
// deliberately absent: a test pod that needs more than this is testing the pod
// API rather than the storage system.
type PodSpec struct {
	Name    string
	Image   string
	Command []string
	Mounts  []MountSpec
	// Node pins the pod. Cross-node cases pin explicitly rather than hoping the
	// scheduler spreads them.
	Node string
	// RunAsUser and FSGroup drive the identity cases.
	RunAsUser  *int64
	RunAsGroup *int64
	FSGroup    *int64
	Privileged bool
	Labels     map[string]string
	// RestartNever makes the pod a one-shot job-like unit whose exit code is
	// the assertion.
	RestartNever bool
	Env          []corev1.EnvVar
}

// PodBuilder assembles a pod object from a PodSpec.
func (f *Framework) PodBuilder(spec PodSpec) *corev1.Pod {
	image := spec.Image
	if image == "" {
		image = Cfg().ToolsImage
	}
	cmd := spec.Command
	if len(cmd) == 0 {
		cmd = []string{"sh", "-c", "sleep infinity"}
	}
	labels := f.Labels()
	for k, v := range spec.Labels {
		labels[k] = v
	}
	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume
	for i, m := range spec.Mounts {
		name := fmt.Sprintf("vol%d", i)
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: m.Path, ReadOnly: m.ReadOnly, SubPath: m.SubPath})
		volumes = append(volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: m.Claim, ReadOnly: m.ReadOnly,
			}},
		})
	}
	restart := corev1.RestartPolicyAlways
	if spec.RestartNever {
		restart = corev1.RestartPolicyNever
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: f.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: restart,
			NodeName:      spec.Node,
			Containers: []corev1.Container{{
				Name:         "main",
				Image:        image,
				Command:      cmd,
				VolumeMounts: mounts,
				Env:          spec.Env,
			}},
			Volumes: volumes,
			// Test pods are disposable; a long grace period only makes
			// force-delete cases slower to observe.
			TerminationGracePeriodSeconds: ptr(int64(5)),
		},
	}
	if spec.Privileged {
		pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr(true)}
	}
	if spec.RunAsUser != nil || spec.FSGroup != nil || spec.RunAsGroup != nil {
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsUser:  spec.RunAsUser,
			RunAsGroup: spec.RunAsGroup,
			FSGroup:    spec.FSGroup,
		}
	}
	return pod
}

// CreatePod creates a pod and waits for it to be Running and Ready.
func (f *Framework) CreatePod(ctx context.Context, spec PodSpec) (*corev1.Pod, error) {
	pod, err := f.C.Kube.CoreV1().Pods(f.Namespace).Create(ctx, f.PodBuilder(spec), metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return f.WaitPodReady(ctx, pod.Name, PodReadyTimeout)
}

// MustPod creates a pod or fails the test.
func (f *Framework) MustPod(ctx context.Context, spec PodSpec) *corev1.Pod {
	f.T.Helper()
	pod, err := f.CreatePod(ctx, spec)
	if err != nil {
		f.T.Fatalf("creating pod %s: %v", spec.Name, err)
	}
	return pod
}

// WaitPodReady waits for a pod to be Running with all containers ready.
func (f *Framework) WaitPodReady(ctx context.Context, name string, timeout time.Duration) (*corev1.Pod, error) {
	var ready *corev1.Pod
	err := Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		pod, err := f.C.Kube.CoreV1().Pods(f.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pod.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("pod %s failed: %s", name, pod.Status.Message)
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if !cs.Ready {
				return false, fmt.Errorf("container %s not ready: %s", cs.Name, containerStateString(cs.State))
			}
		}
		if pod.Status.Phase == corev1.PodRunning && len(pod.Status.ContainerStatuses) > 0 {
			ready = pod
			return true, nil
		}
		return false, fmt.Errorf("pod %s is %s", name, pod.Status.Phase)
	})
	return ready, err
}

// WaitPodGone waits for a pod to disappear from the API.
func (f *Framework) WaitPodGone(ctx context.Context, name string, timeout time.Duration) error {
	return Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		_, err := f.C.Kube.CoreV1().Pods(f.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return IgnoreNotFound(err) == nil, nil
		}
		return false, fmt.Errorf("pod %s still present", name)
	})
}

// DeletePodNow force-deletes a pod, which is how the lock-release cases model a
// client that vanished without unlocking.
func (f *Framework) DeletePodNow(ctx context.Context, name string) error {
	return IgnoreNotFound(f.C.Kube.CoreV1().Pods(f.Namespace).Delete(ctx, name, DeleteNow()))
}

// Sh runs a shell snippet in a pod of this namespace.
func (f *Framework) Sh(ctx context.Context, pod, script string) ExecResult {
	return f.C.Sh(ctx, f.Namespace, pod, "main", script)
}

// MustShf runs a shell snippet in a pod and fails the test on error.
func (f *Framework) MustShf(ctx context.Context, pod, format string, args ...any) string {
	f.T.Helper()
	script := fmt.Sprintf(format, args...)
	out, err := f.C.MustSh(ctx, f.Namespace, pod, "main", script)
	if err != nil {
		f.T.Fatalf("running %q in %s: %v", script, pod, err)
	}
	return out
}

// WorkerNodes returns schedulable nodes that are not control plane, sorted for
// determinism so that a rerun pins the same pods to the same nodes.
func (f *Framework) WorkerNodes(ctx context.Context) ([]string, error) {
	return WorkerNodes(ctx, f.C)
}

// WorkerNodes is the client-level form, usable before a fixture exists.
func WorkerNodes(ctx context.Context, c *Client) ([]string, error) {
	nodes, err := c.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Unschedulable {
			continue
		}
		if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
			continue
		}
		if _, ok := n.Labels["node-role.kubernetes.io/master"]; ok {
			continue
		}
		if !nodeReady(n) {
			continue
		}
		out = append(out, n.Name)
	}
	sort.Strings(out)
	return out, nil
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// TwoNodes returns two distinct schedulable worker nodes, failing the test when
// the cluster cannot host a cross-node case.
func (f *Framework) TwoNodes(ctx context.Context) (string, string) {
	f.T.Helper()
	nodes, err := f.WorkerNodes(ctx)
	if err != nil {
		f.T.Fatalf("listing worker nodes: %v", err)
	}
	if len(nodes) < 2 {
		f.T.Fatalf("insufficient nodes for cross-node cases: %d schedulable workers", len(nodes))
	}
	return nodes[0], nodes[1]
}

func containerStateString(s corev1.ContainerState) string {
	switch {
	case s.Waiting != nil:
		return "Waiting: " + s.Waiting.Reason + " " + s.Waiting.Message
	case s.Terminated != nil:
		return fmt.Sprintf("Terminated: %s exit=%d", s.Terminated.Reason, s.Terminated.ExitCode)
	case s.Running != nil:
		return "Running"
	}
	return "unknown"
}

func ptr[T any](v T) *T { return &v }

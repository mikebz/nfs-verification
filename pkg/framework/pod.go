package framework

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MountSpec attaches a claim into a pod at a path. Claim is a logical name,
// resolved the same way pod names are, so a case names its claim once and never
// has to think about the prefix again.
type MountSpec struct {
	Claim    string
	Path     string
	ReadOnly bool
	SubPath  string
}

// PodSpec is the subset of the pod API the suite needs. Anything not here is
// deliberately absent: a test pod that needs more than this is testing the pod
// API rather than the storage system. Fields arrive with the cases that need
// them, the identity fields with SEC-01 and SEC-02.
type PodSpec struct {
	Name    string
	Image   string
	Command []string
	Mounts  []MountSpec
	// Node pins the pod. Cross-node cases pin explicitly rather than hoping the
	// scheduler spreads them.
	Node   string
	Labels map[string]string
	// RunAsUser and RunAsGroup set the pod's identity. The identity cases need
	// a pod that is not root, because the property under test is whether the
	// uid a pod writes as is the uid another pod reads back.
	//
	// fsGroup and supplementary groups are deliberately absent. fsGroup makes
	// kubelet walk the volume and chown every file in it on each mount, which
	// on a share of any size is slow enough to dominate a case and destructive
	// enough to erase the ownership the identity cases are asserting on. It
	// arrives with SEC-03, which is the case that means to measure that.
	RunAsUser  *int64
	RunAsGroup *int64
}

// podTemplateData is what manifests/client-pod.yaml is rendered against.
type podTemplateData struct {
	Name      string
	Namespace string
	Image     string
	Node      string
	Command   []string
	Labels    map[string]string
	Mounts    []podMountData
	// Security is nil unless the case pins an identity, so the ordinary pod
	// renders exactly as it did before the identity cases existed.
	Security *podSecurityData
}

// podSecurityData carries the identity fields as values rather than pointers,
// because a template cannot dereference one.
type podSecurityData struct {
	SetUser  bool
	User     int64
	SetGroup bool
	Group    int64
}

type podMountData struct {
	VolumeName string
	Claim      string
	Path       string
	ReadOnly   bool
	SubPath    string
}

// PodBuilder renders manifests/client-pod.yaml into a pod object.
func (f *Framework) PodBuilder(spec PodSpec) (*corev1.Pod, error) {
	data := podTemplateData{
		Name:      f.Name(spec.Name),
		Namespace: Namespace,
		Image:     spec.Image,
		Node:      spec.Node,
		Command:   spec.Command,
		Labels:    f.Labels(),
	}
	if data.Image == "" {
		data.Image = Cfg().ToolsImage
	}
	if len(data.Command) == 0 {
		data.Command = []string{"sh", "-c", "sleep infinity"}
	}
	for k, v := range spec.Labels {
		data.Labels[k] = v
	}
	if spec.RunAsUser != nil || spec.RunAsGroup != nil {
		data.Security = &podSecurityData{}
		if spec.RunAsUser != nil {
			data.Security.SetUser, data.Security.User = true, *spec.RunAsUser
		}
		if spec.RunAsGroup != nil {
			data.Security.SetGroup, data.Security.Group = true, *spec.RunAsGroup
		}
	}
	for i, m := range spec.Mounts {
		data.Mounts = append(data.Mounts, podMountData{
			VolumeName: fmt.Sprintf("vol%d", i),
			// Claims are logical names too. Name is idempotent, so a caller
			// that already holds the real name loses nothing by passing it.
			Claim:    f.Name(m.Claim),
			Path:     m.Path,
			ReadOnly: m.ReadOnly,
			SubPath:  m.SubPath,
		})
	}
	if err := CheckObjectName("pod", data.Name); err != nil {
		return nil, err
	}
	for _, m := range data.Mounts {
		if err := CheckObjectName("claim", m.Claim); err != nil {
			return nil, err
		}
	}
	var pod corev1.Pod
	if err := render("client-pod.yaml", data, &pod); err != nil {
		return nil, err
	}
	return &pod, nil
}

// CreatePod creates a pod and waits for it to be Running and Ready.
func (f *Framework) CreatePod(ctx context.Context, spec PodSpec) (*corev1.Pod, error) {
	obj, err := f.PodBuilder(spec)
	if err != nil {
		return nil, err
	}
	pod, err := f.C.Kube.CoreV1().Pods(Namespace).Create(ctx, obj, metav1.CreateOptions{})
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
	name = f.Name(name)
	var ready *corev1.Pod
	err := Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		pod, err := f.C.Kube.CoreV1().Pods(Namespace).Get(ctx, name, metav1.GetOptions{})
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
	name = f.Name(name)
	return Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		_, err := f.C.Kube.CoreV1().Pods(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return IgnoreNotFound(err) == nil, nil
		}
		return false, fmt.Errorf("pod %s still present", name)
	})
}

// DeletePod deletes a pod gracefully, giving kubelet time to unmount before the
// object leaves the API. This is what teardown wants; see DeletePodNow for why.
func (f *Framework) DeletePod(ctx context.Context, name string) error {
	return IgnoreNotFound(f.C.Kube.CoreV1().Pods(Namespace).Delete(ctx, f.Name(name), metav1.DeleteOptions{}))
}

// DeletePodNow force-deletes a pod, which is how the lock cases model a client
// that vanished without unlocking.
//
// Never use it for teardown. The pod leaves the API before kubelet unmounts, so
// anything that then deletes the claim destroys an export a node is still
// mounting, and a hard NFSv4.1 mount retries that forever. See docs/findings.md.
func (f *Framework) DeletePodNow(ctx context.Context, name string) error {
	return IgnoreNotFound(f.C.Kube.CoreV1().Pods(Namespace).Delete(ctx, f.Name(name), DeleteNow()))
}

// Sh runs a shell snippet in a pod of this namespace.
func (f *Framework) Sh(ctx context.Context, pod, script string) ExecResult {
	return f.C.Sh(ctx, Namespace, f.Name(pod), "main", script)
}

// MustShf runs a shell snippet in a pod and fails the test on error.
func (f *Framework) MustShf(ctx context.Context, pod, format string, args ...any) string {
	f.T.Helper()
	script := fmt.Sprintf(format, args...)
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main", script)
	if err != nil {
		f.T.Fatalf("running %q in %s: %v", script, f.Name(pod), err)
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

// Int64 returns a pointer to v, for the optional identity fields on PodSpec.
func Int64(v int64) *int64 { return &v }

// PodRestarts returns how many times a pod's container has restarted. A case
// that asserts "no unmount, no restart" needs the number before and after, not
// just the pod still being Ready afterwards.
func (f *Framework) PodRestarts(ctx context.Context, name string) (int32, error) {
	pod, err := f.C.Kube.CoreV1().Pods(Namespace).Get(ctx, f.Name(name), metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	var n int32
	for _, cs := range pod.Status.ContainerStatuses {
		n += cs.RestartCount
	}
	return n, nil
}

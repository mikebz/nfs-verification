package framework

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestWorkerNodesSelection verifies that WorkerNodes returns only Ready,
// schedulable worker nodes without NoSchedule or NoExecute taints, excludes
// control-plane nodes (both control-plane and master labels), and sorts the
// returned node names deterministically (test plan Section 0 and Section 1).
//
// Why this test exists: test pods rendered by PodBuilder carry no tolerations
// and pin nodes via nodeSelector so WaitForFirstConsumer claims bind through
// kube-scheduler. Returning a tainted or unschedulable node causes pinned test
// pods to hang Pending rather than failing preflight loudly.
//
// Steps:
//  1. Seed a fake clientset with each table case's node list.
//  2. Call WorkerNodes.
//  3. Assert the returned slice matches the expected sorted list of worker names.
func TestWorkerNodesSelection(t *testing.T) {
	ready := []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	notReady := []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}

	cases := []struct {
		name  string
		nodes []*corev1.Node
		want  []string
	}{
		{
			name: "excludes control-plane and master labeled nodes",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "cp-1",
						Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
					},
					Status: corev1.NodeStatus{Conditions: ready},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "master-1",
						Labels: map[string]string{"node-role.kubernetes.io/master": ""},
					},
					Status: corev1.NodeStatus{Conditions: ready},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
					Status:     corev1.NodeStatus{Conditions: ready},
				},
			},
			want: []string{"worker-1"},
		},
		{
			name: "excludes workers with NoSchedule or NoExecute taints and accepts PreferNoSchedule",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-noschedule"},
					Spec: corev1.NodeSpec{
						Taints: []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}},
					},
					Status: corev1.NodeStatus{Conditions: ready},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-noexecute"},
					Spec: corev1.NodeSpec{
						Taints: []corev1.Taint{{Key: "maintenance", Effect: corev1.TaintEffectNoExecute}},
					},
					Status: corev1.NodeStatus{Conditions: ready},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-prefer-noschedule"},
					Spec: corev1.NodeSpec{
						Taints: []corev1.Taint{{Key: "spot", Effect: corev1.TaintEffectPreferNoSchedule}},
					},
					Status: corev1.NodeStatus{Conditions: ready},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-clean"},
					Status:     corev1.NodeStatus{Conditions: ready},
				},
			},
			want: []string{"worker-clean", "worker-prefer-noschedule"},
		},
		{
			name: "excludes unschedulable and not-ready workers",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-cordoned"},
					Spec:       corev1.NodeSpec{Unschedulable: true},
					Status:     corev1.NodeStatus{Conditions: ready},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-notready"},
					Status:     corev1.NodeStatus{Conditions: notReady},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-noconditions"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-ready"},
					Status:     corev1.NodeStatus{Conditions: ready},
				},
			},
			want: []string{"worker-ready"},
		},
		{
			name: "sorts returned worker names deterministically from unsorted input",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-c"},
					Status:     corev1.NodeStatus{Conditions: ready},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-a"},
					Status:     corev1.NodeStatus{Conditions: ready},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-b"},
					Status:     corev1.NodeStatus{Conditions: ready},
				},
			},
			want: []string{"worker-a", "worker-b", "worker-c"},
		},
		{
			name: "all control-plane cluster returns empty list for loud preflight stop",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "cp-1",
						Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
					},
					Status: corev1.NodeStatus{Conditions: ready},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "cp-2",
						Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
					},
					Status: corev1.NodeStatus{Conditions: ready},
				},
			},
			want: nil,
		},
	}

	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kube := fake.NewSimpleClientset()
			for _, n := range tc.nodes {
				if _, err := kube.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
					t.Fatalf("seeding node %s: %v", n.Name, err)
				}
			}
			c := &Client{Kube: kube}
			got, err := WorkerNodes(ctx, c)
			if err != nil {
				t.Fatalf("WorkerNodes failed: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

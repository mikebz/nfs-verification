package framework

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestWorkerNodesSelection checks that WorkerNodes counts a node when a test
// pod could land on it, and not otherwise. Role labels play no part: a
// three-node cluster where every node runs the API server and the workloads
// yields three usable nodes, which is what GDC ships and what an earlier
// role-label filter refused to run on.
//
// Why this test exists: test pods carry no tolerations and pin their node with
// a nodeSelector, so a node returned here that will not accept one leaves the
// pod Pending until the case times out, with nothing naming the cause.
//
// Steps:
//  1. Seed a fake clientset with the case's nodes.
//  2. Call WorkerNodes.
//  3. Compare against the names a pinned pod could actually be scheduled on,
//     in sorted order.
func TestWorkerNodesSelection(t *testing.T) {
	ready := corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}
	notReady := corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}
	controlPlane := map[string]string{"node-role.kubernetes.io/control-plane": ""}

	cases := []struct {
		name  string
		nodes []*corev1.Node
		want  []string
	}{
		{
			// Names are out of order so that the sort is actually exercised.
			name: "every node runs the API server and the workloads",
			nodes: []*corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node-c", Labels: controlPlane}, Status: ready},
				{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: controlPlane}, Status: ready},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: controlPlane},
					Spec:       corev1.NodeSpec{Taints: []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectPreferNoSchedule}}},
					Status:     ready,
				},
			},
			want: []string{"node-a", "node-b", "node-c"},
		},
		{
			name: "a node that would not take a pinned pod does not count",
			nodes: []*corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "cordoned"}, Spec: corev1.NodeSpec{Unschedulable: true}, Status: ready},
				{ObjectMeta: metav1.ObjectMeta{Name: "not-ready"}, Status: notReady},
				{ObjectMeta: metav1.ObjectMeta{Name: "no-conditions"}},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "tainted-noschedule"},
					Spec:       corev1.NodeSpec{Taints: []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}},
					Status:     ready,
				},
				{
					// The stock kubeadm control-plane taint: excluded here on
					// its taint, which is the cluster saying so, rather than on
					// its label.
					ObjectMeta: metav1.ObjectMeta{Name: "tainted-control-plane", Labels: controlPlane},
					Spec:       corev1.NodeSpec{Taints: []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}},
					Status:     ready,
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "tainted-noexecute"},
					Spec:       corev1.NodeSpec{Taints: []corev1.Taint{{Key: "maintenance", Effect: corev1.TaintEffectNoExecute}}},
					Status:     ready,
				},
				{ObjectMeta: metav1.ObjectMeta{Name: "usable"}, Status: ready},
			},
			want: []string{"usable"},
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
			got, err := WorkerNodes(ctx, &Client{Kube: kube})
			if err != nil {
				t.Fatalf("WorkerNodes failed: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

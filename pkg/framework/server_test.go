package framework

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Server discovery falls back to a name and port heuristic when no selector is
// given. The heuristic must never match the suite's own pods: they would be
// recorded as servers, and the chaos cases would then kill the client pods and
// the node agent instead of the thing under test.
// TestServerHeuristicIgnoresHarnessPods covers server discovery when no
// selector was passed. Getting this wrong is not a failed test: the name
// heuristic would match the suite's own pods, and the chaos cases would kill
// the harness instead of the server.
//
// Steps:
//  1. Offer pods that look like servers by port and by name.
//  2. Offer the suite's own client pods and node agent, which also match.
//  3. Assert only the real servers are returned.
func TestServerHeuristicIgnoresHarnessPods(t *testing.T) {
	pod := func(name string, labels map[string]string, port int32, image string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
		c := corev1.Container{Name: "main", Image: image}
		if port != 0 {
			c.Ports = []corev1.ContainerPort{{ContainerPort: port}}
		}
		p.Spec.Containers = []corev1.Container{c}
		return p
	}
	harnessLabels := map[string]string{"nfs-verification/run": "20260910-030844", "nfs-verification/case": "data-05"}

	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"real server by port", pod("nfs-server-provisioner-0", nil, 2049, "quay.io/example/nfs:1.2"), true},
		{"real server by image", pod("storage-0", nil, 0, "registry/nfs-ganesha:5.7"), true},
		{"harness client pod", pod("nfsv-data-05-20260910-030844-holder", harnessLabels, 0, "alpine:3.20"), false},
		{"node agent", pod("nfs-verification-node-agent-abcde", map[string]string{"app": agentDaemonSet}, 0, "alpine:3.20"), false},
		{"harness pod by name alone", pod("nfsv-prov-01-20260910-030844-writer", nil, 0, "alpine:3.20"), false},
		{"csi driver pod", pod("csi-nfs-node-xyz", nil, 0, "registry/csi-driver-nfs:v4.9.0"), false},
		{"unrelated workload", pod("web-7d9f", nil, 8080, "nginx:1.27"), false},
	}
	for _, tc := range cases {
		if got := looksLikeNFSServer(tc.pod); got != tc.want {
			t.Errorf("%s: looksLikeNFSServer(%q) = %t, want %t", tc.name, tc.pod.Name, got, tc.want)
		}
	}
}

// TestPodReadyIgnoresTerminating covers the readiness answer for a pod that is
// being gracefully deleted.
//
// This is the failure with no symptom. A deleted pod keeps phase Running and
// ready containers for its whole termination grace period, so a readiness check
// that looks only at phase and containers says yes about the pod the caller has
// just deleted. Nothing errors. The caller concludes the server is back, the
// pod then leaves the API, and the next question finds no server at all --
// which is how CHAOS-05 failed partway through its cycles, several minutes and
// one misleading success later. See F-013.
//
// Steps:
//  1. Ask about a healthy Running pod with ready containers.
//  2. Ask about the same pod with a deletion timestamp on it, which is the
//     state the API reports for the whole grace period after a DELETE.
//  3. Ask about the states that were already handled, so the deletion check is
//     shown to be an addition rather than a replacement.
func TestPodReadyIgnoresTerminating(t *testing.T) {
	deleting := metav1.NewTime(metav1.Now().Time)
	pod := func(phase corev1.PodPhase, ready bool, deletedAt *metav1.Time) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "nfs-server-0", DeletionTimestamp: deletedAt},
			Status: corev1.PodStatus{
				Phase:             phase,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "nfs", Ready: ready}},
			},
		}
	}

	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"running and ready", pod(corev1.PodRunning, true, nil), true},
		{"running and ready but terminating", pod(corev1.PodRunning, true, &deleting), false},
		{"running with an unready container", pod(corev1.PodRunning, false, nil), false},
		{"not running", pod(corev1.PodPending, true, nil), false},
		{"failed, which is where a deleted pod ends up", pod(corev1.PodFailed, true, nil), false},
		{"no container statuses yet", &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PodReady(tc.pod); got != tc.want {
				t.Errorf("PodReady = %v, want %v. A terminating server pod counted as ready is not a "+
					"wrong answer a caller can see: it is a wait that returns early and a target that "+
					"vanishes afterwards", got, tc.want)
			}
		})
	}
}

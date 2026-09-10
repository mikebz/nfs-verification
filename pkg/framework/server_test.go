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

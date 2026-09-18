package framework

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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

// TestServerRestartCount covers the two states in which the restart count
// cannot be read, and the one in which it can.
//
// This is the failure with no symptom. Every caller of this function reads it
// twice and compares the two readings, so an unreadable state answering zero
// makes the comparison 0 != 0: the case reports that the server survived its
// load, on a cluster where the suite never saw a server container at all.
// Nothing errors, nothing is logged, and PROV-02, PROV-09, PROV-10, PROV-11 and
// DATA-10 all go green on the strength of it.
//
// The answer is a blocked condition rather than a plain error, so that a case
// reaching it reports blocked and says what would fix it: a managed NFS server
// with no pod in this cluster is a fact about the deployment, not a defect in
// it, and a server pod the kubelet has not admitted yet is a state to read
// again rather than a flag to pass.
//
// Steps:
//  1. Ask a cluster whose only pods are the suite's own and an unrelated
//     workload, which is what a managed NFS deployment looks like from here.
//  2. Assert the answer is ErrNoServerPods, that it reads as blocked, and that
//     it names the flags that fix it.
//  3. Ask a cluster whose only server pod is Pending, which is what discovery
//     sees while the server is being rescheduled, and assert the answer is
//     ErrNoServerContainerStatuses and reads as blocked.
//  4. Ask a cluster that does have running server pods, and assert the restart
//     counts are summed across its pods and containers, a Pending pod among
//     them notwithstanding.
func TestServerRestartCount(t *testing.T) {
	// Discovery reads the flags, and one of these subtests needs the heuristic
	// rather than a selector. Restored so the order tests run in cannot matter.
	restore := *Cfg()
	t.Cleanup(func() { *Cfg() = restore })
	Cfg().ServerNamespace, Cfg().ServerSelector = "", ""

	server := func(name string, restarts ...int32) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "nfs"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  "nfs",
				Image: "registry/nfs-ganesha:5.7",
				Ports: []corev1.ContainerPort{{ContainerPort: nfsPort}},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		for i, r := range restarts {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses,
				corev1.ContainerStatus{Name: p.Spec.Containers[0].Name + string(rune('a'+i)), RestartCount: r})
		}
		return p
	}
	harness := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nfsv-prov-02-20260914-101500-consumer",
			Namespace: Namespace,
			Labels:    map[string]string{"nfs-verification/run": "20260914-101500"},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "alpine:3.20"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// A server pod the kubelet has not admitted yet. Discovery keeps it, and it
	// carries no container statuses, which is the second way a reading can come
	// back as a zero that was never measured.
	pending := server("nfs-server-2")
	pending.Status.Phase = corev1.PodPending

	t.Run("no server was discovered", func(t *testing.T) {
		c := &Client{Kube: fake.NewSimpleClientset(harness)}
		got, err := ServerRestartCount(context.Background(), c)
		if err == nil {
			t.Fatalf("ServerRestartCount = (%d, nil) with no server discovered. A caller comparing this "+
				"reading with a later one compares two zeros and reports a server it never saw as one "+
				"that did not restart", got)
		}
		if !errors.Is(err, ErrNoServerPods) {
			t.Errorf("ServerRestartCount returned %v, which callers cannot match with errors.Is against "+
				"ErrNoServerPods", err)
		}
		if !IsBlocked(err) {
			t.Errorf("the empty-discovery error does not read as blocked, so failOrBlock will file a "+
				"managed NFS server that lives outside this cluster as a storage defect: %v", err)
		}
		if !strings.Contains(err.Error(), "-server-selector") {
			t.Errorf("the empty-discovery error does not name the flags that fix it: %v", err)
		}
	})

	t.Run("the discovered pod has no container status", func(t *testing.T) {
		c := &Client{Kube: fake.NewSimpleClientset(harness, pending)}
		got, err := ServerRestartCount(context.Background(), c)
		if err == nil {
			t.Fatalf("ServerRestartCount = (%d, nil) with the only server pod Pending. Discovery found a "+
				"pod, but nothing has reported a container yet, so this reading and the next one are "+
				"both zero and the case passes without having watched anything", got)
		}
		if !errors.Is(err, ErrNoServerContainerStatuses) {
			t.Errorf("ServerRestartCount returned %v, which callers cannot match with errors.Is against "+
				"ErrNoServerContainerStatuses", err)
		}
		if !IsBlocked(err) {
			t.Errorf("the unadmitted-pod error does not read as blocked, so a server that was being "+
				"rescheduled when the case started is filed as a storage defect: %v", err)
		}
	})

	t.Run("restarts are summed across pods and containers", func(t *testing.T) {
		c := &Client{Kube: fake.NewSimpleClientset(
			harness, server("nfs-server-0", 2, 1), server("nfs-server-1", 3), pending)}
		got, err := ServerRestartCount(context.Background(), c)
		if err != nil {
			t.Fatalf("ServerRestartCount on a cluster with two running server pods and one Pending: %v", err)
		}
		if got != 6 {
			t.Errorf("ServerRestartCount = %d, want 6. A pod that reports no container status yet is not "+
				"a reason to refuse a reading the other pods can answer", got)
		}
	})
}

// TestDiscoverServerProcess covers daemon discovery from flags, probes, container
// commands and candidate matching.
//
// Steps:
//  1. Assert explicit -server-process flag wins and is validated.
//  2. Assert generic flag values are rejected.
//  3. Assert container probe mentioning org.ganesha.nfsd discovers ganesha.nfsd.
//  4. Assert container command containing rpc.nfsd discovers rpc.nfsd.
//  5. Assert nfs-provisioner supervisor is not mistaken for a daemon.
func TestDiscoverServerProcess(t *testing.T) {
	restore := *Cfg()
	t.Cleanup(func() { *Cfg() = restore })
	Cfg().ServerNamespace, Cfg().ServerSelector, Cfg().ServerProcess = "", "", ""

	t.Run("explicit flag wins and is returned", func(t *testing.T) {
		Cfg().ServerProcess = "ganesha.nfsd"
		c := &Client{Kube: fake.NewSimpleClientset()}
		got, err := DiscoverServerProcess(context.Background(), c, nil)
		if err != nil || got != "ganesha.nfsd" {
			t.Errorf("DiscoverServerProcess = (%q, %v), want (%q, nil)", got, err, "ganesha.nfsd")
		}
	})

	t.Run("generic flag is rejected loudly", func(t *testing.T) {
		Cfg().ServerProcess = "sh"
		c := &Client{Kube: fake.NewSimpleClientset()}
		got, err := DiscoverServerProcess(context.Background(), c, nil)
		if err == nil {
			t.Errorf("DiscoverServerProcess with generic flag returned %q, want error", got)
		}
	})

	t.Run("probe mentioning ganesha daemon is discovered", func(t *testing.T) {
		Cfg().ServerProcess = ""
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "robin-nfs-0", Namespace: "robinio"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:    "robin-nfs",
						Image:   "registry.example.com/nfs-server:v6.5",
						Command: []string{"/nfs-entry.sh"},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"/bin/sh", "-c", "dbus-send --dest=org.ganesha.nfsd /org/ganesha/nfsd"},
								},
							},
						},
						Ports: []corev1.ContainerPort{{ContainerPort: nfsPort}},
					},
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		c := &Client{Kube: fake.NewSimpleClientset(pod)}
		got, err := DiscoverServerProcess(context.Background(), c, nil)
		if err != nil || got != "ganesha.nfsd" {
			t.Errorf("DiscoverServerProcess from probe = (%q, %v), want (%q, nil)", got, err, "ganesha.nfsd")
		}
	})

	t.Run("container command with valid daemon is discovered", func(t *testing.T) {
		Cfg().ServerProcess = ""
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "kernel-nfs-0", Namespace: "nfs"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:    "nfs",
						Image:   "registry.example.com/kernel-nfs:latest",
						Command: []string{"/usr/sbin/rpc.nfsd", "-G", "10"},
						Ports:   []corev1.ContainerPort{{ContainerPort: nfsPort}},
					},
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		c := &Client{Kube: fake.NewSimpleClientset(pod)}
		got, err := DiscoverServerProcess(context.Background(), c, nil)
		if err != nil || got != "rpc.nfsd" {
			t.Errorf("DiscoverServerProcess from command = (%q, %v), want (%q, nil)", got, err, "rpc.nfsd")
		}
	})

	t.Run("nfs-provisioner supervisor is not returned as daemon", func(t *testing.T) {
		Cfg().ServerProcess = ""
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "nfs-provisioner-0", Namespace: "nfs"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:    "nfs",
						Image:   "registry.example.com/nfs-provisioner:latest",
						Command: []string{"/nfs-provisioner"},
						Ports:   []corev1.ContainerPort{{ContainerPort: nfsPort}},
					},
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		c := &Client{Kube: fake.NewSimpleClientset(pod)}
		got, err := DiscoverServerProcess(context.Background(), c, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("DiscoverServerProcess returned %q, want empty string when supervisor cannot be identified as daemon", got)
		}
	})
}

// TestMatchCandidateProcComm covers matching candidate daemon names from
// in-container /proc/*/comm outputs.
//
// Steps:
//  1. Assert ganesha.nfsd is selected when running alongside nfs-provisioner and rpc helpers.
//  2. Assert rpc.nfsd is selected when running in kernel NFS containers.
//  3. Assert unfsd is selected when running in user-space NFS containers.
//  4. Assert an image running only a supervisor returns empty string without matching the supervisor.
//  5. Assert unrelated processes return empty string.
func TestMatchCandidateProcComm(t *testing.T) {
	cases := []struct {
		name string
		blob string
		want string
	}{
		{
			name: "ganesha alongside supervisor and helpers",
			blob: "nfs-provisioner\nrpcbind\nrpc.statd\ndbus-daemon\nganesha.nfsd\nsh\n",
			want: "ganesha.nfsd",
		},
		{
			name: "kernel nfs userland daemon",
			blob: "systemd\nrpcbind\nrpc.nfsd\n",
			want: "rpc.nfsd",
		},
		{
			name: "unfsd user daemon",
			blob: "unfsd\n",
			want: "unfsd",
		},
		{
			name: "supervisor only with no daemon running",
			blob: "nfs-provisioner\nrpcbind\n",
			want: "",
		},
		{
			name: "empty or unrelated processes",
			blob: "nginx\nsleep\n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchCandidate(tc.blob)
			if got != tc.want {
				t.Errorf("matchCandidate() = %q, want %q", got, tc.want)
			}
		})
	}
}

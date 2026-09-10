package framework

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// The manifests are templates, so an indentation slip or a value that needs
// quoting would only surface against a cluster. These tests render and decode
// them without one.

func TestClientPodManifestRenders(t *testing.T) {
	f := &Framework{CaseID: "DATA-05"}
	pod, err := f.PodBuilder(PodSpec{
		Name:  "holder",
		Image: "alpine:3.20",
		Node:  "worker-1",
		Mounts: []MountSpec{
			// One claim already carries the prefix, one does not. Both must
			// end up addressing the same object.
			{Claim: f.Name("share"), Path: "/mnt/share"},
			{Claim: "second", Path: "/mnt/other", ReadOnly: true, SubPath: "sub"},
		},
		Labels: map[string]string{"role": "stranded"},
	})
	if err != nil {
		t.Fatalf("rendering the client pod: %v", err)
	}
	if !strings.HasPrefix(pod.Name, "nfsv-data-05-") || !strings.HasSuffix(pod.Name, "-holder") {
		t.Errorf("pod name %q does not carry the case prefix", pod.Name)
	}
	if pod.Namespace != Namespace {
		t.Errorf("namespace %q, want %q: the suite creates no namespaces", pod.Namespace, Namespace)
	}
	// A selector, not nodeName: nodeName bypasses the scheduler, and a
	// WaitForFirstConsumer class then never binds.
	if pod.Spec.NodeName != "" {
		t.Errorf("nodeName is set to %q; pinning must go through the scheduler", pod.Spec.NodeName)
	}
	if got := pod.Spec.NodeSelector["kubernetes.io/hostname"]; got != "worker-1" {
		t.Errorf("node selector is %q, want worker-1", got)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != "alpine:3.20" {
		t.Fatalf("unexpected containers: %+v", pod.Spec.Containers)
	}
	if got := pod.Spec.Containers[0].Command; len(got) != 3 || got[2] != "sleep infinity" {
		t.Errorf("default command is %v", got)
	}
	if n := len(pod.Spec.Volumes); n != 2 {
		t.Fatalf("got %d volumes, want 2", n)
	}
	if got, want := pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName, f.Name("share"); got != want {
		t.Errorf("an already-prefixed claim was rewritten: got %q, want %q", got, want)
	}
	if got, want := pod.Spec.Volumes[1].PersistentVolumeClaim.ClaimName, f.Name("second"); got != want {
		t.Errorf("a logical claim name was not resolved: got %q, want %q", got, want)
	}
	vm := pod.Spec.Containers[0].VolumeMounts
	if len(vm) != 2 || vm[0].MountPath != "/mnt/share" || vm[1].SubPath != "sub" || !vm[1].ReadOnly {
		t.Errorf("unexpected volume mounts: %+v", vm)
	}
	if pod.Labels["role"] != "stranded" || pod.Labels["nfs-verification/case"] != "data-05" {
		t.Errorf("unexpected labels: %v", pod.Labels)
	}
	if pod.Spec.TerminationGracePeriodSeconds == nil || *pod.Spec.TerminationGracePeriodSeconds != 5 {
		t.Errorf("termination grace period did not survive decoding")
	}
}

func TestClientPodManifestWithNoMounts(t *testing.T) {
	f := &Framework{CaseID: "PROV-01"}
	pod, err := f.PodBuilder(PodSpec{Name: "writer"})
	if err != nil {
		t.Fatalf("rendering a pod with no mounts: %v", err)
	}
	if len(pod.Spec.Volumes) != 0 || len(pod.Spec.Containers[0].VolumeMounts) != 0 {
		t.Errorf("empty mount list did not render empty: %+v", pod.Spec)
	}
	if len(pod.Spec.NodeSelector) != 0 {
		t.Errorf("unpinned pod got a node selector: %v", pod.Spec.NodeSelector)
	}
}

func TestNodeAgentManifestRenders(t *testing.T) {
	var ds appsv1.DaemonSet
	err := render("node-agent-daemonset.yaml", map[string]string{
		"Name": "nfs-verification-node-agent", "Namespace": Namespace, "Image": "alpine:3.20",
	}, &ds)
	if err != nil {
		t.Fatalf("rendering the node agent: %v", err)
	}
	spec := ds.Spec.Template.Spec
	if !spec.HostPID || !spec.HostIPC || !spec.HostNetwork {
		t.Error("host namespaces are the point of the agent; one of them did not survive decoding")
	}
	if len(spec.Tolerations) != 1 || spec.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("the agent must tolerate every taint to inspect a drained node: %+v", spec.Tolerations)
	}
	sc := spec.Containers[0].SecurityContext
	if sc == nil || sc.Privileged == nil || !*sc.Privileged {
		t.Error("agent container is not privileged, so /proc/mounts and signals are unavailable")
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].HostPath == nil || spec.Volumes[0].HostPath.Path != "/" {
		t.Errorf("host root mount missing: %+v", spec.Volumes)
	}
}

func TestNameIsIdempotent(t *testing.T) {
	f := &Framework{CaseID: "PROV-01"}
	once := f.Name("prov01")
	if twice := f.Name(once); twice != once {
		t.Errorf("prefixing twice changed the name: %q then %q", once, twice)
	}
}

func TestClientPodManifestIdentity(t *testing.T) {
	f := &Framework{CaseID: "SEC-01"}
	pod, err := f.PodBuilder(PodSpec{
		Name:      "writer",
		Mounts:    []MountSpec{{Claim: "share", Path: "/mnt/share"}},
		RunAsUser: Int64(1234), RunAsGroup: Int64(5678),
	})
	if err != nil {
		t.Fatalf("rendering a pod with an identity: %v", err)
	}
	sc := pod.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || sc.RunAsGroup == nil {
		t.Fatalf("identity did not survive decoding: %+v", sc)
	}
	if *sc.RunAsUser != 1234 || *sc.RunAsGroup != 5678 {
		t.Errorf("pod runs as %d:%d, want 1234:5678", *sc.RunAsUser, *sc.RunAsGroup)
	}
}

func TestClientPodManifestIdentityIsOptional(t *testing.T) {
	f := &Framework{CaseID: "DATA-01"}
	// uid 0 is a value, not an absence: a case that pins root must render a
	// security context, and a case that pins nothing must not.
	pod, err := f.PodBuilder(PodSpec{Name: "writer", RunAsUser: Int64(0)})
	if err != nil {
		t.Fatalf("rendering a pod pinned to root: %v", err)
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsUser == nil ||
		*pod.Spec.SecurityContext.RunAsUser != 0 {
		t.Errorf("uid 0 was dropped as if it were unset: %+v", pod.Spec.SecurityContext)
	}
	if pod.Spec.SecurityContext.RunAsGroup != nil {
		t.Errorf("an unset gid rendered anyway: %+v", pod.Spec.SecurityContext)
	}

	plain, err := f.PodBuilder(PodSpec{Name: "writer"})
	if err != nil {
		t.Fatalf("rendering a pod with no identity: %v", err)
	}
	if plain.Spec.SecurityContext != nil && (plain.Spec.SecurityContext.RunAsUser != nil ||
		plain.Spec.SecurityContext.RunAsGroup != nil) {
		t.Errorf("a pod that pinned no identity got one: %+v", plain.Spec.SecurityContext)
	}
}

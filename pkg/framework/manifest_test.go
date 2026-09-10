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
	f := &Framework{CaseID: "DATA-05", Namespace: "default"}
	pod, err := f.PodBuilder(PodSpec{
		Name:  "holder",
		Image: "alpine:3.20",
		Node:  "worker-1",
		Mounts: []MountSpec{
			{Claim: "nfsv-data05-share", Path: "/mnt/share"},
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
	if pod.Namespace != "default" {
		t.Errorf("namespace %q, want default: the suite creates no namespaces", pod.Namespace)
	}
	if pod.Spec.NodeName != "worker-1" {
		t.Errorf("nodeName %q, want worker-1", pod.Spec.NodeName)
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
	if pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "nfsv-data05-share" {
		t.Errorf("claim name was rewritten: %q", pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
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
	f := &Framework{CaseID: "PROV-01", Namespace: "default"}
	pod, err := f.PodBuilder(PodSpec{Name: "writer"})
	if err != nil {
		t.Fatalf("rendering a pod with no mounts: %v", err)
	}
	if len(pod.Spec.Volumes) != 0 || len(pod.Spec.Containers[0].VolumeMounts) != 0 {
		t.Errorf("empty mount list did not render empty: %+v", pod.Spec)
	}
	if pod.Spec.NodeName != "" {
		t.Errorf("unpinned pod got nodeName %q", pod.Spec.NodeName)
	}
}

func TestNodeAgentManifestRenders(t *testing.T) {
	var ds appsv1.DaemonSet
	err := render("node-agent-daemonset.yaml", map[string]string{
		"Name": "nfs-verification-node-agent", "Namespace": "default", "Image": "alpine:3.20",
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

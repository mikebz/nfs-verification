package framework

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseMounts(t *testing.T) {
	const procMounts = `
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
10.0.0.5:/export/pvc-123 /var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/pvc-123/mount nfs4 rw,relatime,vers=4.1,rsize=1048576,hard,proto=tcp 0 0
`
	lines := ParseMounts(procMounts)
	if len(lines) != 2 {
		t.Fatalf("parsed %d lines, want 2", len(lines))
	}
	nfs := lines[1]
	if nfs.FSType != "nfs4" {
		t.Errorf("fstype %q, want nfs4", nfs.FSType)
	}
	if v, ok := nfs.OptionValue("vers"); !ok || v != "4.1" {
		t.Errorf("vers=%q ok=%t, want 4.1", v, ok)
	}
	if !nfs.HasOption("hard") {
		t.Error("hard option not detected; every chaos assertion depends on a hard mount")
	}
	if nfs.HasOption("soft") {
		t.Error("soft option falsely detected")
	}
}

// The preflight cache is keyed by kubeconfig context, and context names are not
// constrained to anything a filename likes.
func TestPreflightCachePath(t *testing.T) {
	cases := map[string]string{
		"gke-w1":                            "artifacts/preflight-gke-w1.json",
		"gke_my-project_us-central1-a_w1":   "artifacts/preflight-gke_my-project_us-central1-a_w1.json",
		"arn:aws:eks:us-east-1:1234:x/prod": "artifacts/preflight-arn-aws-eks-us-east-1-1234-x-prod.json",
		"":                                  "artifacts/preflight-default.json",
	}
	for context, want := range cases {
		if got := PreflightCache(context); got != want {
			t.Errorf("PreflightCache(%q) = %q, want %q", context, got, want)
		}
	}
}

// A relative artifacts path must resolve to the same directory whether the
// process starts at the repository root (cmd/preflight) or in a test package
// (go test), or the preflight cache is written to one place and read from
// another.
func TestArtifactsDirIsAnchored(t *testing.T) {
	original := cfg.ArtifactsDir
	defer func() { cfg.ArtifactsDir = original }()

	cfg.ArtifactsDir = "artifacts"
	if err := FinalizeFlags(); err != nil {
		t.Fatalf("FinalizeFlags: %v", err)
	}
	if !filepath.IsAbs(cfg.ArtifactsDir) {
		t.Fatalf("artifacts dir %q is still relative; the cache would depend on the working directory", cfg.ArtifactsDir)
	}
	// This test runs in pkg/framework, two levels below the module root, so an
	// anchored path proves the walk upward worked.
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	if want := filepath.Join(root, "artifacts"); cfg.ArtifactsDir != want {
		t.Errorf("artifacts dir is %q, want %q", cfg.ArtifactsDir, want)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Errorf("repoRoot returned %q, which has no go.mod: %v", root, err)
	}
}

// An absolute path is left alone.
func TestArtifactsDirAbsoluteIsKept(t *testing.T) {
	original := cfg.ArtifactsDir
	defer func() { cfg.ArtifactsDir = original }()

	cfg.ArtifactsDir = filepath.Join(t.TempDir(), "elsewhere")
	want := cfg.ArtifactsDir
	if err := FinalizeFlags(); err != nil {
		t.Fatalf("FinalizeFlags: %v", err)
	}
	if cfg.ArtifactsDir != want {
		t.Errorf("absolute artifacts dir was rewritten to %q, want %q", cfg.ArtifactsDir, want)
	}
}

// Teardown must not delete a claim a surviving pod still mounts: destroying an
// export under a live hard mount is what takes a node out of service.
func TestClaimsHeldByStuckPods(t *testing.T) {
	pod := func(name, node string, claims ...string) corev1.Pod {
		p := corev1.Pod{}
		p.Name, p.Spec.NodeName = name, node
		for _, c := range claims {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c},
				},
			})
		}
		return p
	}
	f := &Framework{CaseID: "DATA-05"}
	stuck := []corev1.Pod{
		pod("holder", "node-a", "share"),
		pod("contender", "node-b", "share", "second"),
		pod("no-volumes", "node-a"),
	}
	held := f.claimsHeldBy(stuck)
	if !held["share"] || !held["second"] {
		t.Errorf("claims still mounted were not detected: %v", held)
	}
	if len(held) != 2 {
		t.Errorf("held claims = %v, want exactly share and second", held)
	}
	if got := describePods(stuck); got != "holder on node-a, contender on node-b, no-volumes on node-a" {
		t.Errorf("describePods = %q; an operator needs the pod and the node", got)
	}
}

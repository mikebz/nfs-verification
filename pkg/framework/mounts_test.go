package framework

import "testing"

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

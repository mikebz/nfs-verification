package framework

import "testing"

// Gate selection decides which cases run in each make target, so the inclusion
// rules are worth pinning down.
func TestGateInclusion(t *testing.T) {
	cases := []struct {
		selected Gate
		declared Gate
		want     bool
	}{
		{GatePresubmit, GatePresubmit, true},
		// Presubmit deliberately holds no chaos: chaos is slow and its failures
		// need human triage, and red that people learn to ignore is worse than
		// no signal.
		{GatePresubmit, GateNightly, false},
		{GatePresubmit, GateSoak, false},
		{GateNightly, GatePresubmit, true},
		{GateNightly, GateNightly, true},
		{GateNightly, GateSoak, false},
		{GateNightly, GateManual, false},
		{GateSoak, GateSoak, true},
		{GateSoak, GatePresubmit, false},
		{GateManual, GateManual, true},
		{GateManual, GateNightly, false},
		{GateAll, GateManual, true},
		{GateAll, GateSoak, true},
	}
	for _, tc := range cases {
		if got := tc.selected.Includes(tc.declared); got != tc.want {
			t.Errorf("-gate=%s running %s cases: got %t, want %t", tc.selected, tc.declared, got, tc.want)
		}
	}
}

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

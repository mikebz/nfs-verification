package framework

import (
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// A capability set is a hex mask, and every way of getting it wrong produces a
// number rather than an error. SEC-09's whole finding is a bit that is set in
// one place and clear in another, so the decoding is tested directly.

// TestParseProcStatusAgainstThisProcess parses this machine's own process
// status where there is one, so the parser is checked against the kernel's
// spelling rather than against a fixture.
//
// Steps:
//  1. Read /proc/self/status, skipping where the platform has no procfs.
//  2. Parse it and require the bounding set to be non-empty, which it is for
//     any process that exists.
func TestParseProcStatusAgainstThisProcess(t *testing.T) {
	contents, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("no /proc/self/status on this machine, which is ordinary off Linux: %v", err)
	}
	caps, err := ParseProcStatus(string(contents))
	if err != nil {
		t.Fatalf("parsing this process's status: %v", err)
	}
	if caps.Bounding == 0 {
		t.Errorf("this process decoded with an empty bounding set, which no process has")
	}
}

// TestParseProcStatusDecodesTheServersSet parses the exact status lines the
// server this project runs against reports, and checks the two capabilities its
// pod spec asks for come back set.
//
// The mask is verbatim from the deployment: a file-handle backend needs
// CAP_DAC_READ_SEARCH for open_by_handle_at, and the chart adds CAP_SYS_RESOURCE
// beside it. A policy that stripped either is what SEC-09 exists to report.
//
// Steps:
//  1. Parse the five Cap lines.
//  2. Assert the two declared capabilities are present.
//  3. Assert a capability nothing granted is absent, so the decoder is not
//     simply answering yes.
//  4. Assert the ambient set decoded as empty rather than as unread.
func TestParseProcStatusDecodesTheServersSet(t *testing.T) {
	const status = `Name:	ganesha.nfsd
State:	S (sleeping)
CapInh:	0000000000000000
CapPrm:	00000000a90425ff
CapEff:	00000000a90425ff
CapBnd:	00000000a90425ff
CapAmb:	0000000000000000
`
	caps, err := ParseProcStatus(status)
	if err != nil {
		t.Fatalf("parsing the server's status: %v", err)
	}
	for _, want := range []string{"DAC_READ_SEARCH", "CAP_SYS_RESOURCE"} {
		if !HasCap(caps.Effective, want) {
			t.Errorf("%s is not set in %x, but the server's pod spec adds it", want, caps.Effective)
		}
	}
	// Not granted by this set, and the one a privileged container would have.
	if HasCap(caps.Effective, "SYS_MODULE") {
		t.Errorf("SYS_MODULE decoded as set in %x, so the decoder is answering yes to everything",
			caps.Effective)
	}
	if caps.Ambient != 0 {
		t.Errorf("ambient set decoded as %x, want empty", caps.Ambient)
	}
	names := CapNames(caps.Effective)
	if len(names) == 0 {
		t.Fatalf("a non-empty mask rendered as no names")
	}
	if HasCap(caps.Effective, "NOT_A_CAPABILITY") {
		t.Errorf("an unknown capability name was reported as present")
	}
}

// TestParseProcStatusRejectsAFileWithNoCapLines covers the difference between a
// process with no capabilities and a read that went wrong. Both are a struct of
// zeroes, and reporting the second as the first would file a harness failure as
// a deployment finding.
//
// Steps:
//  1. Parse a status file with no Cap lines, and a mask that is not hex.
//  2. Require an error from each.
func TestParseProcStatusRejectsAFileWithNoCapLines(t *testing.T) {
	if caps, err := ParseProcStatus("Name:\tganesha.nfsd\nState:\tS\n"); err == nil {
		t.Errorf("a status file with no capability lines parsed as %+v", caps)
	}
	if caps, err := ParseProcStatus("CapEff:\tnot-a-mask\n"); err == nil {
		t.Errorf("a mask that is not hex parsed as %+v", caps)
	}
}

// TestDeclaredCapsOfReadsTheSpec checks what the case compares the runtime set
// against.
//
// Steps:
//  1. Read a container declaring two adds.
//  2. Read one declaring privileged.
//  3. Read one declaring no security context at all.
func TestDeclaredCapsOfReadsTheSpec(t *testing.T) {
	add := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "nfs-server-provisioner",
		SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
			Add:  []corev1.Capability{"DAC_READ_SEARCH", "SYS_RESOURCE"},
			Drop: []corev1.Capability{"ALL"},
		}},
	}}}}
	got := DeclaredCapsOf(add)
	if len(got.Add) != 2 || got.Add[0] != "DAC_READ_SEARCH" || len(got.Drop) != 1 {
		t.Errorf("declared capabilities read as %+v", got)
	}
	if got.Privileged {
		t.Errorf("a container that is not privileged read as privileged")
	}
	if got.Container != "nfs-server-provisioner" {
		t.Errorf("container name read as %q", got.Container)
	}

	yes := true
	priv := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "server", SecurityContext: &corev1.SecurityContext{Privileged: &yes},
	}}}}
	if !DeclaredCapsOf(priv).Privileged {
		t.Errorf("a privileged container did not read as one")
	}

	bare := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server"}}}}
	if d := DeclaredCapsOf(bare); d.Privileged || len(d.Add) != 0 {
		t.Errorf("a container with no security context read as %+v", d)
	}
}

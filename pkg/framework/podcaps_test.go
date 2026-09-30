package framework

import (
	"os"
	"os/exec"
	"path/filepath"
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
// against, and that it is read from the container the server runs in rather
// than whichever container is listed first.
//
// Steps:
//  1. Read a container declaring two adds, listed second behind a sidecar.
//  2. Read one declaring privileged.
//  3. Read one declaring no security context at all.
//  4. Ask for a container the spec does not have, and require an error rather
//     than an empty declaration SEC-09 would pass on.
func TestDeclaredCapsOfReadsTheSpec(t *testing.T) {
	add := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "sidecar"},
		{
			Name: "nfs-server-provisioner",
			SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
				Add:  []corev1.Capability{"DAC_READ_SEARCH", "SYS_RESOURCE"},
				Drop: []corev1.Capability{"ALL"},
			}},
		},
	}}}
	got, err := DeclaredCapsOf(add, "nfs-server-provisioner")
	if err != nil {
		t.Fatalf("reading the server container: %v", err)
	}
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
	if d, err := DeclaredCapsOf(priv, "server"); err != nil || !d.Privileged {
		t.Errorf("a privileged container read as %+v, %v", d, err)
	}

	bare := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server"}}}}
	if d, err := DeclaredCapsOf(bare, "server"); err != nil || d.Privileged || len(d.Add) != 0 {
		t.Errorf("a container with no security context read as %+v, %v", d, err)
	}

	if d, err := DeclaredCapsOf(add, "not-there"); err == nil {
		t.Errorf("a container the spec does not have read as %+v rather than an error", d)
	}
}

// The masks and cgroup paths below are verbatim from the two reference
// deployments on 2026-09-30, read through the node agent. They are the reason
// SEC-09 reads two processes: on gke-w2 the server holds less than its PID 1.
const (
	// maskFull is nfs-provisioner's set on both deployments, and ganesha.nfsd's
	// on gke-w1: the runtime defaults plus DAC_READ_SEARCH and SYS_RESOURCE.
	maskFull = 0xa90425ff
	// maskNoSysResource is ganesha.nfsd's permitted and effective set on
	// gke-w2, after it lowers CAP_SYS_RESOURCE at start.
	maskNoSysResource = 0xa80425ff
	w2ServerCgroup    = "0::/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod3adc37e4_63c0_42af_82b9_69f90208510a.slice/cri-containerd-c43cce061e18f92cc145f6e5f63de001ffec4060283c17e44f62e42e7629b1bd.scope"
	w2ServerID        = "c43cce061e18f92cc145f6e5f63de001ffec4060283c17e44f62e42e7629b1bd"
)

// TestCapGapsAttributesEachMissingCapability exists because getting this wrong
// files a server's own hardening as a platform defect, or a platform defect as
// the server's choice, and neither produces an error: only a verdict pointing at
// the wrong people.
//
// Steps:
//  1. gke-w1: PID 1 and the server hold everything declared. Nothing is missing.
//  2. gke-w2: PID 1 holds SYS_RESOURCE and the server does not. It is dropped,
//     not stripped, so SEC-09 records it rather than failing.
//  3. A platform that never delivered DAC_READ_SEARCH: stripped, whatever the
//     server holds.
//  4. A server that lowered a capability from its effective set only: it can
//     raise it again, so it is neither.
func TestCapGapsAttributesEachMissingCapability(t *testing.T) {
	declared := []string{"DAC_READ_SEARCH", "SYS_RESOURCE"}
	full := CapSet{Permitted: maskFull, Effective: maskFull, Bounding: maskFull}
	w2Server := CapSet{Permitted: maskNoSysResource, Effective: maskNoSysResource, Bounding: maskFull}

	if s, d := CapGaps(declared, full, full); len(s) != 0 || len(d) != 0 {
		t.Errorf("gke-w1 read as stripped %v, dropped %v; the server holds everything declared", s, d)
	}

	s, d := CapGaps(declared, full, w2Server)
	if len(s) != 0 {
		t.Errorf("gke-w2 read as stripped %v, but PID 1 holds both, so the platform delivered them", s)
	}
	if len(d) != 1 || d[0] != "SYS_RESOURCE" {
		t.Errorf("gke-w2 read as dropped %v, want [SYS_RESOURCE]", d)
	}

	var noDRS uint64 = maskFull &^ (1 << 2) // DAC_READ_SEARCH is bit 2
	stripped := CapSet{Permitted: noDRS, Effective: noDRS, Bounding: noDRS}
	s, d = CapGaps(declared, stripped, stripped)
	if len(s) != 1 || s[0] != "DAC_READ_SEARCH" || len(d) != 0 {
		t.Errorf("a platform that never delivered DAC_READ_SEARCH read as stripped %v, dropped %v", s, d)
	}

	lowered := CapSet{Permitted: maskFull, Effective: maskNoSysResource, Bounding: maskFull}
	if s, d := CapGaps(declared, full, lowered); len(s) != 0 || len(d) != 0 {
		t.Errorf("a server holding SYS_RESOURCE permitted but not effective read as stripped %v, dropped %v; "+
			"it can raise it again", s, d)
	}
}

// TestProcStatusScriptReadsOneProcess runs the shipped reader under a real shell
// against a fixture proc tree, so a quoting slip fails here rather than inside
// SEC-09.
//
// Steps:
//  1. Build a proc tree holding gke-w2's ganesha.nfsd: status and cgroup.
//  2. Run the script over it and parse what it printed.
//  3. Require the name, the permitted set and the container to come back.
//  4. Run it for a pid that does not exist and for one that is not a number,
//     and require an error from each rather than an empty process.
func TestProcStatusScriptReadsOneProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proc")
	dir := filepath.Join(root, "2253488")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	status := "Name:\tganesha.nfsd\nUmask:\t0022\nState:\tS (sleeping)\nPPid:\t2253462\n" +
		"CapInh:\t0000000000000000\nCapPrm:\t00000000a80425ff\nCapEff:\t00000000a80425ff\n" +
		"CapBnd:\t00000000a90425ff\nCapAmb:\t0000000000000000\n"
	for name, body := range map[string]string{"status": status, "cgroup": w2ServerCgroup + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	script := materializeScript(t, "proc-status.sh")

	out, err := exec.Command("sh", script, root, "2253488").CombinedOutput()
	if err != nil {
		t.Fatalf("running the reader: %v: %s", err, out)
	}
	read, err := parseProcRead(string(out))
	if err != nil {
		t.Fatalf("parsing what the reader printed: %v\n%s", err, out)
	}
	if read.name != "ganesha.nfsd" {
		t.Errorf("name read as %q, want ganesha.nfsd", read.name)
	}
	if read.caps.Permitted != maskNoSysResource || read.caps.Bounding != maskFull {
		t.Errorf("permitted %x bounding %x, want %x and %x", read.caps.Permitted, read.caps.Bounding,
			maskNoSysResource, maskFull)
	}
	statuses := []corev1.ContainerStatus{{Name: "nfs-server-provisioner", ContainerID: "containerd://" + w2ServerID}}
	if c, err := containerOf(read.cgroup, statuses); err != nil || c != "nfs-server-provisioner" {
		t.Errorf("container read as %q, %v", c, err)
	}

	for _, pid := range []string{"99999", "1;true"} {
		out, err := exec.Command("sh", script, root, pid).CombinedOutput()
		if err != nil {
			t.Fatalf("running the reader for %q: %v: %s", pid, err, out)
		}
		if read, err := parseProcRead(string(out)); err == nil {
			t.Errorf("pid %q read as %+v rather than an error\n%s", pid, read, out)
		}
	}
}

// TestParseProcReadRefusesAShortRead covers an exec that returned success with
// part of the output, F-011's shape. Read as a process holding nothing, it would
// report every declared capability as stripped by the platform.
//
// Steps:
//  1. Parse output with no end marker, output with no cgroup section, and
//     output whose status carries no Name line.
//  2. Require an error from each.
func TestParseProcReadRefusesAShortRead(t *testing.T) {
	status := "==STATUS\nName:\tganesha.nfsd\nCapPrm:\t00000000a80425ff\n==ENDSTATUS\n"
	cgroup := "==CGROUP\n" + w2ServerCgroup + "\n==ENDCGROUP\n"
	for name, out := range map[string]string{
		"no end marker": status + cgroup,
		"no cgroup":     status + "==END\n",
		"no name":       "==STATUS\nCapPrm:\t00000000a80425ff\n==ENDSTATUS\n" + cgroup + "==END\n",
	} {
		if read, err := parseProcRead(out); err == nil {
			t.Errorf("%s: parsed as %+v rather than an error", name, read)
		}
	}
	if _, err := parseProcRead(status + cgroup + "==END\n"); err != nil {
		t.Errorf("a complete read was refused: %v", err)
	}
}

// TestServerIdentityChecks covers the two checks that stop SEC-09 reading
// somebody else's capabilities when the server restarts between being named
// and being read, and its old pid goes to another process.
//
// Steps:
//  1. The name: exact, a comm the kernel truncated at 15 characters, and a
//     different process.
//  2. The container: gke-w2's cgroup against its own container, against a pod
//     whose containers it does not name, and with a container that has no id
//     yet, which must not match everything.
func TestServerIdentityChecks(t *testing.T) {
	if err := sameProcessName("ganesha.nfsd", "ganesha.nfsd"); err != nil {
		t.Errorf("the same name was refused: %v", err)
	}
	if err := sameProcessName("a-very-long-ser", "a-very-long-server-name"); err != nil {
		t.Errorf("a truncated comm was refused: %v", err)
	}
	for _, other := range []string{"sh", "ganesha", "nfs-provisioner"} {
		if err := sameProcessName(other, "ganesha.nfsd"); err == nil {
			t.Errorf("%q was accepted as ganesha.nfsd", other)
		}
	}

	own := []corev1.ContainerStatus{
		{Name: "starting", ContainerID: ""},
		{Name: "nfs-server-provisioner", ContainerID: "containerd://" + w2ServerID},
	}
	if c, err := containerOf(w2ServerCgroup, own); err != nil || c != "nfs-server-provisioner" {
		t.Errorf("gke-w2's server read as container %q, %v", c, err)
	}
	other := []corev1.ContainerStatus{
		{Name: "starting", ContainerID: ""},
		{Name: "nfs-server-provisioner", ContainerID: "containerd://0ba97a393ecf31ccc79516969e55005337fcf9f53bbfbb5dad93f658a5aaa3cc"},
	}
	if c, err := containerOf(w2ServerCgroup, other); err == nil {
		t.Errorf("a process outside the pod read as container %q", c)
	}
}

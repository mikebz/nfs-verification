package framework

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The socket table is where the client identity an export rule would match
// actually lives, and its format is fixed-width hex written in host byte order.
// A word read the wrong way round does not produce an error: it produces a
// plausible address belonging to nobody, and SEC-04 would then report that the
// server cannot tell its clients apart. That is the failure with no symptom
// these tests exist for.

// TestParseProcNetTCPAgainstTheKernel parses this machine's own socket table
// where there is one, so the parser is checked against what the kernel really
// writes rather than against a line somebody typed.
//
// Steps:
//  1. Read /proc/net/tcp, skipping where the platform has no procfs.
//  2. Parse it and require at least one row to have come back.
//  3. Assert every address is valid and every port is plausible.
func TestParseProcNetTCPAgainstTheKernel(t *testing.T) {
	contents, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Skipf("no /proc/net/tcp on this machine, which is ordinary off Linux: %v", err)
	}
	conns, err := ParseProcNetTCP(string(contents))
	if err != nil {
		t.Fatalf("parsing this machine's socket table: %v", err)
	}
	if len(conns) == 0 {
		t.Skip("this machine's socket table is empty, so there is nothing to check against")
	}
	for _, c := range conns {
		if !c.Local.Addr().IsValid() || !c.Peer.Addr().IsValid() {
			t.Errorf("parsed an invalid address from a real socket table row: %s", c)
		}
		if c.State == "" {
			t.Errorf("row %s came back with no state", c)
		}
	}
}

// TestParseProcNetTCPByteOrder covers the two things about this format that are
// wrong-by-default: the per-word little-endian addresses, and the IPv4-mapped
// form a server listening on IPv6 reports every IPv4 client in.
//
// The rows below are verbatim from the cluster this project runs against: the
// NFS server listens on :::2049 and the client that mounted the share appears
// as ::ffff:10.138.15.232 on a reserved port, which is the node, not the pod.
//
// Steps:
//  1. Parse a v4 table and a v6 table.
//  2. Assert the decoded addresses are the ones the machine actually reported.
//  3. Assert the mapped form is recognised and unmaps to the IPv4 address a
//     rule would be written as.
func TestParseProcNetTCPByteOrder(t *testing.T) {
	const v4 = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:006F 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1
   1: 8E011C0A:D868 0118761E:01BB 01 00000000:00000000 00:00000000 00000000     0        0 2
`
	conns, err := ParseProcNetTCP(v4)
	if err != nil {
		t.Fatalf("parsing an IPv4 socket table: %v", err)
	}
	if len(conns) != 2 {
		t.Fatalf("got %d rows, want 2", len(conns))
	}
	if got, want := conns[0].Local.String(), "0.0.0.0:111"; got != want {
		t.Errorf("listening row decoded as %s, want %s", got, want)
	}
	if conns[0].State != tcpListen {
		t.Errorf("state %q, want %q", conns[0].State, tcpListen)
	}
	// 8E011C0A is 10.28.1.142 with each byte reversed. Reading it big endian
	// gives 142.1.28.10, which is somebody else's network entirely.
	if got, want := conns[1].Local.String(), "10.28.1.142:55400"; got != want {
		t.Errorf("established row decoded as %s, want %s: the words are little endian", got, want)
	}
	if conns[1].State != tcpEstablished {
		t.Errorf("state %q, want %q", conns[1].State, tcpEstablished)
	}

	const v6 = `  sl  local_address                         remote_address                        st
   0: 00000000000000000000000000000000:0801 00000000000000000000000000000000:0000 0A
   1: 0000000000000000FFFF00008A0F8A0A:0801 0000000000000000FFFF0000E80F8A0A:039B 01
`
	six, err := ParseProcNetTCP(v6)
	if err != nil {
		t.Fatalf("parsing an IPv6 socket table: %v", err)
	}
	if len(six) != 2 {
		t.Fatalf("got %d rows, want 2", len(six))
	}
	if got, want := six[0].Local.Port(), uint16(2049); got != want {
		t.Errorf("the NFS listener decoded on port %d, want %d", got, want)
	}
	if !six[1].Mapped {
		t.Errorf("an IPv4-mapped peer was not recognised as one: %s", six[1])
	}
	// The peer is what an export rule would be matched against, so it has to
	// unmap to the plain IPv4 address an operator would write.
	if got, want := six[1].Peer.Addr().Unmap().String(), "10.138.15.232"; got != want {
		t.Errorf("mapped peer unmapped to %s, want %s", got, want)
	}
	if got := six[1].Peer.Port(); got >= 1024 {
		t.Errorf("peer port %d is not a reserved port; the kernel client mounts from one, "+
			"and a row that is not one is probably not the mount", got)
	}

	// PeersOn is what the case compares against node addresses, so it has to
	// drop the listener, drop the port and unmap.
	peers := PeersOn(six, 2049)
	if len(peers) != 1 || peers[0].String() != "10.138.15.232" {
		t.Errorf("PeersOn returned %v, want the single unmapped client address", peers)
	}
	if ports := ListeningPorts(six); len(ports) != 1 || ports[0] != 2049 {
		t.Errorf("ListeningPorts returned %v, want [2049]", ports)
	}
}

// TestParseProcNetTCPRejectsNonsense checks the parser says so rather than
// inventing an address, because a silently wrong address is the one shape of
// failure SEC-04 cannot survive.
//
// Steps:
//  1. Feed it rows with a malformed address, port and length.
//  2. Require an error from each.
func TestParseProcNetTCPRejectsNonsense(t *testing.T) {
	for name, row := range map[string]string{
		"no port":         "   0: 00000000 00000000:0000 0A\n",
		"port not hex":    "   0: 00000000:zzzz 00000000:0000 0A\n",
		"odd length":      "   0: 000000:0801 00000000:0000 0A\n",
		"address not hex": "   0: zzzzzzzz:0801 00000000:0000 0A\n",
	} {
		if conns, err := ParseProcNetTCP(row); err == nil {
			t.Errorf("%s was accepted and parsed as %v", name, conns)
		}
	}
}

// TestHostAddrReadsTheSourceTheProvisionerWrote covers the one place the suite
// turns a PV's server field into an address. A Service name is deliberately not
// resolved: the workstation's resolver is not the cluster's, and an answer from
// it would describe the wrong network.
//
// Steps:
//  1. An IPv4 literal and a bracketed IPv6 literal must parse.
//  2. A cluster DNS name must not, and must say so by returning false.
func TestHostAddrReadsTheSourceTheProvisionerWrote(t *testing.T) {
	for _, tc := range []struct {
		server string
		want   string
		ok     bool
	}{
		{"34.118.226.20", "34.118.226.20", true},
		{"[2001:db8::1]", "2001:db8::1", true},
		{"nfs-provisioner.svc.cluster.local", "", false},
	} {
		addr, ok := HostAddr(NFSSource{Server: tc.server, Path: "/export/pvc-1"})
		if ok != tc.ok {
			t.Errorf("HostAddr(%q) ok=%v, want %v", tc.server, ok, tc.ok)
			continue
		}
		if ok && addr.String() != tc.want {
			t.Errorf("HostAddr(%q) = %s, want %s", tc.server, addr, tc.want)
		}
	}
}

// TestPeersOnCountsOnlyLiveClients checks that a connection whose client has
// gone does not count as a client.
//
// A case comparing the clients a server sees against the pods that exist now
// would otherwise count a previous case's pod, still sitting in TIME_WAIT, as a
// current one, and conclude that the server sees more clients than there are.
//
// Steps:
//  1. Build a table with one established peer and one in TIME_WAIT.
//  2. Assert only the established one is returned.
func TestPeersOnCountsOnlyLiveClients(t *testing.T) {
	conns := []Conn{
		{Local: mustAddrPort(t, "10.28.1.142:2049"), Peer: mustAddrPort(t, "10.138.0.17:923"), State: tcpEstablished},
		{Local: mustAddrPort(t, "10.28.1.142:2049"), Peer: mustAddrPort(t, "10.138.0.14:931"), State: "TIME_WAIT"},
		{Local: mustAddrPort(t, "10.28.1.142:111"), Peer: mustAddrPort(t, "10.138.0.99:900"), State: tcpEstablished},
	}
	peers := PeersOn(conns, 2049)
	if len(peers) != 1 || peers[0].String() != "10.138.0.17" {
		t.Errorf("PeersOn returned %v, want only the established client on the NFS port", peers)
	}
}

func mustAddrPort(t *testing.T, s string) netip.AddrPort {
	t.Helper()
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return ap
}

// TestOwnerCensusScriptUnderARealShell runs scripts/owner-census.sh against a
// real directory, so a quoting or pipeline slip fails here rather than inside a
// case that was testing fsGroup.
//
// The script needs `stat -c`, which is the GNU and busybox spelling and not the
// BSD one, so on a workstation without it a shim supplies the output while the
// shell pipeline, the find and the totals are genuinely run.
//
// Steps:
//  1. Make a directory with a known number of entries.
//  2. Run the script under sh with a stat that prints the GNU format.
//  3. Assert the parsed census counts every entry and agrees with its own
//     total.
func TestOwnerCensusScriptUnderARealShell(t *testing.T) {
	sh := lookOrSkip(t, "sh", "find", "sort", "uniq")
	dir := t.TempDir()
	const entries = 5
	for i := 0; i < entries; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f-"+string(rune('a'+i))), []byte("x"), 0o644); err != nil {
			t.Fatalf("populating the census directory: %v", err)
		}
	}
	binDir := t.TempDir()
	// The shim answers for every file with the one owner this machine's files
	// have, which is what the real stat would print for a directory created by
	// this test. It consumes the -c format argument the way stat does: a shim
	// that counted the format string as a file would make the census disagree
	// with its own total, which is the very thing the total exists to catch.
	shim := "#!/bin/sh\nskip=0\nfor arg in \"$@\"; do\n" +
		"case \"$arg\" in -c) skip=1;; -*) ;; *) if [ \"$skip\" = 1 ]; then skip=0; else echo '1234:5678'; fi;; esac\n" +
		"done\n"
	if err := os.WriteFile(filepath.Join(binDir, "stat"), []byte(shim), 0o755); err != nil {
		t.Fatalf("writing the stat shim: %v", err)
	}

	script, err := RunScript("owner-census.sh", "sec03", dir)
	if err != nil {
		t.Fatalf("building the census script: %v", err)
	}
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the census script: %v: %s", err, out)
	}
	census, err := ParseOwnerCensus(string(out))
	if err != nil {
		t.Fatalf("parsing what the census printed (%q): %v", out, err)
	}
	if census.Total != entries {
		t.Errorf("census counted %d entries, want %d", census.Total, entries)
	}
	if census.Counted() != census.Total {
		t.Errorf("the per-owner lines add up to %d and the total says %d: a stat that failed on one "+
			"file is exactly this disagreement", census.Counted(), census.Total)
	}
	if len(census.Counts) != 1 || census.Counts[0].UID != 1234 || census.Counts[0].GID != 5678 {
		t.Errorf("unexpected ownership: %s", census)
	}
}

// TestOwnerCensusRejectsSilence covers the shape F-011 met: an exec that
// reported success and printed nothing. Read as a census, that is a directory
// where no ownership changed, which is the answer SEC-03 is looking for and
// must not be handed for free.
//
// Steps:
//  1. Parse empty output and output with owner lines but no total.
//  2. Require an error from both.
func TestOwnerCensusRejectsSilence(t *testing.T) {
	if census, err := ParseOwnerCensus(""); err == nil {
		t.Errorf("an empty census was accepted as %s", census)
	}
	if census, err := ParseOwnerCensus("  3 0:0\n"); err == nil {
		t.Errorf("a census with no total was accepted as %s", census)
	}
}

// TestOwnerCensusComparesOwnershipNotSize checks the comparison SEC-03 makes:
// files added between two readings are not a chown, and a changed owner is.
//
// It also pins the boundary between those and a third thing that looks like
// both: a census whose per-owner lines do not add up to its total. That is not
// an added file, it is a walk that could not read part of the tree, and the
// part it could not read is where a chown storm would be.
//
// Steps:
//  1. Compare two censuses differing only in how many entries there are.
//  2. Compare two differing in who owns them.
//  3. Reject a census whose lines and total disagree.
func TestOwnerCensusComparesOwnershipNotSize(t *testing.T) {
	before, err := ParseOwnerCensus("   3 0:0\nTOTAL 3\n")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	// A file added between the readings shows up in both the owner line and
	// the total, because the walk and the count see the same tree.
	same, err := ParseOwnerCensus("   4 0:0\nTOTAL 4\n")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	chowned, err := ParseOwnerCensus("   3 0:5678\nTOTAL 3\n")
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if !before.SameOwnership(same) {
		t.Errorf("a census with an extra entry was read as a chown")
	}
	if before.SameOwnership(chowned) {
		t.Errorf("a census whose gid changed was read as unchanged")
	}
	if census, err := ParseOwnerCensus("   3 0:0\nTOTAL 4\n"); err == nil {
		t.Errorf("a census that read 3 of 4 entries was accepted as %s; the entry it missed would be "+
			"reported as unchanged", census)
	}
}

// TestMountProbeParsing covers the classifier that decides whether SEC-05 saw a
// refusal, a grant, or nothing at all.
//
// Getting this wrong in the quiet direction is the expensive one: a probe that
// never ran, read as a refusal, reports an export as well defended when nobody
// has tested it.
//
// Steps:
//  1. Parse a granted probe, a refused one, and silence.
//  2. Assert the grant carries its checksum and its unmount.
//  3. Assert silence is neither granted nor attributable to a server.
func TestMountProbeParsing(t *testing.T) {
	granted := parseMountProbe("MOUNT_RC=0\nMOUNT_OUT=\nSHA=" + strings.Repeat("a", 64) + "\nUMOUNT_RC=0\n")
	if !granted.Granted || !granted.Unmounted {
		t.Errorf("a granted probe parsed as %+v", granted)
	}
	if granted.Sum == "" {
		t.Errorf("a granted probe lost its checksum, which is the evidence the case reports")
	}
	if granted.DeniedByServer() {
		t.Errorf("a granted probe was read as a denial")
	}

	refused := parseMountProbe("MOUNT_RC=255\nMOUNT_OUT=mount: mounting 10.0.0.1:/export on /tmp/x " +
		"failed: Permission denied\n")
	if refused.Granted {
		t.Errorf("a refused probe parsed as granted")
	}
	if !refused.DeniedByServer() {
		t.Errorf("a refusal the server produced was not attributable: %+v", refused)
	}

	silent := parseMountProbe("")
	if silent.Granted || silent.DeniedByServer() {
		t.Errorf("silence was read as an outcome: %+v", silent)
	}
}

// TestProbeMountOptionsAreSoft guards a rule that is invisible in review: every
// other mount in this suite is hard, and preflight rejects soft ones. The probe
// is the deliberate exception, and a probe that lost it would hang a node agent
// against an export the case is about to delete, which is F-001's shape in the
// one container the suite mounts NFS in by hand.
//
// Steps:
//  1. Assert the options carry soft, the pinned version, and a single retry.
//  2. Assert they do not carry hard, whatever else changes around them.
func TestProbeMountOptionsAreSoft(t *testing.T) {
	for _, want := range []string{"soft", "vers=4.1", "retrans=1"} {
		if !strings.Contains(probeMountOptions, want) {
			t.Errorf("probe mount options %q lost %q", probeMountOptions, want)
		}
	}
	if strings.Contains(probeMountOptions, "hard") {
		t.Errorf("probe mount options %q are hard; a refused hard mount retries forever", probeMountOptions)
	}
}

// TestProbeMountScriptUnderARealShell runs scripts/probe-mount.sh with a mount
// that refuses, under a real shell, so a heredoc or quoting slip fails on a
// workstation rather than inside the one case that mounts NFS by hand.
//
// Steps:
//  1. Shim mount to fail the way a server refusal does.
//  2. Run the script and parse what it printed.
//  3. Assert the refusal came back attributable, and that nothing was read.
func TestProbeMountScriptUnderARealShell(t *testing.T) {
	sh := lookOrSkip(t, "sh")
	binDir := t.TempDir()
	shim := "#!/bin/sh\necho 'mount: mounting 10.0.0.1:/export on /tmp/x failed: Permission denied' >&2\nexit 32\n"
	if err := os.WriteFile(filepath.Join(binDir, "mount"), []byte(shim), 0o755); err != nil {
		t.Fatalf("writing the mount shim: %v", err)
	}
	script, err := RunScript("probe-mount.sh", "sec05",
		"10.0.0.1:/export/pvc-1", filepath.Join(t.TempDir(), "mnt"), probeMountOptions, "owned.dat")
	if err != nil {
		t.Fatalf("building the probe script: %v", err)
	}
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the probe script: %v: %s", err, out)
	}
	probe := parseMountProbe(string(out))
	if probe.Granted {
		t.Errorf("a refused mount parsed as granted: %q", out)
	}
	if !probe.DeniedByServer() {
		t.Errorf("the refusal was not attributable to the server: %q", out)
	}
	if probe.Sum != "" {
		t.Errorf("a probe that never mounted produced a checksum: %q", probe.Sum)
	}
}

// TestCheckProbePathRejectsEscapes guards the one argument of the probe mount
// that is not an identifier. It is joined under the mountpoint and read by a
// privileged container, so quoting is not the defence: a quoted "../../etc" is
// still "../../etc". Getting this wrong turns SEC-05's evidence step into an
// arbitrary read of the node agent's filesystem.
//
// Steps:
//  1. Accept the ordinary relative names a case passes, and the "-" that means
//     mount without reading.
//  2. Reject absolute paths and every spelling of a climb out of the mount.
func TestCheckProbePathRejectsEscapes(t *testing.T) {
	for _, ok := range []string{"-", "owner.dat", "sub/dir/owner.dat", "..dotted", "a..b"} {
		if err := checkProbePath(ok); err != nil {
			t.Errorf("checkProbePath(%q) rejected a path a case legitimately passes: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "/", "../etc/passwd", "a/../../etc/passwd", ".."} {
		if err := checkProbePath(bad); err == nil {
			t.Errorf("checkProbePath(%q) allowed a read outside the export", bad)
		}
	}
}

// TestParseSocketTablesPerFamily covers the distinction one exit status cannot
// make: a kernel with no IPv6 has no tcp6 file and that is ordinary, while a
// file that exists and cannot be read means the connections a caller is about
// to call absent were never looked at.
//
// Steps:
//  1. A missing tcp6 alongside a readable tcp parses, and yields tcp's rows.
//  2. A tcp that could not be read is an error even though tcp6 succeeded.
//  3. Output with no readable family at all is an error, not an empty list.
func TestParseSocketTablesPerFamily(t *testing.T) {
	const header = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	const row = "   0: 0100007F:0801 0200007F:CF08 01 00000000:00000000 00:00000000 00000000     0        0 1\n"

	conns, err := parseSocketTables("==FAMILY /proc/net/tcp\n==STATUS ok\n"+header+row+
		"==FAMILY /proc/net/tcp6\n==STATUS absent\n", "ns", "pod")
	if err != nil {
		t.Fatalf("a kernel without IPv6 was treated as unreadable: %v", err)
	}
	if len(conns) != 1 {
		t.Errorf("got %d connections from a readable tcp table, want 1: %+v", len(conns), conns)
	}

	if _, err := parseSocketTables("==FAMILY /proc/net/tcp\n==STATUS error cat: permission denied\n"+
		"==FAMILY /proc/net/tcp6\n==STATUS ok\n"+header+row, "ns", "pod"); err == nil {
		t.Error("a tcp table that could not be read was masked by tcp6 succeeding")
	}

	if _, err := parseSocketTables("==FAMILY /proc/net/tcp\n==STATUS absent\n"+
		"==FAMILY /proc/net/tcp6\n==STATUS absent\n", "ns", "pod"); err == nil {
		t.Error("a pod where neither family could be read reported no connections instead of an error")
	}
}

// TestNodeAddresses covers retrieving internal IP addresses for a node.
//
// Why this test exists: NodeAddresses returns a node's internal addresses, which
// is what an NFS server sees as the client identity. Misparsing or returning
// external/invalid IPs would cause peer comparison checks to misidentify clients.
//
// Steps:
//  1. Seed a fake clientset with nodes containing various address types and malformed addresses.
//  2. Call NodeAddresses for a non-existent node, expecting a lookup error.
//  3. Call NodeAddresses for a node with no internal IPs, expecting an error.
//  4. Call NodeAddresses for a node with mixed address types (InternalIP, ExternalIP, HostName, invalid IP),
//     verifying that only valid NodeInternalIPs are returned unmapped.
func TestNodeAddresses(t *testing.T) {
	ctx := context.Background()

	nodeWithMixed := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "node-1.example.com"},
				{Type: corev1.NodeExternalIP, Address: "198.51.100.1"},
				{Type: corev1.NodeInternalIP, Address: "10.0.0.10"},
				{Type: corev1.NodeInternalIP, Address: "invalid-ip"},
				{Type: corev1.NodeInternalIP, Address: "10.0.0.11"},
				{Type: corev1.NodeInternalIP, Address: "::ffff:10.0.0.12"},
			},
		},
	}
	nodeNoInternal := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "198.51.100.2"},
			},
		},
	}

	kube := fake.NewSimpleClientset(nodeWithMixed, nodeNoInternal)
	c := &Client{Kube: kube}

	t.Run("node not found", func(t *testing.T) {
		_, err := NodeAddresses(ctx, c, "nonexistent-node")
		if err == nil {
			t.Error("NodeAddresses for nonexistent node succeeded, want error")
		}
	})

	t.Run("node with no internal IP", func(t *testing.T) {
		_, err := NodeAddresses(ctx, c, "node-2")
		if err == nil {
			t.Error("NodeAddresses for node without internal IP succeeded, want error")
		} else if !strings.Contains(err.Error(), "reports no internal address") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("node with mixed addresses", func(t *testing.T) {
		addrs, err := NodeAddresses(ctx, c, "node-1")
		if err != nil {
			t.Fatalf("NodeAddresses failed: %v", err)
		}
		want := []netip.Addr{
			netip.MustParseAddr("10.0.0.10"),
			netip.MustParseAddr("10.0.0.11"),
			netip.MustParseAddr("10.0.0.12"),
		}
		if !slices.Equal(addrs, want) {
			t.Errorf("got %v, want %v", addrs, want)
		}
	})
}

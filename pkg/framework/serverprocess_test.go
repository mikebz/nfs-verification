package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Naming the process that serves NFS is the input to a SIGKILL delivered by
// name across a whole node. A wrong name here does not fail a test, it kills
// something unrelated on somebody's cluster, so the join between the listening
// socket and the process holding it is tested from both ends: the decision on
// tables copied from a real server, and the reader itself under a real shell.

// The tables below are copied from the reference deployment, Ganesha V15.3-mb
// on gke-w2, and not written by hand. The detail that matters is invisible in a
// fixture somebody types: the NFS port appears in tcp6 and not in tcp at all.
// A reader of /proc/net/tcp alone finds rpcbind and statd on this server and
// concludes nothing is serving NFS.
const (
	realProcNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:006F 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8362798 1 0000000000000000 100 0 0 10 0
   1: 00000000:0296 00000000:0000 0A 00000000:00000000 00:00000000 00000000    29        0 8362868 1 0000000000000000 100 0 0 10 0
   2: B502140A:E7BC 01E07622:01BB 01 00000000:00000000 02:00000168 00000000     0        0 8457761 2 0000000000000000 20 4 28 10 -1
`
	realProcNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:2573 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8470532 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:4E50 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8470547 1 0000000000000000 100 0 0 10 0
   2: 00000000000000000000000000000000:0801 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8470545 1 0000000000000000 100 0 0 10 0
   3: 00000000000000000000000000000000:8023 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8470548 1 0000000000000000 100 0 0 10 0
   4: 00000000000000000000000000000000:006F 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8362801 1 0000000000000000 100 0 0 10 0
   5: 00000000000000000000000000000000:036B 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8470550 1 0000000000000000 100 0 0 10 0
   6: 00000000000000000000000000000000:0296 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000    29        0 8363377 1 0000000000000000 100 0 0 10 0
`
)

// readerOutput assembles what scripts/nfs-listener.sh prints. The decision
// tests build it directly so they can pose situations a healthy server will not
// produce; that the real script actually prints this shape is what
// TestNFSListenerScriptJoinsSocketToProcess covers, and the two must stay
// paired for either to mean anything.
type readerOutput struct {
	tables     map[string]string
	procs      []string // "pid comm"
	argv0      []string // "pid argv0"
	fds        []string // "pid socket:[inode]"
	noReadlink bool
	truncated  bool
}

func (r readerOutput) String() string {
	var b strings.Builder
	if r.noReadlink {
		b.WriteString("==NOREADLINK\n")
	}
	for path, body := range r.tables {
		b.WriteString("==TABLE " + path + "\n" + body + "==ENDTABLE\n")
	}
	for _, p := range r.procs {
		b.WriteString("==PROC " + p + "\n")
	}
	for _, a := range r.argv0 {
		b.WriteString("==ARGV0 " + a + "\n")
	}
	for _, f := range r.fds {
		b.WriteString("==FD " + f + "\n")
	}
	if !r.truncated {
		b.WriteString("==END\n")
	}
	return b.String()
}

// serverAsItIs is the reference deployment: a supervisor at PID 1, the NFS
// server as its child, and the listening socket in tcp6 alone.
func serverAsItIs() readerOutput {
	return readerOutput{
		tables: map[string]string{
			"/proc/net/tcp":  realProcNetTCP,
			"/proc/net/tcp6": realProcNetTCP6,
		},
		procs: []string{"1 nfs-provisioner", "14 rpcbind", "16 rpc.statd", "19 dbus-daemon", "152 ganesha.nfsd"},
		argv0: []string{"1 /nfs-provisioner", "152 /usr/bin/ganesha.nfsd"},
		fds:   []string{"1 socket:[8457761]", "152 socket:[8470532]", "152 socket:[8470545]", "152 socket:[8470547]"},
	}
}

// TestDiscoveryNamesWhatHoldsThePort covers the whole point of the join: on a
// supervised server the process serving NFS is not PID 1 and is not what the
// PodSpec declares, and the only thing that distinguishes it is which process
// holds the listening socket.
//
// The two halves are exercised together here because together is how they have
// to agree: the inode the pod reports is the key the node is searched by, and a
// mismatch between them would leave every server unattributable.
//
// Steps:
//  1. Read the reference deployment's socket tables and take the inodes on 2049.
//  2. Search the processes for a holder of one of them.
//  3. Assert the server process is named, not the supervisor, and that the
//     inode that justified it is carried along as evidence.
func TestDiscoveryNamesWhatHoldsThePort(t *testing.T) {
	facts := parseListenerFacts(serverAsItIs().String())
	inodes, err := listeningInodes(facts, NFSPort)
	if err != nil {
		t.Fatalf("finding the socket listening on port %d: %v", NFSPort, err)
	}
	got, err := pickHolder(facts, inodes)
	if err != nil {
		t.Fatalf("naming the process holding port %d: %v", NFSPort, err)
	}
	if got.Name != "ganesha.nfsd" {
		t.Errorf("named %q, want the process holding the listening socket rather than PID 1", got.Name)
	}
	if got.PID != 152 {
		t.Errorf("pid is %d, want 152", got.PID)
	}
	if !strings.Contains(got.Inode, "8470545") {
		t.Errorf("evidence does not carry the socket inode that justified the name: %q", got.Inode)
	}
}

// TestListeningInodesRefusesWhatItCannotRead covers the pod's half. Its answer
// is the key the node is searched by, so a wrong one is worse than none: it
// would send the scan looking for a socket that is not the server's.
//
// Steps:
//  1. Pose a pod with no listener on the NFS port, and one whose reader was cut
//     short.
//  2. Assert neither yields an inode.
//  3. Assert the refusal says which it was, since the actions differ.
func TestListeningInodesRefusesWhatItCannotRead(t *testing.T) {
	noListener := serverAsItIs()
	noListener.tables = map[string]string{"/proc/net/tcp": realProcNetTCP}

	truncated := serverAsItIs()
	truncated.truncated = true

	for _, tc := range []struct {
		name string
		out  readerOutput
		says string
	}{
		{"no listener on the NFS port", noListener, "nothing is listening"},
		{"reader cut short", truncated, "did not run to completion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := listeningInodes(parseListenerFacts(tc.out.String()), NFSPort)
			if err == nil {
				t.Fatalf("returned inodes %v from a reading that establishes none", got)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say which situation it was: %v", err)
			}
		})
	}
}

// TestPickHolderRefusesWhatItCannotAttribute covers the node's half: every way
// the search for a holder can fail to be conclusive. Each must produce a named
// refusal rather than a name, because the alternative is a SIGKILL aimed by
// guesswork, and a case that reports blocked is recoverable.
//
// Steps:
//  1. Pose each inconclusive situation in turn.
//  2. Assert none of them yields a process name.
//  3. Assert the refusal says which situation it was, since the actions differ.
func TestPickHolderRefusesWhatItCannotAttribute(t *testing.T) {
	unheld := serverAsItIs()
	unheld.fds = []string{"1 socket:[8457761]"}

	blind := unheld
	blind.noReadlink = true

	disputed := serverAsItIs()
	disputed.fds = append(disputed.fds, "14 socket:[8470545]")

	truncated := serverAsItIs()
	truncated.truncated = true

	for _, tc := range []struct {
		name string
		out  readerOutput
		says string
	}{
		{"socket no process on this node holds", unheld, "no process there holds it"},
		{"no way to resolve an fd", blind, "readlink"},
		{"socket held by two different programs", disputed, "more than one kind"},
		{"scan cut short", truncated, "did not run to completion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickHolder(parseListenerFacts(tc.out.String()), []string{"8470545"})
			if err == nil {
				t.Fatalf("named %q from an inconclusive reading; that name would be SIGKILLed on a node", got.Name)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say which situation it was: %v", err)
			}
		})
	}
}

// TestProcessNameUsesArgv0WhenCommIsTruncated covers the kernel's 15-character
// limit on comm. A name cut in half is a broader pattern than it looks, and the
// fault record built from it names a process nobody can find afterwards.
//
// Steps:
//  1. Offer a comm at the kernel's limit with a longer argv[0] extending it.
//  2. Assert the full base name is used.
//  3. Assert an argv[0] that disagrees about anything but length loses, since
//     a process can rewrite its own argv and cannot rewrite comm.
func TestProcessNameUsesArgv0WhenCommIsTruncated(t *testing.T) {
	facts := listenerFacts{
		comm:  map[int]string{1: "ganesha.nfsd.lo", 2: "ganesha.nfsd", 3: "rpcbind"},
		argv0: map[int]string{1: "/usr/bin/ganesha.nfsd.long", 2: "/usr/bin/ganesha.nfsd", 3: "/sbin/something-else"},
	}
	if got := processName(facts, 1); got != "ganesha.nfsd.long" {
		t.Errorf("name is %q, want argv[0] to extend a comm the kernel truncated", got)
	}
	if got := processName(facts, 2); got != "ganesha.nfsd" {
		t.Errorf("name is %q, want the untruncated comm as it stands", got)
	}
	if got := processName(facts, 3); got != "rpcbind" {
		t.Errorf("name is %q, want comm to win over an argv[0] that disagrees with it", got)
	}
}

// TestNFSListenerScriptJoinsSocketToProcess runs the shipped reader under a
// real shell against a fixture proc tree. The decision tests above are only
// worth anything if the script really prints what they assume, and a quoting or
// globbing slip should fail on a workstation rather than inside a chaos case
// that was measuring something else.
//
// Steps:
//  1. Build a proc tree: both socket tables, two processes, and fd symlinks
//     pointing at socket inodes the way the kernel writes them.
//  2. Run the script over it with sh, the way a pod would.
//  3. Parse its output and assert the join names the process holding 2049.
func TestNFSListenerScriptJoinsSocketToProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proc")
	write := func(path, contents string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	link := func(pid int, fd int, target string) {
		t.Helper()
		dir := filepath.Join(root, strconv.Itoa(pid), "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
		// Deliberately dangling: an fd symlink points at something that is not
		// a path, which is why the script tests -L and not -e.
		if err := os.Symlink(target, filepath.Join(dir, strconv.Itoa(fd))); err != nil {
			t.Fatalf("linking fd %d of %d: %v", fd, pid, err)
		}
	}

	write(filepath.Join(root, "net", "tcp"), realProcNetTCP)
	write(filepath.Join(root, "net", "tcp6"), realProcNetTCP6)
	write(filepath.Join(root, "1", "comm"), "nfs-provisioner\n")
	write(filepath.Join(root, "1", "cmdline"), "/nfs-provisioner\x00-provisioner=example.com/nfs\x00")
	write(filepath.Join(root, "152", "comm"), "ganesha.nfsd\n")
	write(filepath.Join(root, "152", "cmdline"), "/usr/bin/ganesha.nfsd\x00-F\x00-L\x00/export/ganesha.log\x00")
	link(1, 8, "socket:[8457761]")
	link(152, 4, "socket:[8470532]")
	link(152, 14, "socket:[8470545]")
	link(152, 20, "/dev/null")

	out, err := exec.Command("sh", materializeScript(t, "nfs-listener.sh"), root).CombinedOutput()
	if err != nil {
		t.Fatalf("running the reader: %v: %s", err, out)
	}
	facts := parseListenerFacts(string(out))
	if !facts.complete {
		t.Fatalf("the reader printed no end marker, so a short read cannot be told from an empty server: %s", out)
	}
	if facts.argv0[152] == "" {
		t.Errorf("the reader printed no argv[0], so a truncated comm could not be extended: %s", out)
	}
	inodes, err := listeningInodes(facts, NFSPort)
	if err != nil {
		t.Fatalf("finding the listening socket in what the reader printed: %v\n%s", err, out)
	}
	got, err := pickHolder(facts, inodes)
	if err != nil {
		t.Fatalf("naming the process from what the reader printed: %v\n%s", err, out)
	}
	if got.Name != "ganesha.nfsd" || got.PID != 152 {
		t.Errorf("named %q pid %d, want ganesha.nfsd pid 152", got.Name, got.PID)
	}

	// The node scan passes the inodes it is after, because a node runs
	// hundreds of processes and the unfiltered output would be most of them.
	// The filter has to narrow the output without losing the one process that
	// matters, so it is run here rather than trusted.
	args := append([]string{materializeScript(t, "nfs-listener.sh"), root}, inodes...)
	filtered, err := exec.Command("sh", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("running the reader with an inode filter: %v: %s", err, filtered)
	}
	narrowed := parseListenerFacts(string(filtered))
	got, err = pickHolder(narrowed, inodes)
	if err != nil {
		t.Fatalf("naming the process from a filtered scan: %v\n%s", err, filtered)
	}
	if got.Name != "ganesha.nfsd" || got.PID != 152 {
		t.Errorf("filtered scan named %q pid %d, want ganesha.nfsd pid 152", got.Name, got.PID)
	}
	if _, reported := narrowed.comm[1]; reported {
		t.Errorf("the filtered scan still reported pid 1, which holds none of %v: %s", inodes, filtered)
	}
}

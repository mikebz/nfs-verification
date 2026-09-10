package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The parsers below read what busybox prints inside a test pod. They are unit
// tested because the alternative is discovering a format assumption on a
// cluster, inside a case that was meant to be testing something else.

func TestParseDF(t *testing.T) {
	// POSIX output from `df -P -k`, which is what the harness asks for.
	out := `Filesystem         1024-blocks    Used Available Capacity Mounted on
10.0.0.2:/exports/pvc-1  1048576   10240   1038336       1% /mnt/share`
	got, err := parseDF(out)
	if err != nil {
		t.Fatalf("parsing df output: %v", err)
	}
	if got.TotalBytes != 1048576*1024 {
		t.Errorf("total is %d bytes, want %d", got.TotalBytes, 1048576*1024)
	}
	if got.AvailBytes != 1038336*1024 {
		t.Errorf("available is %d bytes, want %d", got.AvailBytes, 1038336*1024)
	}
}

func TestParseDFRejectsGarbage(t *testing.T) {
	for name, out := range map[string]string{
		"header only":   "Filesystem 1024-blocks Used Available Capacity Mounted on",
		"empty":         "",
		"short line":    "Filesystem 1024-blocks\nfoo 1",
		"non-numeric":   "Filesystem 1024-blocks Used Available Capacity Mounted on\nfoo bar baz qux 1% /mnt/share",
		"df error text": "df: /mnt/share: No such file or directory",
	} {
		if _, err := parseDF(out); err == nil {
			t.Errorf("%s: parsed without error, so a broken mount would read as a capacity", name)
		}
	}
}

func TestParseOwner(t *testing.T) {
	got, err := parseOwner("1234 1234 nfsuser nfsgroup\n")
	if err != nil {
		t.Fatalf("parsing stat output: %v", err)
	}
	if got.UID != 1234 || got.GID != 1234 || got.User != "nfsuser" || got.Group != "nfsgroup" {
		t.Errorf("parsed %+v", got)
	}
	// The unmapped case, which is what the identity case is hunting: busybox
	// prints the numeric id in place of a name it cannot resolve.
	got, err = parseOwner("65534 65534 65534 65534")
	if err != nil {
		t.Fatalf("parsing an unmapped owner: %v", err)
	}
	if got.UID != 65534 || got.String() != "65534(65534):65534(65534)" {
		t.Errorf("parsed %+v, rendered %q", got, got.String())
	}
	for _, bad := range []string{"", "1234 1234", "x y u g", "1234 y u g"} {
		if _, err := parseOwner(bad); err == nil {
			t.Errorf("parsed %q without error", bad)
		}
	}
}

// TestOpenWriteScriptHoldsAndCloses runs the rendered script under a real
// shell. The script carries a heredoc, a detached background process and paths
// that need quoting; a slip in any of them would otherwise surface only against
// a cluster, inside a case that was testing something else entirely. The shell
// here is the harness's, not a pod's, so this says the script is correct, not
// that NFS behaves: what DATA-04 asserts still needs a cluster.
func TestOpenWriteScriptHoldsAndCloses(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on this machine: %v", err)
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("no setsid on this machine: %v", err)
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "openw.state")
	run := filepath.Join(dir, "openw.run")
	// A path with a space and a payload with a quote in it: both go through
	// shellQuote, and both are what a careless format string gets wrong.
	target := filepath.Join(dir, "a file.txt")
	const payload = `payload with 'quotes' and $VARS`

	cmd := exec.Command(sh, "-c", openWriteScript(state, run, "x", target, payload))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launching the writer: %v\n%s", err, out)
	}
	waitForState(t, state, "open")

	// Held open, and the bytes are already there: the file descriptor stays
	// open so that DATA-04 can read before any close happens.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading what the writer produced: %v", err)
	}
	if string(got) != payload {
		t.Errorf("the writer produced %q, want %q: the payload did not survive quoting", got, payload)
	}

	if err := os.Remove(run); err != nil {
		t.Fatalf("signalling the close: %v", err)
	}
	waitForState(t, state, "closed")
}

// waitForState polls the writer's state file, which is how Close and
// HoldOpenWrite know the descriptor reached the state they asked for.
func waitForState(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			last = strings.TrimSpace(string(b))
			if last == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("writer state is %q after 30s, want %q", last, want)
}

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

// TestCapacityParsersAgainstRealTools verifies that the two capacity parsers
// read what the real tools actually print, and that they agree with each other.
// Neither parser is handed a written-out string here, which is the point: a
// golden fixture only proves a parser matches what someone typed into it.
//
// Steps:
//  1. Run `stat -f` and `df -P -k` against a real directory on this machine.
//  2. Parse both with the parsers the harness uses inside a pod.
//  3. Assert each is self-consistent: a positive total, available not above it.
//  4. Assert the two agree on the total to within a kilobyte. Any misread field
//     is off by orders of magnitude, so that tolerance still catches one.
func TestCapacityParsersAgainstRealTools(t *testing.T) {
	stat := lookOrSkip(t, "stat")
	df := lookOrSkip(t, "df")
	dir := t.TempDir()

	statOut, err := exec.Command(stat, "-f", "-c", "%b %a %S", dir).Output()
	if err != nil {
		t.Skipf("stat -f is unavailable here, which is the case MountCapacity keeps df for: %v", err)
	}
	fromStat, err := parseStatFS(string(statOut))
	if err != nil {
		t.Fatalf("parsing real stat -f output %q: %v", statOut, err)
	}
	dfOut, err := exec.Command(df, "-P", "-k", dir).Output()
	if err != nil {
		t.Fatalf("running df: %v", err)
	}
	fromDF, err := parseDF(string(dfOut))
	if err != nil {
		t.Fatalf("parsing real df output %q: %v", dfOut, err)
	}

	for name, got := range map[string]Capacity{"stat -f": fromStat, "df -P -k": fromDF} {
		if got.TotalBytes <= 0 {
			t.Errorf("%s parsed a total of %d bytes, which would read as a volume with no capacity",
				name, got.TotalBytes)
		}
		if got.AvailBytes > got.TotalBytes {
			t.Errorf("%s parsed %d bytes available out of %d total", name, got.AvailBytes, got.TotalBytes)
		}
	}
	// Both derive the total from the same statfs fields, so they agree except
	// for df rounding to whole kilobytes.
	if diff := fromStat.TotalBytes - fromDF.TotalBytes; diff > 1024 || diff < -1024 {
		t.Errorf("stat -f says %d bytes and df says %d, so the two are not reading the same filesystem "+
			"and at least one is reading the wrong field", fromStat.TotalBytes, fromDF.TotalBytes)
	}
}

// TestParseStatFS covers the shape of the output rather than any filesystem's
// numbers, including the case that matters most: an image whose stat has no -f
// prints the format string straight back, and reading that as a capacity of
// zero would look like a full volume.
//
// Steps:
//  1. Parse three integers and check the arithmetic against the block size.
//  2. Reject anything that is not three integers, and any unusable block size.
func TestParseStatFS(t *testing.T) {
	// Total data blocks, blocks available to an ordinary user, block size.
	got, err := parseStatFS("262144 261000 4096\n")
	if err != nil {
		t.Fatalf("parsing statfs output: %v", err)
	}
	if got.TotalBytes != 262144*4096 || got.AvailBytes != 261000*4096 {
		t.Errorf("parsed %+v: capacity is a block count times the block size", got)
	}
	for name, out := range map[string]string{
		"empty":              "",
		"missing a field":    "262144 261000",
		"an extra field":     "262144 261000 4096 0",
		"not numeric":        "a b c",
		"no -f in this stat": "%b %a %S",
		"zero block size":    "262144 261000 0",
	} {
		if c, err := parseStatFS(out); err == nil {
			t.Errorf("%s: parsed %q as %+v instead of failing", name, out, c)
		}
	}
}

// TestParseDF covers the fallback parser against the layout `df -P` guarantees:
// a header, then one line per filesystem with 1K blocks in field 2 and
// available blocks in field 4. Only the field positions are load bearing. The
// device column is whatever the export happens to be called, which is exactly
// why the parser never reads it.
//
// Steps:
//  1. Parse a POSIX df line and check both figures.
//  2. Reject a header with no filesystem line, short lines and error text: a
//     broken mount must not read as a capacity.
func TestParseDF(t *testing.T) {
	out := `Filesystem         1024-blocks    Used Available Capacity Mounted on
some-server:/exports/pvc-1  1048576   10240   1038336       1% /mnt/share`
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
	for name, bad := range map[string]string{
		"header only":   "Filesystem 1024-blocks Used Available Capacity Mounted on",
		"empty":         "",
		"short line":    "Filesystem 1024-blocks\nfoo 1",
		"non-numeric":   "Filesystem 1024-blocks Used Available Capacity Mounted on\nfoo bar baz qux 1% /mnt/share",
		"df error text": "df: /mnt/share: No such file or directory",
	} {
		if _, err := parseDF(bad); err == nil {
			t.Errorf("%s: parsed without error, so a broken mount would read as a capacity", name)
		}
	}
}

// TestParseOwner covers the ownership reader, including the two cases that
// would otherwise be found on a cluster: an unmapped owner, which is what the
// identity cases exist to catch and which busybox prints as a numeric id in
// place of the name, and a name with a space in it, which is ordinary wherever
// names resolve through a directory service.
//
// Steps:
//  1. Parse a resolved owner and check all four fields.
//  2. Parse an owner whose user and group names hold spaces.
//  3. Parse an unmapped owner and check it renders readably in a failure.
//  4. Reject short, empty, non-numeric and undelimited output.
func TestParseOwner(t *testing.T) {
	got, err := parseOwner("1234|1234|nfsuser|nfsgroup\n")
	if err != nil {
		t.Fatalf("parsing stat output: %v", err)
	}
	if got.UID != 1234 || got.GID != 1234 || got.User != "nfsuser" || got.Group != "nfsgroup" {
		t.Errorf("parsed %+v", got)
	}
	// Names with spaces in them. An environment resolving through LDAP, Active
	// Directory or an NSS module hands these back routinely, and splitting on
	// whitespace would turn correct ownership into a parse failure.
	got, err = parseOwner("1234|1234|Domain Admin|Domain Users")
	if err != nil {
		t.Fatalf("parsing an owner whose names hold spaces: %v", err)
	}
	if got.User != "Domain Admin" || got.Group != "Domain Users" {
		t.Errorf("parsed %+v: a name with a space in it was cut", got)
	}
	// The unmapped case, which is what the identity case is hunting: busybox
	// prints the numeric id in place of a name it cannot resolve.
	got, err = parseOwner("65534|65534|65534|65534")
	if err != nil {
		t.Fatalf("parsing an unmapped owner: %v", err)
	}
	if got.UID != 65534 || got.String() != "65534(65534):65534(65534)" {
		t.Errorf("parsed %+v, rendered %q", got, got.String())
	}
	for _, bad := range []string{"", "1234|1234", "x|y|u|g", "1234|y|u|g", "1234 1234 u g"} {
		if _, err := parseOwner(bad); err == nil {
			t.Errorf("parsed %q without error", bad)
		}
	}
}

// TestOpenWriteScriptHoldsAndCloses runs scripts/hold-open-write.sh under a
// real shell, with the same arguments a pod would get. The script detaches
// itself and holds a descriptor open, which is the whole of what DATA-04 needs
// and is not something a Go test can fake. The shell here is the harness's, not
// a pod's, so this says the script is correct, not that NFS behaves.
//
// Steps:
//  1. Launch the writer against a path with a space in it, with a payload
//     holding quotes and something that looks like a shell variable.
//  2. Wait for it to report "open".
//  3. Assert the payload is on disk whole, so nothing was expanded or cut.
//  4. Remove the run file and wait for it to report "closed".
func TestOpenWriteScriptHoldsAndCloses(t *testing.T) {
	sh := lookOrSkip(t, "sh", "setsid")
	dir := t.TempDir()
	state := filepath.Join(dir, "openw.state")
	run := filepath.Join(dir, "openw.run")
	// A path with a space and a payload with a quote in it: both go through
	// shellQuote on the way to the pod, and both are what careless quoting
	// mangles.
	target := filepath.Join(dir, "a file.txt")
	const payload = `payload with 'quotes' and $VARS`

	cmd := exec.Command(sh, materializeScript(t, "hold-open-write.sh"), target, payload, run, state)
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

// waitForState polls a background worker's state file, which is how Close,
// HoldOpenWrite and the lock holders know the thing they started reached the
// state they asked for.
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
	t.Fatalf("the state file says %q after 30s, want %q", last, want)
}

// lookOrSkip skips the test unless every named tool is on this machine, and
// returns the path of the first. These tests run the harness's own scripts, so
// a missing tool is a fact about the workstation, not a failure.
func lookOrSkip(t *testing.T, names ...string) string {
	t.Helper()
	var first string
	for i, n := range names {
		p, err := exec.LookPath(n)
		if err != nil {
			t.Skipf("no %s on this machine: %v", n, err)
		}
		if i == 0 {
			first = p
		}
	}
	return first
}

// TestCheckScriptID covers the identifiers the shell helpers turn into
// filenames inside a pod. Quoting is not enough on its own: a quoted
// "../../etc/x" is still a path escape.
//
// Steps:
//  1. Accept the shapes the cases actually pass.
//  2. Reject separators, metacharacters, whitespace and anything empty or
//     leading with a dot.
func TestCheckScriptID(t *testing.T) {
	for _, id := range []string{"data04", "chaos01", "CHAOS-01", "a", "with_underscore", "with.dot"} {
		if err := CheckScriptID(id); err != nil {
			t.Errorf("%q was rejected but is exactly what a case passes: %v", id, err)
		}
	}
	for _, id := range []string{
		"", ".", "..", "../../etc/passwd", "a/b", "a b", "a;rm -rf /", "a$(id)", "a`id`", "-leading-dash",
		strings.Repeat("x", 65),
	} {
		if err := CheckScriptID(id); err == nil {
			t.Errorf("%q was accepted, and it becomes a path inside the pod", id)
		}
	}
}

// materializeScript writes an embedded script to a temp file so a test can run
// the same bytes the pod would run, with the same arguments.
func materializeScript(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(scriptBody(name)), 0o755); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryScriptIsValidShell syntax-checks every embedded script with the
// shell itself. The point of keeping these as files is that they can be read
// and checked; this is the checking half, and it covers scripts no other test
// happens to run.
//
// Steps:
//  1. List the embedded scripts and assert there are some, so a broken embed
//     directive cannot pass as an empty set.
//  2. Run each through `sh -n`.
//  3. Assert each documents its own usage, since a script nobody can invoke by
//     hand is back to being an opaque string.
func TestEveryScriptIsValidShell(t *testing.T) {
	sh := lookOrSkip(t, "sh")
	entries, err := scriptFS.ReadDir("scripts")
	if err != nil {
		t.Fatalf("reading the embedded scripts: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no scripts are embedded, so every script test below would pass vacuously")
	}
	for _, e := range entries {
		body := scriptBody(e.Name())
		cmd := exec.Command(sh, "-n")
		cmd.Stdin = strings.NewReader(body)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s is not valid shell: %v\n%s", e.Name(), err, out)
		}
		if !strings.Contains(body, "Usage:") {
			t.Errorf("%s does not say how to invoke it", e.Name())
		}
	}
}

// TestRunScriptCarriesArgumentsIntact runs a script through the same shell the
// pod would, and checks that what the script receives is what the caller
// passed. Arguments reach a pod through a quoted command line, and a value
// with a space or a quote in it is exactly what a careless helper mangles.
//
// Steps:
//  1. Build the shell for a script that echoes its arguments back.
//  2. Run it, with arguments holding spaces, quotes and shell metacharacters.
//  3. Assert each argument arrived whole and unexpanded.
func TestRunScriptCarriesArgumentsIntact(t *testing.T) {
	sh := lookOrSkip(t, "sh")
	dir := t.TempDir()
	// A stand-in for a real script: the point under test is the shipping, not
	// any one script's behaviour.
	body := "set -u\nfor a in \"$@\"; do echo \"[$a]\"; done\n"
	name := "echo-args.sh"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("writing the stand-in script: %v", err)
	}

	args := []string{"/mnt/share/a file.txt", "payload with 'quotes'", "$HOME and `id`", "*"}
	script, err := RunScript("verify-records.sh", "unit", args...)
	if err != nil {
		t.Fatalf("building the invocation: %v", err)
	}
	// Swap the real script's body for the stand-in, leaving the shipping and
	// the quoting exactly as RunScript produced them.
	script = strings.Replace(script, strings.TrimRight(scriptBody("verify-records.sh"), "\n"),
		strings.TrimRight(body, "\n"), 1)

	cmd := exec.Command(sh, "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the shipped script: %v\n%s", err, out)
	}
	for _, want := range args {
		if !strings.Contains(string(out), "["+want+"]") {
			t.Errorf("argument %q did not arrive intact; the script saw:\n%s", want, out)
		}
	}
}

// TestRunScriptRejectsABadID covers the identifier that becomes a filename
// inside the pod. Quoting alone does not stop a path escape.
//
// Steps:
//  1. Ship a script under an id holding a path separator.
//  2. Assert it is refused rather than written outside /tmp.
func TestRunScriptRejectsABadID(t *testing.T) {
	if _, err := RunScript("verify-records.sh", "../../etc/cron.d/x"); err == nil {
		t.Error("an id that escapes /tmp was accepted as a script filename")
	}
}

// busyboxFlockOptions is every option busybox's flock applet parses
// (util-linux/flock.c). util-linux accepts a superset, which is why a
// portability defect here cannot be caught by running the script: the
// workstation and the default tools image both accept more than the rule
// allows. See F-005 in docs/findings.md.
var busyboxFlockOptions = map[string]bool{"-s": true, "-x": true, "-u": true, "-n": true}

// TestScriptsUseOnlyPortableFlockOptions holds the scripts to what the
// repository says it assumes: nothing beyond busybox. An option busybox does
// not parse makes every attempt exit on a usage error, which reads as a
// refusal, and a case asserting on grants then passes having asked nothing.
//
// Steps:
//  1. Read every embedded script.
//  2. Find each flock invocation and the dash-prefixed words after it.
//  3. Assert every one of them is in busybox's set.
func TestScriptsUseOnlyPortableFlockOptions(t *testing.T) {
	entries, err := scriptFS.ReadDir("scripts")
	if err != nil {
		t.Fatalf("reading the embedded scripts: %v", err)
	}
	for _, e := range entries {
		for i, line := range strings.Split(scriptBody(e.Name()), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			for _, opt := range flockOptionsIn(line) {
				if !busyboxFlockOptions[opt] {
					t.Errorf("%s:%d passes %q to flock, which busybox does not parse: %s",
						e.Name(), i+1, opt, strings.TrimSpace(line))
				}
			}
		}
	}
}

// flockOptionsIn returns the options passed to every flock call on one line.
// Options are collected up to the end of the command rather than up to the
// first non-option word, because flock takes a file argument between its flags
// in some invocations and stopping there would skip everything after it.
func flockOptionsIn(line string) []string {
	var opts []string
	fields := strings.Fields(line)
	for i, f := range fields {
		if strings.Trim(f, "'\";") != "flock" {
			continue
		}
		for _, arg := range fields[i+1:] {
			if endsCommand(arg) {
				break
			}
			if strings.HasPrefix(arg, "-") {
				opts = append(opts, strings.TrimRight(arg, "'\";"))
			}
		}
	}
	return opts
}

// endsCommand reports whether a word terminates the command it appears in, so
// that the scan above stops before the next one's flags.
func endsCommand(word string) bool {
	for _, sep := range []string{";", "|", "&", ")", "&&", "||"} {
		if strings.Contains(word, sep) {
			return true
		}
	}
	return false
}

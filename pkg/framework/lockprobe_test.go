package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The lock probe is what CHAOS-07 reads to say whether a server granted new
// state while it was still waiting for old state to be reclaimed. Two things
// about it fail silently rather than loudly: a probe that never releases what
// it was granted, which changes what every later attempt means, and a probe
// that reports a refusal as a grant. Both are checked here against a real
// shell and a real flock, so they fail on a workstation rather than inside a
// case that was measuring something else.

// TestLockProbeScriptReportsGrants runs scripts/lock-probe.sh against a local
// file with nothing holding it. It says the script is correct, not that NFS
// behaves.
//
// Steps:
//  1. Launch the probe against a path with a space in it.
//  2. Wait for two granted attempts.
//  3. Assert nothing was reported as refused and nothing was unreadable.
//  4. Take the lock from outside while the probe is still running, and assert
//     it was available, which is only true if the probe released what it was
//     granted.
func TestLockProbeScriptReportsGrants(t *testing.T) {
	sh := lookOrSkip(t, "sh", "setsid", "flock", "date")
	base := t.TempDir()
	log := filepath.Join(base, "probe.log")
	run := filepath.Join(base, "probe.run")
	// A path with a space in it, because the path comes from a case and goes
	// through shell quoting on the way to the pod.
	target := filepath.Join(base, "a lock file")

	cmd := exec.Command(sh, materializeScript(t, "lock-probe.sh"), target, run, log, "1", "5")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launching the probe: %v\n%s", err, out)
	}
	defer os.Remove(run)

	rep := waitForAttempts(t, log, 2)
	if len(rep.Committed()) < 2 {
		t.Fatalf("the probe was granted %d of %d attempts against an unheld file: %+v",
			len(rep.Committed()), len(rep.Records), rep.Records)
	}
	if len(rep.Errors()) != 0 || len(rep.Unparsed) != 0 {
		t.Errorf("the probe reported refusals against a file nothing was holding: refused=%+v unreadable=%v",
			rep.Errors(), rep.Unparsed)
	}

	// The probe must drop what it was granted. A probe that kept the lock would
	// make every later attempt mean something different, and the case would be
	// reading its own footprint.
	flock := lookOrSkip(t, "flock")
	if out, err := exec.Command(flock, "-n", "-x", target, "-c", "true").CombinedOutput(); err != nil {
		t.Errorf("the probe is still holding the lock between attempts: %v\n%s", err, out)
	}
}

// TestLockProbeScriptReportsRefusals is the half that carries the assertion: a
// grant reported where the lock was refused would report a protocol violation
// on a server that behaved correctly.
//
// Steps:
//  1. Hold the target file locked from another process.
//  2. Run the probe against it.
//  3. Assert every attempt it logged while the holder was up is a refusal.
func TestLockProbeScriptReportsRefusals(t *testing.T) {
	sh := lookOrSkip(t, "sh", "setsid", "flock", "date")
	flock := lookOrSkip(t, "flock")
	base := t.TempDir()
	log := filepath.Join(base, "probe.log")
	run := filepath.Join(base, "probe.run")
	target := filepath.Join(base, "held")

	holder := exec.Command(flock, "-x", target, "-c", "sleep 30")
	if err := holder.Start(); err != nil {
		t.Fatalf("taking the lock from outside: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	}()
	// The holder takes the lock as it starts; give it that moment before the
	// probe's first attempt, or the first attempt races it.
	time.Sleep(500 * time.Millisecond)

	cmd := exec.Command(sh, materializeScript(t, "lock-probe.sh"), target, run, log, "1", "5")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launching the probe: %v\n%s", err, out)
	}
	defer os.Remove(run)

	rep := waitForAttempts(t, log, 2)
	if got := rep.Committed(); len(got) != 0 {
		t.Errorf("the probe reported %d grants while another process held the lock: %v. "+
			"A grant reported where there was none files a protocol violation against a correct server",
			len(got), got)
	}
	if len(rep.Errors()) < 2 {
		t.Errorf("the probe logged %d refusals in %d attempts, want every attempt refused",
			len(rep.Errors()), len(rep.Records))
	}
}

// waitForAttempts polls the probe log until it holds at least n attempts, which
// is how a case knows the probe is running before it injects anything.
func waitForAttempts(t *testing.T, log string, n int) LoadReport {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var rep LoadReport
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(log); err == nil {
			rep = parseLoadLog(string(b))
			if len(rep.Records) >= n {
				return rep
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the probe logged %d attempts in 30s, want at least %d", len(rep.Records), n)
	return rep
}

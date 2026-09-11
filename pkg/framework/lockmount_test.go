package framework

import (
	"strings"
	"testing"
)

// TestLocalLockingPerLockKind is the check behind rule 2 of
// docs/04-data-path-and-locktool-design.md: a lock case runs only on a mount
// that sends its locks to the server, and the options that stop it are
// per-kind.
//
// Getting this backwards is expensive in both directions. Blocking too eagerly
// hides a case forever on a mount that was fine; blocking too little lets a
// case fail on a mount where two clients were always going to get the same
// lock, and the failure names the server for a mount option's fault.
//
// Steps:
//  1. Take the option strings a Linux NFS client can produce.
//  2. Ask each of them about both lock kinds.
//  3. Assert the answers, and that the option named back is the one responsible.
func TestLocalLockingPerLockKind(t *testing.T) {
	cases := []struct {
		name       string
		options    string
		flockLocal bool
		posixLocal bool
		wantOption string
	}{
		{
			name:       "default-options-block-nothing",
			options:    "rw,relatime,vers=4.1,rsize=1048576,hard,proto=tcp,local_lock=none",
			flockLocal: false, posixLocal: false,
		},
		{
			name:       "no-locking-option-at-all-blocks-nothing",
			options:    "rw,relatime,vers=4.1,hard,proto=tcp",
			flockLocal: false, posixLocal: false,
		},
		{
			name:       "nolock-blocks-both",
			options:    "rw,relatime,vers=4.1,hard,nolock",
			flockLocal: true, posixLocal: true, wantOption: "nolock",
		},
		{
			name:       "local_lock-all-blocks-both",
			options:    "rw,relatime,vers=4.1,hard,local_lock=all",
			flockLocal: true, posixLocal: true, wantOption: "local_lock=all",
		},
		{
			name:       "local_lock-flock-blocks-only-flock",
			options:    "rw,relatime,vers=4.1,hard,local_lock=flock",
			flockLocal: true, posixLocal: false, wantOption: "local_lock=flock",
		},
		{
			name:       "local_lock-posix-blocks-only-byte-ranges",
			options:    "rw,relatime,vers=4.1,hard,local_lock=posix",
			flockLocal: false, posixLocal: true, wantOption: "local_lock=posix",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []struct {
				kind LockKind
				want bool
			}{{FlockLock, tc.flockLocal}, {PosixLock, tc.posixLocal}} {
				local, option := LocalLocking(tc.options, k.kind)
				if local != k.want {
					t.Errorf("LocalLocking(%q, %s) is %v, want %v", tc.options, k.kind, local, k.want)
					continue
				}
				if local && option != tc.wantOption {
					t.Errorf("LocalLocking(%q, %s) blamed %q, want %q: a blocked report that does not "+
						"name the option it objects to cannot be acted on",
						tc.options, k.kind, option, tc.wantOption)
				}
				if !local && option != "" {
					t.Errorf("LocalLocking(%q, %s) named %q while reporting locks reach the server",
						tc.options, k.kind, option)
				}
			}
		})
	}
}

// TestLockMountBlockedNamesWhatToChange covers the message an operator reads
// when a lock case declines to run. A blocked report with no reason is the
// failure mode this phase adds the most opportunities for.
func TestLockMountBlockedNamesWhatToChange(t *testing.T) {
	m := LockMount{
		Node: "worker-2",
		Line: MountLine{
			Device: "10.0.0.5:/export/pvc-1", MountPoint: "/var/lib/kubelet/pods/a/volumes/x/pvc-1/mount",
			FSType: "nfs4", Options: "rw,vers=4.1,hard,local_lock=all",
		},
		Local: true, Option: "local_lock=all", Kind: PosixLock,
	}
	msg := m.Blocked()
	for _, want := range []string{"worker-2", "local_lock=all", "mountOptions", string(PosixLock)} {
		if !strings.Contains(msg, want) {
			t.Errorf("the blocked report does not mention %q: %s", want, msg)
		}
	}
}

// TestParseProcLocks covers the client's own lock table, which is the half of a
// lock assertion the server cannot provide. It is read on a node during a
// failover, so a parser that fails does so where nothing can be debugged.
//
// Steps:
//  1. Parse a table holding a POSIX range lock, a whole-file FLOCK lock, an OFD
//     lock and a blocked waiter.
//  2. Check the kind, mode, range and EOF marker on each.
//  3. Assert unreadable lines are dropped rather than parsed into a lock that
//     was never there.
func TestParseProcLocks(t *testing.T) {
	const procLocks = `1: POSIX  ADVISORY  WRITE 2451 00:2f:12 0 4095
2: FLOCK  ADVISORY  WRITE 1200 00:2f:13 0 EOF
3: OFDLCK ADVISORY  READ  -1 00:2f:14 8192 16383
4: -> POSIX  ADVISORY  WRITE 3001 00:2f:12 0 4095
this line is not a lock
5: POSIX  ADVISORY  WRITE notapid 00:2f:15 0 1
`
	locks := ParseProcLocks(procLocks)
	if len(locks) != 4 {
		t.Fatalf("parsed %d locks, want 4: %v", len(locks), locks)
	}
	if l := locks[0]; l.Kind != "POSIX" || l.Mode != "WRITE" || l.PID != 2451 ||
		l.Start != 0 || l.End != 4095 || l.EndsAtEOF || l.Waiting {
		t.Errorf("the POSIX range lock parsed as %+v", l)
	}
	if l := locks[1]; l.Kind != "FLOCK" || !l.EndsAtEOF {
		t.Errorf("the whole-file flock lock parsed as %+v; EOF means to the end of the file "+
			"however long it becomes, which is not the same as ending at byte zero", l)
	}
	if l := locks[2]; l.Kind != "OFDLCK" || l.Mode != "READ" || l.PID != -1 || l.Start != 8192 {
		t.Errorf("the OFD lock parsed as %+v", l)
	}
	if l := locks[3]; !l.Waiting {
		t.Errorf("the blocked waiter parsed as %+v, and a waiter reported as a holder would say a "+
			"client holds a range it is still queued for", l)
	}
	if got := locks[1].String(); !strings.Contains(got, "EOF") {
		t.Errorf("rendered as %q, which loses the EOF marker", got)
	}
}

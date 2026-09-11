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
//
// Steps:
//  1. Build the mount a lock case would decline to run on.
//  2. Render its blocked report.
//  3. Assert it names the node, the option responsible, the lock kind that
//     option makes local, and the field to change to fix it. A report missing
//     any of the four leaves an operator guessing which of the mount's dozen
//     options is the problem.
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
//  2. Check the kind, mode, file identity, range and EOF marker on each.
//  3. Assert unreadable lines are dropped rather than parsed into a lock that
//     was never there.
//  4. Assert Covers matches only a held POSIX lock over exactly that range of
//     that file: the table holds every lock on the node, so a range alone is
//     not identity and a waiter is not a holder.
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
	if l := locks[0]; l.Device != "00:2f" || l.Inode != 12 {
		t.Errorf("the POSIX range lock parsed as device %q inode %d, want 00:2f and 12. Without the "+
			"file's identity a lock line says only that something on this node holds these bytes",
			l.Device, l.Inode)
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

	// Identity, not just the range. The first and fourth lines cover the same
	// bytes of the same file, and only the first is a holder; the third covers
	// a different file entirely.
	posix, waiter, other, flock := locks[0], locks[3], locks[2], locks[1]
	if !posix.Covers(12, WriteRange(0, 4096)) {
		t.Errorf("the held POSIX lock %s does not match the range it covers", posix)
	}
	if posix.Covers(99, WriteRange(0, 4096)) {
		t.Errorf("%s matched a range in a different file: a lock over the same offsets somewhere else "+
			"would stand in as proof that this one survived", posix)
	}
	if posix.Covers(12, WriteRange(0, 2048)) || posix.Covers(12, WriteRange(4096, 4096)) {
		t.Errorf("%s matched a range it does not cover", posix)
	}
	if waiter.Covers(12, WriteRange(0, 4096)) {
		t.Errorf("the blocked waiter %s matched as a holder, and a client queued for a range does not "+
			"hold it", waiter)
	}
	// The other two APIs. Both cover real ranges of real files and neither is a
	// POSIX lock, which is the distinction every lock case here turns on: what
	// locktool takes is fcntl, and an OFD or flock lock over the same bytes is
	// a different application asking a different question.
	if other.Covers(14, WriteRange(8192, 8192)) {
		t.Errorf("the OFD lock %s matched a POSIX assertion", other)
	}
	if flock.Covers(13, WriteRange(0, 0)) {
		t.Errorf("the whole-file flock lock %s matched a POSIX assertion", flock)
	}
}

package framework

import (
	"regexp"
	"strconv"
	"strings"
)

// LostLocks is the Linux NFS client saying that locks it held did not come back
// from state recovery: its reclaim was refused, and the application holding them
// was not told. See F-028 in docs/findings.md.
//
// The kernel prints it from nfs4_do_reclaim in fs/nfs/nfs4state.c as
// "NFS: %s: lost %d locks", once per recovery pass. The count covers every lock
// on that node's client for that server whose reclaim failed, which is every pod
// on the node that shares the client, not one pod. It is printed for a reclaim
// refused because grace had ended and for one refused because a conflicting lock
// was already granted alike: the line says the reclaim was refused, never why.
type LostLocks struct {
	// Server is the server as the client names it, cl_hostname: whatever the
	// mount was given, usually an address.
	Server string
	// Count is the number of locks the client gave up on.
	Count int
	// Line is the line as the ring buffer printed it, timestamp included, so a
	// failure message quotes what a reader will find in the bundle.
	Line string
}

// lostLocksPattern matches the kernel's wording and nothing looser. The count
// is always followed by "locks", including "lost 1 locks", because the format
// string does not pluralise.
var lostLocksPattern = regexp.MustCompile(`NFS: (\S+): lost (\d+) locks$`)

// NewLostLocks returns the lost-locks reports in after that were not already in
// before, where both are one node's ring buffer read at two moments.
//
// A window from two reads, rather than the whole buffer, because the buffer
// outlives every case: on gke-w2 a "lost 1 locks" from 2026-09-19 was still in
// the writer node's buffer when CHAOS-06 failed there six days later, and a
// case that read the whole buffer would have blamed that failover on it.
//
// Lines are compared with their timestamp removed, and counted rather than set.
// dmesg -T renders a stamp from the boot time as it stands at the moment of the
// read, so the same record can come back with a different stamp, and a stale
// report that re-rendered must not read as a new one. Counting keeps a second
// report with the same wording as an old one visible. What this cannot see is a
// report that arrived while an old identical one rotated out of the buffer; it
// then returns nothing, which leaves a caller with the less specific message
// rather than a wrong one.
func NewLostLocks(before, after string) []LostLocks {
	seen := map[string]int{}
	for _, line := range strings.Split(before, "\n") {
		if _, ok := parseLostLocks(line); ok {
			seen[withoutStamp(line)]++
		}
	}
	var out []LostLocks
	for _, line := range strings.Split(after, "\n") {
		ll, ok := parseLostLocks(line)
		if !ok {
			continue
		}
		key := withoutStamp(line)
		if seen[key] > 0 {
			seen[key]--
			continue
		}
		out = append(out, ll)
	}
	return out
}

// parseLostLocks reads one ring buffer line, stamped or not.
func parseLostLocks(line string) (LostLocks, bool) {
	line = strings.TrimSpace(line)
	m := lostLocksPattern.FindStringSubmatch(withoutStamp(line))
	if m == nil {
		return LostLocks{}, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return LostLocks{}, false
	}
	return LostLocks{Server: m[1], Count: n, Line: line}, true
}

// withoutStamp removes the leading bracketed timestamp dmesg puts on a line,
// either the seconds since boot or, with -T, a wall clock rendering of them.
func withoutStamp(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "[") {
		if i := strings.Index(line, "]"); i >= 0 {
			line = strings.TrimSpace(line[i+1:])
		}
	}
	return line
}

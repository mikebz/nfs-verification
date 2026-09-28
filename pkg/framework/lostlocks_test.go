package framework

import "testing"

// Real lines, from the gke-w2 bundles, so the parser is held to what the kernel
// prints rather than to what someone typed. The first is the verifier node's
// report from the CHAOS-06 failure behind F-028, run
// w2-e2e-run2-20260925-222955; the second is from run w2-e2e-run2-20260919-014747
// and was still in the writer node's buffer, unchanged, when that failure
// happened six days later.
const (
	lostTwoLocks = "[Fri Sep 25 23:56:04 2026] NFS: 34.118.237.153: lost 2 locks"
	lostOneLock  = "[Sat Sep 19 02:27:37 2026] NFS: 34.118.237.153: lost 1 locks"
	// Neighbours that must not count. The start-up line is from the same
	// buffers. The unhandled-error line is written by hand from the format
	// nfs4_reclaim_locks prints on the same recovery path, because no healthy
	// run has produced one: it shares the "NFS: <word>: " shape and must not
	// be read as a report about a server.
	idResolver   = "[Fri Sep 11 03:53:37 2026] NFS: Registering the id_resolver key type"
	unhandledErr = "[Fri Sep 25 23:56:04 2026] NFS: nfs4_reclaim_locks: unhandled error -5"
)

// TestNewLostLocksWindow exists because the ring buffer
// outlives every case, and getting the window wrong fails silently in the worst
// direction. CHAOS-06 turns a lost-locks report into "the holder's reclaim was
// refused" in its failure message. A report left over from an earlier failover
// that the parser took for this one would explain away a conflicting grant,
// which is the failure the case exists to catch, and nothing would say so.
//
// Steps:
//  1. The F-028 shape: the writer node carries a stale report before and after,
//     the verifier node gains one. Only the verifier's is new.
//  2. A stale report whose stamp re-rendered between the two reads is still
//     stale.
//  3. A second report with the same wording as an old one is still new.
//  4. The unstamped format parses, and the neighbouring NFS lines do not.
func TestNewLostLocksWindow(t *testing.T) {
	// 1. What each node's buffer looked like around the fault in F-028.
	writerBefore := idResolver + "\n" + lostOneLock + "\n"
	writerAfter := writerBefore
	if got := NewLostLocks(writerBefore, writerAfter); len(got) != 0 {
		t.Errorf("the writer node's report from six days earlier was attributed to this fault: %+v", got)
	}
	verifierBefore := idResolver + "\n"
	verifierAfter := verifierBefore + lostTwoLocks + "\n"
	got := NewLostLocks(verifierBefore, verifierAfter)
	if len(got) != 1 {
		t.Fatalf("got %d new reports on the verifier node, want the one it gained: %+v", len(got), got)
	}
	if want := (LostLocks{Server: "34.118.237.153", Count: 2, Line: lostTwoLocks}); got[0] != want {
		t.Errorf("parsed %+v, want %+v", got[0], want)
	}

	// 2. The same record, stamped a second later on the second read.
	drifted := "[Sat Sep 19 02:27:38 2026] NFS: 34.118.237.153: lost 1 locks"
	if got := NewLostLocks(lostOneLock+"\n", drifted+"\n"); len(got) != 0 {
		t.Errorf("a stale report whose stamp re-rendered read as new: %+v", got)
	}

	// 3. An old report and a new one worded identically.
	again := "[Fri Sep 25 23:56:04 2026] NFS: 34.118.237.153: lost 1 locks"
	if got := NewLostLocks(lostOneLock+"\n", lostOneLock+"\n"+again+"\n"); len(got) != 1 || got[0].Line != again {
		t.Errorf("a new report worded like an old one was not found: %+v", got)
	}

	// 4. dmesg without -T, and lines that must not count.
	raw := "[1234567.891011] NFS: nfs.example.internal: lost 3 locks"
	if got := NewLostLocks("", raw); len(got) != 1 || got[0].Count != 3 || got[0].Server != "nfs.example.internal" {
		t.Errorf("the unstamped-by-date format did not parse: %+v", got)
	}
	if got := NewLostLocks("", idResolver+"\n"+unhandledErr+"\n"); len(got) != 0 {
		t.Errorf("an NFS line that is not a lost-locks report was taken for one: %+v", got)
	}
}

package framework

import (
	"strings"
	"testing"
)

// TestClassifyEntries covers the assertion the large-directory case rests on.
//
// A listing racing deletes is allowed to miss entries, so the count is a record
// and never an assertion. A name that was never created is a directory chunk
// read after it was reused, which is the defect the case exists for.
//
// Steps:
//  1. Classify a listing holding created entries, a silly-rename leftover, a
//     truncated name, an index past the end and an index of zero.
//  2. Assert only the created entries are known.
//  3. Assert a listing missing entries is not itself a finding.
//  4. Assert the directory's own path classifies as unknown, so dropping
//     -mindepth 1 from the listing fails here rather than on a cluster.
func TestClassifyEntries(t *testing.T) {
	const dir = "/mnt/share/data10"
	listing := strings.Join([]string{
		dir + "/e-1",
		dir + "/e-2",
		dir + "/e-100",
		dir + "/.nfs00000000000abc0001",
		dir + "/e-",
		dir + "/e-101",
		dir + "/e-0",
		dir + "/nonsense",
	}, "\n")

	listed, unknown := ClassifyEntries(dir, 100, listing)
	if listed != 8 {
		t.Errorf("counted %d entries, want 8", listed)
	}
	want := map[string]bool{
		".nfs00000000000abc0001": true, "e-": true, "e-101": true, "e-0": true, "nonsense": true,
	}
	if len(unknown) != len(want) {
		t.Fatalf("flagged %v, want %d names", unknown, len(want))
	}
	for _, name := range unknown {
		if !want[name] {
			t.Errorf("flagged %q, which the populate step did create", name)
		}
	}

	// A listing that missed almost everything is lawful and must not be a
	// finding: the deletes were running underneath it.
	if _, unknown := ClassifyEntries(dir, 100, dir+"/e-7\n"); len(unknown) != 0 {
		t.Errorf("a listing that returned one of a hundred entries was flagged: %v", unknown)
	}

	// The directory itself. find without -mindepth 1 emits it first, and a
	// classifier that filtered it out would hide the missing flag until a run.
	if _, unknown := ClassifyEntries(dir, 100, dir+"\n"+dir+"/e-1\n"); len(unknown) != 1 {
		t.Errorf("the directory's own path was not flagged, so a listing missing -mindepth 1 would "+
			"pass here and fail on a cluster: %v", unknown)
	}
}

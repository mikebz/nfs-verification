package framework

import "testing"

// TestCompactRangesNamesWhatIsMissing covers the helper that turns a set of
// missing record numbers into the sentence a case prints when records are lost.
//
// It is unit tested because the message is the evidence. DATA-02's file is
// deleted at teardown and the artifact bundle keeps pod logs rather than the
// share, so whatever this prints is the whole record of what a run saw. An
// off-by-one that merged two runs into one, or split one into two, would change
// what the finding says happened: a single contiguous run means one client's
// appends were overwritten wholesale, and scattered singles mean records were
// lost one at a time.
//
// Steps:
//  1. The empty set says so rather than printing nothing.
//  2. Single numbers, adjacent numbers and a mixture render as expected.
//  3. Unsorted and duplicated input gives the same answer as clean input,
//     because a caller walking a map has neither guarantee.
func TestCompactRangesNamesWhatIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []int
		want string
	}{
		{"nothing missing", nil, "none"},
		{"one record", []int{7}, "7"},
		{"a whole contribution", []int{1, 2, 3, 4, 5}, "1-5"},
		{"two apart", []int{1, 3}, "1, 3"},
		{"a run and a straggler", []int{1, 2, 3, 7}, "1-3, 7"},
		{"runs either side", []int{3, 4, 5, 19, 42, 43}, "3-5, 19, 42-43"},
		{"out of order", []int{9, 1, 10, 3, 2}, "1-3, 9-10"},
		{"duplicated", []int{4, 4, 5, 5, 5, 9}, "4-5, 9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompactRanges(tc.in); got != tc.want {
				t.Errorf("CompactRanges(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

package framework

import (
	"fmt"
	"sort"
	"strings"
)

// CompactRanges writes a set of record numbers as ranges: 1,2,3,7,9,10 becomes
// "1-3, 7, 9-10".
//
// It exists so that a case reporting missing records can say which ones without
// printing hundreds of numbers. The shape of the answer is the finding: a
// contiguous run says one client's writes were overwritten wholesale, and
// scattered singles say records were lost individually. A failure message that
// only gives a count cannot tell those apart, and the file itself is gone by
// the time anyone reads the message.
func CompactRanges(nums []int) string {
	if len(nums) == 0 {
		return "none"
	}
	sorted := append([]int(nil), nums...)
	sort.Ints(sorted)

	var out []string
	start, prev := sorted[0], sorted[0]
	flush := func() {
		if start == prev {
			out = append(out, fmt.Sprint(start))
			return
		}
		out = append(out, fmt.Sprintf("%d-%d", start, prev))
	}
	for _, n := range sorted[1:] {
		if n == prev || n == prev+1 {
			prev = n
			continue
		}
		flush()
		start, prev = n, n
	}
	flush()
	return strings.Join(out, ", ")
}

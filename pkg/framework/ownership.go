package framework

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// OwnerCount is how many entries under a path belong to one uid and gid.
type OwnerCount struct {
	UID   int
	GID   int
	Files int
}

// OwnerCensus is the ownership of a directory tree, counted rather than
// sampled.
//
// SEC-03's destructive half is a volume plugin that chowns a whole share on
// mount. What makes that visible is a count taken before and after: a single
// file read twice says nothing about a walk that got halfway, and the walk is
// the thing that erases what SEC-01 and SEC-02 assert on.
type OwnerCensus struct {
	// Counts is one entry per distinct owner, sorted so that two censuses of
	// the same tree compare equal.
	Counts []OwnerCount
	// Total is counted from the directory itself rather than summed from the
	// lines above, so a stat that failed on one file shows up as a
	// disagreement instead of vanishing.
	Total int
	// Raw is what the pod printed, for the artifact bundle.
	Raw string
}

// String renders a census the way a failure message wants it.
func (c OwnerCensus) String() string {
	if len(c.Counts) == 0 {
		return fmt.Sprintf("%d entries, none owned by anybody the census could read", c.Total)
	}
	parts := make([]string, 0, len(c.Counts))
	for _, e := range c.Counts {
		parts = append(parts, fmt.Sprintf("%d:%d x%d", e.UID, e.GID, e.Files))
	}
	return fmt.Sprintf("%d entries (%s)", c.Total, strings.Join(parts, ", "))
}

// Counted sums the per-owner lines, which is the number that has to agree with
// Total for the census to mean anything.
func (c OwnerCensus) Counted() int {
	n := 0
	for _, e := range c.Counts {
		n += e.Files
	}
	return n
}

// SameOwnership reports whether two censuses describe the same ownership. It
// ignores Raw and Total on purpose: a case that added a file between the two
// readings has changed the total without anything having been chowned.
func (c OwnerCensus) SameOwnership(other OwnerCensus) bool {
	if len(c.Counts) != len(other.Counts) {
		return false
	}
	for i := range c.Counts {
		if c.Counts[i] != other.Counts[i] {
			return false
		}
	}
	return true
}

// ParseOwnerCensus reads what scripts/owner-census.sh prints.
func ParseOwnerCensus(out string) (OwnerCensus, error) {
	census := OwnerCensus{Raw: out}
	sawTotal := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[0] == "TOTAL" {
			n, err := strconv.Atoi(fields[1])
			if err != nil {
				return OwnerCensus{}, fmt.Errorf("census total %q: %w", fields[1], err)
			}
			census.Total, sawTotal = n, true
			continue
		}
		count, err := strconv.Atoi(fields[0])
		if err != nil {
			return OwnerCensus{}, fmt.Errorf("census count %q: %w", fields[0], err)
		}
		uidStr, gidStr, ok := strings.Cut(fields[1], ":")
		if !ok {
			return OwnerCensus{}, fmt.Errorf("census owner %q is not uid:gid", fields[1])
		}
		uid, err := strconv.Atoi(uidStr)
		if err != nil {
			return OwnerCensus{}, fmt.Errorf("census uid %q: %w", uidStr, err)
		}
		gid, err := strconv.Atoi(gidStr)
		if err != nil {
			return OwnerCensus{}, fmt.Errorf("census gid %q: %w", gidStr, err)
		}
		census.Counts = append(census.Counts, OwnerCount{UID: uid, GID: gid, Files: count})
	}
	if !sawTotal {
		// An empty answer from an exec that reported success is a shape this
		// repository has already met once (F-011). Without this check it would
		// read as a directory with nothing in it, and the case would report
		// that no ownership changed.
		return OwnerCensus{}, fmt.Errorf("the ownership census printed no total in %d bytes, so it "+
			"cannot be told apart from a census that never ran: %q", len(out), truncate(out, 200))
	}
	sort.Slice(census.Counts, func(i, j int) bool {
		if census.Counts[i].UID != census.Counts[j].UID {
			return census.Counts[i].UID < census.Counts[j].UID
		}
		return census.Counts[i].GID < census.Counts[j].GID
	})
	return census, nil
}

// OwnerCensusOf counts the ownership of a directory tree from inside a pod.
func (f *Framework) OwnerCensusOf(ctx context.Context, pod, dir, id string) (OwnerCensus, error) {
	script, err := RunScript("owner-census.sh", id, dir)
	if err != nil {
		return OwnerCensus{}, err
	}
	r := f.Sh(ctx, pod, script)
	if r.Err != nil {
		return OwnerCensus{}, fmt.Errorf("counting ownership under %s from %s: %w: %s",
			dir, pod, r.Err, truncate(r.Combined(), 200))
	}
	return ParseOwnerCensus(r.Stdout)
}

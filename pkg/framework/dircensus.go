package framework

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A directory listing racing concurrent deletes is allowed to miss entries.
// It is not allowed to return a name that was never created.
//
// The defect this measures is a use-after-free on directory chunk reuse during
// READDIR under cache pressure. From a client that surfaces as a listing
// returning a name that is not in the directory: a fragment of a reused page
// decoded as an entry. It does not surface as a count being off, because a
// listing racing deletes is supposed to have a count that is off. So the
// assertion is on names and the counts are a record.

// DirCensus is what the large-directory case records.
type DirCensus struct {
	// Created is what the populate step found in the directory afterwards.
	Created int
	// Listed is what one listing returned. A record, never an assertion.
	Listed int
	// Deleted is what the deleting pod reports it removed.
	Deleted int
	// Unknown holds every name the listing returned that was never created.
	// This is the assertion.
	Unknown []string
}

func (c DirCensus) String() string {
	return fmt.Sprintf("created %d, listed %d, deleted %d, unknown %d", c.Created, c.Listed, c.Deleted, len(c.Unknown))
}

// dirEntry matches the names the populate step creates.
var dirEntry = regexp.MustCompile(`^e-([0-9]+)$`)

// ClassifyEntries splits a listing into a count and the names nobody created.
//
// The listing is paths, one per line, as `find <dir> -mindepth 1 -maxdepth 1`
// produces them. -mindepth 1 is not decoration: without it find emits the
// directory itself first, and a classifier fed that path would flag the
// directory as a name nobody created and fail the case on every run. This
// still classifies it as unknown if it appears, rather than filtering it out,
// so dropping the flag fails loudly here instead of quietly on a cluster.
func ClassifyEntries(dir string, created int, listing string) (listed int, unknown []string) {
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		listed++
		name := path.Base(line)
		m := dirEntry.FindStringSubmatch(name)
		if m == nil {
			unknown = append(unknown, name)
			continue
		}
		i, err := strconv.Atoi(m[1])
		if err != nil || i < 1 || i > created {
			unknown = append(unknown, name)
		}
	}
	return listed, unknown
}

// PopulateDir fills a directory with entries and returns how many are there,
// counted from the directory rather than from what the loops believed.
//
// The work is split across shells inside the pod. A round trip per file would
// take longer than the case it feeds and would spend the budget in the API
// server rather than on the share.
func (f *Framework) PopulateDir(ctx context.Context, pod, dir string, count, shards int, timeout time.Duration) (int, error) {
	script, err := RunScript("populate-dir.sh", "populate", dir,
		strconv.Itoa(count), strconv.Itoa(shards))
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main", script)
	if err != nil {
		return 0, fmt.Errorf("populating %s with %d entries: %w", dir, count, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("the populate step reported %q rather than a count", out)
	}
	return n, nil
}

// EntryDeleter is a background delete loop in a pod.
type EntryDeleter struct {
	f     *Framework
	Pod   string
	state string
}

// StartDeletingEntries removes a range of entries in the background, so that a
// listing runs against real deletions rather than after them.
func (f *Framework) StartDeletingEntries(ctx context.Context, pod, dir string, from, to int, id string) (*EntryDeleter, error) {
	if err := CheckScriptID(id); err != nil {
		return nil, err
	}
	d := &EntryDeleter{f: f, Pod: f.Name(pod), state: "/tmp/del-" + id + ".state"}
	script, err := RunScript("delete-entries.sh", id, dir,
		strconv.Itoa(from), strconv.Itoa(to), d.state)
	if err != nil {
		return nil, err
	}
	if _, err := f.C.MustSh(ctx, Namespace, d.Pod, "main", script); err != nil {
		return nil, err
	}
	return d, nil
}

// Removed waits for the deleter to finish and returns how many entries it
// removed. Bounded: a delete loop that never finishes is a finding of its own.
func (d *EntryDeleter) Removed(ctx context.Context, timeout time.Duration) (int, error) {
	var n int
	err := Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		out, err := d.f.C.MustSh(ctx, Namespace, d.Pod, "main",
			"cat "+shellQuote(d.state)+" 2>/dev/null || true")
		if err != nil {
			return false, err
		}
		out = strings.TrimSpace(out)
		if out == "" {
			return false, fmt.Errorf("the deleter in %s has not finished", d.Pod)
		}
		n, err = strconv.Atoi(out)
		if err != nil {
			return false, fmt.Errorf("the deleter in %s reported %q rather than a count", d.Pod, out)
		}
		return true, nil
	})
	return n, err
}

// ListDirEntries returns one listing of a directory, streamed.
//
// find rather than ls, because busybox ls sorts, and sorting a hundred thousand
// entries in a pod with no memory limit set is a way to discover the node's OOM
// killer. -mindepth 1 keeps the directory itself out of the listing.
func (f *Framework) ListDirEntries(ctx context.Context, pod, dir string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r := f.Sh(ctx, pod, fmt.Sprintf("find %s -mindepth 1 -maxdepth 1", shellQuote(dir)))
	if r.Err != nil {
		return r.Stdout, fmt.Errorf("listing %s: %w: %s", dir, r.Err, strings.TrimSpace(r.Stderr))
	}
	return r.Stdout, nil
}

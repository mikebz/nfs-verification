package framework

import "testing"

// TestLooksLikeAUsageError covers the discriminator behind rule 9: a tool that
// does not understand a flag and a filesystem that refuses an operation are
// different answers.
//
// Getting this wrong files "NFSv4.1 does not support hole punching" on evidence
// that is really "this image's applet has no -p", which is a finding nobody can
// act on and which would survive the day the mount becomes 4.2.
//
// Steps:
//  1. Offer what busybox and util-linux say when a flag is unknown.
//  2. Offer what a filesystem says when it understood and refused.
//  3. Assert only the first group reads as a missing tool.
func TestLooksLikeAUsageError(t *testing.T) {
	missing := []string{
		"fallocate: unrecognized option '-p'",
		"BusyBox v1.36.1 (2023-11-07 18:53:09 UTC) multi-call binary.\n\nUsage: fallocate [-l size] [-o offset] file",
		"dd: invalid option -- 'i'",
		"usage: fallocate [-n] [-p] [-o offset] -l length filename",
	}
	for _, out := range missing {
		if !looksLikeAUsageError(out) {
			t.Errorf("a tool rejecting a flag read as a filesystem refusing an operation: %q", out)
		}
	}
	refused := []string{
		"fallocate: fallocate failed: Operation not supported",
		"fallocate: keep size mode (-n option) unsupported",
		"dd: error writing '/mnt/share/probe': Invalid argument",
		"",
	}
	for _, out := range refused {
		if looksLikeAUsageError(out) {
			t.Errorf("a filesystem refusing an operation read as a missing tool, which would report "+
				"blocked where the protocol had actually answered: %q", out)
		}
	}
}

// TestParseMountSpace covers the precheck for a large directory. The export may
// be directory-backed with no per-volume quota, so the real limit is the
// backing filesystem's free inodes rather than the claim's size.
//
// Steps:
//  1. Parse a statfs reading with a known block size.
//  2. Assert bytes and inodes come back in the units the precheck uses.
//  3. Assert a filesystem reporting no inode count is unknown rather than full,
//     since reporting "no inodes left" on a healthy export would block a case
//     for a reason that is not true.
//  4. Assert a zero block size is an error rather than a capacity of zero.
func TestParseMountSpace(t *testing.T) {
	got, err := parseMountSpace("262144 4096 500000\n")
	if err != nil {
		t.Fatalf("parsing statfs: %v", err)
	}
	if got.FreeBytes != 262144*4096 {
		t.Errorf("free bytes %d, want %d", got.FreeBytes, 262144*4096)
	}
	if got.FreeInodes != 500000 || !got.InodesKnown {
		t.Errorf("free inodes %d (known %v), want 500000 known", got.FreeInodes, got.InodesKnown)
	}

	noInodes, err := parseMountSpace("262144 4096 0")
	if err != nil {
		t.Fatalf("parsing statfs without an inode count: %v", err)
	}
	if noInodes.InodesKnown {
		t.Error("a filesystem reporting no inode count read as one with no inodes left")
	}

	for _, bad := range []string{"262144 0 500000", "262144 4096", "a b c", ""} {
		if _, err := parseMountSpace(bad); err == nil {
			t.Errorf("parsed %q, and a capacity of zero from unreadable output reads as a full export", bad)
		}
	}
}

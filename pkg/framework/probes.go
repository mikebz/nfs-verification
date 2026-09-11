package framework

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// A tool the image may not carry is probed before it is used. Its absence
// reports blocked and names the flag that fixes it; it never reads as a pass
// and never as a failure.
//
// The distinction the probes here exist to draw is between a tool that cannot
// do something and a protocol that does not define it. They are not the same
// answer, and filing the first as the second produces a finding nobody can act
// on: "NFSv4.1 does not support hole punching" on evidence that is really "this
// image's applet has no -p".

// ToolProbe is the outcome of asking whether the image can do something.
type ToolProbe struct {
	// OK is true when the tool did the thing.
	OK bool
	// Missing is true when the tool did not understand the request at all,
	// which is a fact about the image and reports blocked.
	Missing bool
	// Output is what the tool said, kept verbatim for the report.
	Output string
}

// usageMarkers are what a tool says when it did not understand an option, as
// opposed to understanding it and refusing. busybox prints its applet usage;
// util-linux prints an invalid-option line. Either way the flag was never
// acted on, so nothing about the filesystem has been learned.
var usageMarkers = []string{
	"unrecognized option", "unrecognized-option", "invalid option", "illegal option",
	"usage:", "busybox", "unknown option",
}

// looksLikeAUsageError reports whether output is a tool rejecting a flag rather
// than a filesystem rejecting an operation.
func looksLikeAUsageError(output string) bool {
	low := strings.ToLower(output)
	for _, m := range usageMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// ProbeDirectIO asks whether this image's dd can open a file with O_DIRECT.
//
// busybox dd does carry iflag=direct and oflag=direct, but both live behind the
// FEATURE_DD_IBS_OBS build option, so whether a given image has them is a
// property of that image's build and not something to assume. The Linux NFS
// client imposes no memory-alignment requirement for direct I/O the way a block
// device does, so a busybox dd buffer is usable if the flag parses at all.
//
// Probed against the path the case will use, because the answer is a property
// of the tool and the filesystem together.
func (f *Framework) ProbeDirectIO(ctx context.Context, pod, path string) ToolProbe {
	r := f.Sh(ctx, pod, fmt.Sprintf(
		"dd if=/dev/zero of=%s bs=4096 count=1 oflag=direct 2>&1; rm -f %s",
		shellQuote(path), shellQuote(path)))
	out := strings.TrimSpace(r.Combined())
	if r.Err == nil && !looksLikeAUsageError(out) {
		return ToolProbe{OK: true, Output: out}
	}
	return ToolProbe{Missing: true, Output: out}
}

// ProbeHolePunch asks whether this image's fallocate parses -p, separately from
// whether any filesystem will honour it.
//
// The two refusals are not the same answer and must not be reported as one. The
// busybox fallocate applet parses -l and -o only: -p appears in its own usage
// comment and not in its option string, so on a stock image the flag is
// rejected by the tool whatever the mount underneath it is. That is rule 8, a
// missing tool, and reports blocked. Only a -p that parses and is then refused
// by the filesystem is rule 9, an operation the protocol does not define.
//
// The probe runs on the pod's own filesystem, never on the share, so that the
// answer is about the applet alone. A local filesystem that refuses the punch
// still proves the flag parsed, which is the whole question here.
func (f *Framework) ProbeHolePunch(ctx context.Context, pod string) ToolProbe {
	const probe = "/tmp/nfsv-punch-probe.dat"
	r := f.Sh(ctx, pod, fmt.Sprintf(
		"dd if=/dev/zero of=%[1]s bs=4096 count=2 2>/dev/null; fallocate -p -o 0 -l 4096 %[1]s 2>&1; "+
			"rc=$?; rm -f %[1]s; exit $rc", shellQuote(probe)))
	out := strings.TrimSpace(r.Combined())
	if looksLikeAUsageError(out) {
		return ToolProbe{Missing: true, Output: out}
	}
	// The flag parsed. Whether this local filesystem honoured it is not the
	// question: the share is where that gets asked.
	return ToolProbe{OK: true, Output: out}
}

// FilesystemSpace is what a mount reports about its own room, in the units the
// precheck for a large directory needs.
type FilesystemSpace struct {
	FreeBytes  int64
	FreeInodes int64
	// InodesKnown is false where the filesystem reports no inode count, which
	// is ordinary on some backing filesystems and is not a reason to fail.
	InodesKnown bool
}

// MountSpace reads free space and free inodes as a pod sees them.
//
// Populating a large directory is inode-hungry, and the export may be
// directory-backed with no per-volume quota, so the real limit is the backing
// filesystem's free inodes rather than the claim's size. A case that discovered
// this halfway through would fail on ENOSPC and point at the server.
func (f *Framework) MountSpace(ctx context.Context, pod, path string) (FilesystemSpace, error) {
	out, err := f.C.MustSh(ctx, Namespace, f.Name(pod), "main",
		fmt.Sprintf("stat -f -c '%%a %%S %%d' %s", shellQuote(path)))
	if err != nil {
		return FilesystemSpace{}, fmt.Errorf("reading the free space of %s: %w", path, err)
	}
	return parseMountSpace(out)
}

// parseMountSpace reads `stat -f -c '%a %S %d'`: blocks available to an
// ordinary user, the fundamental block size, and free file nodes.
func parseMountSpace(out string) (FilesystemSpace, error) {
	fields := strings.Fields(out)
	if len(fields) != 3 {
		return FilesystemSpace{}, fmt.Errorf("unexpected statfs output %q", out)
	}
	nums := make([]int64, 3)
	for i, f := range fields {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return FilesystemSpace{}, fmt.Errorf("unexpected statfs output %q: %w", out, err)
		}
		nums[i] = n
	}
	avail, size, inodes := nums[0], nums[1], nums[2]
	if size <= 0 {
		return FilesystemSpace{}, fmt.Errorf("statfs reported a block size of %d in %q", size, out)
	}
	// Zero free inodes is how a filesystem with no inode table reports itself,
	// and it is indistinguishable from a full one. Treated as unknown rather
	// than as zero, because reporting "no inodes left" on a healthy export
	// would block a case for a reason that is not true.
	return FilesystemSpace{FreeBytes: avail * size, FreeInodes: inodes, InodesKnown: inodes > 0}, nil
}

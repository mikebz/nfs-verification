package framework

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// A lock case is only meaningful on a mount that sends its locks to the server.
//
// nolock, and local_lock=all|flock|posix, tell the Linux NFS client to keep
// locks on the node and never send them (fs/nfs/fs_context.c). On such a mount
// two pods on two nodes both get the lock, every time, correctly, by
// configuration. A cross-node exclusion case that ran there would fail, and the
// failure would point at the server for a mount option's fault.
//
// So the lock cases read the options first and report blocked, naming the
// option. It is the same /proc/mounts read the noac case already needs, and it
// converts a week of misattribution into a line of output.

// LocalLocking reports whether a mount keeps locks of a kind on the client, and
// names the option that does it.
//
// It is per-kind because the options are: local_lock=flock makes flock(2) local
// while byte-range locks still reach the server, and local_lock=posix the other
// way round. local_lock=none is the default and means locks do reach the
// server, so it blocks nothing.
func LocalLocking(options string, kind LockKind) (local bool, option string) {
	for _, o := range strings.Split(options, ",") {
		if o == "nolock" {
			return true, o
		}
		value, ok := strings.CutPrefix(o, "local_lock=")
		if !ok {
			continue
		}
		switch value {
		case "all":
			return true, o
		case "flock":
			if kind == FlockLock {
				return true, o
			}
		case "posix":
			if kind == PosixLock {
				return true, o
			}
		}
	}
	return false, ""
}

// LockMount is what a lock case learns about the mount it is about to lock
// through, before it asserts anything on it.
type LockMount struct {
	Node string
	Line MountLine
	// Local is true when this mount keeps locks of the asked-for kind on the
	// client. Option names the mount option responsible.
	Local  bool
	Option string
	Kind   LockKind
}

// Blocked renders the reason a lock case cannot run here.
func (m LockMount) Blocked() string {
	return fmt.Sprintf("the mount of %s on %s carries %s, so %s locks never reach the server and "+
		"cross-node exclusion does not exist to be tested. Remove the option from the StorageClass or "+
		"the PV's mountOptions; the full option list is %q",
		m.Line.MountPoint, m.Node, m.Option, m.Kind, m.Line.Options)
}

// PodVolumeMount returns the NFS mount on a node backing a persistent volume.
//
// Matched by the volume's name appearing in the mount point, which is how
// kubelet lays both shapes out: kubernetes.io~csi/<pv>/mount for a CSI volume
// and kubernetes.io~nfs/<pv> for an in-tree one. A mount it cannot find is an
// error naming the node and the volume, never an empty answer: a case that
// silently found no mount would assert nothing and pass.
func (f *Framework) PodVolumeMount(ctx context.Context, node, pvName string) (MountLine, error) {
	agent, err := NodeAgent(ctx, f.C)
	if err != nil {
		return MountLine{}, err
	}
	mounts, err := agent.Mounts(ctx, node)
	if err != nil {
		return MountLine{}, fmt.Errorf("reading /proc/mounts on %s: %w", node, err)
	}
	for _, m := range mounts {
		if isNFS(m.FSType) && MountPointHolds(m.MountPoint, pvName) {
			return m, nil
		}
	}
	return MountLine{}, fmt.Errorf("no NFS mount of volume %s on node %s: kubelet mounts a volume at a path "+
		"holding its name, so either the pod is not running there or the volume is not mounted", pvName, node)
}

// isNFS reports whether a /proc/mounts filesystem type is an NFS one.
func isNFS(fsType string) bool { return fsType == "nfs" || fsType == "nfs4" }

// MountPointHolds reports whether a mount path names a volume.
//
// A whole path component, not a substring. Kubelet puts the volume's name in
// one component, and a substring test matches any volume whose name merely
// contains this one. That cuts both ways and both are wrong: a lock case would
// read the options of the wrong mount, and an unmount wait would keep seeing a
// different volume and time out after the one it wanted had gone.
func MountPointHolds(mountPoint, pvName string) bool {
	if pvName == "" {
		return false
	}
	for _, part := range strings.Split(mountPoint, "/") {
		if part == pvName {
			return true
		}
	}
	return false
}

// CheckLockMount reads back the options of the mount a pod locks through and
// says whether locks of that kind reach the server.
//
// Read back, not assumed: an option the driver dropped must fail or block the
// case, not pass it vacuously.
func (f *Framework) CheckLockMount(ctx context.Context, node, pvName string, kind LockKind) (LockMount, error) {
	line, err := f.PodVolumeMount(ctx, node, pvName)
	if err != nil {
		return LockMount{}, err
	}
	local, option := LocalLocking(line.Options, kind)
	return LockMount{Node: node, Line: line, Local: local, Option: option, Kind: kind}, nil
}

// ProcLock is one line of /proc/locks: what the *client* believes it holds.
//
// The difference from what the server believes is the point. A lock the client
// thinks it holds and the server has forgotten is the failure CHAOS-06 exists
// for, and it is visible as a line here with no matching refusal at the server.
type ProcLock struct {
	// Kind is POSIX, FLOCK or OFDLCK: which API took it, which is exactly the
	// distinction the lock cases turn on.
	Kind string
	// Enforcement is always ADVISORY here. Mandatory locking is gone from Linux
	// and never crossed NFS.
	Enforcement string
	Mode        string // READ or WRITE
	// PID is the holder in the node's PID namespace.
	PID int
	// Device is the major:minor of the filesystem and Inode the file's number
	// on it. Without them a lock line says only that *something* on this node
	// holds these bytes: the table covers every file and every process, so an
	// unrelated lock over the same offsets would satisfy an assertion that
	// matched on the range alone.
	Device string
	Inode  int64
	Start  int64
	// End is the last locked byte. EndsAtEOF is true where the kernel printed
	// EOF, which is a lock to the end of the file however long it becomes.
	End       int64
	EndsAtEOF bool
	// Waiting marks a blocked waiter rather than a holder: the kernel prints
	// those with an arrow after the index.
	Waiting bool
}

func (l ProcLock) String() string {
	end := strconv.FormatInt(l.End, 10)
	if l.EndsAtEOF {
		end = "EOF"
	}
	state := "held"
	if l.Waiting {
		state = "waiting"
	}
	return fmt.Sprintf("%s %s %s pid=%d %s:%d [%d..%s] %s",
		l.Kind, l.Enforcement, l.Mode, l.PID, l.Device, l.Inode, l.Start, end, state)
}

// FileID is a file's identity as the kernel prints it in /proc/locks: the
// device its filesystem sits on, and its inode number on that device.
//
// Both halves are needed. Inode numbers are unique per filesystem, not per
// node, and the lock table covers every filesystem mounted there, so an inode
// number alone can collide with an unrelated file somewhere else.
type FileID struct {
	// Device is major:minor, two hex digits each, as /proc/locks prints it.
	Device string
	Inode  int64
}

func (f FileID) String() string { return f.Device + ":" + strconv.FormatInt(f.Inode, 10) }

// Known reports whether enough of the identity was readable to match on.
func (f FileID) Known() bool { return f.Inode != 0 }

// DeviceFromStatDev converts an st_dev as stat reports it into the major:minor
// string /proc/locks prints.
//
// The two describe the same device in different encodings. stat returns the
// packed userspace dev_t the kernel builds in new_encode_dev: the minor's low
// eight bits at the bottom, the major's low twelve above them, and the high
// bits of the minor above that. /proc/locks prints the two halves separately in
// hex. One of them has to be converted before they can be compared, and doing
// it here keeps it in reach of a unit test.
func DeviceFromStatDev(dev uint64) string {
	major := ((dev >> 8) & 0xfff) | ((dev >> 32) &^ uint64(0xfff))
	minor := (dev & 0xff) | ((dev >> 12) &^ uint64(0xff))
	return fmt.Sprintf("%02x:%02x", major, minor)
}

// Covers reports whether this lock is a held POSIX lock of the right mode over
// exactly the given range of the given file.
//
// Every clause earns its place. The kind, because flock and OFD locks are
// different APIs asking different questions. Not waiting, because a client
// queued for a range does not hold it. The file, because a range is not
// identity. And the mode, because a write lock that came back shared would let
// a second writer in, which is the failure the cases using this exist to catch.
//
// A file whose device could not be read is matched on inode alone. That is
// weaker and the caller says so; it is still far better than matching on a
// range, and refusing to match at all would turn an unreadable stat into a
// reported protocol failure.
func (l ProcLock) Covers(id FileID, r LockRange) bool {
	if l.Kind != "POSIX" || l.Waiting || l.Inode != id.Inode {
		return false
	}
	if id.Device != "" && l.Device != id.Device {
		return false
	}
	if !l.modeMatches(r.Mode) {
		return false
	}
	if l.Start != r.Start {
		return false
	}
	if r.Len == 0 {
		return l.EndsAtEOF
	}
	return !l.EndsAtEOF && l.End == r.Start+r.Len-1
}

// modeMatches compares the kernel's READ/WRITE against a range's read/write.
func (l ProcLock) modeMatches(mode string) bool {
	switch strings.ToUpper(mode) {
	case "READ":
		return l.Mode == "READ"
	case "WRITE":
		return l.Mode == "WRITE"
	}
	return false
}

// ParseProcLocks parses the contents of /proc/locks. A pure function with a
// unit test over recorded output, as ParseMounts already is: this is read on a
// node during a failover, and a parser that fails there fails where nothing can
// be debugged.
func ParseProcLocks(contents string) []ProcLock {
	var out []ProcLock
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasSuffix(fields[0], ":") {
			continue
		}
		l := ProcLock{}
		fields = fields[1:]
		// A blocked waiter is printed with an arrow in place of its index.
		if fields[0] == "->" {
			l.Waiting = true
			fields = fields[1:]
		}
		// type, enforcement, mode, pid, major:minor:inode, start, end.
		if len(fields) < 7 {
			continue
		}
		l.Kind, l.Enforcement, l.Mode = fields[0], fields[1], fields[2]
		pid, err := strconv.Atoi(fields[3])
		if err != nil {
			continue
		}
		l.PID = pid
		// major:minor:inode. The inode is the last component; the first two are
		// kept together as the device, since that is how the kernel prints them
		// and nothing here needs them apart.
		device, inodeText, ok := cutLast(fields[4], ":")
		if !ok {
			continue
		}
		inode, err := strconv.ParseInt(inodeText, 10, 64)
		if err != nil {
			continue
		}
		l.Device, l.Inode = device, inode
		start, err := strconv.ParseInt(fields[5], 10, 64)
		if err != nil {
			continue
		}
		l.Start = start
		if fields[6] == "EOF" {
			l.EndsAtEOF = true
		} else {
			end, err := strconv.ParseInt(fields[6], 10, 64)
			if err != nil {
				continue
			}
			l.End = end
		}
		out = append(out, l)
	}
	return out
}

// cutLast splits a string around the last occurrence of sep.
func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// Locks reads a node's own lock table. No tool required: it is a kernel file,
// present on every Linux node with nothing installed.
func (a *Agent) Locks(ctx context.Context, node string) ([]ProcLock, error) {
	out, err := a.ReadFile(ctx, node, "/proc/locks")
	if err != nil {
		return nil, err
	}
	return ParseProcLocks(out), nil
}

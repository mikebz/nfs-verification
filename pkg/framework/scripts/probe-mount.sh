#!/bin/sh
# Attempts an NFS mount from a context the export was never asked to grant, and
# reports what happened in tokens rather than in prose.
#
# This is SEC-05's instrument. It runs inside the node agent's own container,
# never in the host mount namespace: a node image's mount helper is a host
# script, and this repository has already met one that doubled the host mount
# table on every mount (F-005) and another that made every unmount fail
# (F-003). A mount made here is the kernel's, involves no helper, and dies with
# the container.
#
# Usage: sh probe-mount.sh <source> <mountpoint> <options> <relpath>
#
#   source      server:/export path to attempt
#   mountpoint  directory in this container to mount on
#   options     mount options, passed by the caller so the timeouts are named
#               constants in Go rather than literals in here
#   relpath     file under the mount to checksum, or "-" to skip the read
#
# Prints, in order:
#   MOUNT_RC=<n>      the mount's exit status, 0 for granted
#   MOUNT_OUT=<text>  what mount said, on one line
#   SHA=<sum>         the checksum of relpath, only when the mount was granted
#                     and the read succeeded
#   READ_OUT=<text>   what the read said when it did not produce a checksum
#   UMOUNT_RC=<n>     the unmount's exit status, only when something was mounted
set -u

src=$1
mnt=$2
opts=$3
rel=$4

mkdir -p "$mnt"

out=$(mount -t nfs4 -o "$opts" "$src" "$mnt" 2>&1)
rc=$?
echo "MOUNT_RC=$rc"
# Newlines would break the token-per-line contract, and a multi-line mount
# error is exactly what an unusual failure produces.
echo "MOUNT_OUT=$(echo "$out" | tr '\n' ' ')"

if [ "$rc" -ne 0 ]; then
	exit 0
fi

# From here the mount is live, and everything below can be interrupted: the
# case's context can expire while sha256sum is blocked on the export, and
# kubectl exec's SIGTERM arrives with the mount still up. Without this trap the
# script exits before the unmount below and leaves NFS mounted in a container
# that lives for the whole run, which is what ProbeMount promises cannot happen
# and what teardown would then be deleting the export underneath.
#
# The trap is deliberately armed here rather than before the mount: there is
# nothing to clean up until the mount is granted, and an unmount attempt against
# a refusal would only add noise to a case that is reading exit statuses.
cleanup() {
	# Lazy as a last resort. An ordinary unmount can fail while the read that
	# was interrupted still holds the mountpoint; detaching is strictly better
	# than leaving it, because this namespace is the container's own.
	umount "$mnt" 2>/dev/null || umount -l "$mnt" 2>/dev/null
}
trap cleanup EXIT HUP INT TERM

if [ "$rel" != "-" ]; then
	# The checksum is the evidence that a granted mount really reached another
	# workload's bytes, so it is taken as its own command and checked, never as
	# the head of a pipeline whose status belongs to something else (F-015).
	sum=$(sha256sum "$mnt/$rel" 2>&1)
	if [ $? -eq 0 ]; then
		echo "SHA=$(echo "$sum" | cut -d' ' -f1)"
	else
		echo "READ_OUT=$(echo "$sum" | tr '\n' ' ')"
	fi
fi

umount "$mnt" 2>/dev/null
umount_rc=$?
echo "UMOUNT_RC=$umount_rc"

# Disarm only once the unmount the caller is told about has succeeded. If it
# failed, the trap gets its lazy attempt on the way out, and UMOUNT_RC still
# reports the failure the caller needs to see.
if [ "$umount_rc" -eq 0 ]; then
	trap - EXIT HUP INT TERM
fi

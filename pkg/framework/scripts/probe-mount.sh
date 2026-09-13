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
echo "UMOUNT_RC=$?"

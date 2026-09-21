#!/bin/sh
# Reads the comm names of all host processes belonging to a container cgroup.
#
# Usage: sh cgroup-comm.sh <container-id>
#
#   container-id   the container runtime ID (without scheme prefix) to match in /proc/<pid>/cgroup
set -u

cid=$1

if [ -z "$cid" ]; then
	exit 1
fi

for p in /proc/[0-9]*; do
	if grep -q "$cid" "$p/cgroup" 2>/dev/null; then
		cat "$p/comm" 2>/dev/null || true
	fi
done

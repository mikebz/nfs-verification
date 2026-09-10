#!/bin/sh
# Writes one record per second and logs the outcome and time of every attempt.
#
# This is the measuring instrument behind every chaos assertion. One log gives
# all three: time from a fault to the first write committed after it, the count
# of I/O errors, and the set of writes the server acknowledged.
#
# Each record is written with conv=fsync, so a logged success means the server
# committed it. Without that, a success would mean only that the client
# accepted it, which is not what post-COMMIT durability means and not something
# a failover case can assert on.
#
# One write per second is deliberate. The measurements are a 60 to 120 second
# recovery bound at one second of resolution, and a tighter loop would fill the
# share with records without sharpening a single assertion.
#
# Usage: sh write-load.sh <dir> <run-file> <log-file>
#
#   dir         directory on the share to write records into
#   run-file    the workload stops once this is removed
#   log-file    one "OK <index> <epoch>" or "ERR <index> <epoch>" per attempt
#
# The run and log files belong on the pod's own filesystem, never on the share:
# a workload that logged its own progress through the filesystem under test
# would stall exactly when the case most needs to know what happened.
set -u

dir=$1
run=$2
log=$3

if [ "${NFSV_WORKER:-}" = "" ]; then
	: > "$log"
	touch "$run"
	mkdir -p "$dir"
	NFSV_WORKER=1 setsid sh "$0" "$@" >/dev/null 2>&1 </dev/null &
	echo launched
	exit 0
fi

i=0
while [ -f "$run" ]; do
	i=$((i + 1))
	if dd if=/dev/zero of="$dir/rec-$i" bs=4096 count=1 conv=fsync >/dev/null 2>&1; then
		echo "OK $i $(date +%s)" >> "$log"
	else
		echo "ERR $i $(date +%s)" >> "$log"
	fi
	sleep 1
done

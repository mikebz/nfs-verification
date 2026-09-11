#!/bin/sh
# Writes one record per second and logs the outcome and time of every attempt.
#
# This is the measuring instrument behind every chaos assertion. One log gives
# all three: time from a fault to the first write committed after it, the count
# of I/O errors, and the set of writes the server acknowledged.
#
# Each record is written with conv=fsync unless the caller asks otherwise, so a
# logged success means the server committed it. Without that, a success would
# mean only that the client accepted it, which is not what post-COMMIT
# durability means and not something a failover case can assert on.
#
# The negative durability case is the one that asks otherwise. It writes without
# the fsync deliberately, so that what survives a crash is what the client
# happened to flush rather than what the server acknowledged, and asserts only
# that nothing came back wrong.
#
# Each record holds a pattern derived from its own index, so a sweep afterwards
# can compute the expected value of any byte from its offset without keeping a
# copy. An all-zero record would make a torn write indistinguishable from a hole.
#
# One write per second is deliberate. The measurements are a 60 to 120 second
# recovery bound at one second of resolution, and a tighter loop would fill the
# share with records without sharpening a single assertion.
#
# Usage: sh write-load.sh <dir> <run-file> <log-file> <record-bytes> <fsync>
#
#   dir            directory on the share to write records into
#   run-file       the workload stops once this is removed
#   log-file       one "OK <index> <epoch>" or "ERR <index> <epoch>" per attempt
#   record-bytes   the length of one record
#   fsync          "fsync" to commit each record, anything else to leave it to
#                  the client
#
# The run and log files belong on the pod's own filesystem, never on the share:
# a workload that logged its own progress through the filesystem under test
# would stall exactly when the case most needs to know what happened.
set -u

dir=$1
run=$2
log=$3
bytes=$4
fsync=$5

if [ "$fsync" = "fsync" ]; then
	conv="conv=fsync"
else
	conv=""
fi

# The pattern is staged on the pod's own filesystem, then copied in one aligned
# block. Feeding dd from a pipe would let a short read produce a record the
# sweep afterwards reports as truncated when nothing truncated it.
pattern="$log.pattern"

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
	yes "rec-$i" | head -c "$bytes" > "$pattern"
	if dd if="$pattern" of="$dir/rec-$i" bs="$bytes" count=1 $conv >/dev/null 2>&1; then
		echo "OK $i $(date +%s)" >> "$log"
	else
		echo "ERR $i $(date +%s)" >> "$log"
	fi
	sleep 1
done

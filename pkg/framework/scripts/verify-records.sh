#!/bin/sh
# Reports a verdict per record: whether it is there, whole, and still says what
# was written.
#
# This replaces an existence check. "Is the file non-empty" is enough for a
# failover case asserting that nothing was lost; it is not enough for the
# durability pair, where a truncated record is an acceptable outcome and a wrong
# byte at a correct offset is corruption. Those two must not collapse into one
# answer.
#
# Each record holds the pattern the workload wrote, derived from its own index,
# so the expected value of any byte is computable here from its offset without
# keeping a copy of the original.
#
# One invocation checks the whole set. A round trip per record would take longer
# than the outage a case is measuring, and would be running while the mount is
# still recovering.
#
# Usage: sh verify-records.sh <dir> <record-bytes> <scratch> <index>...
#
#   dir            directory on the share holding rec-<index> files
#   record-bytes   the full length of one record
#   scratch        a path on the pod's own filesystem for the expected bytes
#   index...       the records to check
#
# Prints one line per record: "<verdict> <index> <offset>", where verdict is
# correct, absent, short or wrong. For "wrong" the offset is the first differing
# byte as cmp reports it, counting from 1, or -1 where cmp said nothing readable.
# For the others it is the observed length.
#
# A record whose length cannot be read at all prints "unreadable <index> -1",
# which is none of the four verdicts and lands in the parser's unreadable-lines
# list. That is deliberate: a read that failed is not the same fact as a record
# that is gone, and reporting it as "absent" would file a harness or mount
# problem as data loss.
#
# The four verdicts are exhaustive over observed length. Below the full length
# is absent or short, exactly the full length is correct or wrong, and above the
# full length is wrong whatever the prefix says: a record that grew holds a byte
# nobody wrote.
set -u

dir=$1
bytes=$2
scratch=$3
shift 3

for i in "$@"; do
	f="$dir/rec-$i"
	if [ ! -f "$f" ]; then
		echo "absent $i 0"
		continue
	fi
	# wc's own status, taken before its output is normalized. A pipeline would
	# report tr's status instead, turning an unreadable record into a length of
	# zero and then into "absent".
	if ! size=$(wc -c < "$f" 2>/dev/null); then
		echo "unreadable $i -1"
		continue
	fi
	size=$(echo "$size" | tr -d ' ')
	if [ -z "$size" ]; then
		echo "unreadable $i -1"
		continue
	fi
	if [ "$size" -eq 0 ]; then
		echo "absent $i 0"
		continue
	fi
	if [ "$size" -gt "$bytes" ]; then
		echo "wrong $i $size"
		continue
	fi
	# The expected bytes, regenerated from the index rather than stored.
	yes "rec-$i" | head -c "$size" > "$scratch"
	if cmp -s "$scratch" "$f"; then
		if [ "$size" -eq "$bytes" ]; then
			echo "correct $i $size"
		else
			echo "short $i $size"
		fi
		continue
	fi
	# busybox cmp says "differ: char N, line M" and GNU cmp "differ: byte N,
	# line M". Either way the first number after differ: is the offset.
	off=$(cmp "$scratch" "$f" 2>&1 | sed -n 's/.*differ: [a-z]* \([0-9][0-9]*\).*/\1/p' | head -n 1)
	[ -n "$off" ] || off=-1
	echo "wrong $i $off"
done

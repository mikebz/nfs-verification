#!/bin/sh
# Reports which of the given records are absent or empty on the share.
#
# One invocation checks the whole set. A round trip per record would take
# longer than the outage a chaos case is measuring, and would be running while
# the mount is still recovering.
#
# An empty record counts as missing: a zero-length file is not a committed
# 4KiB write.
#
# Usage: sh missing-records.sh <dir> <index>...
#
# Prints the index of every record that is not there, one per line, and nothing
# at all when every record survived.
set -u

dir=$1
shift

for i in "$@"; do
	[ -s "$dir/rec-$i" ] || echo "$i"
done

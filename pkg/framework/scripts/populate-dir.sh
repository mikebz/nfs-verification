#!/bin/sh
# Creates <count> entries named e-1 through e-<count> in a directory, and
# reports how many it made.
#
# The work is split across <shards> concurrent shells inside the one pod. A
# round trip per file would take longer than the case it feeds, and driving
# 100k creates over exec would spend the whole budget in the API server rather
# than on the share.
#
# Entries are empty files. The case this feeds is about what a listing returns
# while entries are being removed underneath it, which is a property of
# directory chunks and not of file content.
#
# Usage: sh populate-dir.sh <dir> <count> <shards>
#
#   dir      directory on the share to fill
#   count    how many entries to create, named e-1 .. e-<count>
#   shards   how many concurrent shells to split the work across
#
# Prints the number of entries present when it finishes, counted from the
# directory itself rather than from what the loops believed they did.
set -u

dir=$1
count=$2
shards=$3

mkdir -p "$dir"

shard=0
while [ "$shard" -lt "$shards" ]; do
	(
		i=$((shard + 1))
		while [ "$i" -le "$count" ]; do
			: > "$dir/e-$i"
			i=$((i + shards))
		done
	) &
	shard=$((shard + 1))
done
wait

# Counted from the directory, not from the loops: a create that failed silently
# would otherwise be reported as an entry the case then goes looking for.
find "$dir" -mindepth 1 -maxdepth 1 | wc -l

#!/bin/sh
# Removes entries e-<from> through e-<to> from a directory, one at a time, and
# reports how many it removed.
#
# This is the other half of the large-directory case: a listing has to be racing
# real deletions, not running after them. It backgrounds itself so the listing
# can start while this is still going.
#
# Usage: sh delete-entries.sh <dir> <from> <to> <state-file>
#
#   dir         directory on the share
#   from, to    inclusive range of entry indices to remove
#   state-file  the count of entries removed is written here when it finishes
#
# The state file belongs on the pod's own filesystem, never on the share: a
# deleter that reported its own progress through the directory it is emptying
# would be adding entries to the thing under test.
set -u

dir=$1
from=$2
to=$3
state=$4

if [ "${NFSV_WORKER:-}" = "" ]; then
	: > "$state"
	NFSV_WORKER=1 setsid sh "$0" "$@" >/dev/null 2>&1 </dev/null &
	echo launched
	exit 0
fi

n=0
i=$from
while [ "$i" -le "$to" ]; do
	if rm -f "$dir/e-$i" 2>/dev/null; then
		n=$((n + 1))
	fi
	i=$((i + 1))
done
echo "$n" > "$state"

#!/bin/sh
# Writes a payload to a file and keeps the descriptor open until told to close.
#
# The negative close-to-open case needs a writer that has written but has not
# closed, which an ordinary exec cannot produce: the shell exits and the file
# closes with it. So this backgrounds itself and holds the descriptor.
#
# Usage: sh hold-open-write.sh <path> <payload> <run-file> <state-file>
#
#   path        file on the share to write
#   payload     bytes to write, small enough for one write
#   run-file    the writer closes once this is removed
#   state-file  the writer reports "open" then "closed" here
#
# The run and state files belong on the pod's own filesystem, never on the
# share: a writer that reported its own progress through the filesystem under
# test could not tell a stalled harness from stalled storage.
set -u

path=$1
payload=$2
run=$3
state=$4

# First pass: set up, relaunch detached, and return so the caller is not
# blocked. The marker is what tells the second pass it is the worker.
if [ "${NFSV_WORKER:-}" = "" ]; then
	: > "$state"
	touch "$run"
	NFSV_WORKER=1 setsid sh "$0" "$@" >/dev/null 2>&1 </dev/null &
	echo launched
	exit 0
fi

exec 8> "$path"
printf '%s' "$payload" >&8
echo open > "$state"

# Ask for a fraction of a second and fall back to a whole one, so a close is
# prompt where busybox carries fractional sleep and still works where it does
# not.
while [ -f "$run" ]; do
	sleep 0.2 2>/dev/null || sleep 1
done

exec 8>&-
echo closed > "$state"

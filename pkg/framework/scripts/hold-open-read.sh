#!/bin/sh
# Opens a file for reading, keeps that one descriptor, and reads a chunk
# through it on request.
#
# The silly-rename case needs a reader that held a descriptor from before an
# unlink and can still read through it afterwards. An ordinary exec cannot
# produce that: the shell exits and the descriptor closes with it, and
# reopening the path after an unlink is a different question from reading
# through the descriptor that was already open.
#
# Each request reads the next chunk from the descriptor's current offset, which
# is a real read through the held descriptor rather than a reopen by path.
#
# Usage: sh hold-open-read.sh <path> <chunk-bytes> <run-file> <state-file> <request-file> <result-file>
#
#   path          file on the share to hold open
#   chunk-bytes   how many bytes one request reads
#   run-file      the reader closes once this is removed
#   state-file    the reader reports "open" then "closed" here
#   request-file  creating this asks for one chunk; the reader removes it
#   result-file   the chunk read, or "READ-FAILED" and the error
#
# The run, state, request and result files belong on the pod's own filesystem,
# never on the share: a reader that took its instructions through the file
# being unlinked underneath it could not tell a stalled harness from stalled
# storage.
set -u

path=$1
chunk=$2
run=$3
state=$4
request=$5
result=$6

if [ "${NFSV_WORKER:-}" = "" ]; then
	: > "$state"
	touch "$run"
	rm -f "$request" "$result"
	NFSV_WORKER=1 setsid sh "$0" "$@" >/dev/null 2>&1 </dev/null &
	echo launched
	exit 0
fi

exec 7< "$path"
echo open > "$state"

while [ -f "$run" ]; do
	if [ -f "$request" ]; then
		rm -f "$request"
		# Read from fd 7's current offset. The descriptor was opened before
		# anything happened to the name, and is never reopened.
		if dd bs="$chunk" count=1 <&7 > "$result.part" 2> "$result.err"; then
			mv "$result.part" "$result"
		else
			{ printf 'READ-FAILED '; cat "$result.err"; } > "$result"
		fi
	fi
	sleep 0.2 2>/dev/null || sleep 1
done

exec 7<&-
echo closed > "$state"

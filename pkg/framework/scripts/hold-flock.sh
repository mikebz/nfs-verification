#!/bin/sh
# Takes an exclusive whole-file lock and holds it until told to let go.
#
# Locks are advisory, and they are visible across clients only because every
# client serializes through the one server. Nothing here asserts more than that.
#
# Usage: sh hold-flock.sh <path> <run-file> <state-file>
#
#   path        file on the share to lock
#   run-file    the holder releases once this is removed
#   state-file  the holder reports "held", "released" or "failed" here
set -u

path=$1
run=$2
state=$3

if [ "${NFSV_WORKER:-}" = "" ]; then
	: > "$state"
	touch "$run"
	NFSV_WORKER=1 setsid sh "$0" "$@" >/dev/null 2>&1 </dev/null &
	echo launched
	exit 0
fi

exec 9>>"$path"

# A blocking acquire with no -w: busybox flock has no timeout flag, so the
# bound lives in the caller, which polls the state file.
if flock -x 9; then
	echo held > "$state"
	while [ -f "$run" ]; do
		sleep 0.2 2>/dev/null || sleep 1
	done
	echo released > "$state"
else
	echo failed > "$state"
fi

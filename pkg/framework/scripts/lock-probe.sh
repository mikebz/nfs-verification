#!/bin/sh
# Attempts a new exclusive whole-file lock once per second and logs whether it
# was granted.
#
# This is the instrument behind the grace assertion. During grace a server
# accepts reclaims of state that existed before the crash and refuses everything
# new, so a lock this probe has never held must not be granted while grace is in
# force. The assertion is on grants, never on refusals: during an outage every
# attempt fails for the ordinary reason that the server is not there, so a
# refusal carries no guarantee and a grant carries all of it.
#
# Each attempt runs in a subshell and drops what it was granted when that
# subshell exits. A probe that kept the lock would change what every later
# attempt means.
#
# Usage: sh lock-probe.sh <path> <run-file> <log-file> <wait-seconds> <bound-seconds>
#
#   path           file on the share to attempt a lock on
#   run-file       the probe stops once this is removed
#   log-file       one "OK <index> <epoch>" or "ERR <index> <epoch>" per attempt,
#                  where OK means the lock was granted
#   wait-seconds   how long one attempt keeps retrying before giving up
#   bound-seconds  hard bound on one attempt, in case the client blocks in the
#                  kernel rather than returning
#
# The run and log files belong on the pod's own filesystem, never on the share:
# a probe that reported its own progress through the filesystem under test would
# stall exactly when the case most needs to know what happened.
set -u

path=$1
run=$2
log=$3
wait=$4
bound=$5

if [ "${NFSV_WORKER:-}" = "" ]; then
	: > "$log"
	touch "$run"
	# Create the target while the server is healthy. A lock attempt during
	# grace must be an attempt at a lock and nothing else; if the file did not
	# exist yet, the attempt would also be a create, and a refusal would not
	# say which of the two the server turned down.
	: >> "$path"
	NFSV_WORKER=1 setsid sh "$0" "$@" >/dev/null 2>&1 </dev/null &
	echo launched
	exit 0
fi

# A blocked NFS client can sit in the kernel rather than returning the server's
# retryable error, so the attempt gets a hard bound as well as its own wait.
#
# The wait is a retry loop around flock -n rather than flock -w: busybox flock
# has no -w, and passing it makes every attempt fail on a usage error, which
# reads as a refusal and makes this probe's assertion vacuous. -n is carried by
# busybox and by util-linux alike, so one loop serves both images.
if command -v timeout >/dev/null 2>&1; then
	bounded="timeout $bound"
else
	bounded=""
fi

i=0
while [ -f "$run" ]; do
	i=$((i + 1))
	if $bounded sh -c '
		end=$(( $(date +%s) + $2 ))
		while :; do
			( exec 9>>"$1"; flock -n -x 9 ) && exit 0
			[ "$(date +%s)" -lt "$end" ] || exit 1
			sleep 1
		done' probe "$path" "$wait" >/dev/null 2>&1; then
		echo "OK $i $(date +%s)" >> "$log"
	else
		echo "ERR $i $(date +%s)" >> "$log"
	fi
	sleep 1
done

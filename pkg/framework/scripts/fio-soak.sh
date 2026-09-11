#!/bin/sh
# Runs one fio job against a directory this pod owns alone, in the background,
# and reports its exit status when it finishes.
#
# One pod, one directory. Two pods writing one file with verification on would
# report mismatches that are the harness's fault rather than the storage's,
# which DATA-01 already established; cross-pod interference is DATA-01's and
# SCALE-06's job, not this one's.
#
# verify_fatal stops the job at the mismatch, so the offending offset is in the
# output. Without it fio continues and the report is a count with no location.
#
# It backgrounds itself because the run is an hour: an exec stream held open for
# that long is a connection to lose, and losing it would lose the result.
#
# Usage: sh fio-soak.sh <dir> <runtime-seconds> <files> <size-range> <read-percent> <output-file> <state-file>
#
#   dir            directory on the share this pod owns alone
#   runtime        how long the job runs, in seconds
#   files          how many files the job spreads itself over
#   size-range     individual file sizes, as an fio range such as 4k-1g
#   read-percent   share of the mix that is reads
#   output-file    fio's JSON report lands here
#   state-file     "done <exit-status>" is written here when fio finishes
#
# The output and state files belong on the pod's own filesystem, never on the
# share: a job that reported its own progress through the filesystem under test
# could not tell a stalled harness from stalled storage.
set -u

dir=$1
runtime=$2
files=$3
sizes=$4
readpct=$5
out=$6
state=$7

if [ "${NFSV_WORKER:-}" = "" ]; then
	: > "$state"
	mkdir -p "$dir"
	# setsid where the image has it. This is the one script that runs in an
	# image the operator chose rather than in the tools image, so the applet
	# cannot be assumed; a plain background job is enough here, since the exec
	# that starts it has no controlling terminal to send a hangup.
	if command -v setsid >/dev/null 2>&1; then
		NFSV_WORKER=1 setsid sh "$0" "$@" >/dev/null 2>&1 </dev/null &
	else
		NFSV_WORKER=1 sh "$0" "$@" >/dev/null 2>&1 </dev/null &
	fi
	echo launched
	exit 0
fi

fio --name=soak \
	--directory="$dir" \
	--rw=randrw \
	--rwmixread="$readpct" \
	--filesize="$sizes" \
	--nrfiles="$files" \
	--ioengine=psync \
	--bs=64k \
	--verify=crc32c \
	--verify_fatal=1 \
	--do_verify=1 \
	--time_based=1 \
	--runtime="$runtime" \
	--output-format=json \
	--output="$out" >/dev/null 2>&1
echo "done $?" > "$state"

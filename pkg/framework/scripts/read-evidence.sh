#!/bin/sh
# Reads one file out of a pod for the artifact bundle: its bytes on stdout and
# nothing else at all.
#
# The bundle keeps the files a case names as what it argues from, and this is
# how they leave the pod. Nothing is printed around the contents, because the
# caller writes stdout to the bundle byte for byte: a banner or a progress line
# here becomes a corrupted artifact that still looks like a file.
#
# A path that is not a regular file exits non-zero with a reason rather than
# returning nothing. `head` on a directory is an error on some implementations
# and an empty success on others, and an empty success would put a zero byte
# file in the bundle and call it the evidence.
#
# The caller asks for one byte more than it intends to keep, so that a file
# exactly at the cap is whole and one byte over it is visibly short. This script
# does not know which of those happened and does not need to.
#
# Usage: sh read-evidence.sh <path> <limit>
#
#   path    the file to read, on the share or on the pod's own filesystem
#   limit   the most bytes to return
set -u

path=$1
limit=$2

if [ ! -f "$path" ]; then
	echo "not a regular file" >&2
	exit 3
fi

head -c "$limit" "$path"

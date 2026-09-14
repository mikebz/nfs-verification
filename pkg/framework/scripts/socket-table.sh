#!/bin/sh
# Prints the kernel's TCP socket tables, one family at a time, each tagged with
# whether it could be read.
#
# The status has to be per family. /proc/net/tcp6 does not exist on a kernel
# built without IPv6, which is ordinary and must not stop a case; a container
# the suite cannot read inside is not ordinary and must. A single command
# ending in `cat /proc/net/tcp6` cannot tell those apart: its exit status is the
# last cat's, so a missing tcp6 looks like a failure and a failed tcp read is
# masked whenever tcp6 happens to succeed.
#
# Usage: sh socket-table.sh
#
# Prints, for each of /proc/net/tcp and /proc/net/tcp6, in order:
#   ==FAMILY <path>            the file this block is about
#   ==STATUS ok|absent|error   whether it was read, with the error text if not
#   <the file's contents>      only when the status is ok
#
# The script itself always exits 0. What went wrong is in the status lines,
# where the caller can attribute it, rather than in an exit code that cannot
# say which family it belonged to.
set -u

for f in /proc/net/tcp /proc/net/tcp6; do
	echo "==FAMILY $f"
	if [ ! -e "$f" ]; then
		echo "==STATUS absent"
		continue
	fi
	# Captured rather than streamed, so that a read which fails partway does
	# not emit half a table above its own error.
	if body=$(cat "$f" 2>&1); then
		echo "==STATUS ok"
		echo "$body"
	else
		echo "==STATUS error $(echo "$body" | tr '\n' ' ')"
	fi
done

exit 0

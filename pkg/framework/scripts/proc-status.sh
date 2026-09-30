#!/bin/sh
# Prints what the node says about one process: its status file, which carries
# its name and capability masks, and its cgroup, which says which container it
# runs in.
#
# Nothing here decides anything. The caller checks the name against the process
# it meant to read and the cgroup against the pod's containers, because a pid is
# only a number: between naming a process and reading it, the process can exit
# and the number can be handed to something else.
#
# Usage: sh proc-status.sh procroot pid
#
#   procroot  the proc filesystem to read, /proc in the node agent, whose pid
#             namespace is the node's. A test passes a fixture directory so that
#             this file, and not a copy of it, is what gets exercised on a
#             workstation.
#   pid       the process, as the node numbers it.
#
# Prints, in order:
#   ==STATUS                  the status file follows, until ==ENDSTATUS
#   ==ENDSTATUS
#   ==CGROUP                  the cgroup file follows, until ==ENDCGROUP
#   ==ENDCGROUP
#   ==ERROR <what> <err>      in place of a section that could not be read
#   ==END                     the reader ran to completion
#
# The exit status is always 0. What could not be read is in the output, where
# the caller can say which file it was, rather than in a status that cannot.
set -u

root=$1
pid=$2

case "$pid" in
'' | *[!0-9]*)
	echo "==ERROR pid $pid is not a process id"
	echo "==END"
	exit 0
	;;
esac

d="$root/$pid"

# Captured rather than streamed, so a read that fails partway does not emit
# half a file above its own error.
if body=$(cat "$d/status" 2>&1); then
	echo "==STATUS"
	echo "$body"
	echo "==ENDSTATUS"
else
	echo "==ERROR status $(echo "$body" | tr '\n' ' ')"
fi

if body=$(cat "$d/cgroup" 2>&1); then
	echo "==CGROUP"
	echo "$body"
	echo "==ENDCGROUP"
else
	echo "==ERROR cgroup $(echo "$body" | tr '\n' ' ')"
fi

echo "==END"
exit 0

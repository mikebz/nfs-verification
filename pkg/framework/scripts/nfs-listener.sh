#!/bin/sh
# Prints the facts needed to name the process serving NFS in this container:
# the kernel's TCP socket tables, and which process holds which socket.
#
# Nothing here decides anything. The caller joins the listening socket's inode
# to the process holding it, because that join is the part worth a unit test and
# because a shell that picks a process to SIGKILL is a shell nobody can review.
#
# Usage: sh nfs-listener.sh [procroot] [inode...]
#
#   procroot  the proc filesystem to read, /proc in a container. A test passes a
#             fixture directory so that this file, and not a copy of it, is what
#             gets exercised on a workstation.
#   inode     socket inodes of interest. With none, every process holding any
#             socket is reported, which is what a reader of a single container
#             wants. With some, only the processes holding one of them are, which
#             is what a scan of a whole node wants: a node runs hundreds of
#             processes and all but one of them are noise here.
#
# Prints, in order:
#   ==NOREADLINK              only when readlink is missing, so that "no process
#                             holds the socket" can be told from "this image
#                             cannot say which process holds it"
#   ==TABLE <path>            a socket table follows, until ==ENDTABLE
#   ==ENDTABLE
#   ==TABLEERROR <path> <err> the table exists and could not be read
#   ==PROC <pid> <comm>       comm runs to the end of the line
#   ==ARGV0 <pid> <argv0>     argv[0], where /proc/<pid>/cmdline has one
#   ==FD <pid> socket:[inode] one line per socket the process holds
#   ==END                     the reader ran to completion
#
# Both address families are read. A server bound to IPv6 accepts IPv4 clients
# and appears in tcp6 alone: the reference deployment listens on 2049 in
# /proc/net/tcp6 and not in /proc/net/tcp, so a reader of one file finds no NFS
# server at all. A family whose file does not exist is ordinary, on a kernel
# built without IPv6, and is skipped rather than reported.
#
# The exit status is always 0. What could not be read is in the output, where
# the caller can attribute it to a family or a process, rather than in a status
# that cannot say which.
set -u

root=${1:-/proc}
[ $# -gt 0 ] && shift
wanted=$*

command -v readlink >/dev/null 2>&1 || echo "==NOREADLINK"

for f in "$root/net/tcp" "$root/net/tcp6"; do
	[ -e "$f" ] || continue
	# Captured rather than streamed, so a read that fails partway does not
	# emit half a table above its own error.
	if body=$(cat "$f" 2>&1); then
		echo "==TABLE $f"
		echo "$body"
		echo "==ENDTABLE"
	else
		echo "==TABLEERROR $f $(echo "$body" | tr '\n' ' ')"
	fi
done

for d in "$root"/[0-9]*; do
	[ -d "$d" ] || continue
	pid=${d##*/}
	# A process that exits mid-scan is ordinary and not worth reporting: it
	# cannot be the one still holding a listening socket.
	comm=$(cat "$d/comm" 2>/dev/null) || continue
	[ -n "$comm" ] || continue

	# The fds are collected before anything is printed, so that a filtered
	# scan says nothing at all about the processes it is not interested in.
	found=
	for fd in "$d"/fd/*; do
		# An fd is a symlink to something that does not exist as a path, so
		# -L is the test that holds and -e is the one that does not.
		[ -L "$fd" ] || continue
		link=$(readlink "$fd" 2>/dev/null) || continue
		case "$link" in
		socket:\[*\]) ;;
		*) continue ;;
		esac
		if [ -n "$wanted" ]; then
			inode=${link#socket:\[}
			inode=${inode%\]}
			match=
			for w in $wanted; do
				[ "$w" = "$inode" ] && match=yes && break
			done
			[ -n "$match" ] || continue
		fi
		found="$found==FD $pid $link
"
	done
	[ -n "$found" ] || [ -z "$wanted" ] || continue

	echo "==PROC $pid $comm"
	argv0=$(tr '\000' '\n' <"$d/cmdline" 2>/dev/null | head -1)
	[ -z "$argv0" ] || echo "==ARGV0 $pid $argv0"
	[ -z "$found" ] || printf '%s' "$found"
done

echo "==END"
exit 0

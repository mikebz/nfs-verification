#!/bin/sh
# Prints the tail of every regular file a process names on its own command
# line, read through that process's root, so the caller can find the log the
# server writes without knowing how this server spells its log option.
#
# Nothing here decides which file is a log. The reference server names three
# (its log, its pid file and its configuration) and only the caller can tell
# them apart, by whether their lines carry timestamps it can read. See F-030 in
# docs/findings.md for why the files are found this way rather than from the
# descriptors the process holds open.
#
# Usage: sh server-log.sh procroot pid bytes
#
#   procroot  the proc filesystem to read, /proc in the node agent, whose pid
#             namespace is the node's. A test passes a fixture directory so that
#             this file, and not a copy of it, is what gets exercised on a
#             workstation.
#   pid       the process, as the node numbers it.
#   bytes     how much of the end of each file to print.
#
# An argument names a file when it is an absolute path, or when the part after
# its first "=" is one, so that "--log=/var/log/x" counts as well as "-L /x".
#
# Prints, in order:
#   ==FILE <size> <mtime> <path>  the last <bytes> of the file follow, until
#                                 ==ENDFILE; size in bytes, mtime in seconds
#                                 since the epoch, path as the process names it
#   ==ENDFILE
#   ==ERROR <what> <err>          something that could not be read
#   ==END                         the reader ran to completion
#
# The exit status is always 0. What could not be read is in the output, where
# the caller can say which file it was, rather than in a status that cannot.
set -u

root=$1
pid=$2
bytes=$3

for v in "$pid" "$bytes"; do
	case "$v" in
	'' | *[!0-9]*)
		echo "==ERROR args $v is not a number"
		echo "==END"
		exit 0
		;;
	esac
done

d="$root/$pid"
if ! args=$(tr '\0' '\n' <"$d/cmdline" 2>&1); then
	echo "==ERROR cmdline $(echo "$args" | tr '\n' ' ')"
	echo "==END"
	exit 0
fi

seen=" "
echo "$args" | while IFS= read -r arg; do
	for p in "$arg" "${arg#*=}"; do
		case "$p" in
		/*) ;;
		*) continue ;;
		esac
		case "$seen" in
		*" $p "*) continue ;;
		esac
		seen="$seen$p "
		f="$d/root$p"
		[ -f "$f" ] || continue
		# date -r reads a file's mtime on busybox, GNU and BSD alike, where
		# stat's format flags differ between all three.
		size= mtime=
		if ! size=$(wc -c <"$f" 2>&1) || ! mtime=$(date -r "$f" +%s 2>&1); then
			echo "==ERROR $p $(echo "$size $mtime" | tr '\n' ' ')"
			continue
		fi
		echo "==FILE $(echo "$size" | tr -d ' ') $mtime $p"
		tail -c "$bytes" "$f"
		# A file that does not end in a newline would otherwise put the end
		# marker on the end of its last line.
		echo
		echo "==ENDFILE"
	done
done

echo "==END"
exit 0

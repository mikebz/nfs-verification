#!/bin/sh
# Appends <count> records to <path>, keeping the descriptor open across the loop.
#
# The loop redirect opens the file once with O_APPEND, so every record is
# written through that one descriptor. This models a real log appender and is
# the harder case: reopening per record would revalidate file size each time.
#
# Usage: sh append-records.sh <name> <count> <path>
#
#   name    pod/writer identifier embedded in the record
#   count   number of records to append
#   path    target file path on the share
set -eu

name=$1
count=$2
path=$3

{
	i=0
	while [ "$i" -lt "$count" ]; do
		i=$((i + 1))
		printf 'record-from-%s-%04d\n' "$name" "$i"
	done
} >> "$path"
echo done

#!/bin/sh
# Counts how many entries under a directory belong to each uid and gid.
#
# SEC-03 asks whether a pod declaring fsGroup rewrote the ownership of files
# that were already there. That question needs a count, not an impression: one
# file sampled before and after proves nothing about a walk that chowned half a
# directory, and reading every file back over exec would spend the budget in
# the API server.
#
# Usage: sh owner-census.sh <dir>
#
#   dir   directory on the share to count under, recursively
#
# Prints one line per distinct owner, "<count> <uid>:<gid>", then a final
# "TOTAL <n>" counted from the directory itself. The total is separate on
# purpose: an owner line missing because stat failed on one file is invisible
# otherwise, and the totals then disagree.
set -u

dir=$1

# stat -c is the GNU and busybox spelling and is what every other reader in
# this suite uses. The pipeline's exit status would be sort's, so the total
# below is counted independently rather than inferred from this succeeding.
find "$dir" -mindepth 1 -exec stat -c '%u:%g' {} + 2>/dev/null | sort | uniq -c | while read -r count owner; do
	echo "$count $owner"
done

echo "TOTAL $(find "$dir" -mindepth 1 | wc -l)"

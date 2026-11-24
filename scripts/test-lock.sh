#!/bin/sh
# Runs a command under the shared test lock, first come first served. Plain `flock /tmp/aimebu-test.lock <cmd>` is not fair: waiters race for
# the lock and one run can overtake a queued one, and nobody can see who holds it. This script queues instead:
#
#   - each caller takes a ticket (a file in the queue directory, numbered by a counter that only grows), and only the caller whose ticket is
#     the oldest tries the lock, so the runs go in the order the tickets were taken;
#   - a waiter holds an exclusive flock on its own ticket for as long as it waits. A ticket is stale when that lock can be taken: the kernel
#     frees it when the owner dies, in whatever PID namespace or container it ran (a pid check cannot see across them, and a pid can collide
#     with a live local one). Whoever finds a stale ticket drops it;
#   - while it waits it prints on arrival, when its position or the holder changes, and every AIMEBU_TEST_LOCK_REPORT seconds otherwise;
#   - the holder writes a small metadata file (its command line and start time) next to the tickets and keeps a flock on it while it runs, so
#     a metadata file nobody holds (a crashed holder, or a plain-flock holder after it) is not believed.
#
# The lock itself is a flock on the same file plain `flock` uses, held on a descriptor the command inherits, so `flock <lock> <cmd>` still
# works and excludes this script (and the other way round); a plain-flock holder has no metadata and shows as "an unknown holder".
# The queue directory is world-writable and sticky like /tmp, so callers running as different users can share it.
# usage: scripts/test-lock.sh <command> [args...]
# env:   AIMEBU_TEST_LOCK (default /tmp/aimebu-test.lock), AIMEBU_TEST_QUEUE (default /tmp/aimebu-test.queue),
#        AIMEBU_TEST_LOCK_POLL (seconds between tries, default 1), AIMEBU_TEST_LOCK_REPORT (seconds between reports, default 60)
set -u
if [ $# -eq 0 ]; then
	echo "usage: scripts/test-lock.sh <command> [args...]" >&2
	exit 2
fi
LOCK=${AIMEBU_TEST_LOCK:-/tmp/aimebu-test.lock}
QUEUE=${AIMEBU_TEST_QUEUE:-/tmp/aimebu-test.queue}
POLL=${AIMEBU_TEST_LOCK_POLL:-1}
REPORT=${AIMEBU_TEST_LOCK_REPORT:-60}
mkdir -p "$QUEUE" || exit 1
chmod 1777 "$QUEUE" 2>/dev/null || true
HOLDER=$QUEUE/holder
ticket=
holding=
id=

cleanup() {
	[ -n "$ticket" ] && rm -f "$ticket"
	if [ -n "$holding" ] && [ "$(sed -n 's/^id=//p' "$HOLDER" 2>/dev/null)" = "$id" ]; then rm -f "$HOLDER"; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

field() { sed -n "s/^$1=//p" "$2" 2>/dev/null | head -n 1; }
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
# unowned FILE: true when nobody holds a flock on FILE, that is, whoever made it is gone.
unowned() { flock -n "$1" true 2>/dev/null; }

# A ticket number comes from a counter under a short flock. The ticket is written under a temporary name and locked before it is renamed to
# its numbered name, so it is never visible without its owner's lock, and the order of the names is the order the numbers were handed out.
[ -e "$QUEUE/.seq" ] || { : >>"$QUEUE/.seq" && chmod 666 "$QUEUE/.seq" 2>/dev/null; }
exec 8>>"$QUEUE/.seq"
flock 8
n=$(cat "$QUEUE/.seq" 2>/dev/null)
n=$(( ${n:-0} + 1 ))
printf '%s' "$n" >"$QUEUE/.seq"
id=$n.$$.$(date +%s)
tmp=$QUEUE/.new.$id
printf 'id=%s\nsince=%s\ncmd=%s\n' "$id" "$(now)" "$(printf '%s' "$*" | tr '\n' ' ')" >"$tmp"
chmod 644 "$tmp" 2>/dev/null
exec 7>>"$tmp"
flock 7
ticket=$QUEUE/$(printf '%012d' "$n").ticket
mv "$tmp" "$ticket"
exec 8>&-

exec 9>>"$LOCK"
last_report=-1
last_position=
last_holder=
waited=0
start=$(date +%s)
while :; do
	position=1
	for t in "$QUEUE"/*.ticket; do
		[ -e "$t" ] || continue
		if [ "$t" != "$ticket" ]; then
			if unowned "$t"; then rm -f "$t"; continue; fi
			if [ "$t" \< "$ticket" ]; then position=$((position + 1)); fi
		fi
	done
	if [ "$position" -eq 1 ] && flock -n 9; then break; fi
	if [ -e "$HOLDER" ] && ! unowned "$HOLDER"; then
		holder_id=$(field id "$HOLDER")
		by=$(field cmd "$HOLDER")
		since=$(field since "$HOLDER")
	else
		holder_id=unknown
		by="an unknown holder (a plain flock, or no command line recorded)"
		since="an unknown time"
	fi
	if [ "$last_report" -lt 0 ] || [ "$position" != "$last_position" ] || [ "$holder_id" != "$last_holder" ] || [ $((waited - last_report)) -ge "$REPORT" ]; then
		echo "waiting for the test lock (position $position, held by $by since $since)" >&2
		last_report=$waited
		last_position=$position
		last_holder=$holder_id
	fi
	sleep "$POLL"
	waited=$(($(date +%s) - start))
done

# The lock is ours: leave the queue, publish the metadata (locked before it is renamed into place), and run.
rm -f "$ticket"
ticket=
exec 7>&-
holding=1
printf 'id=%s\nsince=%s\ncmd=%s\n' "$id" "$(now)" "$(printf '%s' "$*" | tr '\n' ' ')" >"$HOLDER.$id"
chmod 644 "$HOLDER.$id" 2>/dev/null
exec 6>>"$HOLDER.$id"
flock 6
mv -f "$HOLDER.$id" "$HOLDER"
"$@"
exit $?

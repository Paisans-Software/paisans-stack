#!/bin/sh
# Rendered by paisans. Do not edit: `paisans apply` overwrites this file.
#
# The entrypoint of auth's app service, mounted read only at
# /paisans/pocket-id-standby.sh. It runs the image's own entrypoint and command
# (given as arguments by compose.yaml) and holds this instance on standby
# while another one is active against the same database.
#
# Pocket ID admits one instance per database while its HA mode is off, and HA
# mode cannot be turned on from the environment. A second instance logs a
# refusal and exits 1, the status of every other failure, so the only way to
# tell a standby from a crash is the text. This keeps the last 50 lines of the
# child's output, and when it exits non zero with the refusal among them,
# touches /tmp/paisans-standby (which the healthcheck reads as healthy),
# waits $PAISANS_STANDBY_RETRY seconds and tries again. Any other exit is
# passed on, so Docker's restart policy and apply's gate see it as before.
#
# The same marker is written to /paisans/run/standby, a directory the admin
# reconciler beside this container mounts read only, so the reconciler can tell a
# standby from a Pocket ID that is down. Both copies are removed whenever this
# script exits.
#
# The markers stay through each retry, so the toolkit's instance probe and the
# reconciler read a waiting standby as standby for the whole wait, not as down
# while the retry is being refused. They go as soon as a retried instance is
# admitted: when the image's healthcheck first passes (asked every second), or
# after $PAISANS_STANDBY_HOLD seconds (10 by default) if the instance is still
# running without passing it, which is longer than a refusal takes. So an
# active instance loses them within a second of serving, and an admitted one
# that never serves reads as down instead of hiding behind the standby's
# healthy healthcheck.
#
# A stop is forwarded to the child as SIGTERM, so an active instance shuts
# down cleanly and deregisters, and a standby elsewhere takes over within one
# retry instead of after the 90 s an unclean stop leaves its registration to
# age.
#
# Delete this file, the entrypoint and the healthcheck override once
# upstream binds HA mode to a variable; see README, "Pocket ID runs on every
# apps site, and one of them is active".
set -u

state=/tmp/paisans-standby
shared=/paisans/run/standby
fifo=/tmp/paisans-standby.fifo
kept=/tmp/paisans-standby.tail
marker='already one instance of Pocket ID running'
retry="${PAISANS_STANDBY_RETRY:-15}"
hold="${PAISANS_STANDBY_HOLD:-10}"
# The image's own healthcheck, as compose.yaml's healthcheck runs it.
ready='/app/pocket-id healthcheck'

child=
stopping=

# finish removes both markers, the shared one outliving the container on the
# host, and exits.
finish() {
	rm -f "$state" "$shared"
	exit "$1"
}

stop() {
	stopping=1
	if [ -n "$child" ]; then
		kill -TERM "$child" 2>/dev/null
	fi
}
# TERM for both: a background job in a non interactive shell ignores SIGINT.
trap stop TERM INT

# reap waits for a background process until it has really exited. A trapped
# signal interrupts `wait` with 128 plus its number while the process is still
# shutting down, so wait again until it is gone.
reap() {
	wait "$1"
	code=$?
	while kill -0 "$1" 2>/dev/null; do
		wait "$1"
		code=$?
	done
	return "$code"
}

# watch removes the markers once the retried child ($1) is admitted: when it
# is ready, or once it has outlived a refusal by $hold seconds. It returns
# when the child is gone, leaving the markers for the loop to keep or remove.
watch() {
	waited=0
	while kill -0 "$1" 2>/dev/null; do
		if $ready >/dev/null 2>&1 || [ "$waited" -ge "$hold" ]; then
			rm -f "$state" "$shared"
			return
		fi
		sleep 1
		waited=$((waited + 1))
	done
}

# A restarted container keeps its /tmp, and the shared marker outlives the
# container, so markers from before the restart would make this first attempt
# read as standby.
rm -f "$state" "$shared"
while :; do
	rm -f "$fifo" "$kept"
	mkfifo "$fifo" || finish 1
	# Every line goes straight to the container's log; only the last 50 are
	# held, in a ring, and written to $kept when the child closes its output.
	awk -v kept="$kept" '
		{ print; fflush(); ring[NR % 50] = $0 }
		END {
			first = NR > 50 ? NR - 49 : 1
			for (i = first; i <= NR; i++) print ring[i % 50] > kept
		}
	' < "$fifo" &
	reader=$!
	"$@" > "$fifo" 2>&1 &
	child=$!
	watcher=
	if [ -f "$state" ]; then
		watch "$child" &
		watcher=$!
	fi
	# A stop that arrived before there was a child to forward it to.
	if [ -n "$stopping" ]; then
		kill -TERM "$child" 2>/dev/null
	fi
	reap "$child"
	status=$?
	child=
	reap "$reader"
	# The watcher sees the child gone within a second; wait for it, so it
	# cannot remove markers the loop is about to keep.
	if [ -n "$watcher" ]; then
		reap "$watcher"
	fi
	rm -f "$fifo"

	if [ "$status" -eq 0 ] || [ -n "$stopping" ]; then
		finish "$status"
	fi
	if ! grep -qF -e "$marker" "$kept" 2>/dev/null; then
		finish "$status"
	fi

	: > "$state"
	: > "$shared"
	echo "paisans-standby: another Pocket ID instance is active on this database; standing by, next attempt in ${retry}s"
	sleep "$retry" &
	child=$!
	reap "$child"
	child=
	if [ -n "$stopping" ]; then
		finish 0
	fi
done

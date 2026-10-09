#!/bin/sh
# Rendered by paisans. Do not edit: `paisans apply` overwrites this file.
#
# The heartbeat of site home-b: one request a minute to each URL in
# HEARTBEAT_URLS, which heartbeat.env sets. A cycle is one minute from its
# start whatever the pushes take, since the sleep runs beside them: a monitor
# that is slow or gone delays no other monitor's push and never stretches the
# cadence the monitors measure staleness against. Each push gives up after
# 10 s and retries twice, so a failed one takes about 33 s at most, well
# inside the minute. The body is dropped and only the error is logged, which
# names the host but never the URL, so the token stays out of the log.
set -u
while true; do
	sleep 60 &
	for url in $HEARTBEAT_URLS; do
		curl -fsS -o /dev/null -m 10 --retry 2 -A "paisans-heartbeat (home-b)" "$url" \
			|| echo "heartbeat: push failed at $(date -u +%Y-%m-%dT%H:%M:%SZ)" >&2 &
	done
	wait
done

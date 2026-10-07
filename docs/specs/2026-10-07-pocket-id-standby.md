# Pocket ID on every apps site, one active and the rest on standby

Date: 2026-10-07. Status: approved by the founder in session. Lifts the
restriction the `site add` spec recorded under *Out* ("Apps on the new site.
Pocket ID v2.14.0 allows one instance per database"), so that a deployment can
run its applications on more than one apps site with the identity provider
among them.

## The facts it rests on

Read from Pocket ID at tag v2.14.0 and from francis at v0.1.0-beta.23, the
actor runtime it embeds.

* **One host per database unless HA is on.** `NewActors` sets `maxHosts := 1`
  and lifts it to 0 (no cap) only when `EnvConfig.HAEnabled`
  (`backend/internal/bootstrap/actors_bootstrap.go:47-53`).
* **HA cannot be turned on.** `HAEnabled` "is intentionally not bound to an
  environment variable while HA support is still being completed"
  (`backend/internal/common/env_config.go:91-94`).
* **Only hosts that checked in recently count.** francis admits a host by
  counting rows whose `host_last_health_check` is within the deadline, under a
  `FOR UPDATE` lock on the cluster's config row, and refuses with
  `ErrClusterFull` when the count reaches the cap
  (`components/postgres/postgres-cluster.go:55-93`). With HA off the deadline
  is 90 seconds (`ActorsHostHealthCheckDeadline`,
  `actors_bootstrap.go:144-151`).
* **A clean stop deregisters.** The host's `Run` unregisters on return, and a
  crash leaves the row to age out by the deadline (francis
  `host/local/host.go:377-393`).
* **The refusal is a log line and exit status 1.** `Bootstrap` turns
  `ErrClusterFull` into "it appears that there's already one instance of
  Pocket ID running - running multiple replicas is not (yet) supported"
  (`backend/internal/bootstrap/bootstrap.go:126-128`), the root command logs it
  and exits 1 (`backend/internal/cmds/root.go:21-24`), the same status as every
  other failure.
* **A refused instance never listens.** The router waits on the actor host's
  ready signal (`bootstrap.go:120-121`), which a refused host never sends, so a
  standby site's port refuses connections.
* **The image.** Entrypoint `["/app/docker/entrypoint.sh"]`, command
  `["/app/pocket-id"]`, healthcheck `/app/pocket-id healthcheck` every 90 s
  (`docker/Dockerfile-prebuilt:20-23`). It is Alpine 3.24.1, so `/bin/sh` is
  busybox with `awk`, `mkfifo`, `kill`, `grep` and `sleep`, checked in the
  image itself.

## The design

**Render Pocket ID on every apps site, under cluster placement, as any other
clustered app.** One instance wins the database; every other one is held on
standby by a wrapper, reports healthy, and retries until the active one goes
away.

### The wrapper

A script rendered into the stack (`/srv/<app>/paisans-standby.sh`), mounted
read only at `/paisans/pocket-id-standby.sh` and set as the service's
`entrypoint`, with the image's own entrypoint and command as its `command`. It:

1. removes `/tmp/paisans-standby`, then runs `/app/docker/entrypoint.sh
   /app/pocket-id` as a child;
2. streams the child's output to the container's log as it comes, keeping only
   the last 50 lines in memory (an `awk` ring buffer fed through a fifo);
3. forwards SIGTERM and SIGINT to the child as SIGTERM, so a `docker stop` of
   the active instance shuts Pocket ID down cleanly and it deregisters;
4. when the child exits non zero **and** the kept lines contain the refusal
   marker, touches `/tmp/paisans-standby`, logs one line, sleeps
   `PAISANS_STANDBY_RETRY` seconds (default 15) in a way a stop interrupts, and
   goes back to 1;
5. on any other exit, exits with the child's status, so every other failure
   behaves exactly as it does without the wrapper.

The healthcheck is overridden to `[ -f /tmp/paisans-standby ] ||
/app/pocket-id healthcheck`: a standby is healthy because standing by is its
job. Its timing is stated in the compose file rather than inherited (30 s
interval, 5 s timeout, 30 s start period at a 5 s start interval, 3 retries),
because the image's 90 s interval is also how long `apply`'s gate would wait
for a first report.

### The marker is pinned per image

The refusal text is a `kinds` table keyed by the whole image reference, like
`kinds.ImageVolumes`, and a test fails for a default Pocket ID image with no
entry, so bumping the image is the moment somebody rereads `bootstrap.go`. The
marker kept is the part without an apostrophe, `already one instance of
Pocket ID running`, so a log handler's quoting cannot hide it.

**It fails safe.** If an image changes the text and the marker stops matching,
the wrapper exits 1 like any other failure, and Docker's restart policy retries
it, more slowly (its back off grows to a minute). Nothing runs twice; the
standby site's stack shows `restarting`, which `apply`'s gate reports.

### Health

* **Per stack.** `apply`'s gate needs nothing new: a standby passes its
  healthcheck through the state file and reads `healthy`.
* **Per deployment.** After any `apply --execute` that acted on a `pocket-id`
  stack, and in `failover test`'s preflight and after each switchover, the
  toolkit asks every apps site the app runs on which state its instance is in:
  `standby` (the state file is present), `active` (its `/healthz` answers on
  the site's mesh address), `down`, or `absent` (no stack there yet). Exactly
  one `active` passes. **Zero** is waited for, up to three minutes, since a
  takeover after an unclean stop needs the 90 s deadline plus a retry, and
  fails after that. **Two or more** fails at once: it is the condition the
  whole design exists to prevent. A site that cannot be reached is reported and
  not counted.

### The gateway

Unchanged, and checked: the Pocket ID route already lists every apps site in
order with `import upstream_failover` (`lb_policy first`, passive health), as
every multi site app does. There is no active health check (the README rejects
one), so a standby can never mark the active site down. Its port refuses, so a
request finds the active site by a failed dial that costs no time and marks the
standby down for `fail_duration`. Expected from the facts above; to be observed
on the staging run, since Docker's userland proxy could accept a connection it
cannot forward.

### `validate` and `site add`

`validate` already accepts Pocket ID with cluster placement on two apps sites;
the render fixture has always had one. `site add` still joins `[data]` only,
but its refusal no longer blames Pocket ID: apps on the new site come from
giving it the `apps` role afterwards and running `host prepare` and `apply`,
since `site add`'s stages start no app stacks.

## Known limitations of more than one apps site

Not built here; recorded so they are chosen rather than found.

* **Mbin's cache is per site.** Each Mbin stack has its own Valkey with the
  application cache and Doctrine's second level cache in it
  (`config/packages/cache.yaml` in the fork). A fail back can serve views
  cached before the failover until their TTL. Sessions are **not** affected:
  the fork stores them in Postgres (`PdoSessionHandler`,
  `config/packages/framework.yaml:26-27`), so a failover keeps members signed
  in. Option: flush the returning site's Valkey on fail back, as a later
  toolkit step.
* **Mbin's scheduled tasks run once per site** until the fork's scheduler lock
  lands (`fix/scheduler_lock`, `fork/scheduler_lock`); see *Mbin's locks are
  split* in the README.
* **The second site's Mbin serves only during a failover.** The gateway sends
  everything to the first site that answers, so the other site's web workers
  are warm spares; its consumers do work all the time.
* **Failover is not instant.** A clean stop hands over within one retry
  (15 s). A site that dies leaves its row to age out, so the standby takes over
  within about 90 s plus a retry, during which sign in is unavailable.

## Later: active/active

When upstream binds HA mode to an environment variable, delete the wrapper and
the healthcheck override, set the variable, and every apps site serves. The
deployment check then changes from "exactly one" to "at least one".

## Testing

* Render goldens for the wrapper and the compose file; a `kinds` test for the
  marker table; a test that the rendered marker is the table's.
* Fake transport tests for the deployment check: one active, none (waited for,
  then failed), two (failed at once), a site absent, a site unreachable; and in
  `failover test`, preflight refusing two actives.
* A Docker test of the wrapper itself, under a build tag: an Alpine container
  whose fake `pocket-id` prints the marker and exits 1 a set number of times
  and then runs; one that exits 2 without it; and a stop that must return
  quickly with the child having received SIGTERM.

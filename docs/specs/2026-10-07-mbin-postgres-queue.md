# Mbin's queues on Postgres by default

Status: founder-approved design, 2026-10-07. Branch `feat/mbin-queue-postgres`.

## Problem

Mbin hands every federated activity and every background job to Symfony
Messenger, and the toolkit renders one RabbitMQ (behind amqproxy) per Mbin
stack, so one per apps site. Mbin's inbox controllers answer 200 as soon as an
activity is on that broker. A remote server that got the 200 never sends the
activity again, so an apps site lost with work still queued loses it for good,
and work it had queued for other servers (deliveries) is stranded until the
site returns, or lost with its disk. The surviving site cannot see either.

The cluster already has one store that survives losing a site: the Patroni
Postgres cluster, synchronously replicated. Symfony Messenger's Doctrine
transport keeps queues as rows there, and consumers on every site share them
with `SELECT ... FOR UPDATE SKIP LOCKED`.

## Decisions

* **Postgres is the default queue backend for every Mbin app**, single site or
  not (founder decision). Adding a second site then never changes the
  backend, so `site add` stays an addition rather than a migration with a
  queue drain in it.
* **RabbitMQ stays available, unchanged, by opting in** (`settings.queue:
  rabbitmq`), for an operator running one large site who wants the broker's
  throughput. The toolkit warns when it is chosen for an app on more than one
  apps site, and does not refuse: it is a risk, not an incoherence.
* **The Mbin fork makes the backend a matter of the DSN**
  (`specs/12-doctrine-transport-queue-names.md` in the fork workspace): a
  decorator lets the unchanged `messenger.yaml` run on `doctrine://`, each
  transport on its own queue. Without it, `doctrine://` fails at the first
  dispatch with `Unknown option found: [queues, exchange]`.

## What is rendered

`settings.queue` on an `mbin` app: `postgres` (default) or `rabbitmq`. Any
other value is refused at validate.

**postgres**

```
MESSENGER_TRANSPORT_DSN=doctrine://default?check_delayed_interval=1000&redeliver_timeout=900
```

No `rabbitmq` or `amqproxy` service in compose, no `RABBITMQ_` variables in
`.env`, and app and messenger no longer depend on amqproxy.

* `doctrine://default` is the app's own connection, so it reaches the
  database exactly as `DATABASE_URL` does (the site's HAProxy, or the pinned
  `postgres`). A message a handler dispatches commits or rolls back with that
  handler's writes.
* `check_delayed_interval=1000` (milliseconds): one consumer process reads
  every transport over one session, and a LISTEN/NOTIFY wake-up popped by one
  queue's receiver is lost to the others, which then wait for this timed poll.
  The default is 60000.
* `redeliver_timeout=900` (seconds): a message taken by a consumer that died,
  on a site that died, is handed out again after fifteen minutes rather than
  the default hour. It must stay above the longest handler run; Mbin's HTTP
  client caps one request at 15 s, and no handler is known to run for minutes.

**rabbitmq**: exactly what is rendered today.

## Not handled by the toolkit

* **Switching an existing deployment** from RabbitMQ to Postgres. `apply`
  renders the new stack but does not drain the old broker, and compose leaves
  the removed `rabbitmq` and `amqproxy` containers running as orphans. The
  procedure (README) is: run one consumer against the old DSN until the
  broker's queues and its `delay_*` retry queues are empty, or accept losing
  pending retries; then `docker compose up -d --remove-orphans`.
* **The fork image.** The default Mbin image must be a fork release carrying
  the decorator before this branch is merged; the image bump lands in the same
  merge. Done: `1.14.0-paisans`, released 2026-10-07. An app that declares an
  older image keeps working only with `queue: rabbitmq`.

## Rejected

* **RabbitMQ by default, Postgres only for more than one apps site.** Keeps
  upstream's shape for one site, but `site add` would change the backend and
  need a drain: an addition becomes a migration.
* **One RabbitMQ cluster across the sites (quorum queues).** Replicates, but is
  a second clustered system with its own partitions and failover to run over
  the mesh, where Postgres is already the one shared, highly available thing.
* **An inbox journal table in the fork.** Protects inbound activities only,
  and is custom code the Doctrine transport already is.

## Tests

* Default renders the Doctrine DSN, no broker services, no `RABBITMQ_`
  variables, no dependency on amqproxy; clustered and pinned.
* `queue: rabbitmq` renders today's stack.
* Validate refuses an unknown value and warns for RabbitMQ on more than one
  apps site.
* Every variable compose requires is still set (the existing test).

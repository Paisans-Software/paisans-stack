# Uptime and health monitoring

Status: approved in session on 2026-10-07, not implemented.

A deployment watches itself. A new `uptime` kind runs the
`josephquigley/uptime` fork, pinned to one site like any other app, and the
toolkit seeds its monitors from `paisans.yaml`: two HTTP checks per app and a
ping per site. Admins sign in through Pocket ID, and only admins can.

## What it must catch

* **An unhealthy app**: a container crashed, crash looping, or answering 5xx,
  including one that sits behind a gate and one that is down on only one of
  the sites a clustered app runs on.
* **A site or host that is gone**: power, ISP, a machine that stopped.

What it does not have to catch: the death of the machine the monitor runs on.
On a single site that is the only machine, and a monitor cannot report its own
host dying. This was accepted in session: the software stacks are what go down
in practice, and a single site losing everything at once is the expected
behaviour of having one site.

## The software

`josephquigley/uptime` is a fork of `codewizdevs/uptime`, a Node.js monitor
with HTTP, TCP, ping, DNS, heartbeat and domain checks, email channels, and
SQLite storage. The fork adds OIDC sign in with group based roles.

The image is `ghcr.io/josephquigley/uptime:1.1.0-oidc.1`. Checked against
ghcr.io on 2026-10-07 by requesting the manifest for that exact tag
anonymously: 200, an OCI image index for linux/amd64 and linux/arm64 (plus two
attestation manifests). **The image tag has no `v`; the git tag does**
(`v1.1.0-oidc.1`), because `docker/metadata-action`'s semver pattern strips
it. `latest` and `sha-c087f3d` point at the same image. The kind's default
moves to the first release carrying the fork changes below.

Choosing this program to fill the capability is the founder's decision and was
made in session.

## Kind and placement

`uptime` is an ordinary app with a new kind, not a site role. Roles say what a
site provides to the deployment; placement says where an app runs.

```yaml
apps:
  status:
    kind: uptime
    hostname: status.example.org
    placement: { pinned: vm }
    settings:
      admin_group: admins
```

* **Pinned only.** It keeps SQLite on a local volume, so `kinds.UsesPostgres`
  is false and the existing `cluster-placement-without-a-cluster` refusal
  applies with no new rule.
* **Any declared site.** A single site's only machine, the gateway VM of a
  multi-site deployment, a home, or a dedicated VM. Pinned placement already
  accepts a site whatever its roles. *Superseded: the site must hold the
  `monitor` role, which is never the gateway or the witness. See the
  amendment of 2026-10-08 below.*
* **No `placement: gateway`.** It was considered, so the monitor would follow
  a gateway move without an edit, and rejected. During a move two sites hold
  the gateway role, and two monitors would send every alert twice. Pinning
  makes the location explicit, and a gateway move is already a sequence of
  config edits.
* **One instance.** Nothing stops two `uptime` apps being declared, but
  nothing coordinates them either, so each alerts on its own.
* **A move loses history.** A newly placed instance starts with an empty
  database and is reseeded. Monitors come back; check history and incidents
  do not. History is not worth migrating.

### A site may have no roles, if something is pinned to it

`structural` refuses `roles: []` today (`internal/config/config.go`, "required.
Give the site at least one of data, apps, gateway, witness"). A dedicated
monitor VM has no role that fits:

* `apps` does not mean "may host an app". It means "runs every clustered app":
  `render/plan.go` places each `cluster` app on every `apps` site, and the role
  also brings HAProxy, container rules to the database proxy and Garage, and a
  place in `failover`. A monitor VM with `apps` would start Mbin and Outline.
* `witness` fits only if the operator also wants the VM as etcd's tiebreaker.

*Superseded for the monitor by the amendment of 2026-10-08 below: a dedicated
monitor VM is `roles: [monitor]`. The rule that follows stands for every other
kind.*

So: **`roles: []` is accepted for a site that at least one app is pinned to.**
A role-less site that nothing is pinned to is still refused, because it is
almost certainly a mistake. A new role meaning "something is pinned here" was
rejected: it would restate what placement already says, and could disagree
with it.

Matrix is the precedent for one-off apps: the `synapse` kind is forced to be
pinned and the example pins it onto `vm`, `[gateway, witness]`, with no `apps`
role. Placement was already the mechanism; only the "a site with no other job"
case was missing.

### Beside etcd

Pinned onto a witness the existing `pinned-app-on-witness` warning fires. It
stays as written: the risk is real and the rule is generic.

What makes uptime a quiet enough neighbour is turning off its VACUUM. The fork
runs retention every six hours (`RETENTION_RUN_INTERVAL_HOURS`, default 6) and
then a full `VACUUM` (`RETENTION_VACUUM`, default true). Estimated, not
measured: about 15 monitors at 60 s kept 90 days is about 1.9 million `checks`
rows and a 200–250 MB file. `VACUUM` rewrites the whole file and in WAL mode
passes it through the WAL and back, so roughly 0.5–0.75 GB of writes with
fsyncs in one burst, every six hours, beside an etcd whose heartbeat is
measured in hundreds of milliseconds. It also buys nothing: each run deletes
about six hours of rows and SQLite reuses the freed pages, so the file
plateaus without it.

**The kind renders `RETENTION_VACUUM=false`.** An operator who wants the space
back after a large one-off deletion can set it through `config:`. Without the
VACUUM, steady state is a few dirtied pages per check, and with
`synchronous = NORMAL` SQLite fsyncs only at a WAL checkpoint, about every
4 MB of WAL.

## What gets checked

Seeded from `paisans.yaml` on every apply. Every seeded monitor is tagged
`managed`. Defaults: 60 s interval, `failure_threshold: 2`.

**Per app, two HTTP checks.**

1. **Public**: `https://<hostname>/`, through DNS, the gateway's Caddy and its
   certificate. Follows no redirects and accepts `200,302`, because a gated
   app's gate redirects to sign in. Catches a dead gateway, a bad Caddy
   reload, an expired certificate (the fork tracks certificate expiry on
   HTTPS checks and warns at 14 days, which covers the DNS-01 renewal worry
   recorded in `2026-09-15-acme-dns-provider.md`).
2. **Direct**: `http://<site mesh address>:<app port><health path>`, straight
   to the container, past Caddy and any gate. One per site the app runs on,
   so a clustered app has one per `apps` site.

Neither check alone is enough, and that was settled with a failure table in
session:

| Failure | Public only | Both |
|---|---|---|
| Mbin (gated, clustered) crashes everywhere | nothing: the gate's redirect stays green | 2 alerts |
| Mbin crashes on one site | nothing: Caddy routes to the other | 1 alert |
| Ungated pinned app crashes | 1 alert | 2 alerts |
| Gateway Caddy down | every public check | the same, and green direct checks say it is the edge |
| A certificate stops renewing | 14 day warning | the same |

The cost is duplicate alerts for an ungated app and a larger burst when a site
goes. Muting public checks that have a direct partner was considered and
deferred until alert volume is shown to be a problem.

**Per site other than the monitor's own:** an ICMP ping to its mesh address.
A single site therefore gets none. Needs `cap_add: NET_RAW`, which the fork's
own compose file already sets.

**Not seeded:**

* **Heartbeats.** The toolkit runs no scheduled jobs to ping one. Admins add
  their own (a backup job, a cron) by hand; see ownership below.
* **Domain expiry.** A WHOIS dependency, and DNS is already exercised by every
  public check.

### Health paths

`kinds.HealthPath(kind)` returns the direct check's path. **Every value is
established against the pinned image before it enters the catalogue**, the same
standard as the image references: run the image, request the path, record the
status and the date in the comment. None is taken from memory or upstream
documentation. A kind with no health route of its own uses `/` and says so in
its comment.

### The monitor reaching its own site

`hostprep` lets `wg0` in whole, so pings and direct checks to other sites
work. A check to an app on the monitor's own site leaves the container on a
Docker bridge and arrives at the host's mesh address on `br-+`, where the
default deny drops it. That is the defect recorded in
`internal/hostprep/firewall.go` for Mbin and its database proxy, and on a
single site it would fail every direct check.

`ContainerRules` gains, for the site an `uptime` app is pinned to, one rule
per app port on that site: `br-+` to the site's mesh address, that port, tcp.

## Seeding

### Mechanism: a rendered file, read at boot

The toolkit renders `srv/<app>/monitors.json` (template
`uptime/monitors.json.secret.tmpl`, so written 0600 and bind mounted
read only) and sets `SEED_FILE` to its path in the container. The fork
reconciles from it on every start.

`apply` already restarts a stack when a non-environment file in it changes
(`internal/apply/apply.go`, `isEnvironment`: only `*.env` and `compose.yaml`
force a recreate), and a restart reruns the fork's boot, so a topology change
reaches the monitor with no new apply behaviour.

Rejected:

* **`apply` calling the fork's `/api/v1/sites` with a write token.** Tokens
  are created in the UI, so a bootstrap token would still need a fork change;
  `apply` would need a post-start phase talking to a running app, which no
  kind has; and it is more moving parts for the same result.
* **Writing the SQLite file directly.** Couples the toolkit to the fork's
  schema.

### The file (abbreviated)

```json
{
  "settings": {
    "smtp_host": "smtp.example.org",
    "smtp_port": 587,
    "smtp_secure": false,
    "smtp_user": "...",
    "smtp_pass": "...",
    "smtp_from_address": "status@example.org",
    "smtp_from_name": "Example Status",
    "status_page_enabled": false
  },
  "monitors": [
    { "name": "talk — public", "monitor_type": "active", "url": "https://talk.example.org/", "...": "..." },
    { "name": "talk — direct (home-a)", "...": "..." },
    { "name": "home-b — ping", "...": "..." }
  ]
}
```

SMTP comes from the deployment's `smtp:` block, with any per app override
applied; see *Amendment, 2026-10-07: SMTP is declared, not borrowed*. (This
paragraph used to say it came from "the values Pocket ID is rendered with".
There are none: see the amendment.)

Monitor names are derived from app and site keys, never from hostnames, so
renaming a hostname updates a monitor rather than replacing it.

**No channels are seeded and no recipients are declared in YAML.** Admins
create their own email channel in the UI.

### Ownership rules

| Monitor | On boot |
|---|---|
| In the seed and in the database | check fields updated; **channel links untouched** |
| In the seed only (a new app or site) | created, and attached to every channel flagged "attach to new managed monitors" |
| In the database only, tagged `managed` (app or site removed) | deleted; its channel links go with it |
| In the database only, not tagged `managed` (made by hand) | untouched |

So an apply that removes nothing never changes a subscription, and a newly
added app is not silently unalerted for an admin who opted in.

## Exposure and sign in

**Routing.** The app has a `hostname` like any other, and the gateway's Caddy
routes it to the pinned site. Publishing a hostname to the public internet
stays a human decision; the kind changes nothing about that.

**Caddy snippet.** Allowed: the UI, sign in, the OIDC callback, and `/ping/*`
(heartbeat URLs). Refused with 404 at the edge: `/status*`, `/badge/*`,
`/metrics`, `/api/*`. The refusal does not depend on any setting in the app.

Why each matters: the fork's status page is enabled and public by default when
its settings row is unset (`src/routes/status.js`), and would list every
monitor, mesh addresses included; `/metrics` is public whenever no API token
exists (`src/routes/api.js`), which under this design is always.

**In the app as well**, the seed sets `status_page_enabled` false.

**Mesh.** The container's port is published on its site's mesh address like
every app's, so a mesh peer reaching it directly can read `/metrics`. That is
the existing trust model (the gate is enforced at the gateway and only there)
and is accepted rather than special-cased.

**Sign in.**

* `OIDC_ISSUER` is the Pocket ID app's URL, from the planner's existing
  `identityProviderURL`, not declared twice.
* `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` from `oidc_clients.<app>`, captured
  after a human approves and runs the mint, as for every OIDC app.
* `OIDC_ADMIN_GROUP` from `settings.admin_group`, **required** (group names are
  community specific, so there is no default). `OIDC_EDITOR_GROUP` and
  `OIDC_VIEWER_GROUP` unset, so the fork refuses anyone outside the group
  ("Your account is not authorised for Uptime").
* The Pocket ID client is also restricted to that group. A Pocket ID client
  mutation: approval required, done by a human.
* `PUBLIC_BASE_URL` is `https://<hostname>`.

**Break glass: password sign in stays enabled.** Pocket ID is one of the
monitored apps; with OIDC only, an admin is locked out of the monitor exactly
when it matters. `ADMIN_USER` is `admin`; `ADMIN_PASS` and `SESSION_SECRET`
are generated at init. `OIDC_DISABLE_PASSWORD_LOGIN=false`.

**`gate:`** is allowed and pointless: the fork checks the admin group itself.
No rule is added against it.

## Changes to the fork

Each its own pull request on `josephquigley/uptime`, then a release tag.

1. **Seed at boot.** When `SEED_FILE` is set, reconcile settings and monitors
   by the ownership rules above. Reuses `importConfig`'s parts (`importSmtp`,
   `insertSite`), but not its `replace` path: that calls `setSiteChannels`
   with the seed's channel list and would reset every admin's subscriptions on
   every restart.
2. **Settings in the seed.** SMTP fields and `status_page_enabled`, applied on
   every boot so the file stays the source of truth for them.
3. **Channel flag "attach to new managed monitors".** Migration, a checkbox on
   the channel form, and the lookup step 1 uses.
4. **Tests** for every row of the ownership table, plus: a hand made monitor
   survives; channel links survive an update; a seed with no monitors deletes
   only `managed` ones.

## Changes to the toolkit

* `config`: `KindUptime`; `roles: []` no longer a structural error.
* `kinds`: catalogue entry (`app`, the image above); `HealthPath`; config
  file `.env`.
* `validate`: refuse a role-less site that nothing is pinned to; require
  `settings.admin_group` for `uptime`.
* `render`: `uptime/compose.yaml.tmpl` (`NET_RAW`, `/data` volume, the seed
  mount), `uptime/.env.secret.tmpl` (including `RETENTION_VACUUM=false`),
  `uptime/caddy.snippet.tmpl`, `uptime/monitors.json.secret.tmpl`, and the
  planner code that builds the monitor list.
* `hostprep`: the `ContainerRules` addition above.
* Secrets: `admin_password` and `session_secret` under the app, generated at
  init.
* README and `examples/paisans.example.yaml`.

## Testing

* Golden render: an `uptime` app in `internal/render/testdata/deployment.yaml`.
* Seed generation: every app has a public and a direct check; a clustered app
  has a direct check per `apps` site; every site but the monitor's own gets a
  ping; a single site gets none; monitor names are stable across a hostname
  change.
* Validate: role-less site with and without a pinned app; missing
  `admin_group`; `cluster` placement refused for `uptime`.
* `hostprep`: `ContainerRules` for the monitor's site.
* Health paths established against the pinned images, with the record in
  each comment.

## Open

* Whether the certificate expiry warning still sends when a monitor's
  notifications are muted. Only matters if muting public checks is ever
  adopted.
* The `apps` role reads as "may host an app" and means "runs every clustered
  app". Renaming it is its own change across the configuration surface and is
  out of scope here.

## Amendment, 2026-10-07: SMTP is declared, not borrowed

Found while planning, and decided by the founder in session. The toolkit
renders SMTP nowhere. Pocket ID's `.env.secret.tmpl` has no SMTP key, because
Pocket ID keeps its SMTP settings in its own database and they are set in its
admin UI; and `external.smtp_password` in `examples/secrets.example.yaml` is
declared but read by no template and no Go code. So there was nothing to copy.

**A deployment wide `smtp:` block**, defaults for every app that sends mail:

```yaml
smtp:
  host: smtp.example.org
  port: 587
  security: starttls      # starttls or tls
  username: status@example.org
  from_address: status@example.org
  from_name: Example Community
```

**Any field can be overridden per app**, and a field the app leaves out is
inherited (founder instruction: "we may want to override any or all of the
smtp settings for each app that supports smtp"):

```yaml
apps:
  status:
    kind: uptime
    smtp:
      from_address: alerts@example.org
      from_name: Example Status
```

**The password** is `apps.<app>.smtp_password` in the secrets when present,
otherwise `external.smtp_password`, so an app sending through a different
account needs no change to anyone else's.

Rules:

* `smtp.security` is `starttls` or `tls`. `none` is not offered: no consumer
  needs it and the uptime fork cannot express it (its `smtp_secure` is a
  boolean, nodemailer's `secure`, and STARTTLS is what `false` means).
* `smtp.port`, when set, is 1 to 65535.
* **`apps.<app>.smtp` on a kind that sends no mail is refused**
  (`smtp-on-a-kind-without-mail`). An override nothing reads is ignored without
  a word, which is the failure this toolkit refuses everywhere else.
  `kinds.SendsMail(kind)` is the list; today it is `uptime` alone. Pocket ID
  joining it is a later change that would read the same block.
* **An `uptime` app with no resolved SMTP host is a warning**
  (`uptime-without-smtp`): it runs and records incidents, but no email channel
  can send. The seed then carries no SMTP settings at all and leaves whatever
  an admin set in the UI alone.
* `external.smtp_password` is owed (`paisans init` lists it) once any app that
  sends mail resolves an SMTP host and no per app password covers it.

## Amendment, 2026-10-07: the container starts as root and drops

Found while planning. `apply` writes rendered files through `sudo`, so the
0600 seed file is owned by root, and a missing bind mount directory is created
by Docker as root. The uptime image runs as `node` (uid 1000), so as shipped it
could read neither the seed nor write its database.

The fix is the pattern Mbin's template already documents ("start as root; the
image's entrypoint chowns ... and then drops"): the toolkit's compose sets
`user: "0:0"`, and the fork gains an entrypoint that, **only when started as
root**, chowns `/data` to `node`, copies `SEED_FILE` to a `node` owned 0600
file under `/tmp` and points `SEED_FILE` at it, then `exec`s the server through
`setpriv` as `node`. Started as anyone else it does nothing, so the image's
behaviour outside this toolkit is unchanged. `setpriv` is in the image already
(util-linux, checked 2026-10-07 by running it).

## Amendment, 2026-10-07: smaller settlements made while planning

* **Direct checks send `Host: <hostname>` and `X-Forwarded-Proto: https`**,
  through the fork's per monitor `request_headers`. An app answering on its own
  port behind a proxy may refuse or redirect a request whose Host is a mesh
  address, and the check is meant to see what the gateway would see, minus the
  gateway.
* **The monitor does not watch itself.** An `uptime` app gets no monitors in
  its own seed: it cannot report its own death, and a check that can only ever
  pass is noise.
* **The fork's changes land as two stacked branches**, not four pull requests:
  `feat/channel-auto-attach` (the flag), then `feat/seed-file` on top of it
  (settings, monitors, entrypoint). The seed's creation path needs the flag, so
  splitting further would only produce a pull request that cannot be tested on
  its own.
* **Releasing the fork and bumping the kind's default image are left to the
  founder.** Pushing a `v*` tag publishes an image, which is outward facing.
  Until then the kind's default stays `1.1.0-oidc.1`, which ignores
  `SEED_FILE`: monitors are not seeded and the container runs as root with no
  entrypoint to drop. The render and its tests are complete either way; the
  bump is one line in `internal/kinds` plus its `ImageVolumes` row.

## Amendment, 2026-10-07: health routes, the public check's path, port, proxy trust

**Health routes**, established from the source at each pinned tag and, where
marked, by running the image (research recorded in the plan):

| Kind | Path | Expect | How established |
|---|---|---|---|
| pocket-id | `/healthz` | `204` | source `healthz_controller.go:18,30-31` at v2.14.0; ran the image |
| outline | `/_health` | `200` | source `server/main.ts:78-105` at v1.10.0 (runs `SELECT 1` and pings Redis) |
| synapse | `/health` | `200` | source `synapse/rest/health.py:38-50` at v1.160.0; ran the image |
| element | `/version` | `200` | ran the image: `/version` 200, `/health` 404 |
| oauth2-proxy | `/ping` | `200` | source `pkg/middleware/healthcheck.go:44-53` at v7.15.4; ran the image |
| mbin | `/` | `200,302` | no health route at `v1.13.3+paisans`; `/` is a deep check, 302 when the instance is private |
| writefreely | `/` | `200,302` | no health route at `sha-ff9dceb`; `/` renders sign in on a private instance |
| uptime | `/healthz` | `200` | source `src/server.js:63` at `1.1.0-oidc.1` (never used: a monitor does not watch itself) |

The fork's `expected_status` is a comma separated list of exact codes, with no
ranges (`src/lib/checker.js:55-61`), which is why each row is a list.

**The public check requests the health path too**, not `/`, at the public
hostname, and also accepts `302` when the app has a gate. `/` answers
differently per kind (an oauth2-proxy answers 403 there), and the health path
makes the public and direct checks of one app ask the same question.

**Port 3001.** The fork defaults to 3000, which Outline also uses, and two apps
on one site publish on the same mesh address. The kind renders `PORT=3001`.

**`TRUST_PROXY`, a fork change.** The fork sets `trust proxy` to loopback, so
behind the gateway every request has the gateway's address and the login rate
limiter (keyed on `req.ip`) would let one attacker lock every admin out of the
break glass password. The fork reads `TRUST_PROXY` (default `loopback`, so
unchanged elsewhere) and the kind renders the mesh subnet, as Pocket ID's does.

**OIDC redirect URI** for the client: `https://<hostname>/login/oidc/callback`
(`src/lib/oidc.js:26`), named by `paisans init` when the client is owed.

## Amendment, 2026-10-07: corrections from the whole-branch review

A fresh reviewer read both branches against this spec. Four corrections:

* **A gated app's public check expects `401`, not `200,302`.** The toolkit's
  gate is `forward_auth` to oauth2-proxy's `/oauth2/auth`, which refuses a
  request with no session with 401 (`oauthproxy.go:1018-1022` at v7.15.4), and
  Caddy copies that back. Nothing here redirects; the 302 came from the live
  deployment's older gate, which redirects browsers only. The public check of a
  gated app therefore proves the edge and the gate, and the direct check proves
  the app, which is the split "What gets checked" argued for anyway.
* **The homeserver is checked at `/_matrix/client/versions`**, not `/health`.
  The gateway sends only `/_matrix/*` and `/_synapse/*` to Synapse and the rest
  to MAS, which has no health resource, so `/health` at the public hostname
  asked the wrong service. The versions endpoint answers without a token
  (`synapse/rest/client/versions.py:40,50-60` at v1.160.0) on both routes.
  The rule this generalises: a health route is chosen so the gateway routes it
  to the same service the direct check reaches.
* **The edge refuses `/api/v1/*`, not `/api/*`.** The fork's own pages fetch
  session authenticated JSON under `/api/sites` (`public/js/dashboard.js:115`,
  `site-detail.js:20`), so refusing the whole prefix broke the dashboard's live
  refresh and the response time chart. The token API, the one the reason above
  was about, is `/api/v1/*`.
* **A role-less site renders no infra stack.** It runs no infrastructure, and
  rendering `srv/infra/compose.yaml` with an empty `services:` map would hand
  `apply` a project with nothing to start.

And one on the fork, fixed before its release: a reseed no longer resets what
an admin set in the UI on a managed monitor (pause, mute, renotify, notes,
display name, status page placement, double verify) unless the file names the
field. "Check fields updated" in the ownership table means exactly that.

**Released.** On the founder's instruction the fork's changes were merged
(josephquigley/uptime #8, #9, #10) and released as `v1.1.0-oidc.2`, which
published `ghcr.io/josephquigley/uptime:1.1.0-oidc.2` (checked 2026-10-07:
manifest 200 for amd64 and arm64, `/data` its only declared volume, the new
entrypoint in place). The kind's default image is that tag.

## Amendment, 2026-10-07: the fork moved to Paisans-Software

Founder instruction: the fork will diverge from upstream with paisans-specific
changes, so it moved from `josephquigley/uptime` to `Paisans-Software/uptime`.
GitHub redirects the old repository path, but a container package owned by a
user does not move with its repository: `1.1.0-oidc.2` was copied by digest
(`sha256:cf8c9b1c…`, identical at both paths) to
`ghcr.io/paisans-software/uptime:1.1.0-oidc.2`, and every later release
publishes there, since the workflow tags `ghcr.io/${{ github.repository }}`.
The kind's default moves to the new path with the next release, once the org
package is public; until then it stays at the old path, which still pulls.


**Released from the organisation.** `v1.1.0-oidc.3` (Paisans-Software/uptime
#11 bulk channel attach/detach and retroactive auto-attach, #12 SMTP lock when
the seed supplies SMTP, #13 version) published
`ghcr.io/paisans-software/uptime:1.1.0-oidc.3`, after the package was made
public and given the repository's Actions write access. Checked 2026-10-07:
anonymous manifest 200, `/data` its only declared volume, and a run as root
with an SMTP seed came up healthy with the seed applied. The kind's default is
that reference.

## Amendment, 2026-10-08: the monitor has a site role of its own

`docs/specs/2026-10-08-monitor-role-and-host-check.md`, Part 1, approved in
session, changes five things here:

* **Placement.** `uptime` is no longer placeable on any site. It is still
  `placement: { pinned: <site> }`, and that site must hold the new `monitor`
  role (`uptime-needs-a-monitor-site`). A monitor site is never the gateway
  (`monitor-on-gateway`) or the witness (`monitor-on-witness`), is warned
  about beside `data` or `apps` (`monitor-shares-a-site`), and must declare
  `public_address`. The gateway VM, which *Kind and placement* named as the
  usual multi-site place, is therefore refused.
* **The role-less monitor site** becomes `roles: [monitor]`. `roles: []`
  with a pinned app stays legal for other kinds.
* **Routing.** The monitor is no longer routed by the gateway's Caddy. It
  serves its own hostname, through the toolkit's Caddy on the monitor
  (`ingress` mode `paisans`) or behind the operator's own web server (mode
  `external`), and its DNS records point at the monitor's own address. What
  *Exposure and sign in* says the gateway refuses at the edge is refused by
  the monitor's Caddy, or by the operator's web server through the snippets
  `paisans ingress show` prints.
* **The seed.** The monitor still gets no direct check of its own container,
  but it gains one check of its own public URL, `https://<hostname>/healthz`
  expecting 200, which proves the path in front of it: DNS, the web server
  and the certificate.
* **`TRUST_PROXY`** is where the proxy in front of the monitor connects from,
  rather than the mesh subnet alone: the mesh subnet behind the monitor's own
  Caddy, and in mode external the network the operator's web server reaches
  the published port from.


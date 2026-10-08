# Monitor role, monitor ingress and the host check

Status: approved in session on 2026-10-08, not implemented.

Three changes. The first two ship together because the second is what makes
the first safe to try on a real machine; the third removes a manual step that
every app with sign in currently needs:

1. **A `monitor` site role.** The `uptime` kind must run on one, a monitor is
   never the gateway, and a monitor site serves its own apps rather than
   routing them through the gateway's Caddy. A monitor that depends on the
   gateway goes dark at exactly the moment it is needed.
2. **A host check.** Before the toolkit changes a host it inventories what is
   already there, works out what the site is about to claim, and refuses when
   something it does not own already holds one of those claims. It also stops
   doing host-wide things (firewall defaults, image and volume cleanup) on a
   host that something else lives on.
3. **`apply` creates OIDC clients.** An app whose kind signs in through Pocket
   ID gets its client created and recorded as part of `apply --execute`, the
   same way its other secrets are generated, instead of needing a separate
   `paisans oidc client create` run first.

This amends `2026-10-07-uptime-monitoring.md`: placement of the `uptime` kind,
the role-less monitor site, and the seed's treatment of the monitor itself.

## Part 1: the `monitor` role

### Roles

`monitor` joins `data`, `apps`, `gateway`, `witness` and `storage`.

| Combination | Result | Rule |
|---|---|---|
| `monitor` + `gateway` | refused | `monitor-on-gateway` |
| `monitor` + `witness` | refused | `monitor-on-witness` |
| `monitor` + `data` or `apps` | warned | `monitor-shares-a-site` |
| `monitor` with no `uptime` app pinned to it | refused | `monitor-without-uptime` |
| `uptime` app pinned to a site without `monitor` | refused | `uptime-needs-a-monitor-site` |

A gateway is refused because the monitor's job is to report the gateway's
failures. A witness is refused because etcd's tiebreaker and the thing that
reports etcd losing quorum must not fail together, and because the witness is
usually the gateway machine anyway. `data` and `apps` are allowed with a
warning: a small deployment may have no other machine, and the warning says
which failures the monitor will then miss. There is no override for the
refusals.

`uptime` stops being placeable on any site. It is still `placement: { pinned:
<site> }`, and that site must hold `monitor`. The role-less site that the
2026-10-07 spec allowed for a dedicated monitor VM becomes `roles: [monitor]`.
`roles: []` with a pinned app stays legal for other kinds.

### Ingress

A monitor site has an `ingress` block. Nothing else may: a gateway always runs
the toolkit's Caddy, and every other site is reached through the gateway.

```yaml
sites:
  watch:
    roles: [monitor]
    address: 10.45.0.4
    endpoint: watch.example.org:51820
    public_address: 203.0.113.10
    ingress:
      mode: external              # paisans (default) | external
      listen: 127.0.0.1:8480      # external only, required there
```

| Rule | Refuses |
|---|---|
| `ingress-outside-monitor` | an `ingress` block on a site without `monitor` |
| `ingress-listen-mode` | `listen` with `mode: paisans`, or `mode: external` without `listen` |
| `ingress-listen-public` | a `listen` address that is not loopback, RFC 1918, or the site's mesh address |

A `listen` address that is not loopback gets a warning that Docker publishes
ports in front of ufw, so the firewall does not protect it.

**`mode: paisans`** (the default). The monitor site runs the toolkit's Caddy
image (`ghcr.io/paisans-software/caddy`, the same digest as the gateway) in
its `infra` stack, with host networking, DNS-01 through `acme.provider`, and a
site block for each app pinned there and nothing else. It claims tcp 80 and
443, and `host prepare` opens both in ufw. `acme.provider` becomes required
when any site is a gateway *or* a monitor in this mode.

**`mode: external`.** The toolkit runs the app's container and nothing in
front of it. The app is published on `listen` as well as on the mesh address
(the direct check uses the mesh one). No Caddy, nothing on 80 or 443, no
certificate. Certificates and renewal belong to whoever runs the web server,
because the toolkit cannot know how an unfamiliar proxy obtains them and must
not edit a configuration it does not own.

In both modes the app is rendered for a proxy in front of it:
`PUBLIC_BASE_URL=https://<hostname>`, and `TRUST_PROXY` set to the address the
proxy connects from (`loopback` for a loopback `listen`, the listen address's
network otherwise). Without these the OIDC callback is built with `http://`.

### Routing and DNS

Apps pinned to a monitor site are left out of the gateway's Caddyfile.
`dns.Desired` points their hostnames at the monitor site's `public_address`
(and `public_address6` when set) instead of the gateway's. A monitor site
without `public_address` is refused (`monitor-without-public-address`).

### The seed

The 2026-10-07 seed skips `uptime` apps because a monitor cannot report its
own death. That stays for the direct check. One monitor is added for the
monitor's own public URL: `https://<hostname>/healthz`, expecting 200. The
monitor process is alive whenever it can run this check, so what it proves is
the path in front of it: DNS, the web server, and the certificate, which the
fork warns about 14 days before expiry. For `mode: external` this is the only
thing that notices a proxy whose renewal has silently stopped.

The existing public check for every other app now runs from a machine that is
not the gateway, so a dead gateway shows as every public check failing while
the direct checks stay green.

### Helping an operator behind their own web server

**`paisans ingress show <app>`** prints a hand-off sheet, from `paisans.yaml`
alone, no host contact:

* the hostname, the upstream (`listen`), the health path;
* what the proxy must do: terminate TLS for the hostname, pass `Host`, set
  `X-Forwarded-For` and `X-Forwarded-Proto`;
* a filled-in snippet for Caddy, nginx and Apache, each marked with where the
  operator's own certificate lines go;
* the ufw warning, when `listen` is not loopback.

**`paisans ingress check <app>`** runs from the operator's machine, read only,
and reports each item as pass or fail with the snippet line that fixes it:

1. The hostname resolves to the site's `public_address`.
2. `https://<hostname>/healthz` answers 200 with a certificate valid for the
   hostname; prints the days until expiry.
3. `https://<hostname>/login/oidc` redirects to the issuer with a
   `redirect_uri` that starts `https://<hostname>/`. This is what proves
   `X-Forwarded-Proto` reaches the app.
4. A TCP connection to `<public_address>:<listen port>` fails. The upstream
   must not be reachable around the proxy.

`ingress show` and `ingress check` on a `mode: paisans` app say there is
nothing to hand off and run only checks 1 and 2.

A guide, `docs/guides/behind-your-own-web-server.md`, walks through the same
steps for a reader without the toolkit open.

## Part 2: the host check

New package `internal/hostcheck`. It has three parts, and each is testable
alone.

### Claims

Computed from `paisans.yaml` and the site name, with no host contact: what the
toolkit will take on that machine.

* **Listening ports** with protocol and bind address. WireGuard's udp port;
  etcd's two for members; Postgres, Patroni and bg_mon for `data`; the HAProxy
  cluster port where it runs; Garage's for `storage`; tcp 80 and 443 for a
  gateway or a `mode: paisans` monitor; `listen` for a `mode: external`
  monitor; each app's published port on the mesh address. Built from
  `render/ports.go`, which stays the single list. `preflight`'s own `wanted()`
  is replaced by it, which also gives `preflight` the 80 and 443 it lacks.
* **The `wg0` interface** and the mesh subnet as a route.
* **Host-wide state:** the ufw default policy and enabled state (on a clean
  host only, see below).

### Inventory

A read-only probe over the existing transport, one command per fact, run with
sudo because `ss -p` needs it to name another user's process:

| Fact | Command |
|---|---|
| Docker present, version, package | `docker version`, `dpkg-query` for `docker-ce`, `docker.io`, `snap list docker` |
| Containers | `docker ps -a` with the compose project label and the container's PID |
| Volumes and networks | `docker volume ls`, `docker network inspect` for subnets |
| Listeners | `ss -Hltnup` |
| Interfaces and routes | `ip -o link`, `ip route` |
| Firewall | `ufw status verbose`, `systemctl is-active firewalld` |
| Toolkit state | whether `/srv/.paisans-manifest.json` exists |

**Ownership.** A container is the toolkit's when its
`com.docker.compose.project` label starts `paisans-`. A listener is the
toolkit's when its process belongs to one of those containers, found through
`/proc/<pid>/cgroup`, which names the container ID for both published ports
(`docker-proxy`) and host-network containers. A listener on loopback only, or
belonging to `sshd`, `systemd-resolved`, `chronyd` or `tailscaled`, is the
base system and counts as neither. Everything else is foreign.

### Classification

* **clean**: nothing foreign.
* **shared**: something foreign is present and holds nothing the site claims.
* **conflict**: something foreign holds a claim. A listener conflicts when the
  ports and protocols match and either side binds a wildcard or both bind the
  same address. A foreign Docker network or route overlapping the mesh subnet
  conflicts. An existing `wg0` that the toolkit did not write conflicts.

The class is computed every run and never stored: a host becomes shared the
day someone installs something beside the toolkit.

### What each class does

`host prepare`, `apply`, `site add` and `prune` run the check before anything
else, in a dry run as well as with `--execute`, and print the report.

**conflict: refused.** Nothing is changed. The report has one line per
conflict: the resource, the `paisans.yaml` key that claims it, and what holds
it (container and compose project, or process and PID). There is no override:
the operator moves the foreign service or changes the config.

**shared: proceed, touching only what is the toolkit's.**

* **Firewall required.** ufw must be active with `Default: deny (incoming)`,
  and firewalld must not be active. Otherwise refused, with the reason. On a
  shared host the toolkit never runs `ufw default` or `ufw --force enable`; it
  only adds and removes rules commented `paisans:`, as it does today.
* **No image cleanup.** The behaviour of `--keep-images`, because an image the
  toolkit renders (`caddy`, `postgres`) may also be what a foreign project
  runs from.
* **`prune` removes only labelled volumes.** An anonymous volume with no
  compose label may be a foreign project's; it is listed and kept.
* **Docker is left alone**, as it already is whenever `docker compose` works.
  Ubuntu's `docker.io` is still refused as today.

**clean: today's behaviour,** including enabling ufw with default deny. An
empty Docker install is clean.

The manifest gate in `apply` is unchanged and still refuses to overwrite a
file it has no record of; the host check runs before it and covers what it
cannot see: ports, interfaces, firewall policy, other people's containers.

## Part 3: OIDC clients created by `apply`

Founder decision, 2026-10-08. Creating an app's client at the deployment's own
Pocket ID is part of setting the app up, and `paisans.yaml` is what approves
it: declaring an app of a kind with a known client shape (`kinds.OIDCClient`,
today `mbin` and `uptime`) is the approval for that client.

`apply` gains an identity step for each app it is about to start on the site
whose kind has a client shape:

1. Find the deployment's `pocket-id` app and the site to reach it on, as
   `oidc client create` does (`pocketIDSite`).
2. Probe, build and execute with the existing `internal/oidcclient` code,
   unchanged: the secret is generated locally, written into the secrets file
   first, then sent, and never printed.
3. Render the app with the recorded credentials.

A dry run prints the client plan lines with the rest of the plan and changes
nothing. `apply` never rotates a secret; `oidc client create --rotate-secret`
stays the way to do that, and the command stays for running the step alone.

**Ordering.** Within the apps phase, a `pocket-id` app on the site is started
first and waited on until its health route answers, then clients are ensured,
then the other apps start. When Pocket ID is on another site and cannot be
reached, or its `static_api_key` is not yet in the secrets file, each app that
needs a client and has none recorded is skipped with the reason, and the rest
of the site is applied. A re-run once Pocket ID is up finishes it. An app whose
client is already recorded is not held back by an unreachable Pocket ID.

**Refusals carry over.** An existing client at Pocket ID that does not match
what the kind needs (`checkExisting`) refuses that app, as the command does
today, and `apply` reports it and goes on with the others.

**Writes to the secrets file.** `apply` writes the secrets file for the first
time. It does so through the same recorder as `oidc client create`, re-encrypted
to the recipients beside the file, and refuses up front, before contacting
Pocket ID, when the file is encrypted and no recipient is configured.

## Testing

* `config` and `validate`: table tests for each new rule, including the
  `monitor` + `gateway` and `monitor` + `witness` refusals and the
  `monitor-shares-a-site` warning.
* `hostcheck`: claims for each role and ingress mode; ownership and
  classification from recorded command output fed through a fake transport.
  Fixtures: a clean host with an empty Docker; a shared host running a foreign
  host-network Caddy on 80 and 443 plus unrelated containers, against a
  `mode: external` monitor (shared) and a `mode: paisans` monitor (conflict);
  a host running a foreign Postgres on 5432 against a `data` site (conflict);
  a shared host with ufw inactive, with default allow, and with firewalld
  (each refused). Fixture addresses are documentation ranges.
* `hostprep`, `apply`, `prune`: the shared-host branches never issue
  `ufw default`, `ufw --force enable`, `docker image rm`, or `docker volume rm`
  on an unlabelled volume; the conflict branch issues nothing after the
  inventory.
* `render`: a monitor's apps absent from the gateway Caddyfile; the
  `mode: paisans` monitor's own Caddyfile; `PUBLIC_BASE_URL` and
  `TRUST_PROXY`; the `listen` publish; the self-check in the seed.
* `dns`: monitor apps point at the monitor's address.
* `ingress check`: against an `httptest` TLS server for each failing item.
* `apply` identity step: a fake Pocket ID transport; client created and
  recorded before the app renders; dry run sends nothing; unreachable Pocket
  ID skips only apps without a recorded client; a mismatched existing client
  refuses that app alone; the secret never appears in output.

## Delivery

Three pull requests into `develop`, the host check first because it is what
makes the others safe to run against a machine that is not empty:

1. `feat/host-check`: Part 2, and `preflight` moved onto it.
2. `feat/monitor-role`: Part 1, the guide, and the amendments to
   `2026-10-07-uptime-monitoring.md`.
3. `feat/apply-oidc-clients`: Part 3.

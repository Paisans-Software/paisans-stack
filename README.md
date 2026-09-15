# paisans-stack

Toolkit for installing and operating a paisans community stack: a private,
federated community on hardware its organizers control.

**Status: nothing is implemented yet.** This repository holds the design and
will hold the implementation. Do not expect anything here to run.

Written in **Go**, distributed as a single static binary so an operator needs no
runtime — see [docs/decisions.md](docs/decisions.md) for why, and why not
Terraform, Ansible or shell.

## What it is meant to do

Install a complete community stack on one machine, then let a second site be
added later as a manually invoked, additive step — without touching application
configuration and without a migration.

```
paisans init --domain example.org          # site 1, standalone, HA-ready
paisans site add --role witness  vm.example.org
paisans site add --role standby  home-b.example.org
paisans site remove home-b.example.org
paisans failover status
paisans failover switchover
paisans check
```

Command names are provisional.

## Design rules that everything else follows from

### Rule 1: the cluster always runs, even when it has one node

A single-site install runs Postgres under Patroni, with etcd and a local
HAProxy, as a cluster of one. It would be simpler to install plain Postgres and
add Patroni when a second site appears — but that is a *conversion*, performed
on live member data, not an addition. Running the control plane from the start
costs a single-site adopter roughly 150 MB and some extra moving parts. It buys
an upgrade path that cannot corrupt anything.

### Rule 2: applications always talk to a local HAProxy

Every stack with `cluster` placement connects to `127.0.0.1:5000` from the first
install, where a local HAProxy holds a backend list that initially has one entry.
Adding a site grows that list. No application configuration changes, ever. This
one indirection is what makes a second site an operation rather than a project.

Pinned stacks talk to their own database directly and are not affected — see
rule 5.

### Rule 3: object storage from the first install

The same logic as rule 1, applied to files. **Garage ships out of the box**,
single-node at `init`, replicated across sites when one is added.

Starting with media on local disk and moving it later is a migration, not an
addition — and for a federated instance it is a migration with consequences
outside the deployment. Mbin's own procedure is sync, shut down, sync again, and
its documentation warns that changing media URLs breaks links on remote
instances that have already cached them. Remote servers hold references we
cannot recall.

What Garage actually absorbs is narrower than "all file storage", and the
toolkit should not overstate it:

| Stack | S3 support | Notes |
|-------|-----------|-------|
| Mbin | full | `S3_KEY`/`S3_SECRET`/`S3_BUCKET`/`S3_REGION`/`S3_ENDPOINT`, plus switching `public_uploads_filesystem` to the S3 adapter in `config/packages/oneup_flysystem.yaml`. Upstream strongly advises a media reverse proxy so URLs stay stable across provider changes |
| Outline | full | `FILE_STORAGE=s3` with the `AWS_*` variables; non-AWS endpoints need `AWS_S3_FORCE_PATH_STYLE=true`. Known upstream bug: the bucket must not be named `outline` |
| Synapse | partial | `synapse-s3-storage-provider` is a *storage provider* that supplements the media store. **A local media directory is still required.** `store_synchronous: True` writes to S3 immediately; the bucket prefix cannot be changed once media exists |
| Pocket ID | full, plus better | `FILE_BACKEND` takes `filesystem` (default), `s3`, or **`database`**. See the note below — `database` is the recommendation |
| WriteFreely | none | no object storage support; see the open question about this stack |

**Pocket ID should use `FILE_BACKEND=database`, not S3.** Its uploads are
profile pictures and admin-uploaded branding — on a real deployment, about a
megabyte in total. Putting them in Postgres means they ride streaming
replication with everything else: a promoted site has them already, with no sync
job and no reconciliation. It also removes a dependency from the one service
that gates every other one — if object storage is unavailable, sign-in still
works. Consistency with the other stacks is worth less here than keeping the
identity provider's dependency list as short as possible.

Left on the default `filesystem` backend, `data/uploads` holds
`profile-pictures/<userId>.png`, generated initials avatars under
`profile-pictures/defaults/`, and `application-images/` (favicon, background,
email logo). None of it is in the database. A site promoted without it serves
broken avatars and silently reverts to stock branding mid-incident — not an
outage, but an alarming one to look at, and the branding is configured state
that an admin set deliberately.

That leaves **Synapse as the only stack with unavoidable node-local state**,
because its S3 support supplements the media store rather than replacing it. Its
local media directory has to be replicated out of band, or accepted as lost on
promotion. Either is defensible; leaving it undecided is not.

### Rule 4: the domain is a one-way door

Federation identity is the hostname. Mbin's actor keys are bound to it, and
remote instances have recorded it. Changing a community's domain after it has
federated is a rebuild, not a rename.

`init --domain` should say so at the prompt, in plain words. It is the one
choice here that cannot be walked back, and it is made in the first thirty
seconds by someone who has not read anything yet.

### Rule 5: placement is a per-app property, and pinned is the plain case

Not every app can follow a failover. Synapse is the first example — its S3
support supplements the media store rather than replacing it, so a local media
directory is always required — but this is a property every app has, not a
Synapse special case.

So each stack in the inventory declares where it runs:

| Placement | What it means | Availability |
|-----------|---------------|--------------|
| `cluster` | joins the HA Postgres cluster, follows its primary | survives losing one site |
| `pinned: <site>` | ordinary self-contained deployment at one named location | that location's |

```yaml
placement: cluster
placement: { pinned: vm }
```

**`pinned` is the plain case and needs nothing built.** It is the app exactly as
upstream ships it: its own compose file, its own Postgres container, its own
volumes. No etcd, no Patroni, no HAProxy, no shared cluster. A single-site
install is every app pinned to the only site, which is why this costs a new
adopter nothing.

`cluster` is the opt-in variant: drop the stack's own `postgres` service, point
it at `127.0.0.1:5000`, put its media in Garage. Two compose profiles per app,
and the toolkit picks one.

Pinned is not restricted to a cloud VM — pinning to a home is equally valid. The
cloud VM is one location among several.

**An app that keeps its data outside the cluster can only be pinned, and
`cluster` is refused for it.** Cluster placement means running on every site
with the apps role and sharing one database through the local proxy. An
application storing its data anywhere else gets neither half: it would be
rendered onto each of those sites with its own separate storage, so one
hostname would serve two deployments that diverge the moment anybody writes,
and a failover would move readers between them. WriteFreely is the case that
exists, having never supported Postgres and running on a SQLite file in its own
data directory. Pinning it is not a downgrade, because that was always its
availability.

#### A pinned app must be pinned all the way down

The tempting half-measure is to pin the app but leave it using the shared
cluster. Do not. It then needs **both** its own location and the cluster site to
be reachable, so its availability becomes the product of two sites — **worse
than either alone** — and every query crosses the WAN.

**A half-pinned app is less available than either site.** Pinning moves a
failure domain; it does not remove one. App, database and media go together or
the placement is a lie.

Backups still centralise to Garage. Only replication and failover are given up.

#### Moving a pinned app

Because a pinned stack is self-contained, relocating it is ordinary work:

```
docker compose down                     # stop first — see below
tar czf stack.tgz /srv/<stack>
scp stack.tgz newhost:
# on the new host: untar, open firewall, docker compose up -d
# then update the DNS A record
```

Four things make that sequence true rather than nearly true:

* **Stop the stack before archiving.** Tarring a live Postgres volume produces a
  torn copy that may restore and then fail later. Stop it, or `pg_dump` instead.
* **Use bind mounts under `/srv/<stack>/`, not named volumes.** This is why the
  tar is one line instead of a `--volumes-from` dance, so the toolkit should lay
  pinned stacks out that way from the start.
* **Lower the DNS TTL before the move**, not during it.
* **The hostname must not change** — same name, new address. For an ActivityPub
  app the domain is its identity (see rule 4); moving hosts is routine, moving
  domains is a rebuild.

For a stack with large media, two `rsync` passes — one while running, one after
stopping — beat a single tar.

`app move` should exist as a command, and should say plainly that it involves
downtime. It is a migration, not a configuration flip.

#### Do not pin an app onto the witness without thinking

If the cloud VM is both the etcd witness and the host for a pinned app, they
compete. etcd's stability depends on fsync latency, and a busy application with
its own database is not a quiet neighbour. Disk contention there means missed
heartbeats, spurious elections, and **the database failing over for no reason**
because something else was compacting.

In order of preference: a separate VM for pinned apps; the same VM with etcd's
data directory on its own device; or a box big enough, with monitoring.

The toolkit should **warn** here rather than refuse. Unlike a co-located witness,
which is incoherent, this is merely risky — and the risk is acceptable if it is
chosen rather than stumbled into.

#### There is one HA cluster, and that is deliberate

Multiple named Postgres clusters are **out of scope**.

The motivating idea — "high availability for the forum but not the wiki" — turns
out not to exist. One Postgres cluster holds many databases, and streaming
replication is physical: the whole cluster replicates together. So every
database in it gets HA **for free**. There is no per-database switch, and
nothing is saved by excluding an app. Leaving it in costs nothing.

A second cluster would only be justified by a different Postgres major version,
observed resource contention between apps, or a wish for independent failover
timing. None of those are worth a second Postgres process, a second failover
drill, and a second thing to get wrong, on home hardware and for a community of
this size. If one of them ever becomes real, it is a decision to revisit with
evidence — not a knob to ship.

An app that must not share the cluster gets `pinned` instead, which is simpler
in every respect.

**A useful consequence: exactly one Patroni per data-bearing host.** Pinned apps
run plain Postgres with no Patroni, and there is only ever one cluster. So the
`/dev/watchdog` device has exactly one claimant, contention cannot arise, and
fencing is available to the one Patroni that needs it. Ruling out multiple
clusters removed an entire class of problem rather than merely deferring it.

#### Consequence for routing

The gateway's Caddy configuration becomes inventory-driven: a pinned app's
hostname points at its location, everything else follows the current primary.
A template job, not an orchestrator job.

It is assembled rather than written: each `kind` ships its own Caddy snippet in
its template set, and the gateway file holds only what is cross-cutting. See
*The template set owns everything app-specific, routing and shims included*.

## Adding a site, and the constraint that shapes it

etcd quorum is a majority: 1 member survives nothing but works; 3 members
survive one loss; **2 members survive nothing and are strictly worse than 1.**

So a join must take the cluster from one voter to three in a single operation —
the second site and a witness together. It can never rest at two.

## The witness is a third location, not a cloud account

A witness is a voting member that stores no data and serves no traffic. It
exists so that when two sites cannot reach each other, exactly one of them can
still form a majority and know it is safe to serve.

**A small always-on cloud VM is the suggested default**, for two reasons: it is
the third failure domain most organizers can actually obtain, and it doubles as
the public entry point for sites on residential connections whose addresses
change. One box, two jobs.

**It is not required.** What is required is a location that fails independently
of both sites. Another organizer's house works. A machine at a workplace, or a
friend's spare room, works.

### A witness must never share a failure domain with a voter

Running a witness container beside the database on a single server is worse than
having no witness at all, and the toolkit must refuse it.

One site with one etcd member has quorum 1. It works, and the only thing that
stops it is the machine itself — which would stop Postgres anyway. Add a witness
on that same machine and quorum becomes 2 of 2: now the witness container
crashing takes the database down with it, while Postgres is perfectly healthy.
The machine dying still loses everything, because both members were always in
the same place. A tiebreaker between two things that can always reach each other,
or are both gone at once, breaks no ties.

**One site correctly runs exactly one etcd member.** That is a stable supported
configuration, not a compromise awaiting a fix, and the toolkit should not
pretend otherwise by installing a placeholder.

### Nothing here is permanent

A witness holds no data, so relocating one is cheap and scriptable — remove the
member, add the new one. Start with whatever third location is available and
move it when a better one appears.

If a gateway VM is in use, the third location already exists before anyone asks
for a witness: the join enables the witness role on a machine already running,
rather than provisioning a new one.

| Stage | etcd members | Witness |
|-------|--------------|---------|
| `init`, no gateway | 1 | none — correct, not missing |
| `init` with gateway | 1 | gateway serves ingress; witness role dormant |
| `site add` | 3 | enable on the gateway, or name a third location |
| later | 3 | `witness move` relocates it |

Declining a witness entirely is supported and means **giving up automatic
failover**: two sites with two voters cannot safely promote anyone, so `site add`
configures plain streaming replication with a documented manual promote instead.
That is a legitimate choice. It is a different mode, not a slightly degraded
version of the same one, and the toolkit should say so plainly.

## Configuration

A deployment is declared in two files, kept in its own directory (usually its
own private git repo). The toolkit ships examples under `examples/`.

| File | Contents | Committable |
|------|----------|-------------|
| `paisans.yaml` | topology, sites, clusters, apps, placements, versions | yes, plain |
| `secrets.enc.yaml` | every credential, encrypted with sops + age | yes, encrypted |

They are separate on purpose. Editing topology is the common operation, and
with one combined file every edit would decrypt everything into an editor and a
temporary file. Split, a hostname change needs no key at all, and a change can
be reviewed by someone who cannot read the secrets.

See [`examples/paisans.example.yaml`](examples/paisans.example.yaml) and
[`examples/secrets.example.yaml`](examples/secrets.example.yaml).

### The config declares intent; etcd holds truth

Nothing about current state belongs in these files — not which node is primary,
not replication lag. `paisans failover status` reads etcd. A config file that
starts recording status drifts, and someone trusts a stale value during an
incident.

### The image an app runs is declared, not implied

`kind` selects which template set renders. It does not decide which image runs.
Keeping those separate is what lets an operator move to a fork, hold a version
back, or take an upgrade on their own schedule, without the toolkit shipping a
release for it:

```yaml
apps:
  talk:
    kind: mbin
    hostname: talk.example.org
    placement: cluster
    images:
      app: ghcr.io/example-org/mbin:v1.2.3
```

Every key is optional and an unset one takes the default its `kind` ships, so an
adopter who never opens this stanza runs a tested set.

**A map, not a single `image`.** A stack is several containers, and naming only
the application one would work until the first operator needed a different
Valkey. The keys are service names in the `kind`'s compose template, which also
means an unknown key is a typo the toolkit can refuse rather than ignore.

**One full reference per key, not `image` plus `tag`.** Switching to a fork
changes registry, repository and tag together. Split into two fields, a
half-finished switch parses cleanly and pulls something nobody intended.

**Changing `images` is not changing `kind`.** A fork that renames an environment
variable, moves a config path or adds a required setting needs a different
template set, and that is a new `kind`. The `images` map is for a different
build of the same software, and pointing it at software that merely resembles
the original produces a stack that renders and then fails at boot.

**Floating tags are refused, not warned about.** `latest` makes two runs of
`apply` produce different deployments from identical inputs, which contradicts
the premise that the config plus the secrets reconstruct the stack. That is
incoherent rather than risky, so it takes a refusal by the rule above. A digest
(`@sha256:...`) is accepted and is the stronger form.

**A version change is not verified to be safe.** Many of these applications run
schema migrations at boot, against live member data, and a downgrade after one
is usually not a downgrade at all. The toolkit cannot know which release does
that, and must not imply it checked: it warns on a changed reference, names the
backup contract, and proceeds. Verifying upstream's upgrade notes is the
operator's work.

**`paisans check` compares the running image against the declared one.** Someone
will eventually run `docker compose pull` on a host, and the difference between
what is declared and what is running is exactly the kind of drift that is
invisible until a rebuild produces a different stack.

### Three kinds of secret

Most of `secrets.enc.yaml` is machine-authored. Nobody invents forty passwords.

| Kind | Examples | Origin |
|------|----------|--------|
| **Generated** | Postgres superuser/admin/replication passwords, Garage keys, WireGuard private keys, Mercure JWT, RabbitMQ and Valkey passwords | created at `init`, never typed or seen |
| **Pasted** | Cloudflare API token, SMTP credentials | issued elsewhere, supplied by a human |
| **Captured** | OIDC client secrets | minted by a running service, then recorded |

The captured case matters. An identity provider mints a client secret and the
app needs the identical value; recording it here makes it reproducible instead
of existing on exactly one host. But minting one is a privileged mutation that
belongs to a human — the toolkit records the value after the fact and must not
automate the approval away.

### Rendered configuration is a build artifact

`paisans apply` renders it. Nobody edits it, and `apply` refuses to clobber a
rendered file that has local modifications until the difference is resolved.

```
WORKSTATION                        HOST                          CONTAINER
───────────                        ────                          ─────────
paisans.yaml      ─┐
secrets.enc.yaml  ─┤ render
age key           ─┘   │
                       │ ssh push
                       ▼
                  /srv/<stack>/.env       (plaintext, 0600) ──► env vars
                  /srv/<stack>/config.ini (plaintext, 0600) ──► bind mount
                  /srv/<stack>/compose.yaml
                  caddy/, haproxy.cfg, patroni env
                                                       docker compose up -d
```

`sops` and `age` are installed on the workstation only. Hosts do not have them.
Containers never know they exist, and nothing is written *into* a container —
Compose passes env at start. Changing a secret in an `.env` therefore means
re-render plus `compose up -d` to **recreate**; a `restart` will not pick it up.
A secret in a bind-mounted file is cheaper to change, for the reason below.

#### A `kind` ships a template set, not a single `.env`

Not every application is configured through environment variables, and the
stacks named in this document are already split on it. Mbin reads `.env`.
WriteFreely reads an INI file, `config.ini`, whose `[server]`, `[app]`,
`[database]` and `[oauth.generic]` sections carry everything it needs, SSO
client credentials and `disable_password_auth` included
([config reference](https://writefreely.org/docs/latest/admin/config)). Synapse
reads [`homeserver.yaml`](https://element-hq.github.io/synapse/latest/usage/configuration/homeserver_sample_config.html).

So the unit the toolkit ships per `kind` is a **template directory**, and
`apply` renders every file in it:

```
templates/mbin/                             templates/writefreely/
  compose.yaml.tmpl                           compose.yaml.tmpl
  .env.tmpl                                   config.ini.tmpl
  caddy.snippet.tmpl                          caddy.snippet.tmpl
  config/packages/oneup_flysystem.yaml.tmpl
```

**A template's path is its destination.** The layout under `templates/<kind>/`
mirrors the layout under `/srv/<stack>/` on the host, so where a rendered file
lands is read off the tree rather than held in a mapping somewhere else. A file
the application expects at a path inside its own image is bind-mounted there by
that kind's `compose.yaml`.

The alternative is to treat `.env` as the shape and bolt on a per-app escape
hatch for anything that is not one. That inverts the majority and the exception
for no gain, because nothing in the secrets path is env-specific: every rule
above holds file by file, whatever the format. A rendered `config.ini` is a
build artifact, is written 0600, receives its secrets at render time on the
workstation, and is refused a clobber when locally edited, on exactly the same
terms as a rendered `.env`.

One consequence runs against the intuition and is worth stating plainly.
Environment variables are readable through `docker inspect` and
`/proc/<pid>/environ`, as the limits below say. A bind-mounted file at 0600 is
not. **File-configured applications are the better case here, not the awkward
one**, and where an application accepts either, the file is preferred.

Reload differs as well. A changed `.env` needs `compose up -d` to recreate the
container, because Compose passes environment only at start. A changed
bind-mounted file needs a restart at most, and the container keeps its identity.
`apply` should take the narrower action rather than recreating a stack it did
not have to, since a recreate is an outage however brief.

#### The template set owns everything app-specific, routing and shims included

The template directory is not just the files an application reads at startup. It
is **everything the deployment needs that is knowledge about that application**,
and two categories are easy to leave out.

**Its Caddy snippet.** Routing is rarely uniform across a stack. Mbin's
documentation advises a media reverse proxy so URLs survive a provider change
(rule 3). A Matrix homeserver needs its `.well-known` responses and federation
listener. An auth gate has to be told which callback paths to leave alone or it
breaks the sign-in it exists to protect. Each of those is a fact about one
application, so it lives beside that application's other templates and arrives
and leaves with it.

**Its shims.** Where upstream ships a file the deployment has to override, the
override is a template like any other. Rule 3 already names one, and it is small
enough to show. Mbin's `config/packages/oneup_flysystem.yaml` defines both a
local adapter and an S3 adapter, and binds uploads to the local one:

```yaml
  filesystems:
    public_uploads_filesystem:
      adapter: default_adapter
      #adapter: kbin.s3_adapter
```

Rule 3 puts media in Garage from the first install, so the rendered override is
that file with the adapter switched to `kbin.s3_adapter`
([Mbin's S3 documentation](https://docs.joinmbin.org/admin/optional-features/s3_storage/)).
The `S3_*` variables in `.env` supply the endpoint and credentials; this file is
what decides whether they are used at all, which is why setting the variables
alone appears to work and silently keeps writing to local disk. It renders to
`/srv/talk/config/packages/oneup_flysystem.yaml` and is bind-mounted over the
copy in the image. Entrypoint wrappers, module configuration and other
dropped-in override files work the same way.

**A shim is a rendered, readable file in the template directory, never a private
image build.** Baking one into an image moves it somewhere an operator cannot
read during an incident, and quietly makes the `images` reference above a lie
about what is running.

A shim is also a deliberate divergence from a file that upstream owns, and
upstream can change that file underneath it. There is no way to remove that
cost, so make it visible instead: **a shim template names the upstream path it
overrides and what it changes**, in a comment at the top, and a changed image
reference is the thing to go looking at when a stack starts behaving as though
the override is not there.

The rejected alternative is one gateway Caddyfile template that knows every
`kind` through conditionals. Two things go wrong with it. Adding an application
becomes a diff in a file that every other application shares, so the cost of
adding one grows with how many are already there. And a mistake in one
application's routing takes the whole gateway config with it, rather than one
host block.

That leaves the gateway's own template holding only what is genuinely
cross-cutting: TLS and the DNS-01 configuration, the trusted proxy setting
(fixed to the mesh subnet, not configurable), the auth gate, and one host block
per app that includes that app's snippet. A snippet never hardcodes where its
application runs; it receives the location from the inventory at render, which
is what keeps a pinned app and a cluster app the same shape.

One consequence follows from assembling a shared file out of per-app parts, and
it takes a gate: **`apply` validates the assembled gateway configuration before
reloading it, and refuses to reload one that does not validate.** A rendered
snippet that is wrong should cost the operator an error message on the
workstation, not the public address of every application at once.

### Decryption happens on a workstation, not on a host

Rendering locally and pushing means no age key ever reaches a host. That
follows the existing posture: on a shared host, users with `sudo` or `docker`
membership are root-equivalent, so a decryption key there is readable by
someone other than its owner.

Hosts end up with plaintext `.env` regardless — Compose requires it — so the
gain is not host hardening. It is **blast radius**: root on one host yields that
host's rendered secrets, not the gateway's WireGuard key or another site's
credentials. The complete set exists only encrypted.

Decrypting on the hosts instead is simpler to automate and gives unattended
deploys. A single-admin fork may prefer it. It should not be the default, and
choosing it should print what it gives up.

Two honest limits, which belong in any doc that describes this:

* **Encryption protects the secret everywhere it is stored, not where it is
  used.** The host's `.env` is plaintext. What changes is that it becomes a
  disposable build artifact rather than the only copy in existence.
* **Environment variables are visible** in `docker inspect` and
  `/proc/<pid>/environ`. `env_file` does not change that. Encrypted at rest
  does not mean hidden at runtime.

### Backup and restore

`paisans backup` must produce **four** things. Any three of them is a partial
restore that looks complete until someone goes looking for their account.

| Artifact | Holds | Without it |
|----------|-------|------------|
| `paisans.yaml` | topology | nothing knows what to build |
| `secrets.enc.yaml` | credentials | every secret must be regenerated and every OIDC client re-minted |
| Database dumps / WAL archive | member accounts, posts, settings | the community is empty |
| Object storage | media, uploads, attachments | posts render with broken images |

The config files capture **infrastructure**, not **application state**. Groups,
magazines, moderation, collections, and anything set through an admin panel
live in the database. A community's identity provider configuration is
particularly easy to assume is in the config when it is not.

The age private keys are the fifth thing, and they must be backed up somewhere
other than the repo they decrypt.

## Bootstrapping a site

`apply` renders configuration on a workstation and pushes it over SSH — but the
WireGuard mesh those files create does not exist yet. First contact with any host
has to happen over a path that is not the tunnel.

That is what `ssh:` in a site block is: the **bootstrap route**. A LAN address, a
public hostname, whatever the operator can actually reach on day one. Used once
per site and never again; afterwards everything goes over the mesh addresses.

**It must be a real address the operator already has.** Not an overlay network,
not a name that only resolves inside one deployment's private mesh. A toolkit
that quietly depended on one would work on the machines it was written for and
fail on every fork.

### Where WireGuard keys are generated

On the workstation, not on the host — which is the opposite of the usual advice,
for a reason.

Generating on the host and reading the public key back means the private key
never traverses anything. But then it exists in exactly one place, it is not in
`secrets.enc.yaml`, and rebuilding a dead node produces a *new* identity that
every peer must be reconfigured to accept.

Generating on the workstation puts the key in the encrypted file with everything
else. Rebuilding a node from bare metal restores the **same** identity and no
peer changes at all. The key travels over SSH — the same channel already trusted
to carry every other rendered secret.

### `init` — one site, no mesh

There is nothing to tunnel to yet.

```
paisans init --domain example.org --site home-a --ssh home-a.local
```

1. Preflight over SSH. Refuse loudly rather than proceed on a warning.
2. Generate every secret — WireGuard keypair, database passwords, object storage
   keys — into `secrets.enc.yaml`.
3. Render and push.
4. **Bring up `wg0` with this site's address and an empty peer list.**
5. Spilo as a cluster of one, etcd as a single member, HAProxy with one backend,
   Garage single-node.
6. Apps start, pointed at `127.0.0.1:5000`.

Step 4 is the one that is easy to skip and expensive to add later. A `wg0` with
zero peers still provides `10.44.0.1`, and every service binds to it from the
first install. Joining a site then only **adds peers** — no service is
reconfigured and no address changes.

Same principle as running the cluster at one node: build the final shape
immediately, then grow it.

### `site add` — the gateway and witness

The straightforward case, because the VM has a stable address and is the one
thing everything else can dial.

```
paisans site add vm --roles gateway,witness --ssh vm.example.org
```

1. Preflight the VM.
2. Generate its keypair; record it.
3. Render the VM's `wg0`: peer home-a, no endpoint — home-a dials out.
4. Render home-a's `wg0`: peer vm **with** an endpoint, and `PersistentKeepalive`
   so the NAT mapping stays open.
5. Push both; bring both up.
6. **Verify handshakes in both directions.** Stop here on failure.
7. etcd stays a single member. The VM does not become a voter yet, because two
   voters are worse than one.

### `site add` — a second data site, and the one hard problem

```
paisans site add home-b --roles data,apps --ssh home-b.local
```

Home-b reaches the VM the same way home-a does. Fine.

**But home-a and home-b are both behind NAT with changing addresses, and neither
has an endpoint the other can dial.** WireGuard needs one side to know where to
send the first packet, and neither does.

Three ways out:

* **Relay home↔home through the VM.** Works immediately with no new machinery.
  Cost: with `synchronous_mode: true` every commit waits a round trip, so writes
  pay home-a → VM → home-b instead of going direct. Largely mitigated by siting
  the VM near the homes, which costs nothing.
* **Dynamic DNS per home.** Each home gets a resolvable endpoint. Two problems:
  WireGuard resolves endpoints at load and does not re-resolve on its own, and
  **publishing a home's public address in DNS is a privacy decision**, not merely
  a technical one.
* **Endpoint discovery through the VM.** Homes report their current address; the
  VM distributes it. This is rebuilding part of what a coordination service does,
  and it is real work for a toolkit meant to stay small.

**Recommendation: relay, and site the VM near the homes.** Then measure. Direct
home-to-home is an optimisation to pursue if the measurement is bad, not a
prerequisite — and relaying keeps the networking to static configuration with no
daemon and no coordination service, which is why plain WireGuard was chosen.

### The staged gate, and the half-joined site

**Never touch etcd membership until every tunnel is verified.** The reason is
arithmetic: going from one member to three with both new members unreachable
leaves one of three, no majority, and **the cluster stops.** A working
single-site deployment can be taken down by trying to add to it.

So a join is staged, and every stage is a gate:

| Stage | Gate before proceeding |
|-------|------------------------|
| 1. Preflight | every check passes |
| 2. WireGuard pushed and up | **handshake verified in both directions, from both sides** |
| 3. etcd one → three, atomically | all three members report healthy |
| 4. Spilo joins, clones, streams | replication lag converging |
| 5. HAProxy backends, watchdog, Garage | smoke test passes |

Failing at stage 2 rolls back cleanly: drop the peer entries, leave the running
cluster untouched. Failing at stage 3 or later is harder to undo, which is
exactly why stage 2's gate must be strict.

`site add` must be **idempotent** — re-running after a fixed network problem
resumes rather than restarting.

### Preflight

Half the design's assumptions are mechanically checkable, and every unchecked one
will eventually be violated:

* SSH reachable; privilege escalation works
* Docker present and recent enough
* **OS and glibc major version match the existing sites** — mismatched collation
  libraries between replicas risk corruption
* Kernel WireGuard available; `/dev/watchdog` present
* Ports free: 51820, 2379/2380, 5000
* The mesh subnet does not collide with anything the host already routes
* Clock synchronised — etcd and Patroni both depend on it
* Storage is local, not network-attached, with room
* **Round-trip time measured to every existing site**, and etcd's heartbeat and
  election timeout derived from it rather than guessed

That last one turns a documented manual step into an automatic one, which is the
difference between a tuning rule people follow and one they read once.

## Moving the gateway

A single site runs `roles: [data, apps, gateway]` — the same machine serving the
public internet and holding the database. That is correct for one site, and total
failure of that site is the expected behaviour of having one site.

Once a **second data site** exists, the gateway should move off both of them.

The reason is that otherwise the failover does not reach the public path. If
home-a is the gateway and home-a dies, the database fails over in about a minute
— but public DNS still points at home-a, and nothing is reachable until a record
changes and propagates. Automatic database failover sitting behind a manual DNS
change is not automatic.

The gateway's job is to be **the address that never moves**, and a residential
connection cannot be that.

### Make it an overlap, not a cutover

The move should never be "stop here, start there."

```
1. The new gateway comes up, obtains certificates, and serves —
   while the old one is still serving
2. Verify the new gateway answers correctly for every hostname
3. Repoint DNS
4. Wait out the old TTL; the old gateway keeps serving stragglers throughout
5. Stop the old gateway
```

No gap, and reversible at any point before step 5 — repoint DNS back and the old
gateway is still running. That is the difference between an easy migration and a
nervous one.

This works because the new gateway can reach the apps over the mesh, which was
established when its site joined. The ordering already holds.

### DNS-01 is what makes step 1 possible

With HTTP-01, a server can only obtain a certificate for a name that already
points at it — so the new gateway cannot hold valid certificates until DNS moves,
and DNS should not move to a server without them.

**DNS-01 does not require inbound reachability.** The new gateway proves control
of the domain through the DNS provider's API and can hold fully valid
certificates before serving a single request.

Consequence: no certificate state is migrated. The new gateway issues its own.

#### The provider is declared, and its module has to be in the binary

A DNS provider in Caddy is a Go module compiled into the binary with xcaddy, not
a setting. Upstream's image carries none, so a configuration naming a provider
cannot load on upstream's image.

```yaml
acme:
  provider: desec
```

That one field decides two things: the `acme_dns` directive in the gateway's
Caddyfile, and which image the gateway runs.

**Nothing is built on a host.** The gateway is also the etcd witness in the
suggested topology, and etcd's stability is a function of fsync latency, so a
compile there competes for exactly the resource that placement was chosen
around. Images with the modules compiled in are published at
`ghcr.io/paisans-software/caddy`, pinned here by digest because every tag that
repository publishes moves, exactly as upstream's do.

The cost is stated rather than hidden: a deployment's gateway depends on that
registry and on those images being rebuilt. The escape hatch is what keeps it
from being lock-in.

**A provider we publish no image for is supported, with an image of your own:**

```yaml
acme:
  provider: route53
  image: ghcr.io/example-org/caddy-route53:2.11.4
```

Declaring a provider with no published image and no `acme.image` is **refused**:
the module would never reach the gateway, so it would hold no certificate for
any hostname. Declaring upstream's own image is **refused** for the same reason,
and it is the only image a reference alone can be judged by.

Everything else is checked at `apply`, by asking the binary which modules it has
rather than inferring it from a name.

### Trusted proxies must be the mesh subnet — set this at `init`

This is the one item that is expensive to retrofit, and it fails quietly.

Applications behind a reverse proxy decide which upstream to trust for
`X-Forwarded-For`. If that is set to a specific host because that host was the
gateway on day one, then the moment a different node starts proxying, every
request arrives from an untrusted source. The symptoms are unpleasant and
indirect: wrong client addresses in logs and rate limiting, possibly broken
redirects, and for anything checking addresses during a session, broken logins.

**Set trusted proxies to the mesh subnet from the first install.** Then any node
in the mesh can front the apps and the gateway role becomes genuinely portable.

Same principle as apps connecting to `127.0.0.1:5000` rather than a named
database host: an application should not know what is on the other side of an
indirection.

### What travels with the role

The **auth gate goes with the gateway.** It sits on the edge beside the reverse
proxy and is part of it, not a separate app. The toolkit should treat reverse
proxy and auth gate as one unit rather than letting someone move half of it.

OIDC callback URLs point at the public domain, not at the gateway host, so they
need no change. Worth stating plainly, because "will this break single sign-on"
is the first question anyone asks about this move.

### It is a config change

```yaml
sites:
  home-a: { roles: [data, apps] }        # gateway removed
  vm:     { roles: [gateway, witness] }  # gateway added
```

`apply` then performs the overlap above. Two things it should do unasked: lower
the DNS TTL well in advance and restore it afterwards, and refuse to stop the old
gateway until the new one has answered correctly for every hostname.

Rollback is moving the role back and applying again.

## Worked example: should the identity provider live on the gateway?

A reasonable question, and the answer is more interesting than a flat no.

First, a modelling point. `pocket-id` is not a site role — **sites declare roles,
apps declare placement**:

```yaml
sites:
  vm: { roles: [gateway, witness, apps] }   # the vm may host apps

apps:
  auth:
    placement: { pinned: vm }               # the actual move
```

Roles say what a site provides; placement says where an app runs. Keep those
separate or every new app becomes a new role.

It is feasible. It is not advisable.

**Pinning gives up failover.** On `cluster` placement the identity provider rides
the HA database and survives losing a site entirely. Pinned to the gateway, its
availability becomes exactly that machine's, with no failover at all.

The natural counterargument is that losing the gateway blacks out public access
anyway, so the identity provider being there costs nothing extra. That is true,
and it is why this is not a silly idea. But it means the move **gains nothing** —
it survives nothing it would not otherwise have survived — while the costs are
all real:

* **A second database to operate**, separately backed up and upgraded, outside
  the archive that covers everything else. That partly undoes "one story instead
  of four", for the app that least needs it.
* **Gateway sizing.** A witness box is specified for a reverse proxy, a tunnel
  and etcd. Adding a database and an application roughly doubles it.
* **fsync contention with etcd** — the same risk recorded above, whose failure
  mode is spurious database failovers.

**And the argument that settles it: the gateway is the public attack surface;
the identity provider is the crown jewels.** Co-locating them puts the service
holding every member's credentials on the most internet-exposed machine in the
deployment. Today it sits behind the gateway on a host with no inbound
reachability at all.

There is a superficially similar argument in the other direction — isolate the
identity provider so compromising an application host does not yield it. But
that points at a separate *unexposed* host, not at the one running the public
web server.

**Recommendation: leave it on `cluster` placement.** It gets high availability
for free, its uploads ride replication when `FILE_BACKEND` is `database`, it
stays off the public-facing box, and it needs no second database.

The one scenario that would change this: if authentication outages became the
dominant complaint *and* the gateway itself were made redundant. With two
gateways, "the gateway died" stops meaning "everything is dark", and pinning the
identity provider to the more reliable tier starts to buy something. That is a
different design.

## Where documentation lives

**Generic material is canonical here.** Architecture, runbooks and the recovery
procedure — at a level true for any community, naming no hosts — live in this
repository. A community that has lost everything can still read them, because
they are not hosted on the thing that failed.

That is the whole point. The outage that started this design took the stack down
and the documentation with it, including the instructions for recovering the
stack. Documentation that shares a failure domain with its subject is not
available when it is needed.

A template for [deployment agent rules](docs/deployment-agent-rules.md) is
included for the same reason. It carries the two things that transfer between
communities unaltered (decision authority, and the blast-radius tiers that say
how much human involvement an action requires), so that after a catastrophic
loss the rules governing the recovery are readable before the wiki is back.
Operators copy it into their own configuration repository as `AGENTS.md`, where
every agent reads it without being told to.

It is a **floor**, and it is not a Charter. A community's Charter is its
mission, values and code of conduct; it stays in the community's wiki and it
governs these rules rather than being replaced by them. Those values, and every
community-specific choice, are absent here by design.

**Community-specific material stays with the community.** Its current state,
decision log, deployment particulars and governance are not generic, and
publishing them here would leak a deployment's shape to a third party. That is
the same boundary described under *Relationship to `paisans.community`*, and it
is a privacy control rather than tidiness.

**Operators may mirror these documents into their own wiki**, so sysadmins read
them where they read everything else. Two rules keep that honest:

* **The mirror is one-way and labelled as a copy.** Edits happen here. Without
  that, the two drift and nobody can tell which is current — which is worse than
  having one copy.
* **Nothing crosses the other way.** Convenience is exactly how a hostname ends
  up in a public repository.

**Recovery needs four things, and only the first is here:** this documentation,
plus the deployment's config, its secrets, and its data. See the backup contract
above. Documentation tells you what to rebuild; it contains nothing to rebuild
it *from*.

## Where this repository lives

`Paisans-Software/paisans-stack`, private, since 2026-09-15. It was
`josephquigley/paisans-stack` until then. GitHub redirects the old path, and a
redirect is a courtesy rather than a contract, so a clone that predates the move
should have its remote updated rather than relying on one.

The Go module path moved with it, to
`github.com/paisans-software/paisans-stack`.

## Relationship to `paisans.community`

`paisans.community` is one deployment: its compose files, its Caddy config, its
operational scripts. This repository is the generalisation — the part another
organizer could adopt without inheriting this community's specifics.

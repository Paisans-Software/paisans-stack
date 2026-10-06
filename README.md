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

Every stack with `cluster` placement connects to the HAProxy on its own site,
at that site's mesh address and port 5000 (Eg: `10.44.0.1:5000` on `home-a`),
from the first install. That HAProxy holds a backend list that initially has one
entry. Adding a site grows that list. No application configuration changes,
ever: an instance is only ever given its own site's address, and that address
does not move when another site joins. This one indirection is what makes a
second site an operation rather than a project.

The address is the mesh one rather than `127.0.0.1` because every application
runs in a bridge networked compose container, and inside one of those the
loopback address is the container itself, so a DSN naming it reaches nothing.
Host networking for the apps would fix that and cost the isolation of each
stack's own sidecars, and two kinds listening on the same port would collide.
The Docker bridge gateway would work only if HAProxy bound an address that
differs per compose network and does not exist until Docker creates it. The
mesh address exists as soon as `wg0` is up, before anything else starts, and
is already declared in the configuration. HAProxy still listens on loopback as
well, for a tool run on the host.

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
| Mbin | full | `S3_KEY`/`S3_SECRET`/`S3_BUCKET`/`S3_REGION`/`S3_ENDPOINT`. The Docker image's entrypoint then switches `public_uploads_filesystem` to the S3 adapter itself; only a bare metal or VM install edits `config/packages/oneup_flysystem.yaml` by hand. Upstream strongly advises a media reverse proxy so URLs stay stable across provider changes |
| Outline | full | `FILE_STORAGE=s3` with the `AWS_*` variables; non-AWS endpoints need `AWS_S3_FORCE_PATH_STYLE=true`. Known upstream bug: the bucket must not be named `outline` |
| Synapse | partial | `synapse-s3-storage-provider` is a *storage provider* that supplements the media store. **A local media directory is still required.** `store_synchronous: True` writes to S3 immediately; the bucket prefix cannot be changed once media exists |
| Pocket ID | full, plus better | `FILE_BACKEND` takes `filesystem` (default), `s3`, or **`database`**. See the note below — `database` is the recommendation |
| WriteFreely | full, but private | the kind means the [writefreely-wisp](https://github.com/josephquigley/writefreely-wisp) fork, which adds `[storage] type = s3`. Its bucket is never routed to the media hostname: the fork streams uploads through its own `/uploads/` route rather than emitting an S3 URL, so nothing anonymous reaches the bucket directly |

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

**`storage.media_hostname` says where objects are served from, and it is
required once any app stores objects, refused as
`object-storage-without-a-media-hostname` otherwise.** It is one hostname for
the whole deployment rather than one per app, because a bucket is a path under
it. The endpoint an app writes through is a mesh address, and a browser cannot
reach one: without this hostname the toolkit would render an app that uploads
successfully and publishes a URL nothing outside the mesh can fetch, and a
federating instance that has cached such a URL keeps it.

This check lives in `internal/validate` rather than as a structural error in
`internal/config`. `internal/kinds`, which knows which kinds use object
storage, already imports `internal/config`; `config` asking `kinds` back would
be an import cycle.

**A public bucket's objects are served without a credential, and a private
bucket's are not.** Garage's S3 API refuses every unauthenticated request
outright, with `403 Forbidden: Garage does not support anonymous access yet`,
and that is the only mode it has. Garage's separate web endpoint is the one
that serves an object anonymously, so the toolkit renders it as `[s3_web]` in
`garage.toml` and grants `garage bucket website --allow` on public buckets
only.

The gateway is what joins the two. The media hostname carries one route per
public bucket: the bucket prefix is stripped from the path and the `Host` is
rewritten to the vhost form the web endpoint resolves a bucket from, which is
an internal suffix that resolves nowhere on purpose. Everything else falls
through to the S3 API with `Host` forwarded unchanged, which is load bearing
rather than tidy: Outline presigns its object URLs, a SigV4 signature covers
the host it was signed with, and rewriting that header would invalidate every
URL Outline issues.

Nothing an app publishes changes, and that is the point of rewriting at the
gateway rather than changing the URL: `KBIN_STORAGE_URL` stays the path style
media URL it always was, because remote instances have cached Mbin's URLs and
we cannot recall them.

Which kinds are public is a property of the kind, in `internal/kinds`, not a
setting. A federating server fetching an image is a machine with no account,
so Mbin's media has to be readable without one; Outline's is read by people
who are signed in, and its bucket is never routed to the web endpoint at all.
There is no configuration key that can get this wrong in either direction.

`TestAnonymousFetchReadsMbinsMediaAndNotOutlines` in `internal/garage` is what
keeps that honest, against a real Garage behind a real Caddy.

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

**For the `synapse` kind it is not a choice: `placement: cluster` is refused.**
The media store cannot leave the node, and the kind renders its own Postgres
plus the initialisation hook that creates the authentication service's database
beside it, neither of which exists on a clustered site. A clustered homeserver
would render and then meet the missing database at the first sign in. The same
reasoning refuses `element`, which is a static client with no server side
state, and `oauth2-proxy`, whose session is a cookie. So the refusal is the
existing shape rather than a special case for Matrix: a kind with no Postgres
service has nothing to join, and cluster placement would render it onto every
apps site with storage of its own.

`writefreely` used to be in that list. It no longer is: the kind now means the
[writefreely-wisp](https://github.com/josephquigley/writefreely-wisp) fork,
which adds Postgres and S3 support upstream WriteFreely does not have, so the
blog can join the cluster like any other app. **This is a breaking change for
anyone who adopted this toolkit before it:** the fork's image replaces
upstream's, and upstream's own image will reject the configuration this kind
now renders, because it has no `[database]` Postgres driver and no
`[storage]` section at all. See `docs/decisions.md` for the record.

**"The authentication service's database" is Matrix Authentication Service,
and the `synapse` kind renders it as a second container beside the
homeserver, not as a setting inside it.** Synapse stopped being where a
member logs in: it is a resource server now, and MAS is what owns the login,
holds sessions, and talks to the identity provider on the homeserver's
behalf. The identity provider itself sits upstream of MAS, one hop further
out than every other application's OIDC client, which is why registering it
takes a different redirect URI. `paisans init` prints that URI, in the form
`https://<hostname>/upstream/callback/<ulid>`, alongside every other owed
secret, because a client registered against the wrong path fails at the end
of the first sign in rather than at registration. MAS keeps its own Postgres
database beside the homeserver's, which is part of why `placement: cluster`
is refused above: a clustered site has nowhere to put either.

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
it at its own site's HAProxy (rule 2), put its media in Garage. Two compose profiles per app,
and the toolkit picks one.

Pinned is not restricted to a cloud VM — pinning to a home is equally valid. The
cloud VM is one location among several.

**An app that keeps its data outside the cluster can only be pinned, and
`cluster` is refused for it.** Cluster placement means running on every site
with the apps role and sharing one database through the local proxy. An
application storing its data anywhere else gets neither half: it would be
rendered onto each of those sites with its own separate storage, so one
hostname would serve two deployments that diverge the moment anybody writes,
and a failover would move readers between them. Element and oauth2-proxy are
the cases that exist: a static client with no server side state, and a
session kept in a cookie rather than a database. Pinning either is not a
downgrade, because that was always its availability. WriteFreely used to be
in this group too, before the fork it now runs gained Postgres support; see
Rule 5 above.

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

### A config key is placed, not interpreted

An app takes two maps that look alike and are not.

**`settings` is input the toolkit reasons about.** A template asks for each key
by name, and some are validated or acted on: `s3_bucket` is a setting because
`validate` refuses one named `outline` and `storage init` creates it. A key no
template asks for is silently ignored. That is the direction in which choosing
wrong is silent, and the reason this section exists: an application option put
under `settings` renders nothing and says nothing.

**`config` is passthrough.** The toolkit does not interpret the key. It does not
know whether the application has such an option, does not check the value's
type, and decides nothing with it. It places the key, in that file's own syntax,
in the file the app's kind renders, and has no other opinion about it. A typo
renders cleanly and is the application's to ignore or refuse at boot.

The kinds do not share a format, so a key means something different per kind:

| Format | Kinds | File | A key means |
|---|---|---|---|
| env | mbin, outline, pocket-id, oauth2-proxy | `.env` | the variable name |
| ini | writefreely | `config.ini` | `section.key`, exactly one dot |
| yaml | synapse | `homeserver.yaml` | a nested path, any depth |
| json | element | `config.json` | a nested path, any depth |

One of each:

```yaml
apps:
  talk:
    kind: mbin
    config:
      KBIN_META_TITLE: A place to talk          # env: a variable in .env
  blog:
    kind: writefreely
    config:
      app.max_blogs: 3                          # ini: max_blogs in [app]
  chat:
    kind: synapse
    config:
      require_auth_for_profile_requests: true   # yaml: a top level key
  web:
    kind: element
    config:
      default_theme: dark                       # json: a top level key
```

Values are scalars: a string, a number or a boolean. A list or a map is
refused, because no format here has one way to write it that every application
reads the same.

**Where the key lands.** Env appends a labelled block to the rendered `.env`,
quoting a value only where compose's `env_file` would otherwise rewrite it, so
the application receives it byte for byte. Ini
inserts the key into the section that is already there, after its last
assignment, rather than repeating the section header; a section the template
does not write is created at the end. Yaml appends a new top level key as its
own block and leaves the rest of `homeserver.yaml` byte for byte as rendered.
A key under a mapping the template already writes, such as
`database.args.sslmode`, makes the file be re-emitted: comments survive, but the
blank lines between top level keys do not. Json keeps the template's key order.
In every format except json, which has no comments, a comment says `config`
put the key there: above the appended block in env and yaml, above the inserted
lines in ini, and on the key itself when yaml re-emits under an existing
mapping.

Three mistakes are knowable from the file alone, so `paisans validate` refuses
them:

**A dotted key on an env file is refused as
`config-key-is-nested-in-an-env-file`.** Env has no nesting, so a dot is a typo
for an underscore rather than a path.

**A key named like a credential is refused as `config-key-looks-like-a-secret`.**
`paisans.yaml` is plaintext and meant to be committed; the value belongs in
`secrets.enc.yaml`. The check is by name: the key is lowercased, `_`, `-` and
`.` are removed, and what is left is searched for `password`, `secret`,
`token`, `apikey` and `privatekey`, so `SENDGRID_API_KEY` and `privateKey` are
both caught. It will not catch a credential called something else. It refuses
the one thing it can see and does not claim to be a policy.

Being by name, it also refuses some keys that are not credentials at all.
WriteFreely's `app.disable_password_auth` is a switch that turns the password
login off, and it is refused because its name contains `password`. Such a key
needs a `settings` key or a template change.

**An env key starting `COMPOSE_` or `DOCKER_` is refused as
`config-key-steers-compose`.** `apply` runs `docker compose -f
/srv/<app>/compose.yaml up -d`, and compose reads the `.env` beside that file
for its own settings as well as handing it to the application. With
`docker compose config` on Docker Compose v5.4.0, run on the operator's Mac
rather than a host, `COMPOSE_PROJECT_NAME=other` in that `.env` renamed the
project to `other` and `COMPOSE_PROFILES` switched on a service the compose
file kept behind a profile. `DOCKER_HOST` in the same `.env` did not redirect
`docker compose ps` in that run; `DOCKER_` is refused anyway, as the docker
CLI's own namespace, so the rule does not depend on knowing which of those
compose honours from a file.

**A key the template already writes is refused when rendering, as
`config-key-already-rendered`.** The message names the key and the file, and
either the template that writes it or, when there is one, the `settings` key
that is the sanctioned input for the same value. `config` adds; it never
overrides. Two sources of truth for one value is how a deployment ends up with
a setting nobody can locate, and an override would route around decisions the
templates encode: `database.type = sqlite3` into a blog that belongs in the
cluster is refused this way, because the template writes `type = postgres`.

The same rule covers a key a template leaves out on purpose, because no
collision would ever catch one of those coming back. The message says the
template omits it by decision, and why. The list is limited to what a
template's own comments say it leaves out on purpose, and today that is:

* Synapse: `oidc_providers`, `oidc_config`, `saml2_config`, `cas_config`,
  `jwt_config` and `password_config`, and any path beneath them, such as
  `oidc_config.enabled`. The homeserver is a resource server behind Matrix
  Authentication Service, and an identity provider or password database of its
  own would be a second way in.
* WriteFreely: `uploads.dir`. Uploads go to S3, and `dir` is read only by the
  local image store, so it would do nothing.

Any other key the toolkit does not render is passed through, so this list is
the whole of what is enforced beyond collisions. To change a template owned
value, use its `settings` key if it has one. If it has none, the answer is a
pull request that makes it one, or a template change, and both are reviewable
where a local override is not.

For the env kinds the check reaches past `.env`. A variable the stack's
`compose.yaml` sets under `environment:`, or interpolates as `${VAR}`, is
refused too, because compose gives `environment:` precedence over `env_file`
and the `.env` value would be dropped without a word. That precedence was
observed with `docker compose config` on Docker Compose v5.4.0 on the
operator's Mac; the version on any host was not checked.

**Some keys cannot be written at all.**

* A key whose own name contains a dot. In yaml and json a dot is always a path
  separator, so Element's `setting_defaults` key `UIFeature.feedback` cannot be
  expressed. A path that could mean an existing dotted key, such as anything
  under `default_server_config.m.homeserver`, is refused as ambiguous rather
  than guessed at.
* An ini section whose name contains a dot. `[oauth.generic]` is unreachable,
  because a key names exactly one section and one key.
* The ini section `DEFAULT`, which is the parser's name for the lines before
  any header.
* An ini value that WriteFreely's parser would read as something else: one
  containing `;` or `#`, which it takes as a comment; one with leading or
  trailing whitespace, which it trims; one that starts with a quote or a
  backtick, which it strips; one ending in a backslash, which joins the next
  line onto it; and one with a newline.

**WriteFreely reads its settings from `config.ini` once.** The fork moves most
of `[app]` and `[uploads]` into its database on the first start against an empty
database, and from then on the database is in force. That much was run: a key
added to `config` after the first start is rendered, and when the integration
test restarted the blog once against the existing database, the fork logged
`config.ini disagrees with the database on app.max_blogs; the database value
is in force` and `writefreely settings get` still reported `0`, the value the database already
held.

The rest is read from the fork's source at the revision the pinned image
carries (`ff9dcebe`) and was not run. `importSettings` in
`settings_runtime.go` says it "runs at every start", so the same line is
expected on every boot, not only the one restart observed. `writefreely
settings set <name> <value>` exists in `cmd/writefreely/settings.go` ("running
servers apply it on their next request") and looks like the way to change a
running blog's setting, but nobody has used it here. And the drift check
compares every database-bound key present in the file, which reads as though a
template's own key in those sections, such as `app.site_name` from the
`site_name` setting, would be ignored the same way after the first start;
that too is untested. This is the fork's design, not the toolkit's.

**What has been run, and what has not.** The ini insertion is proven against the
software: `go test -tags writefreely_integration ./internal/render/` boots the
pinned fork against the `config.ini` this toolkit renders, with a Postgres
beside it, and reads the value back with `writefreely settings get`, along with
the same file less that one line as a control. The yaml and json merges are
proven against Go's parsers only. Nothing here runs Synapse or Element, so the
tests show that the rendered file parses and carries the value at the path
asked for, not that those applications accept it.

### Three kinds of secret

Most of `secrets.enc.yaml` is machine-authored. Nobody invents forty passwords.

| Kind | Examples | Origin |
|------|----------|--------|
| **Generated** | Postgres superuser/admin/replication passwords, Garage keys, WireGuard private keys, Mercure JWT, RabbitMQ and Valkey passwords, Mbin's OAuth2 server keypair | created at `init`, never typed or seen |
| **Pasted** | Cloudflare API token, SMTP credentials | issued elsewhere, supplied by a human |
| **Captured** | OIDC client secrets | minted by a running service, then recorded |

**Mbin's OAuth2 keypair is generated, not placed.** Mbin signs the tokens it
issues to API clients and apps with an RSA key that its image does not create;
upstream's Docker install has the operator run `openssl genrsa -des3 ... 4096`
on the host (`docs/02-admin/01-installation/02-docker.md` in the Mbin
repository). `init` generates the same thing instead: 4096 bits, the private
half encrypted with the app's `oauth_passphrase`, both halves kept in the
secrets file and rendered to `/srv/<app>/oauth/`. A host step is what the
toolkit exists to remove, and keeping the pair in the secrets file is what
gives every apps site under cluster placement the same one, so a token one
site issues verifies on another. Like every generated secret it is never
replaced, and here that is visible to members: a new key signs out every API
client at once.

**It is also the one private key rendered 0644, on purpose.** `apply` writes
every file as root, and Mbin reads the key as uid 1000 after its entrypoint
drops privileges, so a 0600 key is a key Mbin cannot open. The key is
encrypted, and the passphrase that opens it is only in the 0600 `.env`, so a
host user who can read `private.pem` holds ciphertext. The alternatives were
worse: `apply` chowning the file to 1000 couples the generic writer to one
image's runtime user; running the container as root defeats the entrypoint's
own privilege drop; and an unencrypted key at 0644 is a plaintext signing key
for anyone on the host.

The captured case matters. An identity provider mints a client secret and the
app needs the identical value; recording it here makes it reproducible instead
of existing on exactly one host. But minting one is a privileged mutation that
belongs to a human — the toolkit records the value after the fact and must not
automate the approval away.

**Pasted and captured secrets go in through a pipe.** `paisans secrets set
<dotted.key>` reads the value from stdin, writes it into `secrets.enc.yaml`
re-encrypted to the recipients in `.sops.yaml`, and prints only `set <key>`:

```
security find-generic-password -s acme -w | paisans secrets set external.acme_dns_token
```

Every other way of getting a value into the file leaves a copy: an argument is
in shell history and in `ps`, a prompt is in a scrollback buffer, and `sops
secrets.enc.yaml` opens the whole decrypted file in an editor that may keep swap
or backup files. It refuses a terminal on stdin for the same reason. It accepts
a key that is already set (a rotation), a key `init` reports as owed, or any key
under `external`; it refuses a generated key nobody created, because `init`
creates those with the shapes their consumers require, and it refuses an unknown
top level section, because a value written where nothing reads it looks
delivered and is not.

**A gateway without its DNS token is refused at `render` and `apply`.** `init`
lists `external.acme_dns_token` as owed rather than refusing, because an
install is assembled in steps. Rendering without it is different: the gateway's
Caddy starts, loads its configuration, and fails every DNS-01 challenge, so no
hostname gets a certificate and the apply reports success. The first person to
find out would be a visitor.

**A hand edited Garage key is refused as `garage-key-is-malformed`.** A
generated S3 access key ID is the literal `GK` followed by exactly 24
lowercase hex characters, and a generated secret key is exactly 64 lowercase
hex characters; a generator only ever produces that shape, so this cannot fire
on a generated value. It exists for a hand edited `secrets.enc.yaml`, where
Garage would otherwise refuse the credential at provisioning time on a host,
after it has already imported some keys, with a message about hex encoding
that names no field and no file. This check lives in `internal/secretsgen`
as the exported `CheckGarageKeys(cfg, secrets)`, rather than in
`internal/validate`: `validate.Check` takes only a `*config.Config` and never
sees the secrets file, so it has nothing to check this against. `Fill` calls
it for `init`, and `render` and `apply` both call it themselves right after
`config.LoadSecrets`, because both load secrets directly and never call
`Fill`: a key hand edited into the file after the last `init` has to be
caught on every path that can reach a host, not only on the one that
happens to regenerate secrets.

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
`[database]`, `[storage]` and `[oauth.generic]` sections carry everything it
needs, SSO client credentials and `disable_password_auth` included. The
`[database]` and `[storage]` sections are the writefreely-wisp fork's own,
read from its `config/config.go` and `config/storage.go` rather than from
upstream's [config reference](https://writefreely.org/docs/latest/admin/config),
which documents neither. Synapse reads
[`homeserver.yaml`](https://element-hq.github.io/synapse/latest/usage/configuration/homeserver_sample_config.html).

So the unit the toolkit ships per `kind` is a **template directory**, and
`apply` renders every file in it:

```
templates/mbin/                             templates/writefreely/
  compose.yaml.tmpl                           compose.yaml.tmpl
  .env.tmpl                                   config.ini.tmpl
  caddy.snippet.tmpl                          .env.tmpl
  valkey.conf.tmpl                            caddy.snippet.tmpl
  oauth/private.pem.tmpl
  oauth/public.pem.tmpl
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
override is a template like any other. Rule 3 names the first candidate, and it
turned out to be the cautionary case. Mbin's
`config/packages/oneup_flysystem.yaml` defines both a local adapter and an S3
adapter, and binds uploads to the local one:

```yaml
  filesystems:
    public_uploads_filesystem:
      adapter: default_adapter
      #adapter: kbin.s3_adapter
```

On a bare metal or VM install, setting the `S3_*` variables alone appears to
work and silently keeps writing to local disk, and the fix is editing this file.
[Mbin's S3 documentation](https://docs.joinmbin.org/admin/optional-features/s3_storage/)
says that edit is "only needed on bare metal/VM, not Docker", because the Docker
image's entrypoint makes it: with `APP_ENV=prod` (the image's default) and
`S3_KEY` set, it runs `sed -i` on that file at every start, switching the
adapter to `kbin.s3_adapter`. On the image this toolkit runs, the variables in
`.env` are the whole switch, and the toolkit renders no override.

A rendered copy bind-mounted over the file was the first design, and it is worse
than none: `sed -i` cannot replace a bind-mounted file, the entrypoint runs
under `set -e`, and the container exits before it serves anything. So **before
shimming a file, read the image's entrypoint.** A file it edits in place cannot
be mounted over, and a file it edits is usually one the image already handles.
Entrypoint wrappers, module configuration and other dropped-in override files
that the image leaves alone are templates like any other.

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

#### An app may answer on more than one hostname

```yaml
apps:
  chat:
    kind: synapse
    hostname: matrix.example.org
    hostnames:
      wellknown: example.org
```

Each hostname gets its own host block and its own snippet, because a second
hostname almost always exists to serve something *different*. A homeserver is
the worked example: the API lives on one name, and the `.well-known`
delegation documents live on whatever name appears in user identifiers. Routing
them identically would publish the entire API on the apex.

The key is a **role**, not a label: it selects which snippet the kind ships. A
role a kind does not understand is refused, because the alternative is a
gateway that fails to load its whole configuration over one missing import.

**A hostname may be claimed once, by one app and one role.** Two claims on one
name are refused. Caddy will not choose between two site blocks that hold the
same address, and it refuses the whole file rather than the block, so a name
typed twice takes every hostname in the deployment down rather than the one
that was duplicated. This is easiest to hit now that an app can declare
several: setting one app's `hostname` to the value another app already uses for
a role reads as two unrelated lines.

**For a homeserver, `hostnames.wellknown` is not only routing. It is
`server_name`.** It becomes the part after the colon in every user identifier
the homeserver ever mints, and it is written into every room that homeserver
has joined. It cannot be changed once an account or a room exists: a homeserver
restored under a different `server_name` is, to its own history, a different
server. Editing this key on a deployment that is already running is a rebuild
and a migration of member data, not a configuration change, and nothing in this
toolkit can undo it for you or warn you at the moment you do it, because
`apply` compares the configuration against templates rather than against what
some earlier `apply` already put on a host.

Choose it once, before the first `apply`, and choose the name you want members
to have. Declaring no `wellknown` hostname is a legitimate choice with the same
weight: `server_name` is then the app's own hostname, the homeserver serves its
own delegation documents there, and that name is equally permanent.

#### The gate is a per-app property

```yaml
apps:
  talk:
    kind: mbin
    hostname: talk.example.org
    gate: members
  chat:
    kind: synapse
    hostname: matrix.example.org
    gate: none
```

`gate` is `none`, `provisional` or `members`, and it is declared on the app
rather than implied by which Caddy snippet somebody remembered to import. It is
access policy, and access policy belongs in the configuration, not in a hand
edited include.

**The gate is enforced at the gateway, and only there. An app's published host
port is not behind it.** The gate renders as `forward_auth` inside the
gateway's Caddy site block, so it covers requests that arrive through the
gateway's hostname. It does not cover the port the app's own compose file
publishes: `talk` above is `gate: members`, and its stack publishes port 8080,
so anything that can open that port reaches Mbin with no gate in front of it at
all. The same is true of every gated app.

**So every published port binds the site's mesh address and nothing else**
(Eg: `"10.44.0.1:8080:8080"`, never `"8080:8080"`). The gateway reaches every
app over the mesh, so the mesh is the only interface a port needs, and the
members of the mesh are the hosts this deployment already trusts. Leaving the
port on every interface and relying on a host firewall instead was the
previous position, and it was rejected: Docker writes its own iptables rules
for a published port, ahead of the host's, so a port published on all
interfaces of a host with a public address is reachable from the internet
whatever ufw says. A firewall the container runtime routes around is not a
control, and a cloud VM holding the apps role is exactly that host.

What remains the operator's is the mesh itself. A port on the mesh address is
reachable from every mesh peer, so a compromised peer reaches the app ungated.
`gate: members` is not protection against that, and nothing in this file
claims it is. It also means `wg0` has to be up before the app stacks start,
because Docker cannot publish on an address the host does not have yet; that
is the ordering that step 4 under *`init`, one site, no mesh* already requires.

**Leaving `gate` out of an app's stanza means the same thing as `gate: none`:
ungated, reachable by anyone who can resolve the hostname.** That default has
to be stated here, not only in a doc comment, because it is the one setting
where forgetting it is indistinguishable from choosing it: an app with no
`gate` key renders exactly like an app that explicitly opted out, and nothing
at render time flags the absence as a decision skipped rather than made.

**A gate on an app of kind `synapse` is refused**, which is the rule below
enforced rather than merely stated. The case that makes this load bearing is a
negative one. A Matrix hostname must never be gated: a Matrix client is not a browser and will not follow a redirect
to a passkey prompt, so gating a homeserver's API or its `.well-known` apex
breaks federation and every client's login, not just one member's. Leaving
gating to a hand edited include means that failure shows up as clients
mysteriously unable to log in, with nothing in the configuration saying why. A
declared `gate: none` makes the absence of a gate a fact about the app that a
reviewer can see, rather than an inference from what nobody wrote.

Declaring a gate when no `oauth2-proxy` app exists is refused, for the same
reason an unknown hostname role is: the import would name a snippet nothing
renders, and Caddy fails to load its entire configuration over one missing
import, taking every hostname down rather than one.

### `storage init` provisions object storage

`apply` renders files and reconciles them against a manifest it wrote; that is
what makes its refusal to clobber a locally edited file trustworthy. Garage's
cluster layout, each app's S3 key, and each app's bucket are not files. They
are state inside a running service that no manifest describes, so they are a
separate command: `paisans storage init --site <name>`.

It checks what a site's Garage node already has and creates only what is
missing, so running it again is safe. Like `apply`, it prints a plan and
writes nothing without `--execute`.

It has to run after the infrastructure stack is up, because Garage has to be
reachable to be asked what it already has. **`paisans apply` alone leaves
object storage unusable**: the containers come up, but no bucket exists and no
application key can reach one, so the failure an adopter meets is an
application error with no obvious cause, not a message naming a missing step.
`storage init` is that missing step.

#### A second Garage site has to be joined by hand first

**One Garage site needs nothing extra.** Run `paisans storage init --site
<name> --execute` after the infrastructure stack is up and it does the whole
job.

**A second Garage site needs a `garage node connect` first, run by you.** The
toolkit does not plan that join and nothing it renders performs one: there is
no `bootstrap_peers` in the rendered `garage.toml`, and no Consul or Kubernetes
discovery. The shared `rpc_secret` every site carries authenticates a peer; it
does not find one. Two nodes brought up from rendered configuration sit alone
indefinitely, each listing only itself in `garage status`.

Until they are joined, `storage init` cannot converge on either site. With one
node visible, `garage layout apply` is refused because the node count is below
the declared `replication`, and `storage init` stops at the first failing step,
so no application key, no bucket and no website grant is created anywhere.

From one site, once both nodes are running:

```sh
# On the second site, read its node ID. This is the command shape
# `storage init` prints for everything else it runs.
ssh <site-b> docker compose -f /srv/infra/compose.yaml exec -T garage \
    /garage node id -q

# On the first site, join it. Pass the whole id@address that printed.
ssh <site-a> docker compose -f /srv/infra/compose.yaml exec -T garage \
    /garage node connect <id@address>
```

`garage status` on either node should then list both. Run `paisans storage init
--site <name> --execute` for each site afterwards, in either order.

### `dns init` creates the records a deployment needs

Every public name a deployment answers on needs a DNS record, and the toolkit
creates them, not the operator and not an agent working by hand. A record typed
into a provider's console is a record nothing checks against the configuration,
and the first anyone hears of a typo is a visitor who cannot reach the site.
Founder requirement.

```sh
paisans dns init                # shows what it would create
paisans dns init --execute      # creates it
```

**The records are derived, never declared.** Each app's `hostname`, each of its
role `hostnames`, and `storage.media_hostname` get an A record pointing at the
gateway site's `public_address`. A site whose `endpoint` is a name rather than
an address gets an A record pointing at that site's own `public_address`,
because that is the name the other sites' WireGuard dials. Where a site also
declares `public_address6`, each of its names gets an AAAA record as well. A
list of records in `paisans.yaml` would be simpler to read, but it would be a
second place to write every hostname, and the copy that drifts is the one that
sends visitors somewhere else.

```yaml
sites:
  vm:
    roles: [gateway, witness]
    address: 10.44.0.3
    endpoint: vm.example.org:51820
    public_address: 203.0.113.10       # what the internet reaches, not the interface
    # public_address6: 2001:db8::10    # optional, adds AAAA records
```

**`public_address` is declared, not discovered.** The address a host sees on
its own interface is often not the one the internet sees, behind NAT or a cloud
provider's one to one mapping, and a guessed address published in DNS is cached
for the record's lifetime. `validate` refuses a value that is not an address of
the right family, and one the internet cannot reach: private space (RFC 1918
and RFC 4193), carrier grade NAT (100.64.0.0/10), loopback, link local, or an
address inside `mesh.subnet`. A missing `public_address` is not refused by
`validate`, because a deployment may manage its DNS some other way. `dns init`
refuses it, naming the site, wherever a record would need it.

**It only creates. It never updates and never deletes.** For each record it
needs, the provider holds one of three things:

| Provider holds | Plan |
|----------------|------|
| the same record, unproxied | `present` |
| nothing at that name of that type | `create` |
| a different address, a CNAME, a proxied copy, or an A or AAAA the deployment does not declare | `conflict` |

**Any conflict refuses the whole run before a single record is written.** This
is the same rule `apply` follows for a file it did not write: a record that
disagrees with the configuration is somebody's, and overwriting it would take
down whatever they pointed it at. Creating the records that do not conflict and
skipping the rest was rejected too, because half a set of records is a
deployment some names reach and others do not, which is harder to diagnose
than a refusal. A stray AAAA is a conflict because clients on IPv6 would reach
it instead of the gateway, silently. After `--execute`, every record created is
read back and compared.

**Records are DNS only, never proxied through a provider's CDN.** A proxied
name resolves to the CDN's addresses rather than the gateway's, so a WireGuard
endpoint behind it cannot be dialled at all, and every request reaches Caddy
from the CDN rather than the client, undoing the client address handling the
mesh subnet setting exists for. A proxied record that otherwise matches is a
conflict, not a match.

**One gateway only, for now.** With several gateway sites, which address a
hostname should resolve to is a design that does not exist yet. `dns init`
refuses rather than picking one. The same reasoning covers moving the gateway:
the old records point at the old gateway, so they are conflicts, and repointing
them is the human step in *Make it an overlap, not a cutover*, not something
this command does behind an operator's back.

**It reaches no host.** It talks to the DNS provider's API from the
workstation, using `external.acme_dns_token`, the same zone scoped token the
gateway uses to answer ACME challenges. A second credential with the same reach
would be one more thing to rotate and leak. The token travels only in a request
header and is redacted from every error. Finding which zone holds a name means
reading the zone, so the token needs read access to the zone as well as
permission to edit its records. The zone is found by trying each suffix of the
name, longest first, so a delegated subzone wins over its parent.

**Cloudflare is implemented. Other providers are not yet**, deSEC included,
although both answer ACME challenges. Answering a challenge and managing
ordinary records are different code, and `dns init` refuses an unimplemented
provider by name rather than pretending. Until one is implemented, create the
records it would have created by hand, unproxied.

### A data site's watchdog is declared, not assumed

Patroni fences itself with `/dev/watchdog`: if it stops renewing the timer
while it is leader, the machine reboots before another node's promotion can
leave two primaries taking writes. The rendered Patroni runs with
`PATRONI_WATCHDOG_MODE=required` and maps `/dev/watchdog` into its container,
so on a data site without a device compose will not even create the container.

Which device a host has is a property of the machine, not of the deployment: a
board with an Intel TCO timer (`iTCO_wdt`), an AMD one (`sp5100_tco`), a server
with a BMC (`ipmi_watchdog`), a VM with an emulated one (`i6300esb`), or a small
cloud instance with nothing. So it is a per site key, read only where the site
holds the data role:

```yaml
sites:
  home-a:
    roles: [data, apps]
    address: 10.44.0.1
    ssh: home-a.local
    watchdog: auto   # auto | required | softdog | off; auto when absent
```

| Mode | What `host prepare` does | What is rendered |
|------|---------------------------|------------------|
| `auto` | uses the device the host has, whatever its driver; with none, loads and persists `softdog` and prints a warning | `required`, device mapped |
| `required` | refuses unless a driver other than `softdog` is present | `required`, device mapped |
| `softdog` | loads and persists `softdog`, without a warning | `required`, device mapped |
| `off` | nothing | `PATRONI_WATCHDOG_MODE=off`, no device mapping |

**`softdog` is a fallback, never the assumption.** It is a kernel timer, so it
reboots a machine whose Patroni hung but not one whose kernel did, and a hung
kernel is exactly the case where the node cannot fence itself any other way. A
hardware or hypervisor watchdog is better wherever one exists. Assuming
`softdog` everywhere would have been simpler to write and would have quietly
downgraded every host that had a real one; `auto` takes the real one when it
is there.

**`off` is a warning, not a refusal** (`watchdog-off-on-data-site`). Some hosts
genuinely cannot load any driver, and a single data site has nobody to split
brain with. It is still risky once a second data site exists, so it has to be
chosen rather than stumbled into. `off` also drops the device mapping from the
infrastructure compose file: compose will not create a container whose device
does not exist, so keeping the mapping would fail the exact host `off` is for.

**Only one process can hold the device, and it has to be Patroni.** `host
prepare` refuses a host where systemd's own runtime watchdog
(`RuntimeWatchdogSec`) is set or a `watchdog` daemon is running, rather than
leaving Patroni to fail to open the device at its first start. This is the
host side of the one Patroni per data bearing host rule in *There is one HA
cluster, and that is deliberate*.

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

### `host prepare` takes a blank host to an apply-able state

`apply` assumes a host that already has Docker with the compose plugin, the
WireGuard tools, a firewall, and on a data site a `/dev/watchdog` for Patroni.
**None of that is done by hand.** A step an operator performs from a wiki page
is a step done differently on every host, and the first time it matters is a
rebuild at the worst possible moment. So it is a command:

```
paisans host prepare --site home-a            # shows what it would do
paisans host prepare --site home-a --execute  # does it
```

It reaches the host over the `ssh:` bootstrap route with the operator's own
ssh, exactly as `apply` does. It probes read only, plans only what is missing,
and changes nothing without `--execute`; a prepared host plans nothing, so
re-running it after a failure resumes rather than restarts. It is not part of
`apply` for the same reason `storage init` is not: installed packages, firewall
rules and loaded kernel modules are host state that no rendered manifest
describes, so they are probed rather than compared.

#### Operating systems are profiles

Everything that depends on the operating system lives in a **host profile**,
chosen from `/etc/os-release` by `ID` and `VERSION_ID` together. A profile
answers four questions (how to install the packages, how to enable services,
how to load a kernel module now and at boot, how to drive the firewall), and
its shell lives as templates in its own directory under
`internal/hostprep/profiles/`. Adding Debian or Fedora is adding a directory
and a registration, not editing the command.

Only `ubuntu 24.04` ships. Matching on `ID` alone would be simpler and was
rejected: Docker's apt source names the release codename, and a profile that
accepted every Ubuntu would write one release's packages onto another. A host
no profile matches is refused, naming what is supported:

```
debian 12 is not supported. Use one of the following:
ubuntu 24.04
```

#### Docker comes from Docker's repository

On Ubuntu, Docker Engine and the compose plugin are installed from Docker's own
apt repository, as
[Docker's install page](https://docs.docker.com/engine/install/ubuntu/)
describes, not from Ubuntu's `docker.io`. Every host then runs the same engine
from the same source whenever it was prepared. The packages that page lists as
conflicting (`docker.io`, `docker-compose-v2`, `containerd`, `runc` and the
rest) are **refused, not removed**: on a host already running containers from
them, removing them stops those containers, and that is a decision for
whoever started them. Every `apt-get` runs non-interactively.

#### The firewall follows roles

| Rule | Sites |
|------|-------|
| deny incoming, allow outgoing by default | every site |
| 22/tcp | every site |
| 51820/udp, WireGuard | every site |
| everything arriving on `wg0` | every site |
| 80/tcp, 443/tcp | sites with the gateway role |

Nothing is listed per site. A site that gains the gateway role gains 80 and 443
at its next prepare. Nothing is ever removed: a rule an operator added is not
the toolkit's to delete, and a site that loses a role keeps its old rules until
someone removes them deliberately.

The mesh is let in whole, by interface, rather than port by port. etcd,
Patroni, Garage and HAProxy all listen on the mesh address, and a new mesh
service would otherwise be one more rule to remember on every site.

**SSH is never shut out.** Every allow, SSH first, is in place before the
default deny, and the firewall is enabled last with `ufw --force enable`, which
does not stop to ask whether to disrupt existing connections.

**Docker-published ports bypass the firewall.** Docker's install page says so
in as many words: "When you expose container ports using Docker, these ports
bypass your firewall rules." What ufw protects here is everything on the host network,
which is etcd, Patroni, Garage, HAProxy and Caddy, all `network_mode: host`. A
container that publishes a port is not covered by this table, which is why
published ports are to be bound to the mesh address rather than to every
interface. That binding is separate work and is not part of `host prepare`.

#### The watchdog

On a data site, `host prepare` gives Patroni the device the site's `watchdog`
mode asks for; *A data site's watchdog is declared, not assumed* has the modes.
It reports which driver it found, from `/sys/class/watchdog/*/identity`.

On Ubuntu, `softdog` is persisted with a small systemd unit that runs
`modprobe softdog` before Docker starts, not with a line in
`/etc/modules-load.d`. That would be the obvious place and it does not work:
Ubuntu's kernel package blacklists the watchdog drivers, `softdog` among them,
and `systemd-modules-load` honours the blacklist
([Launchpad bug 1535840](https://bugs.launchpad.net/bugs/1535840)),
so the line would be skipped at every boot while looking correct. `modprobe`
named on a command line does not apply the blacklist. This is the kind of
difference a profile exists to hold: a distribution that does not blacklist
`softdog` can use `modules-load.d`.

**Provisional: none of this has run on a host yet.** The command is tested
against a fake transport. The probe output it parses (`ufw show added`,
`systemctl show -p RuntimeWatchdogUSec`, `dpkg-query`) is written from the
tools' documented behaviour, not observed on Ubuntu 24.04, and the first real
run should be read rather than trusted.

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
6. Apps start, pointed at this site's own HAProxy on its mesh address, port
   5000.

Step 4 is the one that is easy to skip and expensive to add later. A `wg0` with
zero peers still provides `10.44.0.1`, and every service binds to it from the
first install. Joining a site then only **adds peers** — no service is
reconfigured and no address changes.

Same principle as running the cluster at one node: build the final shape
immediately, then grow it.

### `apply` brings `wg0` up before anything binds to it

Step 4 is `apply`'s job, and it runs after the files are written and before any
container is started, checked or restarted. A stack started first fails to bind
its mesh address, and Docker restarts it in a loop rather than reporting it, so
the apply would claim success over a site where nothing listens.

What it runs depends on what changed, and the narrower action wins here as it
does for stacks:

| `wg0.conf` | Interface | Action |
|------------|-----------|--------|
| new | any | `systemctl enable --now wg-quick@wg0` |
| unchanged | down | the same, so a rebooted or hand stopped site recovers on the next apply |
| unchanged | up | nothing |
| changed peers | up | `wg syncconf` from `wg-quick strip`, in place |
| changed `Address`, `MTU`, `PostUp` or another wg-quick only line | up | `systemctl restart wg-quick@wg0` |

**A peer change is synced rather than restarted.** Restarting is simpler and
always correct, but it takes the interface down, and on a data site that
partitions etcd and Patroni for as long as it is down; with election timeouts
measured in seconds, adding a site could start a failover on every existing one.
`wg syncconf` changes peers without touching the interface. It cannot apply the
lines wg-quick handles itself, because `wg-quick strip` removes them before `wg`
sees the file, so a change to one of those is the one case that restarts.
Nor does it add routes, which wg-quick does at start for each peer's
`AllowedIPs`; that is safe here only because every peer's `AllowedIPs` sits
inside the mesh subnet, which the interface's own `Address` already routes
(wireguard-tools, `src/wg-quick/linux.bash`).

"Up" is read from the kernel (`ip link show wg0`), not from the unit. An
interface brought up by hand serves every service just as well, and starting the
unit on top of it would fail on an interface that already exists.

### `apply` creates each clustered app's role and database

Step 6 needs something step 5 does not provide. Patroni creates its superuser,
its replication user and an `admin` role from `patroni.env`, but nothing creates
`talk` or `docs`, and an app started without its role fails to authenticate and
is restarted in a loop. So on a site in `cluster.sites`, after the
infrastructure stack and before the first app stack, `apply`:

1. waits up to three minutes for Patroni's `GET /cluster` to name a running
   leader;
2. if the leader is another site in `cluster.sites`, skips the rest and says
   which site to apply, since a replica cannot create roles;
3. if the leader is not in `cluster.sites` at all, stops, naming the leader it
   saw and the sites it expected;
4. otherwise sends one psql script on stdin to the Spilo container that, per
   clustered app using Postgres, creates the role if it is missing, **sets its
   password every time**, and creates the database owned by it if missing.

A timeout, an unknown leader or a psql failure stops the apply before any app
stack starts, and the next apply resumes at the same place. It is a gate, not a warning: an app
pointed at a role that does not exist has nothing to fall back on.

**Setting the password every time is what makes rotation an addition.** The
alternative, creating with a password once and never touching it again, means
a changed `apps.<name>.database_password` renders a new `.env` the role does not
accept, and rotation becomes a manual `ALTER ROLE` on the primary.

**The SQL goes on stdin, never on a command line.** A command line is visible in
`ps` to every user on the host and is quoted back in ssh errors. For the same
reason the script switches off statement logging for its own session first:
Spilo ships `log_statement = 'ddl'`, and `CREATE ROLE` and `ALTER ROLE` are DDL,
so the server log would otherwise hold every password
(zalando/spilo, `postgres-appliance/scripts/configure_spilo.py`).

**It runs as `postgres` over the container's own socket**, which Spilo's
`pg_hba` trusts (`local all all trust`, same file), so the toolkit passes no
database credential to do it. `CREATE DATABASE` cannot run in a transaction or a
`DO` block, so the conditional creates use psql's `\gexec`, which runs a
generated statement at top level.

**An app whose role would be `postgres`, `admin`, `standby` or `pg_*` is
refused**, because the bootstrap would set that app's password on a role the
cluster uses itself.

**Only a declared site can be the reason to skip.** The rendered Patroni names
each member after its site, so "the leader is not me" can be read as "the
leader is that site" only when the name is one of `cluster.sites`. On the first
real host the member was named after the machine instead, and an apply that
took any other name for a replica skipped the databases and started every app
with none. A name nobody declared means that pin did not hold, which is a
fault to fix rather than someone else's work, so it is a gate. It is not
polled out like a missing leader: a running member's name does not change.

`/cluster` is asked rather than `/primary` because it answers both questions:
`/primary` returns the same 503 to a replica as to a node still running initdb,
while `/cluster` names the leader (Patroni v4.1.0, `docs/rest_api.rst`).

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

An `acme.image` on a tag that does not name one build, meaning `latest` or no
tag at all, is **refused**, exactly as a floating tag on an app image is. It
matters more here than there: a rebuild behind a moving tag can drop the
provider's module, and a gateway that cannot answer a DNS-01 challenge holds no
certificate for any hostname in the deployment.

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

Same principle as apps connecting to their own site's HAProxy rather than a
named database host: an application should not know what is on the other side of an
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

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
| WriteFreely | full, and public | the kind means the [writefreely-wisp](https://github.com/josephquigley/writefreely-wisp) fork, which adds `[storage] type = s3`. With S3 the fork is direct only: it writes images and never serves them, requires `[storage] image_url_base`, and redirects its own `/uploads/` route there, so the blog's bucket is served on the blog's media hostname like Mbin's. That needs a fork build containing writefreely-wisp#171; an older one ignores the key and streams images itself |

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

**Each app that stores objects serves them on a hostname of its own**, a
sibling of the app's: an app at `talk.example.org` serves its media at
`talk-media.example.org`. The name is derived, `<label>-media.<domain>` from the
first label of the app's hostname, and an app can choose another under
`hostnames.media`. Every one of them needs a DNS record pointing at the
gateway, the same as the app's own hostname. The endpoint an app writes through
is a mesh address, and a browser cannot reach one: without a public hostname
the toolkit would render an app that uploads successfully and publishes a URL
nothing outside the mesh can fetch, and a federating instance that has cached
such a URL keeps it.

One hostname per app, rather than one for the deployment with a bucket as a
path under it, because a hostname is the unit DNS can move. Pointing one app's
media at a CDN, another provider or another Garage is a record change for that
app and no other; a path under a shared hostname can only move with every other
app's. Two apps' uploads on two hostnames are also two browser origins, so a
file one app stored cannot script against another's. The shared hostname was
this toolkit's first shape, and it was replaced before anything was released
under it.

The name is a **sibling, never a child**, and never a name for the backend.
`media.talk.example.org` sits under the app's own host, so a cookie the app
scopes to that host reaches every media request and a file served there could
set cookies the app reads back; that is refused as
`media-hostname-under-an-app-hostname`. `garage.` or `s3.` would name what
answers today rather than whose objects these are, which is the one thing about
the URL that should never change. A declared name must be a hostname
(`media-hostname-is-not-a-hostname`) under `community.domain`
(`media-hostname-outside-the-domain`), because its certificate is issued over
DNS-01 against the deployment's own zone. A derived name is checked the same
way, and so is a clash with any other hostname in the deployment
(`duplicate-hostname`): a derived name nobody wrote is still a site address,
and it is the claim most likely to collide unseen.

These checks live in `internal/validate` rather than as structural errors in
`internal/config`. `internal/kinds`, which knows which kinds use object
storage, already imports `internal/config`; `config` asking `kinds` back would
be an import cycle.

**Media is public to anyone holding the link, and a media hostname is never
gated.** A federating server fetching an image is a machine with no account; it
will not sign a request and it will not follow a redirect to a passkey prompt,
so a gate would break every federated image. That holds for the app whose own
hostname is gated, too: the gate stays on the app's hostname and does not
follow it to its media.

**A public bucket's objects are served without a credential, and a private
bucket's are not.** Garage's S3 API refuses every unauthenticated request
outright, with `403 Forbidden: Garage does not support anonymous access yet`,
and that is the only mode it has. Garage's separate web endpoint is the one
that serves an object anonymously, so the toolkit renders it as `[s3_web]` in
`garage.toml` and grants `garage bucket website --allow` on public buckets
only.

The gateway is what joins the two, one site block per media hostname:

| Kind | Objects | The media hostname goes to | Published as |
|------|---------|----------------------------|--------------|
| Mbin | public | Garage's web endpoint on 3902, `Host` rewritten to `<bucket>.web.garage.internal` | `KBIN_STORAGE_URL=https://<media host>` |
| WriteFreely (the fork) | public | the same | `[storage] image_url_base = https://<media host>` |
| Outline | private | Garage's S3 API on 3900, `Host` forwarded unchanged | `AWS_S3_UPLOAD_BUCKET_URL=https://<media host>`, path style |

The rewritten `Host` is an internal suffix that resolves nowhere on purpose, so
there is no DNS record or certificate for it. Because a hostname is one bucket,
the path is the object key and nothing is stripped from it. Outline's
forwarding is load bearing rather than tidy: Outline presigns its object URLs
and the upload a browser submits, a SigV4 signature covers the host it was
signed with, and rewriting that header would invalidate every URL Outline
issues. Its URLs are path style, `https://<media host>/<bucket>/<key>`, because
the media hostname is not under Garage's S3 root domain and Garage then reads
the bucket from the path. Outline also decides path style by looking for the
bucket name anywhere in that URL, so a bucket whose name appears in it is
refused as `outline-bucket-in-media-url`.

**Every public media hostname sends `Content-Security-Policy: default-src
'none'; style-src 'unsafe-inline'; sandbox` and `X-Content-Type-Options:
nosniff`.** Mbin and the blog store what they are given byte for byte, an SVG
that carries script among it, and no S3 provider can store a response header on
an object, so the gateway is the only place one can be set. The sandbox keeps
such a file inert when it is opened directly rather than through an `<img>`.
Outline's media hostname sends nosniff and not the policy, because Outline
embeds PDF attachments in the page and a browser will not run its PDF viewer in
a sandboxed document. What the policy would protect against there is already
handled upstream: Outline stores every type outside a short list of images,
PDF, audio and video as `Content-Disposition: attachment`, and leaves SVG off
that list deliberately.

Which kinds are public is a property of the kind, in `internal/kinds`, not a
setting. Outline's attachments are read by people who are signed in, and its
bucket is never routed to the web endpoint at all. There is no configuration
key that can get this wrong in either direction.

`TestEachAppsMediaHostnameServesOnlyItsOwnBucket` in `internal/garage` is what
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
`validate` refuses one named `outline` and `storage init` creates it, and
`sso_dashboard_link` is one because `oidc client create` sends it to the
identity provider (see *`oidc client create` makes an app's client at Pocket
ID*). A key no
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
| **Captured** | OIDC client secrets | belong to a running service; recorded here, by `oidc client create` for Pocket ID |

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

The captured case matters. An identity provider holds a client secret and the
app needs the identical value; recording it here makes it reproducible instead
of existing on exactly one host. Creating one is a privileged mutation that
belongs to a human, and the toolkit does not automate that approval away: `oidc
client create` prints every mutation and changes nothing until the operator
re-runs it with `--execute`. For Pocket ID the value is not even captured. The
toolkit generates it, records it here, and then hands it to Pocket ID, which
accepts a caller supplied secret; see *`oidc client create` makes an app's
client at Pocket ID*.

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
missing, so running it again is safe. It assigns the cluster layout only when
the site is the one Garage site; joining several is `storage add`, below. Like `apply`, it prints a plan and
writes nothing without `--execute`.

It has to run after the infrastructure stack is up, because Garage has to be
reachable to be asked what it already has. **`paisans apply` alone leaves
object storage unusable**: the containers come up, but no bucket exists and no
application key can reach one, so the failure an adopter meets is an
application error with no obvious cause, not a message naming a missing step.
`storage init` is that missing step.

#### More than one Garage site: `storage add`

**One Garage site needs nothing extra.** Run `paisans storage init --site
<name> --execute` after the infrastructure stack is up and it does the whole
job, layout included.

**Several Garage sites are joined by `paisans storage add`**, and by nothing
done by hand. A server is changed only by the toolkit. `storage init` lays a
node out only when it is the one Garage site; with several it refuses a node
that has no role and names `storage add`, because assigning one node alone
either fails (`layout apply` refuses fewer nodes than the replication factor)
or, at replication 1, starts a second cluster that never meets the first.

```sh
paisans apply --site home-b       # writes garage.toml, starts an isolated node
paisans storage add               # dry run: every stage, read from live state
paisans storage add --execute     # add --change-replication if the factor changes
```

It is cluster-wide and takes no `--site`. A layout at replication 3 cannot
grow one node at a time, since Garage refuses a layout with fewer storage
nodes than the factor, and another factor is a whole-cluster stop; a per-site
command would have to refuse half its invocations. It reads every node and
plans only what differs, in gated stages: the nodes as they are, the
replication reset when one is needed, connect, one layout version with each
node in a zone named after its site (only distinct zones spread copies across
sites), sync, provisioning planned by `storage init`'s own planner and found
present, the gateway's media routes, and a smoke test that writes a probe and
reads it back through every node and through the app's own media hostname. The design, with
Garage v1.0.1's source behind each gate, is
`docs/specs/2026-10-07-multisite-garage.md`.

**It does not wait for Garage.** Copying data to a new node runs at the speed
of the slowest site's upload, which on a home line can be hours. A stage
waiting on Garage exits with status 75, "try again later", and the next run
reads live state and carries on; `--wait <duration>` polls instead. Everything
a resumed run needs is on the hosts, so any machine can resume it. Rejected:
blocking in the foreground, which leaves an operator's terminal hostage to a
residential uplink.

**`storage.garage.sites` is a preference order, not a set.** The first site
serves every media read the gateway passes on, for every app's media
hostname, and takes every app's writes
(`S3_ENDPOINT`); the others serve only when the ones before them are down. A
Garage node that receives a write sends the other copies itself, and a home
line's upload is its slow direction, so the site with the best upload goes
first. A new site joins at the end of the list and moves up only after
`storage add` has passed, because a node with no role answers every bucket as
missing; `storage add` refuses the other order. Rejected: each app writing to
its own site's node (every write would leave the home once per remote copy),
choosing the fastest node at run time (Caddy 2.11.6 has no latency aware
policy, and a render must not change with network conditions), and an active
health check on Garage's `/health` (it judges with the consistent write
quorum whatever the configured mode, so at replication 2 on two nodes it calls
the survivor unavailable while it serves every read). Media failover is
passive: `lb_policy first`, `lb_try_duration` so the request that finds a
node dead is retried on the next, and `unhealthy_status 503` for a node that
has lost quorum.

**What one site down costs depends on replication and consistency.** Garage
v1.0.1's quorums, each observed on real containers as well as read from its
source:

| Layout | Uploads with one site down | Reads |
|---|---|---|
| replication 2 on two sites, `consistent` | fail until it returns | work |
| replication 2 on two sites, `dangerous` | work; an upload is confirmed once one copy exists | work |
| replication 2 on three sites | the ones landing on the down site fail, about two in three | work |
| replication 3 on three sites | work | work |

Garage has no data-less tie breaker: a layout gateway node stores nothing and
does not count toward an object's write quorum, so availability with a site
down takes a third copy on a third host, with the disk for it.
`storage.garage.consistency` sets the mode, `consistent` by default, and
validation warns on `dangerous` and on the two-site trade above. What
`dangerous` risks: an upload exists on one disk until the other site catches
up, uploads made during an outage are single copy until it returns, and a
read can miss a recent change, including a delete, after a failover.
Changing the mode is an ordinary `apply`.

**Changing `replication` is not an ordinary apply.** Garage refuses to start
when its stored layout was built at another factor, so `apply` refuses to
write such a `garage.toml`. `storage add --change-replication` runs the only
procedure Garage documents, which it calls unsupported: every node at the old
factor is stopped before any is changed (a node that meets a peer at a higher
factor exits), each stored layout is moved aside to
`meta/cluster_layout.rf<N>` rather than deleted, `garage.toml` is rewritten,
and the cluster is laid out again. Media is unavailable for about a minute.
Before it stops anything it waits for every node to have caught up, so nothing
written at `dangerous` exists on one node only, and it records each bucket's
object count on a host, to be compared once the data has moved. Rejected:
rebuilding Garage empty (loses data, and contradicts rule 3's growth from one
node), and a second cluster plus an S3 copy (not in Garage's documentation,
twice the disk, and a cutover of every app's endpoint).

**Removing or replacing a node is not built.** After `garage layout remove`
the old node must stay online until its data directory is empty, because
Garage does not track block migration, and a dead one needs `layout
skip-dead-nodes`, possibly with `--allow-missing-data`. Both are decisions
about losing data, and will get their own gates.

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
role `hostnames`, and the media hostname of each app that stores objects,
derived or declared, get an A record pointing at the gateway site's
`public_address`. A site whose `endpoint` is a name rather than
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
    ssh:
      host: home-a.local
      user: ubuntu
      public_key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
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

That is what the `ssh` section of a site is: the **bootstrap route**, and the
record of who may use it. Its host is a LAN address, a public hostname,
whatever the operator can actually reach on day one.

**It must be a real address the operator already has.** Not an overlay network,
not a name that only resolves inside one deployment's private mesh. A toolkit
that quietly depended on one would work on the machines it was written for and
fail on every fork.

### Every site has an `ssh` section

Every site, the gateway included, declares one:

```yaml
sites:
  vm:
    roles: [gateway, witness]
    address: 10.44.0.3
    public_address: 203.0.113.10
    ssh:
      host: 203.0.113.10   # optional: defaults to public_address
      user: ubuntu         # required, and must already exist on the host
      port: 22             # optional: empty or 0 means 22
      public_key: |        # required: one or more keys, one per line
        ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
        ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB bob@example.org
```

| Key | Required | Refused when |
|-----|----------|--------------|
| `host` | only without `public_address` | neither a hostname nor an IP address (so `user@host` is refused: the user has its own key) |
| `user` | yes | not a lowercase letter or underscore followed by lowercase letters, digits, underscores or hyphens, 32 characters at most |
| `port` | no | outside 1 to 65535 |
| `public_key` | yes | a line that is not an OpenSSH public key; a line with options (`from=`, `command=`, `restrict`); the same key twice, by fingerprint |

`host` defaults to `public_address` because on most sites they are the same
address written twice. The user name is held to a conservative form because it
goes into commands and a file name on the host. Blank lines in `public_key` are
ignored. Options are refused rather than carried: a line `host prepare` writes
and later compares has to mean the same thing everywhere, and a restricted key
is added to `authorized_keys` by hand, where `host prepare` leaves it alone.

The section replaced a single string, `ssh: home-a.local`. That form is now
refused, and the refusal prints the section to write with the old host filled
in. Keeping it as a shorthand was rejected (founder decision): two ways to write
a site's access is one more thing to read twice, and the string has nowhere to
put the keys.

#### Public keys, not a path to a private key

`paisans.yaml` is one file that every admin of the deployment shares and
commits. A private key is one admin's, on one admin's machine, at a path that is
different on everybody's. So the file lists **public** keys, every admin's,
which are safe to share and which are the same everywhere, and it never names a
private key or where one lives. The toolkit never reads a private key at all.

The same list is the record of who may log in: `host prepare` makes those keys
the login user's authorized keys (see *host prepare manages the login user's
authorized keys*). Adding an admin is adding their `.pub` line and preparing
again; removing one is deleting it and preparing again.

#### How the operator's ssh uses them

Every command that reaches a site runs the operator's own `ssh` (see *Why ssh
is shelled out to and sops is not* in `docs/development.md`) as:

```
ssh -p <port> -o IdentitiesOnly=yes -i <key-1.pub> -i <key-2.pub> <user>@<host> <command>
```

Each listed key is written to a file of its own, 0600, in a fresh 0700
temporary directory that is removed when the command returns. `-i` names a
**public** key file on purpose. OpenSSH documents exactly this: ssh(1) on `-i`
and ssh_config(5) on `IdentityFile` say a public key file may be given "to use
the corresponding private key that is loaded in ssh-agent(1) when the private
key file is not present locally", and ssh_config(5) adds that `IdentityFile`
"may be used in conjunction with `IdentitiesOnly` to select which identities in
an agent are offered during authentication" (OpenSSH 9.9p2's pages). So each
admin's ssh offers the listed keys, finds whichever one's private half is in
that admin's agent, and signs with it there.

`IdentitiesOnly=yes` is what makes it a selection. Without it ssh also offers
every other key the agent holds, first, and a server that allows a handful of
attempts can refuse the operator before it reaches the right one. Several `-i`
are tried in order, so the toolkit does not need to know which admin is running
it.

What this asks of an operator: **the private key must be in their agent**
(`ssh-add -l` lists it), or named by an `IdentityFile` in their own
`~/.ssh/config`, which `IdentitiesOnly` still honours. On macOS the agent is
the one launchd starts on demand (`com.openssh.ssh-agent`, its socket in
`SSH_AUTH_SOCK`). The Keychain holds passphrases, not keys:
`ssh-add --apple-use-keychain` stores a key's passphrase there, and
`ssh-add --apple-load-keychain` adds keys to the agent using the stored ones
(ssh-add(1) on macOS), which is how a key gets back into the agent after a
login without typing anything. An agent that keeps keys elsewhere (a
hardware token's, a password manager's) works the same way, since all ssh sees
is the agent.

Everything else in `~/.ssh/config` still applies (`ProxyJump`, `known_hosts`,
`ServerAliveInterval`), except where the command line already says: `-p` and
the `user@` win over that file's `Port` and `User`, since ssh_config(5) takes
"the first obtained value" and command line options come first. `BatchMode` is
not set, so a first connection can still ask the operator to accept a host key;
refusing that would push them to turn host key checking off instead.

#### `--ssh` replaces the whole section

Every command that reaches a host takes `--ssh <destination>`, the escape hatch
for a route the section cannot describe. When it is given, it is passed to ssh
verbatim, exactly as before the section existed, and the section's user, host,
port and keys are **not** used for the connection. It replaces all of them
rather than one, so what is used is always either everything the file says or
exactly what the operator typed, never a blend. `host prepare` still manages
the section's user's authorized keys under `--ssh`, because those come from the
file, not from the connection.

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

It reaches the host through the site's `ssh` section with the operator's own
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
| the site's `ssh.port`, 22 unless declared | every site |
| 51820/udp, WireGuard | every site |
| everything arriving on `wg0` | every site |
| 80/tcp, 443/tcp | sites with the gateway role |
| `br-+` to the site's mesh address, cluster port and Garage's S3 port | sites with the apps role |

Nothing is listed per site. A site that gains the gateway role gains 80 and 443
at its next prepare, and one that loses it loses them at its next prepare.

#### host prepare owns its rules, and only its rules

Every rule `host prepare` adds carries a ufw comment, `paisans: <why>`, and
that comment is the whole of ownership. A later prepare compares what the site
derives with what `ufw show added` prints, and decides per rule:

| ufw holds | The plan says | What happens |
|-----------|---------------|--------------|
| nothing matching | `change` | added, with the comment |
| the rule, with the comment | `present` | nothing |
| the rule, with no comment | `present`, then `adopt` | re-added with the comment |
| the rule, with someone else's comment | `present (not paisans)` | nothing; it satisfies the rule |
| the same traffic, another action (`limit`, `deny`) | `WARNING` | nothing; a `deny` or `reject` on SSH is refused |
| a commented rule nothing derives any more | `remove` | deleted, last |
| any uncommented or foreign rule | `present (not paisans)` if it bears on a derived rule | nothing, ever |

The alternative was the previous rule, that nothing is ever removed. It is
safe, and it leaves a site that lost the gateway role serving 80 and 443 until
someone notices, which for a firewall is the wrong way round. Removing anything
not derived would be simpler still, and would delete the rule an operator added
for their monitoring agent. The comment separates the two without a state file
that could drift from the host.

Rules are matched the way ufw matches them, on everything except action and
comment. That is not a choice: ufw keeps one rule per match and **adding a rule
that differs only in comment or action replaces the existing one in place**
(ufw 0.36.2, the version Ubuntu 24.04 ships: `UFWRule.match` in `common.py`,
`set_rule` in `backend_iptables.py`). So adding ours over a rule someone else
commented would rewrite their comment, and adding an allow over their `limit`
would loosen it. Both are left alone.

The same behaviour makes adoption one command. A host prepared before rules
carried a comment holds exactly the derived rules, uncommented. Re-adding each
with the comment rewrites it in place, so there is no moment without the rule
and nothing to delete afterwards. Deleting the uncommented copy as a second
step was considered and rejected: ufw lets a delete that names no comment
remove a rule that has one, so it would remove the rule just adopted, and for
SSH, lock the host out. An uncommented rule that no longer matches anything
derived (a role lost before the upgrade) is indistinguishable from an
operator's and stays until removed by hand.

**SSH is never removed**, not even an allow `host prepare` added itself. With
incoming denied by default, that deletion drops the next connection, every
later prepare and apply arrives over it, and recovering takes a console. A
stale SSH allow costs almost nothing; a wrong removal costs the host. A `deny`
or `reject` on the SSH port is refused, whatever the port is.

**Moving `ssh.port` adds the new allow and keeps the old one.** `host prepare`
does not change the port sshd listens on, and it cannot tell whether sshd
already listens on the new one; if it does not, the old allow is the only way
back in. The old allow is recognised by its comment (`paisans: ssh, the
bootstrap route`) and the plan says so:

```
  change    firewall: allow 2222/tcp (ssh, the bootstrap route)
  present   firewall: `ufw allow 22/tcp comment 'paisans: ssh, the bootstrap route'` kept; it is the SSH allow for an earlier ssh.port, and host prepare never removes an SSH allow. Delete it yourself once SSH on 2222 works
```

Removing it once the new one is in was rejected: it saves one open port and
risks a lockout that takes a console to undo.

Removals run after every addition, the default policy and enabling, so a rule
is only taken away once everything that replaces it is in place.

The mesh is let in whole, by interface, rather than port by port. etcd,
Patroni, Garage and HAProxy all listen on the mesh address, and a new mesh
service would otherwise be one more rule to remember on every site.

**SSH is never shut out.** Every allow, SSH first, is in place before the
default deny, and the firewall is enabled with `ufw --force enable`, which
does not stop to ask whether to disrupt existing connections.

**Docker-published ports bypass the firewall.** Docker's install page says so
in as many words: "When you expose container ports using Docker, these ports
bypass your firewall rules." What ufw protects here is everything on the host network,
which is etcd, Patroni, Garage, HAProxy and Caddy, all `network_mode: host`. A
container that publishes a port is not covered by this table, which is why
published ports are to be bound to the mesh address rather than to every
interface. That binding is separate work and is not part of `host prepare`.

#### host prepare manages the login user's authorized keys

`host prepare` makes every key in `ssh.public_key` an authorized key of
`ssh.user`, in `~<user>/.ssh/authorized_keys`. When the directory or the file
is missing it is created, 0700 and 0600, owned by the user. A user that does
not exist is refused: creating users is out of scope.

`authorized_keys` is shared with whoever else manages the host. cloud-init puts
the provider's key there, and an operator may add a restricted key by hand. So
`host prepare` adds only listed keys, and removes only keys **it added
itself**. What it added is recorded in a sidecar,
`/etc/paisans/authorized_keys.<user>.owned`, root's and 0600, one fingerprint
and comment per line. Keys are compared by fingerprint, so a key whose comment
was changed is still the same key.

| `authorized_keys` holds | Sidecar | Listed | The plan says | What happens |
|-------------------------|---------|--------|---------------|--------------|
| nothing for the key | | yes | `add` | appended verbatim, recorded |
| the key | yes | yes | `present` | nothing |
| the key | no | yes | `adopt` | recorded, not added a second time |
| the key, only with options | | yes | `present (not paisans)` | nothing; adding the plain key would undo the restriction |
| the key | yes | no | `remove` | that exact line deleted, last of all |
| nothing for the key | yes | no | `change` (forget) | dropped from the sidecar |
| any other key | no | no | not mentioned | nothing, ever |

```
home-a (ubuntu 24.04)
  adopt     ssh: key SHA256:kmYcvdi2GkPeWxB6XLjrZB8JHsy2Hm8luHMFp9GMvqk (alice@example.org) is already authorized for ubuntu; record it as host prepare's
  add       ssh: authorize key SHA256:RXm/ruZ0eTzRXKwi1AQEDynB0VgHQ2ac9KPSFdf/YnA (bob@example.org) for ubuntu
  remove    ssh: remove key SHA256:baqJQcVDEweKmw1OiZxGooCG2MGxYtwsQQzzOstxmiA (carol@example.org) from /home/ubuntu/.ssh/authorized_keys, which host prepare added and ssh.public_key no longer lists
```

**Ownership lives in the sidecar, not in the key's comment.** Appending a
marker to the comment of every line host prepare writes, and adopting a
cloud-init key by rewriting its comment, would need no second file. It was
rejected because the comment is how an operator recognises a key
(`alice@laptop`), and rewriting it changes what they see in the one place they
look. The sidecar keeps `authorized_keys` exactly as the keys were pasted. Each
step rewrites the sidecar along with its change, so a stopped run resumes: a
key in the file and not the sidecar is adopted again, and a sidecar entry whose
key someone already deleted is forgotten, so a copy they add by hand later is
never taken for host prepare's.

**Nothing is removed until everything is in place.** Removals run after every
addition and after the firewall, and a removal goes through a temporary file
given the original's owner and mode and renamed over it, so sshd never reads a
half written file. A plan that would leave the user with none of the listed
keys is refused. The key the current connection used is not removed either, by
construction: the transport offers only listed keys, so the session's key is a
listed one, unless `--ssh` or the operator's own `IdentityFile` chose another.

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

**Provisional: little of this has run on a host yet.** The command is tested
against a fake transport. `ufw show added` was observed on Ubuntu 24.04 for
uncommented rules; the commented form, adoption and removal are written from
ufw's source, not observed, as are `systemctl show -p RuntimeWatchdogUSec` and
`dpkg-query`. The first real run of each should be read rather than trusted.

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

### `apply` restarts only the infrastructure service whose file changed

The infrastructure stack is one compose project holding etcd, Patroni, HAProxy,
Garage and the gateway's Caddy. A change to a bind mounted file there used to
restart the whole project, so a new `garage.toml` restarted Patroni, which on
the primary is a failover, and HAProxy, which drops every app's database
connection. Each service's bind mounted files live in a directory named for
it (`haproxy/`, `garage/`, `caddy/`), so a restart now names the services
whose directories changed: `docker compose -f /srv/infra/compose.yaml restart
garage`. A file at the top of the stack still restarts the whole project, and
an environment or compose change is still a recreate, which Compose limits to
the services whose configuration changed.

**Mapping files to services by parsing each compose file's volumes was
rejected.** It gives the same answer for every file the templates render, with
a YAML parser in the path of every apply, and the templates are the toolkit's
own.

### A stopped apply force-recreates what it still owes

A stack whose action did not finish is in an unknown state, and Compose cannot
see that. On a real host an `up -d` for the Mbin stack failed part way, on a
host port already in use, and left the `app` container created but attached to
no network. The next apply resumed from the pending record and ran the same
plain `up -d`. Compose compared the configuration, found it unchanged, and
started the half built container, which ran with no networks and no routes.

So a stack the pending record still owes runs `docker compose up -d
--force-recreate`, which replaces its containers whatever Compose thinks of
them. The record drops each stack as soon as its action finishes, so only the
stack that stopped and the ones after it are forced, not the ones that were
already fine. An operator who finds a stack in the same state with no record
names it: `apply --recreate <stack>`, one stack per flag, refused for a stack
the site does not render, and planned even when nothing changed. The ordering
holds either way: the infrastructure stack first, the databases, then apps.

**Forcing every recreate was rejected.** It would cover this case without a
record, but it replaces every container of every stack an apply touches, every
time, and a recreate is an outage however brief.

### `apply` checks each stack before the next one moves

`docker compose up -d` and `restart` exit as soon as containers start. On a
real host that let an apply report success while Mbin's app could not reach its
database: the containers were up, and nothing was asking whether they worked.

So after each stack's action `apply` waits, up to five minutes and polling
every five seconds, until every container of that compose project is running
and every one that defines a healthcheck reports `healthy`. It reads `docker
compose ps --all --format json`, `--all` because an exited container is exactly
what the check is for (docker/compose, `cmd/compose/ps.go`). A container with
no healthcheck counts once it runs: the toolkit cannot invent a check an image
does not ship, and refusing such a stack would refuse most of them.

| What `ps` shows | What `apply` does |
|-----------------|-------------------|
| every container running, healthchecks `healthy` | moves on to the next stack |
| a container still `created`, or a healthcheck `starting` | keeps polling |
| a container `exited`, `dead` or `restarting`, or a healthcheck `unhealthy` | stops now |
| still polling after five minutes | stops |

A stop names the stack and the services, carries the last 30 log lines of each
(`docker compose logs --tail 30`), and starts nothing after it. The stack stays
in the pending record, so the next apply resumes at it and, by the section
above, recreates it outright. The infrastructure stack is checked like any
other, which is also the check on the gateway's Caddy after a reload or an
image change; Patroni's own wait for a primary still gates the database
bootstrap, because a running Spilo container is not yet a primary.

**Failing on the first `restarting` rather than waiting it out is deliberate.**
Every rendered service carries `restart: unless-stopped`, so `restarting` means
the process already crashed once after its dependencies were up, and Docker will
keep trying forever without telling anyone. Treating a timeout as a warning was
rejected for the same reason the other gates refuse rather than warn: an apply
that reports success over a broken stack is how this was found.

### `apply` restarts the apps whose database path changed

An app with `cluster` placement reaches the database through its own site's
HAProxy (rule 2), so a restarted HAProxy, or a Patroni that is recreated and
hands the primary to another member, closes every connection that app holds.
Most apps reconnect. Mbin does not: on a real host, after `site add` restarted
HAProxy, Mbin answered HTTP 500 from then on (`SSL SYSCALL error: EOF
detected`, then `no connection to the server`), because its FrankenPHP workers
keep their connections open across requests and never open a new one. Its
healthcheck does not touch the database, so the container stayed `healthy` and
nothing in the toolkit noticed. Pocket ID recovered on its own.

The toolkit owns this (founder decision): whatever it does that changes the
database path restarts the app stacks that depend on it, and says so in the
plan. In `apply` that is a change to the infrastructure stack that moves
HAProxy or Patroni: `haproxy.cfg` or `patroni.env` changed, the `haproxy` or
`patroni` service changed in `compose.yaml`, or the action restarts or force
recreates either. Then every app on the site with `cluster` placement and a
kind that uses Postgres gets a restart after the infrastructure stack's action
and health gate and after the database bootstrap, and is held to the same gate:

```
restart   talk
    its database path changed (HAProxy/Patroni moved), and an app's open database connections do not survive that
```

An app that already has its own restart or recreate keeps that one action,
with the reason added; it is not acted on twice. A change that moves only etcd,
Garage or Caddy restarts no app: a restart of the infrastructure stack is
narrowed to the services whose files changed (see "`apply` restarts only the
infrastructure service whose file changed"), and a recreate replaces only the
services whose configuration Compose sees changed. A pinned app has its own
Postgres and is not affected. `--only` without `infra` moves nothing under the
apps and triggers nothing; `--only infra` that restarts HAProxy restarts the
apps with it, because leaving them on dead connections is the incident.
`site add` stops the apps around its HAProxy restart, and `failover test`
restarts them on every apps site after each switchover; see those sections.

A `restart` is enough. `docker compose restart` stops and starts the same
container, so its process tree is new and every connection it held is gone,
while its configuration is not reread. Checked with Docker Compose v5.4.0 on
Engine 29.7.2: across a restart the container ID stayed the same, the start
time changed, and every process inside, PID 1 and its children, had a new
PID.

**Fixing reconnection inside each app was rejected as the toolkit's answer.**
It is right in principle, and worth reporting upstream, but the toolkit deploys
apps it does not write and cannot guarantee it for every one of them; a restart
it can guarantee. **Restarting every app after every infrastructure change was
rejected too:** each restart is an outage, however brief, and most
infrastructure changes (a Garage setting, a Caddy route, an etcd timing) never
touch the database path.

### `apply` checks free space before it pulls

A small host fills up with images. The first real host had a 10 GB root disk,
and with the infrastructure stack and Mbin deployed it was 81% used with 1.8 GB
free; one Mbin application image is about 1.4 GB, and an upgrade pulls the new
one while the old one is still on disk. A pull that runs out of space fails
part way through `docker compose up -d`, after the old containers have
stopped, and a full disk takes the database and every log with it.

So `Build` asks the host which of the site's rendered images it already has
(`docker image inspect`, one round trip for all of them). When a stack that
will be recreated names one it does not have, the action will pull it, and
`apply` reads the free space on Docker's data root (`docker info --format
'{{.DockerRootDir}}'`, then `df -B1 --output=avail`). Below the threshold,
`apply` refuses before writing anything, says how much is free and how much it
needs, quotes what `docker system df` says is reclaimable, and names
`--min-free`. The dry run shows the same numbers as a `check disk:` line. A
`restart` pulls nothing and is not checked, and nor is a plan that moves no
stack.

The threshold is 3 GiB, one Mbin image and room beside it, and `apply
--min-free <size>` (Eg: `2G`; K, M, G and T are binary, as `df -h` means them)
changes it for one run, for an operator who knows the pull fits.

**Estimating the pull's size was rejected.** A registry's manifest gives the
compressed size of each layer, not what it unpacks to, and layers already on
the host are shared, so an estimate would be wrong in both directions and need
registry credentials from the host. A fixed floor is crude and is right about
the case that matters: a small disk nearly full. **Warning instead of
refusing was rejected** for the reason every other gate refuses: the failure
it warns of happens with the old stack already stopped.

### `apply` prunes the images it superseded

The free space check above refuses an upgrade on a full disk; this is what
keeps the disk from filling. Every upgrade leaves the old image behind, and on
a small host a second Mbin image is most of the space left.

So after a stack's action passes the health gate, `apply` removes images that
are all three of:

* from a repository that stack's compose file names,
* not an image any stack of this site renders, by ID or by tag, so two apps
  pinning different tags of one repository never prune each other's, and
* not used by any container, running or stopped, by name (`docker ps -a`) or by
  image ID (`docker container inspect`), since a container whose tag moved on
  shows only the ID.

Each goes with `docker image rm <id>`. A removal that fails is a warning and
not a stop: the stack is already healthy, and an image left behind costs disk,
not service. The dry run lists what is on the host and would be superseded as
`prune <repository:tag>` lines under each stack; `Execute` reads the host again
after the gate, because the action has just moved containers off the old
image. Waiting for the gate is the point: until the new image is healthy, the
old one is what a rollback would run. `apply --keep-images` skips pruning for
one run, for an operator who wants to keep that image a while longer.

**`docker image prune -a` was rejected.** It removes every image no container
uses, including ones the toolkit never pulled and knows nothing about: an
operator's own tools, an image staged by hand for a later apply. The toolkit
removes only what its own stacks superseded. **Keeping the previous N versions
was rejected** because of the disk this is for: on a 10 GB host one extra Mbin
image is 1.4 GB, and `--keep-images` covers the rollback case when an operator
actually wants it.

### `app admin create` makes an app's first administrator

An app whose registrations are closed has no way to make its first account
from a browser, and making it by hand means a shell on the server and a
password typed into a command line. So the toolkit makes it:

```sh
security find-generic-password -s talk-admin -w \
  | paisans app admin create --app talk --username founder \
      --email founder@example.org            # shows what it would do
security find-generic-password -s talk-admin -w \
  | paisans app admin create --app talk --username founder \
      --email founder@example.org --execute  # does it
```

The result is a user that exists, is verified and is an administrator. The
command probes first and plans only what is missing, one line each: `create
user`, `verify`, `grant admin`, or `present` when there is nothing to do. An
account that already exists keeps its password; only `--reset-password`
replaces it, so re-running the command can never lock out an admin who has
since changed theirs. After `--execute` it probes again and fails unless the
user is now all three, because a command that exits zero without doing its job
is the failure nobody notices.

It runs on one site: a pinned app's site, or for a clustered app the first apps
site in sorted order, since every copy shares one database. `--site` picks
another, but only one the app runs on.

**The password goes in on stdin and never on a command line.** A command line
is in the process table, readable by every user on the host, for as long as it
runs. Mbin's own commands take a password only as a positional argument
(`mbin:user:create`, `src/Command/UserCommand.php:35-37`, and
`mbin:user:password`, `src/Command/UserPasswordCommand.php:35-36`, at the
fork's tag `v1.13.3+paisans`); neither prompts. So the toolkit pipes a short PHP
script to `php` in the app container. It boots the same kernel `bin/console`
boots and runs those same commands, plus `mbin:user:verify --activate` and
`mbin:user:admin`, through the console Application with the arguments held in
memory. Running the fork's commands rather than writing the user directly keeps
the fork the owner of what an admin is: its pre-approval, its verification
flag and its role array. The script runs as `MBIN_USER` through `gosu`, as the
image's entrypoint runs the server, so nothing it writes under `var/` ends up
owned by root.

Admin creation is per kind, behind one small interface, and Mbin and Pocket ID
implement it. Any other kind is refused by name, with the list of kinds that
are implemented, before any host is reached.

Two alternatives were rejected:

* **Opening registrations briefly.** The window is public: on a federated,
  publicly reachable instance anyone who finds it during the window can
  register, and closing it again is a second manual step that can be
  forgotten. It also needs the setting changed and changed back on a running
  app, which is two mutations to make one account.
* **An admin account declared by an environment variable in the image.** It
  puts a password in the rendered `.env` and in the container's environment,
  where `docker inspect` shows it, for the life of the container. It also needs
  image code that runs at every start and decides whether to act, which is a
  fork change for a one-time event, and it makes rotating that password a
  redeploy.

#### Pocket ID's administrator gets a login link, not a password

Pocket ID users sign in with passkeys, so there is no password to pipe in, and
a new administrator cannot sign in at all until they have registered one. For
a `pocket-id` app the command therefore creates the user as an administrator
and issues a **one-time login link**:

```sh
paisans app admin create --app auth --username founder \
    --email founder@example.org --first-name Fern     # shows what it would do
paisans app admin create --app auth --username founder \
    --email founder@example.org --first-name Fern --execute
```

The dry run prints the exact body a create would send, and the link step:

```
auth on home-a (pocket-id)
  create user founder as an administrator: POST /api/users {"username":"founder","email":"founder@example.org","emailVerified":true,"firstName":"Fern","lastName":"","displayName":"Fern","isAdmin":true}
  issue one-time login link for founder: valid 20m0s and for one sign in, printed once and only with --execute
```

The link is Pocket ID's own mechanism: an admin issues a one-time access token
for a user (`POST /api/users/:id/one-time-access-token`,
`onetimeaccess/module.go:68` at `v2.14.0`), and `{APP_URL}/lc/{token}` signs
that user in once, the same link the binary's `one-time-access-token`
subcommand prints (`cmds/one_time_access_token.go:78`). The founder opens it
and registers a passkey.

**The link is a credential, and it is handled like one.** It is printed once,
to the terminal running the command, only with `--execute`, with its expiry.
It never appears in a plan line or an error and is never written to the
secrets file. It lives twenty minutes: long enough to open a link just
printed, short enough that one left in scrollback is dead soon after. Not
Pocket ID's own fifteen minute default for an admin issued token
(`onetimeaccess/handler.go:17`), because a TTL of fifteen minutes or less gets
a 6 character code (`onetimeaccess/service.go:271-274` at `v2.14.0`), which the
login page accepts only when unauthenticated email login is enabled; on a host
without it the page waits for 12 characters and the link is unusable. Over
fifteen minutes gets the 12 character code. If it
expires, `--login-link --execute` issues a fresh one for an account that
exists; without `--login-link`, an existing administrator plans nothing. An
existing account that is not an administrator plans `grant admin` only, and a
disabled one is refused rather than re-enabled.

**The administrator's email is created verified** (`"emailVerified": true`,
`dto/user_dto.go:29`, stored as sent at `service/user_service.go:299`), and an
existing account whose email is not verified plans `mark email verified for
<u>`: a `PUT /api/users/<id>` that sends every field back as read with only
`emailVerified` changed, since that update overwrites the whole user
(`service/user_service.go:495-519`). Without it Pocket ID sends
`email_verified: false`, and the paisans Mbin fork refuses an OIDC sign in whose
email matches an existing local account unless the provider marked it
verified. That guard stops anyone taking over an account by claiming its
address at the provider, and it is right; the operator creating an
administrator is vouching for the address they typed, which is the
verification it asks for. Turning the guard off in the fork was rejected: it
protects every member who signs up at Pocket ID by themselves, and the
founder's case is fixed where the vouching happens. An account with no email
has nothing to verify and plans nothing for it.

**A password piped to a `pocket-id` run is refused**, not ignored. Ignoring it
would let an operator reusing the Mbin incantation believe a password was set.
`--reset-password` is refused for the same reason and points at
`--login-link`. Stdin is not read at all when it is a terminal, so an
interactive run never waits on it.

The calls go through Pocket ID's REST API with its static API key; see *The
toolkit administers Pocket ID through its static API key*. Three alternatives
were rejected:

* **The binary's `one-time-access-token` subcommand** in the container. It
  takes the username on the command line, which is harmless, but it always
  issues a one hour token (`cmds/one_time_access_token.go:68-78`), and the user
  has to exist first, which only the API can arrange.
* **Pocket ID's first-admin setup page** (`POST /api/signup/setup`,
  `usersignup/module.go:87`). It is open to whoever reaches it first while no
  user exists, which on a public hostname is a race with the internet, and it
  is a browser step.
* **Storing the link**, in the secrets file or anywhere else, so it could be
  printed again. A stored link is a stored way to sign in as an administrator,
  and issuing a new one is one command.

### The toolkit administers Pocket ID through its static API key

Pocket ID is the one application whose first administrator and whose clients
this toolkit creates for itself, and every one of those is a call to its REST
API. What makes the calls possible without a browser is `STATIC_API_KEY`
(`backend/internal/common/env_config.go:59` at tag `v2.14.0`, at least 16
characters at `:182-184`): a request whose `X-API-Key` header equals it
authenticates as a synthetic administrator that Pocket ID creates on first use
and deletes at startup once the variable is unset
(`apikey/service.go:31-36`, `:156-163`, `:221-259`;
`middleware/api_key_auth.go:38`). `init` generates it as
`apps.<app>.static_api_key`, the `.env` renders it, and `render` refuses a
Pocket ID app without it, as it refuses one without its `ENCRYPTION_KEY`.

A call goes from the workstation over the operator's own ssh to the site the
app runs on, where `curl` calls the port Pocket ID publishes on the mesh
address. The command line is one constant, `curl --silent --show-error
--globoff --config -`; the URL, the key and any request body are a curl
configuration on stdin. A command line is in the process table, readable by
every user on the host, for as long as it runs; stdin is not. `curl` runs on
the host because the image has none (`docker/Dockerfile-prebuilt` is Alpine
plus `su-exec`), and the host has it from `host prepare`.

The key is a standing admin credential on every apps site that runs Pocket ID,
which is worth stating plainly. It adds nothing to what root on that host
already holds: the same `.env` carries the database connection string and the
encryption key, and either is all of Pocket ID. It is reachable only over the
mesh, because the port is bound to the mesh address and nowhere else.

Three alternatives were rejected:

* **Clicking through the admin UI.** It is a step on a host nobody can repeat
  or review, and it cannot run before the first administrator exists, which is
  the problem being solved.
* **A key file on the host for an operator's own `curl`.** That is a second
  long lived copy of an admin credential, outside the rendered set `apply` owns
  and checks, readable by whoever can read that file rather than by whoever
  holds the age key to the secrets file.
* **A user API key minted in the UI.** It expires, it belongs to a person who
  might leave, and minting it is the clicking this exists to remove.

### `oidc client create` makes an app's client at Pocket ID

An app that signs members in through Pocket ID needs a client there, and the
app needs that client's ID and secret. The toolkit creates the client and
records both in the secrets file, where `render` already reads them
(`oidc_clients.<app>.client_id` and `.client_secret`), so the next `apply`
renders the app's sign in settings with nothing else to do:

```sh
paisans oidc client create --app talk --admin-user founder            # shows what it would do
paisans oidc client create --app talk --admin-user founder --execute  # does it
paisans apply --site home-a --execute                                 # renders OAUTH_OIDC_*
```

For an app whose configuration sets `OAUTH_OIDC_ADMIN_GROUP: admins`, the dry
run against an empty Pocket ID prints:

```
talk's client at auth on home-a (pocket-id)
  create group admins: POST /api/user-groups {"friendlyName":"admins","name":"admins"}
  create client talk: POST /api/oidc/clients {"name":"talk","callbackURLs":["https://talk.example.org/oauth/oidc/verify"],"isPublic":false,"pkceEnabled":true,"isGroupRestricted":false,"launchURL":"https://talk.example.org/oauth/oidc/connect"}
  create client secret for talk: generated on this workstation, written to oidc_clients.talk.client_id and oidc_clients.talk.client_secret, then sent to POST /api/oidc/clients/<id>/secrets. Never printed
  add founder to group admins: PUT /api/users/<user id>/user-groups with the groups founder is in now, plus admins

Nothing was changed. Re-run with --execute to apply this.
```

**Each line is a Pocket ID mutation, and printing it is what makes approving
it possible.** Client, group and user changes at the identity provider need a
human's approval every time. The command cannot know whether it has one, so it
never assumes it: the dry run is the request, and the operator's `--execute`
is the approval of exactly what was printed. The same probe runs again on
`--execute`, so what executes is what a fresh probe plans.

The client is the app kind's, not a choice made at the command line. For Mbin
that is the paisans fork's verify route as the only callback, PKCE on because
the fork always sends a code challenge, and a confidential client. The groups
are the app's own: the fork reads `OAUTH_OIDC_ADMIN_GROUP` and
`OAUTH_OIDC_MEMBER_GROUP` from the `groups` claim, which Pocket ID fills with
group names (`oidc/claims_service.go:157-162`), so the command reads the same
two keys from the app's `config` and creates whichever group is missing. A
member group also restricts the client to the member and admin groups, so a
refused member is stopped at Pocket ID before the app sees them.
`--admin-user` adds a Pocket ID user, made with `app admin create`, to the
admin group.

**The client is created with a launch URL** (`launchURL`, `dto/oidc_dto.go:52`).
Pocket ID's dashboard lists only clients that have one
(`service/oidc_service.go:673-680` and `:758-765` at `v2.14.0`), so a client
without it works for signing in and is invisible to the members looking for the
app; the first real host's founder saw no apps at all. The launch URL is only
what the app's tile on that dashboard opens; signing in from the app itself
uses the callback URL and does not read it.

It is `https://<hostname>` plus a path. Each kind names its own
(`kinds.DashboardPath`): Mbin's is `/oauth/oidc/connect`, the paisans fork's
route that starts the OIDC flow (`config/mbin_routes/security.yaml:146-148` at
`v1.13.3+paisans`), because the bare host lands a member signed out with a log
in button still to press. Every other kind's is `/`, written as the bare host
with no trailing slash, which is what clients got before kinds named a path.
An app may replace the kind's path:

```yaml
apps:
  talk:
    kind: mbin
    hostname: talk.example.org
    settings:
      sso_dashboard_link: /magazines   # tile opens https://talk.example.org/magazines
```

`validate` refuses a value that is not a path starting with a single `/`,
including a full URL. The host is always the app's own hostname; accepting a
URL would be a second place to spell it, one that can disagree with
`hostname`.

**An existing client's launch URL is changed only when the toolkit put it
there.** That means empty, which is a client made before this command set one,
or one of the toolkit's own defaults (`kinds.ToolkitLaunchURLs`): the bare
`https://<hostname>`, the only default before kinds named a path, and the
kind's current default. Either plans `set launch URL for client <app>`: a `PUT
/api/oidc/clients/<id>` that sends every field back as read with only
`launchURL` changed, because the update overwrites the whole client. That keeps
the callback URLs, PKCE, the public flag, the group restriction (sending it
false would clear the allowed groups) and the federated credentials; it sends no
secret and no logo URL, and Pocket ID ignores a secret sent there anyway. So a
Mbin client made with the bare host moves to the connect route on the next run:

```
  set launch URL for client talk: PUT /api/oidc/clients/<id> with launchURL "https://talk.example.org/oauth/oidc/connect" and every other field sent back as it is now
```

Any other value is an operator's, is reported `present` with a note, and is
never overwritten. If `sso_dashboard_link` is set and the client holds a
different custom value, the command also warns, naming both, and still changes
nothing: clear the launch URL in Pocket ID's admin UI and re-run to use the
setting, or remove the setting or make it match to keep the client's. The list
of toolkit defaults is explicit rather than a rule like "anything under the
app's hostname", because such a rule would overwrite a tile an operator pointed
at a magazine on purpose. A value joins the list only once a release has
actually sent it.

**The secret is never printed and never captured.** Pocket ID accepts a client
secret the caller supplies (`dto/oidc_dto.go:77-83` at `v2.14.0`), so the
toolkit generates it on the workstation, writes it into the secrets file,
re-encrypted to the recipients in `.sops.yaml`, and only then sends it. The
order is the point. A run interrupted between the two leaves a recorded secret
that Pocket ID does not hold; the next run sees that, because Pocket ID keeps
each secret's first four characters (`model/oidc.go:43`), and sends the
recorded one. The other order would leave a live secret at Pocket ID that is
recorded nowhere. If the secrets file cannot be written, Pocket ID is sent
nothing.

**It is idempotent from the probe.** An existing client with the right
callback, PKCE and restriction is `present`, and its secret is left alone
unless `--rotate-secret` is given. A rotation adds a secret and leaves the old
one valid, because Pocket ID allows several (`controller/oidc_controller.go:292`),
so the app keeps signing people in until `apply` renders the new one. A client
that differs from what the app needs is refused, with what differs, rather than
reshaped: updating a client rewrites every field, and a client somebody shaped
by hand is not this command's to change. A launch URL that is empty or a
toolkit default is the one exception, set as above, because neither is a
choice anybody made. After `--execute` it probes again and
fails unless a fresh plan is empty.

Four alternatives were rejected:

* **Creating the client in Pocket ID's UI and pasting the secret into `secrets
  set`.** It is the step this replaces: a browser on the admin UI, a secret
  shown on screen and copied by hand, and a callback URL typed from memory.
  `secrets set` still accepts the key for a client made elsewhere.
* **Letting Pocket ID generate the secret and capturing it from the
  response.** The value would exist only at Pocket ID until the write that
  follows succeeded, and an interruption there leaves a live secret nobody has.
* **Flags for the group names.** The name has to agree with the app's own
  setting, and a flag is a second place to type it that can disagree. Reading
  the app's `config` is the one place a passthrough key is read rather than
  only placed, and this is why.
* **Updating a client that differs.** It would quietly undo whatever made it
  differ, which might have been deliberate.

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
| 3. etcd one to three, one learner at a time | all three members report healthy |
| 4. Spilo joins, clones, streams | replication lag converging |
| 5. Synchronous mode, when configured | a `Sync Standby` exists |
| 6. HAProxy backends | HAProxy routes only to the primary |

Failing at stage 2 rolls back cleanly: drop the peer entries, leave the running
cluster untouched. Failing at stage 3 or later is harder to undo, which is
exactly why stage 2's gate must be strict.

`site add` must be **idempotent**: re-running after a fixed network problem
resumes rather than restarting.

### `site add` is built for sites that dial each other

`paisans site add <site>` implements the table above for one case: a new site
with `roles: [data]` joining a running cluster of one, an existing site gaining
the witness role in the same join, and **every site declaring an `endpoint`**.
A site without one is refused, naming the relay design above, which is still a
design: the first deployment that needs it builds it. Apps on the new site, a
second Garage node and removing a site are out of scope too, each refused or
left alone. `docs/specs/2026-10-07-site-add.md` is the approved specification.

```
paisans site add home-b             # every stage, its steps and its gate
paisans site add home-b --execute   # runs them, stopping at the first failed gate
```

The configuration is the end state, and `site add` plans only what differs
from the live deployment: the `wg0.conf` on each site, `etcdctl member list`,
`patronictl list` and `show-config`, and HAProxy's statistics. A stage the live
state already satisfies plans no steps, and its gate is still checked, so a
re-run proves each stage again and resumes at the first gate that fails.
Preflight is the exception: it checks a host the join has not touched, and once
stage 2 has run the new site's own `wg0` holds 51820/udp, which the ports check
refuses. A join is recognised as started from what only stage 2 or later leaves
(the new site's `wg0.conf`, or its etcd member), and the plan says preflight is
skipped and why.

| Stage | What runs | Gate |
|-------|-----------|------|
| 2. Mesh | each site's `wg0.conf`, as a scoped `apply`, then `wg syncconf` (the new site's `wg0` is started) | a handshake younger than two minutes for every pair, read on both ends, and a ping of the new mesh address from every site |
| 3. etcd | for each joiner, witness first: `member add --learner`, its compose file and record written, `up -d etcd` alone, `member promote` retried until etcd accepts it | `endpoint health --cluster` all healthy, and the voters are exactly `etcd.members` |
| 4. Replica | the new site's remaining files by a whole `apply`, then `up -d`, which starts Patroni beside the running etcd | the member `streaming`, its replay lag zero or falling over three samples five seconds apart |
| 5. Cluster configuration | `patronictl edit-config --force -q -s synchronous_mode=true -s synchronous_mode_strict=<value>` when the live values differ | a member with the role `Sync Standby` |
| 6. HAProxy | `haproxy.cfg` as a scoped `apply`; the site's cluster database apps stopped, HAProxy alone restarted, the apps started and each through `apply`'s health gate | the statistics list every cluster site, the leader `UP` and every replica `DOWN` |

**Learners, one at a time.** A full voter added to a cluster of one makes the
quorum two before it has started; if it then fails to start, the cluster stops.
A learner does not vote, so the cluster keeps its quorum while it catches up,
and etcd refuses the promotion until it has (`ErrLearnerNotReady`, etcd
v3.5.16), which is why the promotion is retried with a doubling delay rather
than timed. Rejected: both new members added as voters at once, where any
failure costs quorum, and a re-bootstrap at three, which is an outage for a
routine growth step. Between the witness's promotion and the new site's, the
cluster has two voters; that is the price of one learner at a time, and it
lasts one stage.

**A member keeps the flags it was born with.** etcd reads
`--initial-cluster` and `--initial-cluster-state` only on a member's first
start and ignores them once its data directory exists. They are inert on a
running member, so the only thing changing them can do is make `apply` see a
new compose file and recreate a healthy member. Each etcd host therefore has
`/srv/infra/etcd-initial`, a rendered file holding the two flags its member
started with, which every later render repeats. A founder records `new` and the
founding set. A joiner records `existing` and the membership **right after its
own `member add`**, which etcd requires exactly: a joiner whose
`--initial-cluster` counts a different number of members than the cluster has
is refused with "member count is unequal" (etcd v3.5.16,
`membership/cluster.go`, `ValidateClusterAndAssignIDs`). When the witness joins
first, the new data site is not a member yet, so the two joiners' flags differ.
A host applied before the record existed has its compose file's own flags read
instead, so its first apply under this rule changes nothing it runs. Writing
the record never acts on a stack, because no container reads it.

The specification named the file `etcd-founders` and gave every joiner all of
`etcd.members`; both were corrected here for the reason above. Rejected:
deciding `new` or `existing` from whether a data directory is non empty, which
flips a running founder's flag on its first apply and recreates it.

**`apply` refuses a half grown cluster.** While the live membership (learners
included) differs from `etcd.members`, `apply` on a site that is, or is
configured to be, an etcd member refuses to write or act on its infrastructure
stack, and says to run `site add`. An operator who edited the configuration
and ran `apply` first would otherwise start an etcd the cluster never admitted.
An app only change on the same site is let through.

**Existing sites move one file each.** A whole `apply` of the primary's site
would recreate its Patroni, because `patroni.env` now lists every etcd member:
a failover in the middle of a join. So stages 2 and 6 use a scoped `apply`,
which compares, refuses on conflict, writes and records exactly like a whole
one, but only for the named file, and runs only the one command that file
needs. The primary's `patroni.env` is left, and the plan says so: its Patroni
keeps working against its own etcd member, which stays a voter throughout, and
a later `apply` of that site, when a primary restart is acceptable, brings it
up to date.

**HAProxy is restarted, not reloaded.** Its configuration is a single file bind
mount, `apply` replaces a file by renaming a new one over it, and a bind mount
of a single file keeps the inode it started with (moby/moby#15793), so a
reloaded HAProxy would read the old file. Restarting HAProxy alone remounts it,
and is what `apply` itself does for a changed `haproxy.cfg` (see "`apply`
restarts only the infrastructure service whose file changed"). The gate
reads HAProxy's statistics from a listener on `127.0.0.1:8404`, as CSV. That
listener is new in every rendered `haproxy.cfg`, so a deployment that applies
before growing restarts its infrastructure stack once for it. Rejected: a
stats socket, which needs a client the image may not carry, and a query
through HAProxy for `pg_is_in_recovery()`, which needs the superuser password
over TCP and shows only that a primary answered, not that a replica is out of
the rotation.

**The apps that use the database are stopped around HAProxy's restart.** On
the first real join, stage 6 restarted HAProxy under a running Mbin, and Mbin
answered HTTP 500 from then on (`SSL SYSCALL error: EOF detected`, then `no
connection to the server`) until it was recreated by hand. Its FrankenPHP
workers keep their database connections open across requests and do not
reconnect when one is closed, and its healthcheck does not touch the database,
so the container stayed `healthy` throughout. Pocket ID recovered on its own.
So on each site whose HAProxy restarts, stage 6 first stops every app stack
there with `cluster` placement and a kind that uses Postgres (`docker compose
-f /srv/<app>/compose.yaml stop`), restarts HAProxy, starts them again (`up
-d`), and holds each to `apply`'s health gate; a failure stops the join there
with the app's logs. The plan lists the outage: stop, restart, start, check. A
pinned app has its own Postgres and is left alone. This is one instance of a
rule the toolkit owns: whatever changes the database path restarts the apps
that depend on it (see "`apply` restarts the apps whose database path
changed").

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

#### What `paisans preflight` checks, and how

`paisans preflight --site <new>` runs the checks on their own and changes
nothing; `site add` runs the same checks as its first stage. Each finding is
`ok`, `WARNING` or `REFUSED`, and any refusal stops the join. A check that
could not be made (a host that did not answer, output that did not parse) is
a refusal, not a skip: preflight exists so that every assumption was looked
at, and "could not look" is not "looked and it was fine".

| Check | Where | How | Refused when |
|---|---|---|---|
| SSH and sudo | every site | `sudo -n true` | the host does not answer, or sudo wants a password |
| Clock | every site | `timedatectl show -p NTPSynchronized --value` | not `yes` |
| Prepared | new site | `host prepare`'s own plan | it has any step left |
| Platform | new data site, against each existing data site | `/etc/os-release` `ID` and `VERSION_ID`, and the release at the end of `ldd --version`'s first line | any of the three differs |
| WireGuard | new site | `/sys/module/wireguard`, else `modprobe -n wireguard` | neither |
| Watchdog | new data site, unless its mode is `off` | `/dev/watchdog` exists | it does not |
| Ports | new site | `ss -Hltnu` | anything listens on 51820/udp; on 2379 or 2380 for an etcd member; on 5432, 8008 or 8009 for a data site; on the cluster port where the site runs HAProxy |
| Routes | new site | `ip -j route` | a route overlaps `mesh.subnet`, other than one through `wg0` |
| Disk | new data site | the leader's `sum(pg_database_size(...))`, through its Patroni container's `psql`; `df` on the deepest existing directory towards `/srv/infra/postgres` | free space is under that plus 2 GiB |
| Round trip | new site to every other site | three TCP connects to the site's `public_address` on its ssh port, timed with bash's `/dev/tcp` and `$EPOCHREALTIME`; the median is used | `etcd.election_timeout_ms` is under five round trips |

Three of these need a word.

**glibc's "major version" is its 2.N release.** glibc has been 2.x since 1997
and numbers releases by the second part, and collation changes ride on that
(2.28 is the notorious one). Comparing the leading `2` would compare nothing.

**The round trip warns as well as refusing.** It warns when `etcd.heartbeat_ms`
is under one round trip, and when the election timeout is under ten: etcd's own
tuning guide (v3.5 docs, *Tuning*) puts the heartbeat at "around 0.5-1.5x the
round-trip time" and says election timeouts "must be at least 10 times the
round-trip time". The refusal sits at five, the spec's number, so a deployment
that tuned close to etcd's guidance is warned rather than stopped. The list
above asks for the timings to be derived from the measurement; the site add
spec (`docs/specs/2026-10-07-site-add.md`) checks them instead, because a
derived value would change the rendered etcd flags on every member, and
changing those on a running cluster is an operation of its own.

**Two items above are covered indirectly or not at all.** Docker's presence
is part of `host prepare`'s plan, so "prepared" covers it. Whether storage is
local rather than network attached is not checked: nothing on a host says so
reliably.

The ports check reads listeners, not owners. After the mesh stage the new
site's own `wg0` holds 51820/udp, so preflight is the gate for a join that
has not begun, and a join resumed past stage 1 must not be sent back through
it.

### `failover test`: a switchover on purpose

A cluster that has never failed over has an untested failover, and the first
test should not be an outage. `paisans failover test` moves the Patroni
primary to another data site and back, and it is gated like `site add`: it
checks before it moves anything, every switch ends at a gate, and a gate that
does not pass stops it where it is rather than switching back blind.

1. **Checks.** Every member of `cluster.sites` is in the cluster, the leader
   is `running` and every other member `streaming` with lag zero; a Sync
   Standby exists when `cluster.synchronous` is true; every app stack is
   healthy on every site it runs on, by the same judgement as `apply`'s health
   gate; and every app answers through the gateway, an HTTPS request from the
   workstation to its hostname answering under 500 (a redirect to a login page
   counts). Pocket ID is asked for `/.well-known/openid-configuration`, the
   document every client of it fetches first. Lag is looked at up to three
   times, because Patroni compares a replica with the leader's last reported
   position and a busy moment shows bytes that are gone on the next look.
2. **Switch.** In the leader's Patroni container, `patronictl -c
   /home/postgres/postgres.yml switchover --leader <current> --candidate
   <other> --force`. The candidate is the Sync Standby where there is one,
   since that is who Patroni would promote on a real failure. The flags are
   Patroni v4.1.0's (`patroni/ctl.py`): `--leader` is checked against the
   live leader, so a plan made against a leader that has since changed fails
   rather than switching the wrong way, and `--force` with no `--scheduled`
   means now. The config path is the one Spilo runs Patroni with
   (`postgres-appliance/runit/patroni/run`, at every Spilo tag the toolkit
   pins), a link to the `/run/postgres.yml` Spilo writes at start.
3. **Gate.** Polled for up to three minutes: the candidate leads and the old
   leader is `streaming`. The gate reads `/cluster` rather than trusting
   patronictl's exit status, because when Patroni refuses a switchover
   patronictl prints `Switchover failed` and exits normally.
4. **Restart the database apps.** Every app with `cluster` placement and a
   kind that uses Postgres is restarted on **every** apps site (`docker
   compose -f /srv/<app>/compose.yaml restart`), not only the old primary's:
   a leader change closes every client's connection to the old primary
   wherever the client runs, and an app whose workers never reconnect, as
   Mbin's do not (see "`apply` restarts the apps whose database path
   changed"), answers 500 until restarted while its container stays
   `healthy`. A pinned app has its own Postgres and is left alone. A failed
   restart stops the test there.
5. **Gate.** Polled for up to three minutes: the cluster still as above, and
   every app healthy and answering again.
6. **Switch back**, the same way, through the same gates and restarts.

It is a dry run by default, which prints the checks, both commands and the
expected interruption: writes fail from the old primary's demotion until each
site's HAProxy marks the new one up, which with the rendered `inter 3s` and
`rise 2` is several seconds after promotion, twice. Connections to the old
primary are closed when it is marked down (`on-marked-down
shutdown-sessions`), and reads through HAProxy pause too, since it routes only
to the primary. The plan lists each app restart after each switch, a further
few seconds per app. Waiting for each app to reconnect on its own was
rejected: some never do, and their healthchecks do not notice.

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

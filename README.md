# paisans-stack

Toolkit for installing and operating a paisans community stack: a private,
federated community on hardware its organizers control.

**Status: nothing is implemented yet.** This repository holds the design and
will hold the scripts. Do not expect anything here to run.

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

Every stack with a `cluster` placement connects to its cluster's local HAProxy port (`127.0.0.1:5000` for the default cluster) from the first
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
| `pinned: <site>` | ordinary self-contained deployment at one named location | that location's |
| `cluster: <name>` | joins the named HA Postgres cluster, follows its primary | survives losing one site |

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

#### An app may have its own cluster

`cluster` takes a **name**, so a community that wants high availability for one
app but not another is not forced into all-or-nothing:

```yaml
mbin:      { placement: { cluster: mbin } }   # its own HA cluster
outline:   { placement: { cluster: main } }
pocket-id: { placement: { cluster: main } }
synapse:   { placement: { pinned: home-a } }
```

**This is not a second copy of the HA machinery.** Patroni's FAQ is explicit that
multiple clusters share one DCS as long as their namespace and scope differ; the
only failure is reusing a namespace/scope pair, where the second cluster finds a
mismatched Postgres system identifier and correctly refuses to manage its
database.

| Built once, shared | Duplicated per cluster |
|--------------------|------------------------|
| etcd (3 voters including the witness) | a Spilo pair, one container per site, with its own `SCOPE` |
| WireGuard mesh | a HAProxy port (5000, 5001, …) |
| Gateway and Caddy | a WAL-G prefix in Garage |
| Garage | |

So "one shared Postgres cluster for everything" is not an architectural rule —
it is **the default value**, every app on `cluster: main`. Naming a second
cluster costs another Spilo container per site and a port.

It also recovers something the single-cluster default gives up: with one
cluster, a failover moves every app at once. Per-app clusters make blast radius
a per-app decision.

Three real costs, and the toolkit should not push anyone toward many clusters:

* **Memory.** Each cluster is a real Postgres — its own shared buffers, WAL
  writer and autovacuum workers on every node. On home hardware this binds
  before CPU does.
* **Rehearsals multiply.** Each cluster has its own failover to drill. They no
  longer fail over together, which is the point, but it is N drills.
* **The watchdog is an open question.** Patroni arms `/dev/watchdog` before
  promoting, and the documentation does not say what happens when two Patroni
  instances share a host. `/dev/watchdog` is a single character device normally
  opened exclusively, so the second instance may fail to arm it — and under
  `watchdog: mode: required` that means **it refuses to become primary**, which
  presents as a broken cluster rather than a misconfiguration. **Test this
  before running more than one cluster per host.** Likely answers are that one
  cluster takes the watchdog and the rest run without it (weaker fencing, still
  safe unless Patroni itself hangs), or that separate devices can be provided.
  Not yet verified either way.

#### Consequence for routing

The gateway's Caddy configuration becomes inventory-driven: a pinned app's
hostname points at its location, everything else follows the current primary.
A template job, not an orchestrator job.

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

### `.env` files are build artifacts

`paisans apply` renders them. Nobody edits them, and `apply` refuses to clobber
one that has local modifications until the difference is resolved.

```
WORKSTATION                        HOST                          CONTAINER
───────────                        ────                          ─────────
paisans.yaml      ─┐
secrets.enc.yaml  ─┤ render
age key           ─┘   │
                       │ ssh push
                       ▼
                  /srv/talk/.env   (plaintext, 0600) ──► env vars
                  /srv/talk/compose.yaml
                  caddy/, haproxy.cfg, patroni env
                                                       docker compose up -d
```

`sops` and `age` are installed on the workstation only. Hosts do not have them.
Containers never know they exist, and nothing is written *into* a container —
Compose passes env at start. Changing a secret therefore means re-render plus
`compose up -d` to **recreate**; a `restart` will not pick it up.

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

## Design documents

The full design lives in the community's Outline instance, in Operations. This
repository will carry implementation notes and runbooks specific to the toolkit
as they are written.

## Relationship to `paisans.community`

`paisans.community` is one deployment: its compose files, its Caddy config, its
operational scripts. This repository is the generalisation — the part another
organizer could adopt without inheriting this community's specifics.

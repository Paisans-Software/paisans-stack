# Multi-site Garage: `paisans storage add`

Date: 2026-10-07. Status: **approved by the founder in session**, with the
decisions recorded under *The founder's decisions*. Replaces the README section *A second Garage site has to be joined
by hand first*, whose `garage node connect` is a hand step on a server, which
the founder's rule forbids: a server is changed only by the toolkit.

Every claim about Garage below is about `dxflrs/garage:v1.0.1`, the image the
toolkit pins, and carries one of two kinds of evidence:

* **source**: a `path:line` in the Garage tree at tag `v1.0.1` (commit
  `7a143f46`), or a page of its in-tree documentation under `doc/book/`. The
  live website was not used, since it may describe a later release.
* **observed**: run against real `dxflrs/garage:v1.0.1` containers on a
  Docker network while writing this, two and three nodes, each in its own
  zone. Where the two agree, both are cited.

## Scope

In:

* Joining Garage nodes into one cluster with no hand step, staged and gated in
  the style of `paisans site add`, dry run by default, resumable.
* Changing `replication_factor` on a cluster that already holds data, which
  Garage does not do in place (below). Growth from one node is the toolkit's
  own premise (*Object storage ships from the first install, single node at
  init and replicated once a site is added*, `garage.toml.tmpl`), so this is
  not optional.
* Rendering: a per-site `S3_ENDPOINT`, the gateway's media routes across every
  Garage node with failover, and a `garage.toml` correct for many nodes.
* Validation for incoherent and risky combinations.
* A smoke test as the final gate, reading through every node.

Out, each for a stated reason:

* **Removing or replacing a node.** Not trivial: after `layout remove` the old
  node must stay online until its data directory is empty, because Garage does
  not track block migration (source: `src/garage/cli/layout.rs:401-406`, "the
  migration of data blocks is not tracked, so you might want to keep old nodes
  online until their data directories become empty"), and a dead node needs
  `layout skip-dead-nodes`, with `--allow-missing-data` as the escape hatch
  (`layout.rs:412-469`). Both are decisions about losing data, which deserve
  their own gates and their own spec.
* **Lowering `replication_factor`.** The same procedure as raising it, but it
  discards copies; nobody needs it yet.
* **Garage gateway nodes** (`layout assign -g`, a role with no capacity). They
  forward requests without storing data (`doc/book/cookbook/gateways.md:6`),
  which adds nothing to availability here: every site that would want one can
  be a storage node.
* **Sites behind NAT.** Garage's RPC needs every node to dial every other, the
  same constraint `site add` documents for the mesh.

## What Garage v1.0.1 guarantees

### Quorums

`replication_factor` is any integer of at least 1, with no default
(`src/util/config.rs:41-45`); `consistency_mode` is `consistent` (the
default), `degraded` or `dangerous` (`config.rs:47-51`,
`src/rpc/replication_mode.rs:10-26`). The quorums
(`replication_mode.rs:45-59`, and the table at
`doc/book/reference-manual/configuration.md:257-266`):

| `replication_factor` | `consistency_mode` | write quorum | read quorum |
|---|---|---:|---:|
| 1 | any | 1 | 1 |
| 2 | `consistent` | 2 | 1 |
| 2 | `degraded` | 2 | 1 |
| 2 | `dangerous` | 1 | 1 |
| 3 | `consistent` | 2 | 2 |
| 3 | `degraded` | 2 | 1 |
| 3 | `dangerous` | 1 | 1 |

Three layers use them differently (`src/model/garage.rs:146-188`):

* **Data blocks** write with the configured write quorum; reads try one node
  at a time, local first (`src/block/manager.rs:288-339`, `:367-409`).
* **Object metadata** (object, version, block_ref tables) uses both configured
  quorums.
* **Buckets, keys, aliases, grants and website settings** are fully replicated
  to every node with a role. They read locally and a write needs every member
  but one, whatever the consistency mode (`src/table/replication/fullcopy.rs:34-54`).

A failed quorum reaches an S3 client as `503 ServiceUnavailable`
(`src/api/common_error.rs:58-63`).

### Two nodes, one down

| Setting | Uploads | Reads | Buckets and keys | Evidence |
|---|---|---|---|---|
| RF 2, `consistent` | **fail**, 503 | work | work | observed; `configuration.md:176-180`: "Data remains available in read-only mode when one node is down, but write operations will fail" |
| RF 2, `degraded` | **fail**, 503 | work | work | observed; `configuration.md:242-244`: its properties "are the same as with consistency mode `consistent`" |
| RF 2, `dangerous` | work | work | work | source (`replication_mode.rs:52-55`); observed |

So **uploads stop when either of two nodes is down**, at every setting but
`dangerous`. What `dangerous` risks, in the docs' words
(`configuration.md:246-255`): a write can be acknowledged "before the second
copy is fully written (or even before it even starts being written)", data is
more easily lost if that node then fails, and written objects "might not be
visible immediately in read operations". Nothing queues a missed write for the
absent node: it catches up only through background table sync, every ten
minutes and on each layout change (`src/table/sync.rs:31`, `:579-590`), and
block resync (`src/block/resync.rs:456-477`).

**Observed, and not in the docs:** there is a window after a node goes down,
before its peers mark it failed, in which requests that need it hang rather
than fail. A PUT through the surviving node ran into a 90 second client
timeout. Peers marked the stopped node failed after between 4 and 90
seconds in these runs, most often about 85. Media reads served from the surviving node's own copy
did not hang.

### Three nodes, one down

| Setting | Uploads | Reads | Evidence |
|---|---|---|---|
| RF 3, `consistent` | work | work | source: write 2 of 3, read 2 of 3; `configuration.md:182-187`, "reading and writing data to Garage can continue normally"; observed |
| RF 2 on three nodes | **fail for the partitions the down node holds**, about two thirds of them | work | source: each partition sits on 2 of the 3 nodes (`src/rpc/layout/version.rs:117-125`), and those that include the down node lose write quorum; observed, `/v1/health` reported quorum on 85 of 256 partitions |

RF 2 on three nodes is the worst of both: three hosts' worth of cost, and most
uploads still fail when one is down.

### Zones

The layout spreads each partition's copies across at least
`min(distinct zones, RF)` zones by default (`version.rs:18-23`, `:157-172`,
`:314-319`), so **replicas land on different sites only if the zone names
differ**. `storage init` already assigns zone = site name, and this keeps it.

### Changing `replication_factor`

**It cannot change in place.** Three facts, each from source and the first two
observed:

1. A node whose stored layout has a different factor refuses to start:
   "Prevous cluster layout has replication factor 1, which is different than
   the one specified in the config file (2). The previous cluster layout can be
   purged, if you know what you are doing, simply by deleting the
   `cluster_layout` file in your metadata directory."
   (`src/rpc/layout/manager.rs:46-56`; observed, the container exits 1.)
2. The docs: "Never run a Garage cluster where that is not the case [the same
   factor on every node]" (`configuration.md:197-198`).
3. A node that sees a peer with a **higher** factor exits: "This is not
   supported and will lead to data corruption. Shutting down for safety."
   (`src/rpc/system.rs:583-587`). Connecting an RF 2 node to a running RF 1
   node stops the RF 1 node.

The **only procedure in Garage's documentation** (`configuration.md:200-212`),
which it calls "a dangerous operation that is not officially supported": stop
every node, delete `cluster_layout` from each metadata directory, change the
factor in every configuration, start them, and build a new layout from
scratch. A full rebalance follows; data "might temporarily appear
unavailable", and public access should be shut off meanwhile.

**Observed:** on a single RF 1 node holding a bucket, a key, a website grant
and an object, moving `cluster_layout` aside, restarting at RF 2, joining a
second node and applying a fresh layout at version 1 kept everything: the
object read back through both nodes, and `bucket list` on the new node showed
the bucket. Until the new layout was applied, every read answered 503 ("Could
not reach quorum of 1"), so the outage lasts from the stop to the apply.

A new cluster plus a copy with an S3 client is the other way to do it. Garage's
v1.0.1 docs do not describe it, and it needs twice the disk and a second set of
keys, so it is listed as an alternative and not recommended.

### Joining a node

* `garage node id -q` prints `<id>@<rpc_public_addr>`
  (`src/garage/cli/init.rs:16-22`).
* `garage node connect <id>@<host>:<port>` dials it; "this will establish
  communication both ways", and "nodes will discover one another
  transitively" (`doc/book/cookbook/real-world.md:255-263`; peer list
  exchange in `src/net/peering.rs:450-490`). One reachable peer is enough.
* Connected peers are saved to `<metadata_dir>/peer_list` every 60 seconds and
  redialled after a restart (`system.rs:43`, `:274`, `:645-650`, `:722-749`).
  A join therefore survives restarts with no `bootstrap_peers`.
* `garage layout assign -z <zone> -c <capacity> <id>` needs the node to be
  known, connected or already in the layout (`src/garage/cli/layout.rs:47-70`).
* `garage layout apply --version N` requires N to be the current version plus
  one, and refuses no flag at all (`src/rpc/layout/history.rs:270-283`).
  Several doc pages show the command without `--version`; the source is what
  runs.
* Assignment fails with fewer storage nodes than the factor
  (`version.rs:325-332`). **An RF 3 layout has to be applied with three nodes
  at once**; it cannot be grown one node at a time from one.

### Knowing the data is replicated

* `garage layout history` prints "Your cluster is currently in a stable state
  with a single live layout version. No metadata migration is in progress"
  once every node has synced the newest version, and "Several layout versions
  are currently live in the cluster, and data is being migrated" before that
  (`layout.rs:359-406`). That covers metadata only.
* Blocks are covered by `garage stats`, which prints "resync queue length: N"
  and "blocks with resync errors: N" per node
  (`src/garage/admin/mod.rs:184-240`); the recovery guide monitors a new node
  the same way (`doc/book/operations/recovering.md:112-113`).
* The admin API has no machine readable "migration finished" signal:
  `/v1/status` and `/v1/layout` carry no history or trackers
  (`src/api/admin/cluster.rs:19-205`). So the gate reads the CLI.

### Any node serves

* The S3 API, the web endpoint and the admin API start on every node that
  configures them (`src/garage/server.rs:83-128`), and a request is routed by
  the layout from whichever node received it, local copy first
  (`src/rpc/rpc_helper.rs:566-600`). Writing through any node works.
  **Observed:** a PUT through one node read back through the other, over both
  the S3 API (3900) and the web endpoint (3902, Host
  `<bucket>.web.garage.internal`).
* The web endpoint resolves the bucket from the local alias table and serves
  through the same handlers as S3 (`src/web/web_server.rs:200-260`).
* **A connected node with no role has empty bucket and key tables**, because
  only layout members receive the fully replicated tables
  (`fullcopy.rs:29-36`; inference from the code). It answers every bucket as
  missing. This matters for ordering below.

### Buckets and keys on a joined node

Buckets, aliases, keys, grants and website settings live in the fully
replicated tables (`src/model/garage.rs:54-58`, `:168-188`), so a node that
has joined the layout holds them all. **Observed:** `garage bucket list` on a
freshly joined node listed the bucket created on the first. `storage init`
against a joined node therefore finds every key, bucket, grant and website
setting `present` and plans nothing; the new command's provisioning stage
gates on exactly that.

### Health

`GET /health` on the admin port answers 200 when the cluster is healthy or
degraded and 503 when "Quorum is not available for some/all partitions"
(`src/api/admin/api_server.rs:170-193`). It is computed with the **consistent
write quorum whatever `consistency_mode` says** (`system.rs:432-434`). With RF
2 on two nodes and one down, the survivor answers 503 while it still serves
every read (observed). The S3 and web ports have no health endpoint.

## The founder's decisions

Recorded in session on 2026-10-07, after the evidence above was walked
through.

### 1. Replication and node count: RF 2 on two sites, `dangerous`, now

| Option | One site down | Cost |
|---|---|---|
| A. RF 2 on two sites, `consistent` | media keeps loading; **every upload fails** until it returns | none beyond the second site |
| **B. RF 2 on two sites, `dangerous`** (chosen) | uploads and reads continue | an acknowledged upload can exist on one disk only, and reads can miss a recent change; Garage's docs say it "severely breaks the consistency and durability guarantees" |
| C. RF 3 on both sites plus the gateway | uploads and reads continue | the gateway holds a full copy of every object. **Ruled out: the gateway does not have the disk** |
| D. RF 3 on both sites plus a third host with good upload, listed first | uploads and reads continue; reads and write fan-out use that host's bandwidth, not a home's upload | a third host with disk for a full copy |
| E. RF 3 on both sites plus two small hosts | uploads and reads continue | the two small hosts share the third copy; the cluster holds no more than their combined capacity |

**Decision: B now, growing to D later** through decision 2. The founder
judged B's risks acceptable for now, having weighed them as follows.

* **A one-disk window on every upload.** The receiving node confirms once its
  own copy exists; the second copy follows in the background, and nothing
  queues it if it fails (`rpc_helper.rs:507-515`).
* **Uploads during an outage are single copy** until the absent site returns
  and catches up through table sync, every ten minutes (`sync.rs:31`), and
  block resync. Losing the surviving disk in that period loses them, while
  Patroni's synchronous replica keeps the posts that point at them.
* **A deleted object can reappear after a failover.** A delete is a newer
  record replicated like any write. If one node missed it and the node that
  took it then fails before they sync, the gateway's failover serves the old
  copy until they do. Both conditions together are rare. (Inference from the
  quorums; not exercised.)
* What is **not** at risk: buckets, keys and grants, which are fully
  replicated whatever the mode, and the consistency mode itself, which changes
  with a configuration edit and a restart (observed: a live two node cluster
  switched from `consistent` to `degraded` that way).

Garage has no data-less tie breaker, which is what made a third copy the only
way to option D's availability. A layout gateway node (`-g`) stores nothing
and is not counted in an object's write quorum, which is taken over the
storage nodes holding that partition (`src/rpc/layout/manager.rs:146-156`).

**Observed, option E's shape:** four nodes at RF 3 with capacities 100G, 100G,
10G and 30G. `layout show` gave the two large nodes all 256 partitions each and
split the third copy 64 to 192 between the small ones, in proportion to
capacity, with usable capacity 120 GB of 240. With one large node stopped,
`/v1/health` reported `degraded` with quorum on all 256 partitions. RF 2 on
four equal nodes kept quorum on 128 of 256. That the large nodes take full
copies is what the layout optimiser chose, not a rule found in the source.

Two things option D or E will need, deferred until a third host exists:

* **A storage-only site.** Every site needs one of `data`, `apps`, `gateway`
  or `witness` today (`internal/config/config.go`, the roles check in
  `Validate`). The proposal is to accept a site with no roles when
  `storage.garage.sites` lists it, so that list stays the one place that says
  where storage runs. Rejected: a `storage` role, which would say it twice.
* **Per-site capacity**, for option E only.

### 2. Changing the factor: the toolkit runs Garage's documented reset

`storage add --change-replication` runs the procedure in
`configuration.md:200-212`: stop every node, set each `cluster_layout` aside,
write the new `garage.toml` everywhere, start them, and build a new layout.
The flag is explicit because Garage calls the procedure unsupported. Without
it, a plan that needs it stops and says why.

Rejected: refusing and rebuilding Garage empty (loses data, needs a
destructive command of its own, and contradicts the growth premise in
`garage.toml`'s own comment); a new cluster plus an S3 copy (not in Garage's
docs, double the disk, a second set of keys, an endpoint cutover).

### 3. One command, cluster-wide: `paisans storage add`

No `--site`:

* An RF 3 layout cannot be grown one node at a time (`version.rs:325-332`), and
  a factor change is a whole-cluster stop. Both are cluster operations; a
  per-site command would have to refuse half its invocations.
* `site add` is scoped to a new `[data]` site joining etcd and Patroni, and
  a site that already runs Patroni is a member already. Folding storage into it would leave an
  existing site, or a storage-only host, without a command.
* `site add` can call the same planner as a later stage when a site gains data
  and storage at once; nothing here prevents it.

### 4. `--execute`, as every other command

Dry run by default; `--execute` changes hosts. Founder decision, keeping the
toolkit's one convention: `apply`, `host prepare`, `storage init`, `site add`,
`failover test` and `dns init` all work this way, and a `storage add` typed to
see where things stand must never be able to stop every Garage node. Rejected:
acting by default with a `--status` flag, which puts that safety on
remembering a flag.

### 5. It does not wait

`storage add --execute` runs every stage that can finish now and **exits at
the first stage that is waiting on Garage**, printing what it is waiting for,
with exit status 75 (`EX_TEMPFAIL`, "try again later"), so a script can tell
"not finished" from "failed". The next run reads live state, finds the stages
already done, and carries on. `--wait <duration>` opts into polling instead.
Founder direction: the operator should not have to leave the toolkit running
while Garage copies data over a slow home upload.

Everything a resumed run needs is on the hosts, never on the operator's
machine (below), so any machine can resume, at any time.

### 6. Consistency mode is a configuration key

`storage.garage.consistency`: `consistent` (the default), `degraded` or
`dangerous`, rendered as `consistency_mode` in `garage.toml`. Validation warns
on `dangerous` and on `degraded` at RF 2, where it changes nothing
(`configuration.md:242-244`). Changing it is an ordinary `apply`: Garage reads
it at start and does not record it in the layout.

### 7. No node-stop test yet

Stopping a node to prove failover makes every upload fail for its duration at
RF 2 `consistent`, and at RF 2 `dangerous` it creates exactly the single-copy
window decision 1 accepts only as a rare event. It is deferred to the RF 3
work, where one node down costs nothing. The smoke test reads through every
node instead, which proves each could serve.

## The operator's flow

The first deployment's target, from RF 1 on `home-a` today:

```yaml
sites:
  vm:     { roles: [gateway, witness], address: 10.44.0.1, ... }
  home-a: { roles: [data, apps], address: 10.44.0.2, ... }
  home-b: { roles: [data], address: 10.44.0.3, ... }
storage:
  media_hostname: media.example.org
  garage:
    sites: [home-a, home-b]     # preference order, see Rendering
    replication: 2
    consistency: dangerous
    capacity: 100G
```

```
paisans apply --site home-b       # writes garage.toml, starts an isolated node
paisans storage add               # dry run: every stage, read from live state
paisans storage add --execute --change-replication
paisans storage add --execute     # again, whenever: resumes, or reports done
paisans apply --site vm           # only if the plan says the media routes are owed
```

`apply --site home-a` is refused while its Garage runs at a different factor
(below); `storage add` is what changes that file.

Growing later to option D, with a host `store` that has good upload:

```
paisans apply --site store                          # appended to the end of the list
paisans storage add --execute --change-replication  # RF 2 to 3, join, sync
# when it reports done: move store to the front of storage.garage.sites
paisans apply --site vm
paisans apply --site home-a
```

## Stages and gates

| Stage | Work | Gate |
|---|---|---|
| 1. Nodes | Read every Garage site: `garage node id -q`, the deployed `garage.toml`'s `replication_factor`, `layout show`, `status`, and the reset's markers (below) | every node answers; no site without a role is listed ahead of one with a role; a factor that differs plans stage 3, and is refused without `--change-replication` |
| 2. Settle (only before a reset) | Nothing to run | on every node already in the layout, `layout history` reports a single live version and `garage stats` reports "resync queue length: 0" and "blocks with resync errors: 0". A **waiting** gate |
| 3. Reset (only with `--change-replication`) | Record each bucket's object count on the anchor's host; stop Garage on every node; move `meta/cluster_layout` to `meta/cluster_layout.rf<old>`; write the new `garage.toml` on every node; start them | every node answers, at layout version 0 |
| 4. Connect | From the anchor, `garage node connect <id>@<mesh address>:3901` for every node it does not see | `garage status` on every node lists every configured node as healthy and none as failed |
| 5. Layout | On the anchor, `layout assign -z <site> -c <capacity> <id>` for every node with no role, then one `layout apply --version <current+1>` | `layout show` on every node reports the same version and a role for every configured site, in the zone named after it |
| 6. Sync | Nothing to run: Garage moves the data | `layout history` reports a single live version on every node, and then every node's `garage stats` reports an empty resync queue and no errors. A **waiting** gate |
| 7. Provision | What `storage init` would do, planned against the anchor | the plan has nothing left, and when a reset ran, every bucket's object count matches the one recorded |
| 8. Media routes | Re-render the gateway's `media.caddy`, validate and reload Caddy if it differs | the gateway's live file matches the render |
| 9. Smoke | Write a probe object through the first listed node, read it back through every node's S3 API and web endpoint and through the media hostname, then delete it | every read returns the probe's content |

The **anchor** is the first site in `storage.garage.sites` that holds a role
in the live layout, or, on a cluster with no layout, the first listed.

Stage 2 exists for `dangerous`: until the nodes have caught up, an upload may
exist on one node only, and a reset must not start with anything single copy
in flight.

Why the resync queue is read only after `layout history` is stable: a block is
queued for resync when its reference reaches a node (`block/manager.rs:453-476`),
so an empty queue before the tables have synced proves nothing.

### Resuming

Each stage plans only what live state still lacks, and every gate is checked
again on every run, as in `site add`. The state lives on the hosts:

| What a run finds | What it concludes |
|---|---|
| a deployed factor that differs from the configuration | the reset has not finished: it is planned, from stage 2 if no node has been stopped yet |
| `meta/cluster_layout.rf<old>` on a node | that node's layout was already set aside; it is not moved again |
| the object counts file on the anchor's host | they were recorded before the stop; they are not recorded again, and stage 7 compares against them, then removes the file |
| every factor matching and the layout complete | stages 2 and 3 have nothing to do |

The reset itself is about a minute of work with nothing to wait on, so it runs
in one go. Interrupted halfway, the next run stops any node that is still
running, sets aside any layout not yet set aside, writes any `garage.toml` not
yet written, and starts them all. Stopping every node before rewriting any is
what keeps a node at the old factor from meeting one at the new: a node that
sees a peer with a higher factor exits (`system.rs:583-587`).

Without `--wait`, stages 2 and 6 read once. With it, they poll every ten
seconds until the duration runs out. Table sync runs on every layout change and
every ten minutes after (`sync.rs:31`), and Garage's repair docs place block
repair "a few hours after `garage layout apply`" on large clusters
(`doc/book/operations/durability-repairs.md:49-60`).

### The probe

The probe is written into the first bucket that is served publicly (Eg:
`<app>-uploads`), under `paisans-probe/<random>`, with that app's own key, by
`curl --aws-sigv4` run on the anchor's host against the first listed node's
mesh address. The key travels to `curl` as a config on stdin, never in
`argv`, the rule `apply.Transport.RunInput` exists for. The web endpoint is
read with the bucket's internal vhost; the media hostname is read from the
gateway's host, resolved to its own Caddy. A deployment with no public bucket
skips the probe and says so.

### `apply` and the factor

`apply` refuses to replace a `garage.toml` whose deployed `replication_factor`
differs from the rendered one, and names `storage add --change-replication`.
Without this, editing `replication` and running `apply` restarts Garage into
the refusal quoted above, and media stays down until somebody reads the
container log. Refuse, not warn: the outcome is certain.

`storage init` keeps assigning the layout when its site is the only Garage
site. With more than one it plans keys, buckets, grants and website access
only, and leaves the layout to `storage add`; it used to assign a node alone,
which with a factor above one is refused by `layout apply` and with a factor of
one starts a second, separate cluster.

## Rendering

### `garage.toml`

* `replication_factor` and `consistency_mode` from the configuration, the
  mode rendered even at its default so the setting is visible on the host.
* `rpc_public_addr` stays the site's mesh address, which every peer can dial.
* **No `bootstrap_peers`.** Its entries are `<node public key>@<address>`
  (`config.rs:98-100`), and the key is generated by the node on first start,
  so the configuration cannot know it before the node exists. Generating node
  keys into the secrets would let it, at the price of a new secret per site.
  It buys nothing the persisted `peer_list` does not already give once stage 3
  has connected the node once.

### `S3_ENDPOINT`

Every app, on every site, writes through the **first site in
`storage.garage.sites`**. Founder decision: the list is an explicit preference
order, and both the media routes and the apps follow it.

The reason is home upload bandwidth. A Garage node that receives a write sends
the copies to every other replica at once (`rpc_helper.rs:414-431`). An app on
a home site writing to its own node would push every copy out over that home's
upload, the slow direction of a residential line. Writing through a node with
good upload, Eg: a datacenter host listed first, sends one copy out of the
home, and that node fans the rest out over the homes' download links.

Rejected: the site's own node first, then the list. It keeps uploads working
when the first listed node is down, at the cost of every write leaving the home
once per remote replica. While the first node is down, uploads fail until the
operator reorders the list and runs `apply`; reads keep working through the
gateway's failover.

Rejected: measuring which node is nearest or fastest. A render must not change
with network conditions, and Caddy 2.11.6 has no latency-aware selection policy
to hand the choice to (below).

### Gateway media routes

Every Garage node is an upstream, in `storage.garage.sites` order, for both
the web endpoint (3902) and the S3 API (3900):

```
reverse_proxy 10.44.0.2:3902 10.44.0.3:3902 10.44.0.1:3902 {
	lb_policy first
	lb_try_duration 5s
	fail_duration 10s
	unhealthy_status 503
	header_up Host talk-uploads.web.garage.internal
}
```

* `lb_policy first` and `fail_duration`, as the app routes already do: the
  first listed node serves while it is up.
* `lb_try_duration` retries a request whose connection failed on the next
  upstream, so the request that discovers a dead node does not fail with it.
* `unhealthy_status 503` takes a node that has lost quorum out of rotation.
* **No active health check on `/health`.** Its answer uses the consistent
  write quorum (`system.rs:432-434`), so on RF 2 with two nodes it would mark
  the surviving node unhealthy while it serves every read, and take media down
  in exactly the outage the check exists for.
* **No "fastest node" policy.** Caddy 2.11.6 registers twelve selection
  policies (`random`, `random_choose`, `least_conn`, `round_robin`,
  `weighted_round_robin`, `first`, `ip_hash`, `client_ip_hash`, `uri_hash`,
  `query`, `header`, `cookie`;
  `modules/caddyhttp/reverseproxy/selectionpolicies.go` at `v2.11.6`), and
  none measures latency or throughput. `least_conn` breaks ties at random, so
  on a quiet day a slow home would serve its share of reads. Founder decision:
  explicit order, `first`.
* **Reads stay on the first node.** Garage serves a block from the receiving
  node's own copy before asking any other (`block/manager.rs:288-339`,
  `rpc_helper.rs:566-600`), so a first node holding a full copy sends no read
  traffic through a home's upload. Object metadata at RF 3 `consistent` still
  needs a second replica's answer (read quorum 2), which is a small request to
  the lowest ping peer, not the object.
* **Order is preference order, not sorted.** A node that has not yet been
  given a role answers every bucket as missing. `apply --site vm` may render
  the new node into the route before `storage add` has joined it; listing it
  after the existing node keeps it from serving until the first fails, and
  stage 7 renders the route again once the join is done. So **a new site joins
  at the end of the list**, and moves up only after `storage add` has passed;
  then `apply` on the gateway and the apps sites puts the new order into
  effect. `storage add` refuses a plan in which a site with no role is listed
  ahead of one that has a role, and says to append it instead. The same rule
  protects `S3_ENDPOINT`, which follows the first listed site.

## Validation

Refuse:

* `replication` greater than the number of Garage sites (exists).
* A Garage site that is not a declared site (exists, via undeclared sites).

* `consistency` that is not `consistent`, `degraded` or `dangerous`. Garage
  refuses to start on any other value (`replication_mode.rs:10-26`).

Warn:

* RF 2 on two sites at `consistent`: "uploads stop while either site is down;
  reads continue. A third Garage site at replication 3 keeps both."
* RF 2 on three or more sites: uploads to the partitions the down node holds
  fail while any one is down.
* RF 1 on more than one site: each object lives on one site only.
* `dangerous`: an upload is confirmed once one copy exists.
* `degraded` at RF 2: it is identical to `consistent` there.

## Testing

* Unit tests with the fake transport for every stage's planning and gate:
  a fresh join, a resumed join at each stage, the factor change with and
  without the flag, the object count mismatch, the sync timeout, the probe
  failing on one node, and the `apply` refusal.
* Render tests for `S3_ENDPOINT` per site, the media snippet's upstream list
  and order, and `garage.toml`.
* The Docker integration suite (`garage_integration`) gains a multi-node test
  that drives the real planner against `dxflrs/garage:v1.0.1` containers: an
  RF 1 node with a key, a bucket and an object, a second node, a reset to RF 2
  `dangerous`, the join, the sync gate, provisioning found present, and the
  object read back through both nodes.
* The real test is a running deployment, run stage by stage with the
  founder approving each `--execute`.

## Rejected alternatives, in one place

* **`bootstrap_peers`**: needs node keys known before the node exists.
* **Active `/health` checks for media**: wrong answer on two-node RF 2.
* **Per-site `storage add --site`**: cannot express an RF 3 layout or a factor
  change.
* **Acting by default with `--status` to look**: every other command is dry
  run by default.
* **Waiting in the foreground by default**: a sync over a home upload can take
  hours; the state is on the hosts, so a later run resumes.
* **The site's own Garage node as `S3_ENDPOINT`**: every write would leave a
  home once per remote replica, over its slow upload.
* **A latency-aware media policy**: Caddy 2.11.6 has none, and a render must
  not depend on network conditions.
* **A storage stage inside `site add`**: leaves an existing site and the
  gateway with no command.
* **Rebuilding Garage empty to change the factor**: loses data and contradicts
  the growth premise.
* **A new cluster plus an S3 copy**: undocumented upstream, doubles disk.
* **Sorted upstreams**: let a node with no role answer first.

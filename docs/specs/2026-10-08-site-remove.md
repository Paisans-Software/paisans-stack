# `paisans site remove`

Date: 2026-10-08. Status: approved by the founder in session. The reverse of
`site add` (docs/specs/2026-10-07-site-add.md): take one site out of a running
deployment, move its data elsewhere first, take it out of the cluster, clean
its host, and take it out of `paisans.yaml`. On the host it leaves everything
that is not provably this deployment's alone.

## Scope

In:

* Any site whose removal leaves a configuration that validates: a data site, a
  witness, an apps site, a storage site, a monitor, or a gateway while another
  gateway exists.
* The site's Patroni leadership handed to a synchronous standby first, its
  Garage node taken out of the layout with the data moved off it, its etcd
  member removed, and every remaining site's mesh, HAProxy and gateway routes
  brought up to date.
* One data site out of two data sites and a witness, which shrinks etcd from
  three voters to one: the witness leaves etcd and loses the witness role in
  the same removal (below, *Two data sites and a witness*).
* Every remaining replica's `patroni.env` applied, one replica at a time.
* The host cleaned of this deployment, with `--host-gone` for a host that is
  never coming back, and the gateway's Caddy handed over to the host's owner
  when somebody else's sites rely on it.

Out, each for a stated reason:

* **Three data sites down to two.** Two data sites would stay as two etcd
  voters, which validation refuses (two voters are worse than one), and
  neither can leave etcd while it holds data. Add a witness first, so three
  voters remain.
* **Moving an app.** A site an app is pinned to is refused; moving the app is
  its own operation (README, *Moving a pinned app*).
* **Editing the secrets file or DNS.** Like `app remove`, the command reports
  both and changes neither.

## The operator's flow

The site stays declared while the command runs: its ssh section is how its
host is reached, and its roles say what it holds. Everything else is computed
from the configuration with the site taken out in memory, the end state.

```
paisans site remove home-b                 # dry run: refusals, then every stage
paisans site remove home-b --execute
paisans site remove home-b --execute --delete-data   # asks for "home-b" at a terminal
paisans site remove home-b --execute --host-gone     # the host is never coming back
```

The last stage edits `paisans.yaml`, so a finished removal leaves nothing to
run again. A run stopped part way resumes: every stage is built from live
state, so a re-run plans only what is still to do and checks every gate again.

## Refusals

Checked before anything changes, each with the evidence and what to do.

| Refused | Because | What to do |
|---|---|---|
| the only gateway | nothing else would answer 80 and 443 | give another site the gateway role and apply it first |
| the only apps site while an app is placed `cluster` | the app would run nowhere | give another site the apps role first |
| a site an app is pinned to | the app would run nowhere | move the app first |
| the only data site | the database would have no home | add another data site first (`site add`) |
| an end state that does not validate | Eg: two data sites left as two etcd voters | the message is validate's own; for two voters, add a witness first |
| two data sites and a witness, and witness is the witness site's only role, with no app pinned to it | it would be left with no role, which the configuration allows only for a site an app is pinned to | give it another role and apply it first, or `site remove` it once this removal is done |
| two data sites and a witness, and either the remaining data site's etcd or the witness's is unhealthy | going from two voters to one is a change both must commit | fix the member, run again |
| removing the site leaves etcd without a quorum of healthy members, or any other member is unhealthy | a membership change on an unhealthy cluster can stop it | fix the member, run again |
| any other Patroni member not running or streaming | the switchover and the stop need a healthy cluster | fix it, run again |
| the site holds the leader and no healthy Sync Standby exists | only a synchronous standby takes over without losing acknowledged writes | turn on `cluster.synchronous`, or switch over by hand |
| the first site in `storage.garage.sites` while apps are declared | it is the node every app writes objects through, and they would lose it the moment it stops | move another Garage site to the front and apply every site running an app |
| a Garage site whose removal leaves fewer nodes than `storage.garage.replication`, or another Garage node unhealthy | the data on it would have nowhere to go | add a node or lower the factor first (`storage add`) |
| the host does not answer over ssh | its cleaning cannot be planned | fix ssh, or `--host-gone` |
| `--delete-data` without a terminal | member data is deleted for good | run it from an interactive shell; there is no flag that answers |
| `--delete-data` with `--host-gone` | nothing can be deleted on a host that is not reached | drop one of them |

## Stages and gates

Every stage ends at a gate, and a failed gate stops the command with the
evidence. Nothing after it runs.

| Stage | Work | Gate |
|---|---|---|
| 1. Data out of the site | The site's Patroni leadership switched over to the Sync Standby; synchronous mode turned off when one cluster site remains; Patroni stopped on the site and its member key deleted from etcd; the site's Garage node removed from the layout and the layout applied | the new leader leads; `patronictl list` no longer lists the site; a Sync Standby exists when the end state wants one; no layout row for the site, and Garage settled (one live layout version, empty resync queue) on every remaining node, within a bounded wait |
| 2. Out of the cluster | `etcdctl member remove`, then the site's etcd stopped, since a removed member cannot rejoin and would restart in a loop; with two data sites and a witness, the witness's member too (below); on every remaining site, the files whose render changes with the site taken out, by scoped applies: the mesh file (`wg syncconf`), HAProxy (restarted with its database apps stopped around it, as `site add` does) and the gateway's routes (validated, then Caddy reloaded); then every remaining replica's `patroni.env`, one replica at a time (below) | etcd's voters are exactly the end state's and every member is healthy; HAProxy lists exactly the end state's cluster sites with the leader `UP`; no remaining site has the removed site as a WireGuard peer; each replica streams again before the next is touched |
| 3. Clean the host | Below. Skipped with `--host-gone`. When the site holds the active Pocket ID instance, the plan says that once this stage stops it, sign in is unavailable for a few seconds while a standby on another site takes over, up to about 90 seconds if the instance does not stop cleanly. The stop is clean, so the standby takes over at its next retry; 90 seconds is how long a registration takes to age when an instance dies (`docs/specs/2026-10-07-pocket-id-standby.md`) | nothing of this deployment's is left on the host but what the plan said it keeps |
| 4. Config | The site taken out of `paisans.yaml`, comments kept; with two data sites and a witness, in the same write, the witness out of `etcd.members` and the `witness` role off its site | the file loads without the site, and without the witness in etcd |

### Two data sites and a witness

Taking one data site out of two data sites and a witness would leave two etcd
voters. One voter is strictly better than two, and with one data site the
witness has no tie left to break. So when the end state's etcd members would be
exactly the one remaining data site and a site that is a member only because it
is the witness, the end state also takes that site out of `etcd.members` and
takes the `witness` role off it, keeping its other roles. That end state must
validate like any other. A witness whose only role is the witness would be left
with none, which the configuration allows only for a site an app is pinned to;
otherwise the removal is refused, with advice to give it another role first or
to `site remove` it afterwards.

Stage 1 is unchanged: the switchover when the leaving site leads, then
`synchronous_mode` off, since one data site remains. In stage 2 the leaving
data site's member goes first and its etcd stops, as for any removal. A check
follows: etcd's voters are exactly the remaining data site and the witness,
both healthy. Then the witness's member is removed, which leaves the remaining
data site the one voter with a quorum of one, and the witness's etcd container
is stopped. It is found by this deployment's label, the infrastructure stack's
compose project and the `etcd` service, so nothing else on the host can match,
and not by the compose file, which no longer declares the service once it is
written. The witness's `infra/compose.yaml`, rendered without etcd, is written
by the same scoped apply as its mesh file and routes. Its `etcd-initial`, no
longer rendered, is reported for a whole `apply`, which marks it left over.
The gate is then the ordinary one: the voters are exactly the remaining data
site, and it is healthy.

Going from two voters to one is a membership change both voters must commit,
so it is safe only when both are healthy, and the removal is refused before
anything changes otherwise. The stage is planned from live state, so a run
stopped after the first member removal recognises that only the witness's is
left, and one stopped after both plans neither.

### `patroni.env` on the sites that stay

A remaining data site's `patroni.env` lists the etcd hosts (`ETCD3_HOSTS`), so
it changes with the membership. Patroni reads that list only to reach etcd at
start: with `use_proxies` off, the default and what Spilo renders from
`ETCD3_HOSTS`, it asks etcd for the cluster's members and uses those from then
on, refreshing them as it runs (Patroni v4.1.0, the version Spilo 4.1-p2 pins,
`patroni/dcs/etcd.py`, `AbstractEtcdClientWithFailover._load_machines_cache`
and `_refresh_machines_cache`; `patroni/dcs/etcd3.py`,
`Etcd3Client._get_members`). An out of date list is harmless while Patroni
runs; it matters at its next start.

So each remaining replica's file is applied in stage 2, one replica at a time:
a scoped apply of `patroni.env`, then `up -d --no-deps --force-recreate
patroni`. Recreating a replica's Patroni is a replica restart, which HAProxy,
routing only to the primary, does not notice. Before the next replica is
touched, the recreated container must run with the new `ETCD3_HOSTS`, and its
own Patroni, asked from inside the container through its REST API (`GET
/patroni` on the site's restapi listen address, with python3, which Spilo runs
Patroni on), must answer `state` `running` and `replication_state`
`streaming` (Patroni v4.1.0, `patroni/api.py`, `do_GET_patroni` and
`get_postgresql_status`). `patronictl list` is not asked for this, because it
reads the member key the old process wrote, which can still say `streaming`
until its TTL runs out; it is read only for a `Sync Standby` when the end state
keeps one. A replica that leads by the time its turn comes is left
alone, since recreating it would be a failover. The step is planned from what
the replica's file and its running container hold, so a re-run recreates only
a replica still running with other hosts.

The leader's `patroni.env` is left, as `site add` leaves it: recreating its
Patroni would be a failover. The report says it is picked up at the leader's
next restart, `paisans apply --site <site>` when a failover is acceptable, and
why waiting is safe. `apply.ReplicaEnv` is shared with `site add`. Any other
file whose render changes is noted with `paisans apply --site <site>`. etcd's
`--initial-cluster` flags are not among them: every member's are rendered from
what its host records, as `site add` does, and they are inert once its data
directory exists.

## Cleaning the host

Each removal is proven before it happens; anything else found is reported as
kept, with why.

| What | Proven by | Removed |
|---|---|---|
| containers and networks | the deployment label carrying this id (Docker's own filter) | stopped and removed; with `--delete-data` the containers' anonymous volumes too |
| named volumes | the same label | only with `--delete-data` |
| rendered files | an entry in this deployment's manifest whose hash the file still has | deleted; an edited file is kept and named; then the manifest |
| `wg-quick@psns-<token>` and `/etc/wireguard/psns-<token>.conf` | the token in the name, and the file hashing to its manifest entry | the unit disabled and stopped, the file deleted with the other rendered files |
| units and drop-ins | the name `paisans-<token>-*` under `/etc/systemd/system`, and drop-in directories there | disabled, stopped, deleted, and systemd reloaded |
| ufw rules | a comment starting with exactly `paisans-<token>:` | deleted, except the SSH allow, which `host prepare` never removes either: with incoming denied, deleting it cuts the next connection |
| authorized keys | a fingerprint in this deployment's record, `/etc/paisans/authorized_keys.<user>.paisans-<token>.owned` | the key's plain lines deleted, unless another deployment's record lists the same fingerprint (both added one line, so it stays) or deleting them would leave the user with no key at all; then the record |
| the deployment root, `/srv/paisans/<token>` | its path | empty directories removed; with `--delete-data`, all of it, unless an edited file is in it |
| the registry entry | this id's line in `/var/lib/paisans/registry.json` | deleted under the same flock as a claim |

The authorized keys go last, after the registry entry, because they may be
what the command reaches the host with.

### The gateway's Caddy, when somebody else relies on it

A gateway's Caddy also serves the host owner's sites in `/srv/caddy.d`
(README, *The gateway host's own sites live in `/srv/caddy.d`*). Removing it
would take those sites down. When `internal/ownership` reports a `*.caddy`
file there, the Caddy is handed over:

1. `/srv/caddy/` is written: `compose.yaml` with project `caddy`, no
   deployment label, the same image and host networking, mounting
   `/srv/caddy.d` and a `Caddyfile` with only the global options ACME needs,
   the snippets the owner's files may import (`upstream_unavailable`,
   `upstream_failover`, `upstream_single`) and `import /etc/caddy.d/*.caddy`.
   The DNS provider's token is copied on the host into `/srv/caddy/caddy.env`,
   because the owner's certificates were issued and renew through DNS-01; the
   report says plainly that it is a credential left for the owner.
2. The configuration is validated in a one-off container before anything
   stops.
3. The paisans Caddy is stopped, its `data` and `config` directories moved to
   `/srv/caddy/`, so certificates survive, and the new Caddy started.
4. Gate: the new Caddy runs and `caddy validate` passes inside it. On failure
   it is stopped, the directories moved back and the paisans Caddy started
   again.
5. `/srv/caddy/HANDED-OVER` records the deployment id, domain, site and time.

From then on paisans never touches `/srv/caddy`. A run that finds the marker
with this id treats the hand-over as done; one that finds `/srv/caddy` holding
anything the hand-over did not write, or a compose project named `caddy`
already on the host, refuses. With no foreign user, the Caddy goes like every
other container.

### What `--host-gone` leaves

Stages 1, 2 and 4 run; stage 3 is skipped and the report lists what a host
that comes back still holds: this deployment's containers, files under
`/srv/paisans/<token>`, its units, its ufw rules, its keys and record, its
registry entry. Nothing it runs can reach the cluster again, because no
remaining site has it as a WireGuard peer and its etcd member is gone.

## What it reports at the end

The site's secrets in `secrets.enc.yaml`, which it never edits; the DNS records
pointing at the host's public address, which `paisans dns prune` deletes; the
leader's `patroni.env` and the files a scoped apply does not move; everything
kept on the host, with why.

## Testing

Unit tests against a fake world in maps (etcd, Patroni, Garage's layout, Docker
objects, files, units, ufw rules and keys per host): every refusal, a whole
removal leaving nothing to do, resuming from each stage, `--host-gone`, foreign
containers, files and rules kept, an edited file kept, the gateway hand-over
and the plain removal, the registry's awk against its Go counterpart, and
`--delete-data` refused without a terminal. For two data sites and a witness:
the shrink end to end, a resume after the first member removal and after the
witness's, an unhealthy witness refused, a witness with no other role refused
and allowed with an app pinned to it, and three data sites to two still
refused. Replicas' `patroni.env` applied one at a time with the gate between
them, a replica that does not stream again stopping the next, a replica that
leads by then left alone, the leader's noted, and the Pocket ID note only when
the leaving site holds the active instance. The real test is the staging
deployment, stage by stage, with the founder approving each `--execute`.

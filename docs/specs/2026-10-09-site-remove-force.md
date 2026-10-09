# `paisans site remove --force`, and the hosts a deployment has used

Date: 2026-10-09. Status: draft, awaiting the founder's review.

Two gaps in `site remove` (docs/specs/2026-10-08-site-remove.md), both found
when the hosts of a deployment were lost and the hosts that survived had to be
cleaned:

1. **Cleaning a host is only reachable through a full removal.** A full
   removal checks the cluster first and refuses a site an app is pinned to, a
   site whose removal leaves a configuration that does not validate, and a
   cluster that is unhealthy. When the other hosts are gone, every one of those
   holds, and nothing cleans the host that is left.
2. **A site taken out of `paisans.yaml`, or moved to a new host, leaves its old
   host unknown.** The toolkit reaches a host only through a declared site's
   ssh section. Once the site is gone from the file, or its ssh section names
   the new host, nothing records that the old one still holds this deployment.

`--force` closes the first. A record of the hosts each site has been deployed
to closes the second. Cleaning also starts removing images, which it does not
do today, for a normal removal as much as a forced one.

## Scope

In:

* `site remove <site> --force`: stage 3 alone, the cleaning of one host, with
  none of the cluster's refusals and without editing `paisans.yaml`.
* A site that is declared, a site that is no longer declared, and a host a
  declared site has moved away from.
* `sites.<name>.hosts` in `secrets.enc.yaml`: every host the site has been
  prepared or applied on and not yet cleaned, written by `host prepare` and
  `apply`.
* A note from `validate`, `plan` and `apply` naming each such host that the
  configuration no longer explains, with the command that cleans it.
* Images used only by the containers that cleaning removes.

Out, each for a stated reason:

* **Rebuilding a site on a new host.** A site's identity (its WireGuard key,
  the database and Garage passwords, its sign-in clients) lives in
  `secrets.enc.yaml`, not on the host, so a rebuilt site keeps it and the
  surviving sites need no new keys. Bringing a site up on a new host is an
  `apply` once its ssh section and addresses name the new one; making that
  work for every role is its own piece of work.
* **Restoring data.** Out for now.
* **The sites that remain.** `--force` reaches one host. A site still declared
  elsewhere that has the cleaned host as a WireGuard peer, an etcd member or a
  Garage node keeps it until that site's next `apply`, or until a full
  `site remove` of the cleaned site. The report names those sites.
* **DNS and sign-in clients.** As for a full removal, reported, not changed:
  `paisans dns prune` deletes the records, and a client in a Pocket ID that no
  longer exists has nothing to delete it from.

## The operator's flow

```
paisans site remove monitor-a --force                        # dry run
paisans site remove monitor-a --force --execute
paisans site remove monitor-a --force --execute --delete-data
paisans site remove home-b --force --ssh admin@203.0.113.9   # a host home-b moved away from
paisans site remove home-b --force --ssh admin@203.0.113.9 --host-gone --execute
```

### Which host

In this order:

1. `--ssh user@host[:port]`, when given. For a declared site it must be the
   declared host or one of the site's recorded hosts; anything else is
   refused, so a typo cannot take a running site's place. For a site that is
   not declared, any host is accepted: cleaning removes only what the host
   proves is this deployment's, so a host that never held it plans nothing.
2. The site's declared ssh section, when the site is declared.
3. The site's one recorded host, when the site is not declared. With several
   recorded, the command lists them and asks for `--ssh`.

A site neither declared nor recorded is refused with: name its host with
`--ssh`. That is also how a host prepared before the record existed is
cleaned.

### What `--force` changes

* Stages 1 and 2 do not run, and none of their refusals are checked: not the
  only gateway, not a pinned app, not the end state validating, not etcd's,
  Patroni's or Garage's health.
* Stage 3 runs as it does today (*Cleaning the host* in the site-remove spec),
  plus images (below). What the host holds is read from the host: the
  deployment label, the manifest, the token in names, the ufw comments, the
  keys record and the registry entry. The site's roles, which decide whether
  the gateway's Caddy is handed over, come from its registry entry on the
  host, not from `paisans.yaml`, so they are right for an undeclared site and
  for a host the site has left.
* Stage 4 does not edit `paisans.yaml`. It removes the cleaned host from
  `sites.<name>.hosts`, and nothing else in `secrets.enc.yaml`.
* `--host-gone` with `--force` reaches no host and runs nothing but the record
  edit: the host is taken out of `sites.<name>.hosts`. It is how a destroyed
  host stops being reported.

### Cleaning the host a declared site runs on

The cluster still counts on that host: its etcd member, its Patroni replica,
its Garage node, its place in every other site's mesh. Cleaning it from under
the cluster is what `--force` is for, and it is never the default. So when the
host chosen is the declared site's current host, the dry run says what the
cluster loses, and `--execute` asks for the site's name at a terminal, the
same way `--delete-data` does. There is no flag that answers. The next `apply`
of the site deploys it again.

A recorded host the site has moved away from, and a site no longer declared,
are not asked about: nothing in the configuration counts on them.

## Refusals

| Refused | Because | What to do |
|---|---|---|
| `--ssh` naming a host that is neither a declared site's host nor one it recorded | it would clean a host the configuration says nothing about while the site runs elsewhere | check the address |
| an undeclared site with several recorded hosts and no `--ssh` | the command would have to guess | pass `--ssh` with one of the listed hosts |
| an undeclared, unrecorded site with no `--ssh` | there is no way to reach its host | pass `--ssh` |
| the host does not answer over ssh | its cleaning cannot be planned | fix ssh, or `--host-gone` |
| the site's current host, with `--execute` and no terminal | it takes a running site out from under its cluster | run it from an interactive shell |
| `--delete-data` without a terminal | member data is deleted for good | run it from an interactive shell |
| `--delete-data` with `--host-gone` | nothing can be deleted on a host that is not reached | drop one of them |

The host's registry entry is checked for this deployment's id before anything
is planned. A host without one is not refused: the plan holds whatever the
label, the token and the manifest still prove, which is nothing on a host this
deployment never reached, and the record entry is removed.

## Images

Cleaning reads the image of every container it is about to remove. After the
containers are gone, each of those images is removed unless a container still
on the host uses it, whoever's that container is. The image is removed by ID,
without `--force`, so Docker refuses an image in use rather than the toolkit
judging it; a refusal is reported as kept, with Docker's reason. Images this
deployment pulled but no container uses are not found this way. Those are left
by an `apply` that failed before its containers started, and `apply` prunes
what it supersedes already.

This holds for a full removal too: images are part of stage 3, not of
`--force`.

## The record of hosts

`sites.<name>.hosts` in `secrets.enc.yaml` is a list of destinations,
`user@host:port`, with the port always written:

```yaml
sites:
  home-b:
    wireguard_private_key: ...
    heartbeat_token: ...
    hosts:
      - admin@203.0.113.9:22
      - admin@198.51.100.4:22
```

* **Written** by `host prepare` and `apply --execute`, once the host answers and
  before anything on it changes, so a run that fails part way still leaves the
  host recorded. A destination already listed is not added again.
* **Removed** by a cleaning that finishes, forced or not, and by `--host-gone`.
  A full removal with `--host-gone` removes the site's declared host from it,
  since that host is the one it gave up on.
* **Not a secret, kept with the secrets**: it is the one file every admin
  shares that the toolkit already writes, and it is encrypted. It holds no
  credential.

The site's other secrets are not removed when its last host goes. A full
`site remove` leaves them for the same reason it leaves them today.

### What the toolkit reports from it

`validate`, `plan` and `apply` read the record and print a note, never a
refusal, for each host the configuration no longer explains:

* A recorded host of a site that is not declared:
  `monitor-a was deployed to admin@203.0.113.9:22 and is no longer declared:
  paisans site remove monitor-a --force --execute`.
* A recorded host of a declared site that is not its declared host:
  `home-b was deployed to admin@203.0.113.9:22 and now names
  admin@198.51.100.4:22: paisans site remove home-b --force --ssh
  admin@203.0.113.9:22 --execute, or --host-gone if that host no longer
  exists`.

## Where this leaves a lost deployment

A deployment whose data and gateway hosts were destroyed, and whose monitors
survived on hosts that also run other people's services:

1. The monitors are taken out: their sites and their uptime apps come out of
   `paisans.yaml`, and `paisans site remove <monitor> --force --execute
   --delete-data` runs once per monitor, from a terminal, with `--ssh` for a
   host prepared before the record existed. Only what is this
   deployment's goes; the owner's containers, images, Caddy and ufw rules stay.
2. The destroyed sites stay declared. Their ssh sections and addresses are
   changed to the new hosts, and `apply` deploys them there. Each old host is
   reported until `site remove <site> --force --ssh <old> --host-gone
   --execute` takes it off the record.
3. `paisans dns prune` deletes the records still pointing at addresses nobody
   uses.

## Testing

In `internal/siteremove`, against the existing fake world:

* `--force` on a site with an app pinned to it, on the only gateway, and with
  an unhealthy etcd: no refusal, stages 1 and 2 absent from the plan, stage 3
  identical to the unforced plan's for the same host.
* `--force` never writes `paisans.yaml`, and removes exactly the cleaned
  destination from `sites.<name>.hosts`.
* Host selection: `--ssh` outside the declared and recorded hosts refused;
  undeclared with one recorded host chosen; with two, refused with both
  listed; undeclared and unrecorded with `--ssh` accepted.
* The site's current host without a terminal refused at `--execute`; a former
  host and an undeclared site not asked.
* Roles from the host's registry entry: an undeclared gateway whose host has a
  `/srv/caddy.d/*.caddy` gets the Caddy hand-over.
* Images: an image used only by removed containers is removed; one also used
  by a foreign container is kept; Docker's refusal is reported as kept.
* `--host-gone --force` reaches no host and edits only the record.

In `internal/config`: `sites.<name>.hosts` round-trips through
`WriteSecrets`, and `Get`/`Set` refuse it as a dotted key, since it is a list
written only by the toolkit.

In `cmd/paisans`: `host prepare` and `apply --execute` record the destination
before their first change; a dry run records nothing; the two notes above
appear in `validate`, `plan` and `apply` output.

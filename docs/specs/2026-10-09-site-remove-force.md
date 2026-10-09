# `paisans site remove --force`, and orphaned secrets

Date: 2026-10-09. Status: approved by the founder in session.

Three gaps in removing things (docs/specs/2026-10-08-site-remove.md), found
when the hosts of a deployment were lost and the hosts that survived had to be
cleaned:

1. **Cleaning a host is only reachable through a full removal.** A full
   removal checks the cluster first and refuses a site an app is pinned to, a
   site whose removal leaves a configuration that does not validate, and a
   cluster that is unhealthy. When the other hosts are gone, every one of those
   holds, and nothing cleans the host that is left.
2. **Cleaning leaves images behind.** Stage 3 removes this deployment's
   containers but not the images only they ran.
3. **The secrets of what was removed stay, unnoticed.** A site or an app taken
   out of `paisans.yaml` leaves its WireGuard key, passwords and sign-in
   client in `secrets.enc.yaml`, and the only advice is to edit the file with
   sops by hand.

`site remove --force` closes the first, stage 3 removing images closes the
second, and a warning plus `paisans secrets prune` closes the third.

## Scope

In:

* `site remove <site> --force`: stage 3 alone, the cleaning of one host, with
  none of the cluster's refusals and without editing `paisans.yaml`.
* `--ssh user@host[:port]`, naming the host to clean: required for a site no
  longer declared, optional for one that is (Eg: the host it ran on before its
  address changed).
* Images used only by the containers that cleaning removes, for a full
  removal and a forced one alike.
* A warning from `init` and `apply` for each secret that names something the
  configuration no longer declares, and `paisans secrets prune`, which removes
  them.

Out, each for a stated reason:

* **Rebuilding a site on a new host.** A site's identity (its WireGuard key,
  the database and Garage passwords, its sign-in clients) lives in
  `secrets.enc.yaml`, not on the host, so a rebuilt site keeps it and the
  surviving sites need no new keys. Bringing a site up on a new host is an
  `apply` once its ssh section and addresses name the new one; making that
  work for every role is its own piece of work.
* **Restoring data.** Out for now.
* **Finding hosts on its own.** The toolkit reaches a host through a declared
  site's ssh section or the `--ssh` an operator types. It keeps no list of
  where sites used to run.
* **The sites that remain.** `--force` reaches one host. A site still declared
  elsewhere that has the cleaned host as a WireGuard peer, an etcd member or a
  Garage node keeps it until that site's next `apply`, or until a full
  `site remove` of the cleaned site.
* **DNS and Pocket ID.** Reported, not changed. `paisans dns prune` deletes a
  record only while its address is still declared, so a record of a site
  already taken out of `paisans.yaml` is deleted at the provider by hand.
  Pruning a sign-in client's secret
  does not delete the client at Pocket ID, and the prune says so.

## `site remove --force`

```
paisans site remove monitor-a --force --ssh admin@203.0.113.9           # dry run
paisans site remove monitor-a --force --ssh admin@203.0.113.9 --execute
paisans site remove monitor-a --force --ssh admin@203.0.113.9 --execute --delete-data
paisans site remove home-b --force --execute                             # its declared host
paisans site remove home-b --force --ssh admin@198.51.100.4 --execute    # a host it ran on before
```

### Which host

`--ssh user@host[:port]` when given, any host. Otherwise the declared site's
own ssh section. A site that is not declared and has no `--ssh` is refused:
name its host with `--ssh`.

Any host is accepted because nothing on it is touched unless the host proves
it is this deployment's (below). A host this deployment never used plans
nothing.

### What it removes

Stage 3 as it is today (*Cleaning the host* in the site-remove spec), plus
images. Every match is by this deployment's id, or the token taken from it:

| What | Proven by | Removed |
|---|---|---|
| containers and networks | the label `community.paisans.deployment=<id>` | yes |
| named volumes | the same label | only with `--delete-data` |
| images | run by the containers above, or named by a compose file the manifest proves, and used by no other container | yes, by ID; Docker refuses one in use |
| rendered files | an entry in this deployment's manifest whose hash the file still has | yes; an edited file is kept and named |
| `wg-quick@psns-<token>` and its config | the token in the name, and the file's hash | yes |
| units and drop-ins | `paisans-<token>-*` under `/etc/systemd/system` | yes |
| ufw rules | a comment starting `paisans-<token>:` | yes, except the SSH allow |
| the registry entry | the `<id>` key in `/var/lib/paisans/registry.json` | yes |
| authorized keys | this deployment's record of the keys it added | yes, unless another deployment's record lists them or they are the user's last |
| `/srv/paisans/<token>` | its path | empty directories; everything with `--delete-data` |

Everything else found is reported as kept, with why.

The site's ssh user, whose authorized keys are cleaned, is `--ssh`'s user or
the declared one. The site's roles, which decide whether a gateway's Caddy is
handed over to the host's owner, come from this deployment's registry entry on
the host, not from `paisans.yaml`, so they are right for an undeclared site
and for a host the site has left. With no registry entry, the declared roles
are used, and an undeclared site has none.

### What `--force` changes

* Stages 1 and 2 do not run, and none of their refusals are checked: not the
  only gateway, not a pinned app, not the end state validating, not etcd's,
  Patroni's or Garage's health. No other site is reached.
* Stage 3 runs.
* `paisans.yaml` is not edited, and nothing in `secrets.enc.yaml` is.

`--host-gone` has nothing to do with `--force`, which exists to reach a host,
and is refused with it.

### Cleaning the host a declared site runs on

The cluster still counts on that host: its etcd member, its Patroni replica,
its Garage node, its place in every other site's mesh. Cleaning it from under
the cluster is what `--force` is for, and it is never the default. So when the
host chosen is the declared site's own, the dry run says what the cluster
loses, and `--execute` asks for the site's name at a terminal, the same way
`--delete-data` does. There is no flag that answers. The next `apply` of the
site deploys it again.

Any other host is not asked about: nothing in the configuration counts on it.

### Refusals

| Refused | Because | What to do |
|---|---|---|
| an undeclared site with no `--ssh` | there is no way to reach its host | pass `--ssh` |
| `--ssh` not of the form `user@host[:port]` | the user is whose keys are cleaned | Eg: `admin@203.0.113.9` |
| `--ssh` without `--force` | a full removal reaches the site through its ssh section | drop `--ssh`, or add `--force` |
| `--force` with `--host-gone` | `--force` is cleaning one host, and `--host-gone` reaches none | drop one of them |
| the host does not answer over ssh | its cleaning cannot be planned | fix ssh |
| the site's own host, with `--execute` and no terminal | it takes a running site out from under its cluster | run it from an interactive shell |
| `--delete-data` without a terminal | member data is deleted for good | run it from an interactive shell |

## Images

Cleaning reads the image of every container it is about to remove, and every
image named by a compose file whose manifest entry and hash prove it is this
deployment's. The files find the images even when a run stopped after the
containers were removed and before their images were, or an `apply` pulled an
image no container started from: the files are deleted after the images. After
the containers are gone, each of those images is removed unless a container
still on the host uses it, running or stopped, whoever's that container is. The
image is removed by ID, without `--force`, so Docker refuses an image in use
rather than the toolkit judging it; a refusal is reported as kept, with
Docker's reason.

## Orphaned secrets

A secret is orphaned when it names something `paisans.yaml` does not declare:

| Secret | Orphaned when |
|---|---|
| `sites.<name>` | no site `<name>` is declared |
| `apps.<name>` | no app `<name>` is declared |
| `oidc_clients.<name>` | no app `<name>` is declared |
| `pocket_id_groups.<name>` | no Pocket ID app's `signup_default_groups` names `<name>` |

`init` and `apply` already read the secrets file. Both warn, never refuse,
once per orphan: `secrets: sites.monitor-a names a site paisans.yaml does not
declare; paisans secrets prune removes it`.

`paisans secrets prune` lists every orphan, by key and never by value, and
changes nothing. With `--execute` it removes them and writes the file,
encrypted to the same recipients. For an `oidc_clients` entry it says that the
client still exists at Pocket ID, if that Pocket ID still runs, and is removed
there by hand. A removed site's WireGuard key is safe to prune once no site's
mesh lists it, which is true after a full `site remove`, or after every
remaining site's next `apply`.

`site remove`'s closing report points at `paisans secrets prune` instead of
editing the file with sops.

## Where this leaves a lost deployment

A deployment whose data and gateway hosts were destroyed, and whose monitors
survived on hosts that also run other people's services:

1. The monitors are taken out: their sites and their uptime apps come out of
   `paisans.yaml`, and `paisans site remove <monitor> --force --ssh
   <its host> --execute --delete-data` runs once per monitor, from a terminal.
   Only what is this deployment's goes; the owner's containers, images, Caddy
   and ufw rules stay.
2. `paisans secrets prune --execute` removes the monitors' and their apps'
   secrets.
3. The destroyed sites stay declared. Their ssh sections and addresses are
   changed to the new hosts, and `apply` deploys them there.
4. DNS is changed at the provider by hand. `paisans dns` never updates a
   record, so each name still wanted that points at a lost address (the
   gateway's) is reported as a conflict; the operator changes it to the new
   address, and `paisans dns` then finds nothing to do. `paisans dns prune`
   deletes only a record pointing at an address `paisans.yaml` declares, so
   the records of the monitors' hostnames, whose addresses are no longer
   declared once the monitors are taken out, are deleted by hand too.

## Testing

In `internal/siteremove`, against the existing fake world:

* `--force` on a site with an app pinned to it and with an unhealthy etcd: no
  refusal, no other host reached, `paisans.yaml` unchanged, and the host
  stage identical to the unforced plan's for the same host.
* An undeclared site cleaned through a destination it was not declared with;
  a host with no registry entry plans nothing.
* Roles from the host's registry entry: an undeclared gateway whose host has a
  `/srv/caddy.d/*.caddy` gets the Caddy hand-over.
* The site's own host marked as such, and its plan saying what the cluster
  loses.
* Images: an image used only by removed containers is removed; one also used
  by a foreign container is kept; Docker's refusal is reported as kept.

In `internal/config`: `ParseDestination` for `user@host`, a port, an IPv6
host, and a refusal for an alias without a user.

In `internal/secretsgen` (beside the code that knows what secrets a
configuration needs): orphans found for each of the four kinds; nothing found
for a configuration and secrets that match.

In `cmd/paisans`: the refusals above that need no host; the site's own host
without a terminal refused at `--execute`; `secrets prune` changing nothing
without `--execute`, removing exactly the orphans with it, and naming the
Pocket ID client it leaves.

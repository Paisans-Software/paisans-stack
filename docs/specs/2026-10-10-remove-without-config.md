# Removing a deployment from a host without its `paisans.yaml`

Date: 2026-10-10. Status: approved by the founder in session.

`site remove --force` (docs/specs/2026-10-09-site-remove-force.md) cleans one
host of one deployment. It names the deployment by `id` in `paisans.yaml`, so
when that file is lost nothing can name it, and the deployment stays on every
host it reached. The id is still on each of those hosts, in the registry at
`/var/lib/paisans/registry.json`, with the site's name and roles.

Two commands close that:

* `paisans host deployments` lists what a host's registry holds, and what of a
  deployment is on the host without a registry entry.
* `paisans site remove --force --ssh <dest> --id <id or token>` cleans the host
  of the deployment the registry names, reading neither `paisans.yaml` nor the
  secrets file.

## Scope

In:

* `host deployments --ssh user@host[:port]`, read only, with no configuration.
* `site remove --force --ssh user@host[:port] --id <id or token>`, with no
  configuration: the host stage of the forced removal, planned from the
  registry entry.
* Usage, help and the README.

Out, each for a stated reason:

* **A deployment with no registry entry.** `host deployments` lists its
  directory and its compose projects, and `--id` does not remove them: the
  entry is what names the site and the roles, and is the proof the host was
  claimed by that id. They are removed by hand.
* **Every other host.** One host at a time, as with `--force`.
* **The secrets, DNS and Pocket ID.** Not read and not changed. The closing
  report says so for DNS only: it has no provider or token without a
  configuration, so nothing there can be deleted safely.

## `host deployments`

```
paisans host deployments --ssh admin@203.0.113.9
paisans host deployments --ssh admin@203.0.113.9:2222 --sudo=false
```

It reads the registry and runs one probe, and changes nothing. For each entry
it shows the id, the token, the domain, the site, the roles and the root.

The probe lists each directory under `/srv/paisans/` and, when Docker is
installed, the deployment label and compose project of every container that
carries the label. What no entry accounts for is listed under its own heading,
marked as having no registry entry:

| Listed | When |
|---|---|
| `/srv/paisans/<token>` | no entry has that token |
| compose project `<project>` | its containers' label holds an id no entry has |

A host with no registry and nothing else of paisans on it says so. `--sudo`
defaults to true, as for every command that reads root's files: the registry
is root's and 0600.

It never lists partly. When the registry cannot be read or does not parse,
when `/var/lib/paisans` or `/srv/paisans` exists and cannot be read (without
sudo, the registry's directory would otherwise read as holding no registry),
when `docker ps` fails, or when the probe's answer is cut short or has a line
it does not know, it fails with an error naming what failed and prints
nothing. The probe's last line is `end`, which is how a cut short answer is
told from a host holding nothing.

## `site remove --force --id`

```
paisans site remove --force --ssh admin@203.0.113.9 --id f2a9                # dry run
paisans site remove --force --ssh admin@203.0.113.9 --id f2a9 --execute      # asks for the site's name
paisans site remove --force --ssh admin@203.0.113.9 --id f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01 --execute --delete-data
```

### Which deployment

`--id` is the full id or its four hex digit token, matched against the keys
and tokens of the host's registry. Exactly one entry must match. The entry
must agree with its id: the key is a lowercase version 4 UUID, and the entry's
token and root are the ones the id gives. Everything that is removed is named
from the id, as for every other removal, so an entry that disagrees with it is
refused rather than trusted. So is an entry whose token or root another entry
holds too, even when `--id` is the full id: every name but the Docker label is
made from the token, and the two would share them.

### What it removes

The host stage of `site remove --force`, unchanged, against the host's entry:
the site is the entry's `site`, the roles its `roles`, and the ssh user, whose
authorized keys are cleaned, is `--ssh`'s. Every match is by the id, or the
token taken from it, exactly as in the forced removal's table: containers and
networks by label, images proven to be the deployment's and removed by ID
without `-f`, named volumes only with `--delete-data`, files the manifest
proves, `wg-quick@psns-<token>`, units, ufw rules, the authorized keys its
record lists, `/srv/paisans/<token>` (empty directories, or everything with
`--delete-data`), `/var/lib/paisans/deployed.<token>.json`, and the registry
entry, last but for the keys.

### How it reuses the forced removal

The host stage reads the configuration for three things: the id, the site's
ssh user and destination, and the site's roles, which the registry entry and
`--ssh` hold. So
`siteremove.BuildForcedByID` reads the registry, resolves `--id`, and builds a
configuration holding only the id, the entry's domain and one site with `--ssh`
as its destination and the entry's roles. The plan it makes is marked as having
no configuration, and the host stage is the same `buildHost` and `runHost` the
forced removal runs. The mark changes two things only:

* the Pocket ID note at the head of the stage is not made, since it reads the
  configuration's apps;
* the remains say DNS was not modified.

### A gateway

As with a configuration (docs/specs/2026-10-10-gateway-caddy-kept.md): while
`/srv/caddy.d` holds a site file, the Caddy container is kept and its
Caddyfile reduced to the owner's sites, built from the Caddyfile on the host,
so the result is the same with `--id` as with a configuration.

A deployment whose roles include no gateway, the first real use being a
monitor-only one, is unaffected.

### Remains

Besides what the host stage keeps, with why, one line each:

* **DNS:** on every `--id` run, dry run and `--execute` alike, and when the
  host holds nothing of the deployment, one warning:

  ```
  ! DNS records for this deployment, if any exist, were not modified
    They carry the comment "paisans-f2a9: created by paisans dns init"
  ```

  With no configuration there is no provider and no token, so nothing can be
  deleted safely. The detail line gives the comment to search for at the
  provider, and adds the address when `--ssh` named the host by one.

### Confirmation

Nothing says whether the deployment still runs elsewhere, or whether someone
still holds its `paisans.yaml`. So `--execute` always asks for the site's name,
as the entry records it, at a terminal, with `--delete-data` or without. No
flag answers it. The question names the deployment and the host, the owner's
sites in `/srv/caddy.d` a kept Caddy goes on serving on a gateway, and the
data when `--delete-data` deletes it, since `--execute` shows no plan.

## Refusals

All but the last two are refused before any host is reached.

| Refused | Because | What to do |
|---|---|---|
| `--id` without `--force` | `--id` names a deployment for cleaning one host | add `--force` |
| `--id` without `--ssh` | there is no configuration to reach a host through | pass `--ssh` |
| `--id` with `--config` or `--secrets` | `--id` reads neither; a configuration names its own id | drop one of them |
| `--id` with a site named | the registry entry names the site | drop the site |
| `--id` that is neither an id nor four hex digits | it can match no entry | Eg: `--id f2a9` |
| `--force` with `--host-gone` | as today | drop one of them |
| `--execute` without a terminal | it asks for the site's name | run it from an interactive shell |
| no entry matches, or several do | nothing, or not one thing, is named | the refusal lists every entry on the host |
| the entry disagrees with its id | its token or root is not the id's, its key is not an id, or another entry holds its token or root | look at the registry by hand |

## Testing

In `internal/registry`: `--id` resolved by full id and by token; no match and
an ambiguous match refused, each refusal listing the host's entries.

In `internal/siteremove`, against the existing fake world:

* a monitor-only deployment cleaned with no configuration, its host stage the
  same as the forced removal's with one;
* on a host carrying another deployment, nothing of that deployment's
  (container, unit, ufw rule, registry entry) is planned or touched;
* a gateway with sites in `/srv/caddy.d`: the same Caddy result as with a
  configuration;
* an entry whose token or root disagrees with its id refused, and a full id
  refused when another entry holds its token or root;
* the question `--execute` asks naming the sites a kept Caddy serves.

In `cmd/paisans`: the refusals that need no host, reaching none; `--id` with
`--config` refused; no match and an ambiguous match through a fake host;
`host deployments` listing entries and leftovers, and failing, with nothing
listed, for an unreadable or malformed registry, an unreadable directory,
`docker ps` failing, an answer cut short and a line it does not know.

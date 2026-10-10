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
* **A gateway's Caddy hand over.** It writes a Caddyfile with the snippets of
  the one the gateway was rendered with, which needs the configuration. See
  *A gateway* below.
* **Every other host.** One host at a time, as with `--force`.
* **The secrets, DNS and Pocket ID.** Not read and not changed; the closing
  report says so.

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

The host stage reads the configuration for four things: the id, the site's ssh
user and destination, the site's roles, and, on a gateway, the rendered
Caddyfile. The first three are in the registry entry and `--ssh`. So
`siteremove.BuildForcedByID` reads the registry, resolves `--id`, and builds a
configuration holding only the id, the entry's domain and one site with `--ssh`
as its destination and the entry's roles. The plan it makes is marked as having
no configuration, and the host stage is the same `buildHost` and `runHost` the
forced removal runs. The mark changes three things only:

* a gateway's hand over is not planned (below);
* the Pocket ID note at the head of the stage is not made, since it reads the
  configuration's apps;
* the remains say what was not read.

### A gateway

Without the configuration the hand over cannot render its Caddyfile, so it is
not planned. This deployment's Caddy is removed with its other containers, by
label, and nothing in `/srv/caddy.d` or `/srv/caddy` is touched. The remains
say the hand over was not done and list the `*.caddy` files in `/srv/caddy.d`:
each is a site the host's owner serves through the removed Caddy, which stops
being served until the owner runs a Caddy of their own for it. A host with no
such files says there was nothing to hand over. The certificates stay under
`/srv/paisans/<token>/infra/caddy/data` unless `--delete-data` deletes them.

A deployment whose roles include no gateway, the first real use being a
monitor-only one, is unaffected.

### Remains

Besides what the host stage keeps, with why, one line each:

* **Secrets:** not read; `sites.<site>` stays in any surviving secrets file
  until `paisans secrets prune`.
* **Other hosts:** not reached; each keeps its entry and its part of the
  deployment until this command runs there.
* **Pocket ID**, on a site with the `apps` role: not checked; if this host held
  the active instance, sign in stops until a standby takes over.
* **DNS:** the forced removal's line for an undeclared site, unchanged.
* **Caddy**, on a gateway: as above.

### Confirmation

Nothing says whether the deployment still runs elsewhere, or whether someone
still holds its `paisans.yaml`. So `--execute` always asks for the site's name,
as the entry records it, at a terminal, with `--delete-data` or without. No
flag answers it. The question names the deployment and the host, the owner's
sites in `/srv/caddy.d` that stop being served on a gateway, and the data when
`--delete-data` deletes it, since `--execute` shows no plan.

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
* a gateway: no hand over planned, and the remains name `/srv/caddy.d`'s files;
* an entry whose token or root disagrees with its id refused, and a full id
  refused when another entry holds its token or root;
* the question `--execute` asks naming a gateway's unserved sites.

In `cmd/paisans`: the refusals that need no host, reaching none; `--id` with
`--config` refused; no match and an ambiguous match through a fake host;
`host deployments` listing entries and leftovers, and failing, with nothing
listed, for an unreadable or malformed registry, an unreadable directory,
`docker ps` failing, an answer cut short and a line it does not know.

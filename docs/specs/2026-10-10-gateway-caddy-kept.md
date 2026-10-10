# A gateway's Caddy is kept while it serves the host's own sites

Date: 2026-10-10. Status: approved by the founder in session.

Cleaning a gateway's host (stage 3 of `site remove`, `site remove --force`,
and `site remove --force --id`) removes everything of the deployment's:
its configuration, its artifacts, its images, and its data with
`--delete-data`. One thing is not only the deployment's. The gateway's Caddy
also serves the site blocks the host's owner puts in `/srv/caddy.d` (README,
*The gateway host's own sites live in `/srv/caddy.d`*), and removing it would
take those sites offline. So while `/srv/caddy.d` holds a site file, the
Caddy container stays where it is, and its configuration is reduced to the
owner's sites.

## Scope

In: the host stage of every `site remove`, with a configuration and with
`--id`; the registry entry of a host whose Caddy is kept; `host deployments`.

Out:

* **Moving the owner's sites anywhere.** They stay in `/srv/caddy.d`, served
  by the same container. Moving them to a Caddy of their own is the owner's.
* **Forcing Caddy out while the owner's sites exist.** No flag does it. The
  owner moves or deletes the files in `/srv/caddy.d`, and the next run removes
  Caddy.

## When Caddy is kept

All three hold:

* the site has the gateway role (from the host's registry entry under
  `--force`, as before);
* `/srv/caddy.d` holds a `*.caddy` file: what `internal/ownership` reports as
  a foreign user of the gateway's Caddy (`Report.HostSites`, read from the
  host's inventory, so it needs no configuration);
* this deployment's Caddy container exists: the container labelled with this
  id, in compose project `paisans-<token>-infra`, service `caddy`.

Otherwise Caddy goes with everything else, as before. A Caddy container that
no longer exists serves nothing, so there is nothing to keep.

## What is kept

The Caddy container itself, unchanged: the same container, compose project,
image and restart policy. And what it needs to run, renew certificates and
start again after a reboot, all under `/srv/paisans/<token>/infra/`:

| Kept | Why |
|---|---|
| `compose.yaml` | the container's compose file |
| `caddy/Caddyfile` | reduced to the owner's sites (below) |
| `caddy/caddy.env` | the DNS provider's token, which renews the owner's certificates |
| `caddy/data`, `caddy/config` | the certificates and Caddy's own state |
| `caddy/snippets/` | a bind mount of the container: the directory stays, emptied of the deployment's files, so the container starts again |
| the ufw allows for 80 and 443 `host prepare` added (comments `paisans-<token>: the gateway, HTTP` and `HTTPS`) | ufw denies incoming by default, so without them nothing from outside reaches the owner's sites |

These are kept with `--delete-data` too, and the plan says so. Everything else
of the deployment goes as before: every other container, network, image and,
with `--delete-data`, named volume; every other rendered file; the rest of
`/srv/paisans/<token>`; the units, every other ufw rule, the mesh interface,
the keys and the deployment record. Caddy's image stays, because the kept container runs from
it.

The manifest stays too, rewritten to list exactly the kept files with their
hashes now. That is what proves them this deployment's when a later run
removes them.

## The reduced Caddyfile

Built from the Caddyfile on the host, not from `paisans.yaml`, so a run with a
configuration and a run with `--id` write the same file. It holds:

* the global options block, which keeps the ACME email and the DNS provider
  directive, so the owner's certificates still renew;
* the snippets a file in `/srv/caddy.d` may import: `upstream_unavailable`,
  `upstream_failover`, `upstream_single`;
* `import /etc/caddy.d/*.caddy`;

and no site block of the deployment's, so Caddy neither proxies to stacks that
are gone nor renews the deployment's certificates. A host Caddyfile without
the global block, one of the snippets or the import is refused, and nothing is
changed. Reducing a reduced file gives the same file, which is how a later run
knows the work is done.

## The order

Caddy is reduced first, before anything else of the deployment is touched:

1. The reduced file is written beside the snippets, in a directory the running
   container mounts, and validated inside the container (`caddy validate`).
2. The running Caddy is reloaded from that file (`caddy reload`), which is
   atomic: a reload that fails leaves the running configuration as it was.
3. Only then, on the host, the original `Caddyfile` is copied aside and the
   staged file copied over it in place, since it is a single file bind mount
   and a renamed file would not reach the container. It is read back and
   compared.
4. The staged file and the copy are deleted.

A failure in 1 or 2 deletes the staged file, reloads the original
`Caddyfile` after a failed reload, and stops with the error. A failure in 3
copies the original back from the copy on the host, reads it back, and
reloads it; if that fails too, the error says where the copy is. Either way the command stops before
removing anything else, so the deployment is exactly as it was and the owner's
sites never stopped being served. Once Caddy is reduced, the rest is the
cleaning it always was, and a run that stops part way resumes it.

Caddy must be running to be reduced; one that is not is refused, with nothing
changed: start it and run again.

A plan that removes a gateway's Caddy, because `/srv/caddy.d` held no site
file when it was made, checks again when it runs: if a site file is there now
and the Caddy still runs, it stops before removing anything, and the next run
keeps the Caddy.

## The registry entry

The entry stays, with one field more, `"kept": "caddy"`. A host whose entry
is gone gets one written, as a claim would write it, marked kept. It still holds what
the kept Caddy holds on the host: the gateway role, so no other deployment
takes ports 80 and 443 from under it, and the token and root, whose directory
still exists. The value names what is kept, so the entry says why it is still
there. A claim by the same id (an `apply` of the deployment again) writes the
entry without it.

`host deployments` shows such an entry as `caddy kept: serves /srv/caddy.d
sites`, with the files.

## Running it again

* **The owner's sites are still there:** nothing changes. The command reports
  that Caddy is kept and names the files.
* **`/srv/caddy.d` holds no site file now:** Caddy is not kept. The container
  goes, its image, the kept files (proven by the rewritten manifest), the
  manifest, `caddy/data` and `caddy/config` with `--delete-data` and as data
  left without it, and the registry entry.

## A kept Caddy another deployment meets

A kept Caddy holds ports 80 and 443. A sysadmin may also delete the registry,
or the entry, by hand and keep the Caddy running for their sites; the
container still carries the old deployment's label. Two places name it for
what it is.

**The host check.** When another deployment's host check finds a claim, a
gateway's 80 and 443 or any other, held by a container that is another paisans
deployment's Caddy (its compose service `caddy` in a project
`paisans-<token>-infra`) while `/srv/caddy.d` holds a site file, the conflict
line says so and what to do, in one line:

* with that deployment's entry in the registry: `container <name>: paisans
  deployment <token>'s Caddy, kept because it serves /srv/caddy.d sites. Move
  those sites to a Caddy of your own and remove this container, or run paisans
  site remove --force --ssh <host> --id <token> once they are gone`;
* with none: `container <name>: paisans deployment <token>'s Caddy, kept
  because it serves /srv/caddy.d sites; its registry entry was removed, so
  remove this container by hand once those sites have moved`.

The host check reads the registry for this, and a host without one has no
entry. A Caddy whose entry is there and not marked kept is a live gateway, and
a registry that cannot be read says nothing about any entry: both keep the
plain wording, as does any other container of another deployment. The host
check never fails on the registry.

**`host deployments`.** A container like that, whose deployment has no
registry entry, while `/srv/caddy.d` holds site files, is listed under *not in
the registry* as `<id> orphaned Caddy: serves /srv/caddy.d sites (a.caddy,
b.caddy); its registry entry was removed, so remove it by hand once those
sites have moved`, in place of its compose project's line. Every other
leftover is listed as before, and a kept Caddy whose entry is there is shown
with its entry.

## The plan and the report

The dry run lists the reduction as the stage's first step, naming the owner's
files, and each kept file. The remains say Caddy is kept, why, and that a run
once the sites have moved removes it. `--id`'s question at `--execute` says
Caddy is kept for those sites.

## Testing

In `internal/siteremove`, against the fake world:

* a gateway with an empty `/srv/caddy.d` removed completely;
* a gateway with a site there: the container kept; the reduced Caddyfile
  holding the global block, the snippets and the import and no site block;
  validation before the swap and the reload; Caddy's files kept with
  `--delete-data`; the entry marked kept; the record gone;
* validation failing: the original Caddyfile in place, an error, and nothing
  else of the deployment removed;
* a second run with the sites still there changing nothing, and one after
  they are gone removing everything and the entry;
* the `--id` path and the configuration path giving the same Caddyfile.

In `internal/registry`: the entry with `kept` written and read.

In `internal/hostcheck`: the conflict line for another deployment's kept
Caddy, with its registry entry and without; another deployment's container
that is not its Caddy keeping the plain wording.

In `cmd/paisans`: `host deployments` showing a kept Caddy, an orphaned one,
and a kept Caddy whose entry exists shown with it rather than as orphaned.

# The deployment record on each gateway

Date: 2026-10-09. Status: draft, for the founder's review.

`paisans secrets prune` (docs/specs/2026-10-09-site-remove-force.md) removes
the secrets of every site, app and Pocket ID group `paisans.yaml` does not
declare. That is right after a removal and wrong while the file is being
edited: a site commented out, an app renamed, a bad merge, and `prune
--execute` deletes a running site's WireGuard key or a running app's database
password for good.

The yaml alone cannot tell "removed" from "missing". A record of what is
deployed can. Each gateway keeps one, `apply` adds to it, the removal commands
take from it, and prune removes only what the yaml does not declare **and** no
record lists.

## Scope

In:

* The record: a file on each gateway listing the deployment's sites, apps and
  Pocket ID groups.
* Its writers: `apply` adds; `site remove`, `site remove --force` and `app
  remove` take out.
* Its readers: `secrets prune`, which keeps what a record lists, and `init`
  and `apply`, which warn when the yaml has dropped something still deployed.
* `secrets prune --without-record`, for a deployment with no gateway to read.

Out:

* **Making the yaml agree with the record.** The record is never used to
  restore a declaration, and nothing refuses an `apply` over a difference: the
  warning says what to do.
* **Anything but secrets.** The record guards `secrets prune`. It does not
  gate what `apply`, `site remove` or `app remove` do on hosts.

## The record

`/var/lib/paisans/deployed.<token>.json` on every gateway the yaml declares,
beside `registry.json`, root's and 0600, written under the registry's lock and
replaced atomically, as the registry is:

```json
{"version": 1, "revision": 7, "updated_at": "2026-10-09T18:40:00Z", "sites": ["home-a", "home-b", "vm"], "apps": ["auth", "talk"], "pocket_id_groups": ["members"]}
```

Names only, sorted. It holds no secret and no address. `revision` orders the
records: every change raises it by one. `updated_at` is when the change was
made, by the writer's clock, for a person reading the file; nothing compares
it, since two operators' clocks need not agree. The token in its name
is the proof it is this deployment's, so cleaning a gateway host deletes it
with the rest (stage 3 of `site remove`, either way).

## Who writes it

| Command | Change |
|---|---|
| `apply` of a gateway, after the apply succeeds | adds every site, app and group the yaml declares; never takes one out |
| `site remove`, in stage 2 | takes the site out |
| `site remove --force`, on a site the yaml no longer declares | takes the site out |
| `app remove`, at the end of the removal | takes the app out, and each group no remaining Pocket ID app's `signup_default_groups` names |

Every change is made the same way, on every gateway the yaml declares at once:

1. Read the record on each gateway that answers.
2. Start from the newest of them, the highest revision, and make the change.
3. Write the result, with the revision raised by one, to every gateway that
   answered, each under the registry's lock and only if its file still hashes
   to what was read.

A gateway that does not answer, or whose write is refused, is a warning, never
a failure: `vm2 missed this change to the deployment record; it is brought up
to date the next time a command that writes the record reaches it`. When the
change leaves the names as they were, nothing is raised, and only a gateway
holding an older record is written: that is how a gateway that was down is
brought up to date.

`apply` only adds, so an `apply` of a yaml mid-edit cannot shrink the record:
that is the property the record exists for. Starting from the newest record
means a gateway that missed a removal does not bring the removed site back.

## Who reads it

Readers take the newest record, the highest revision among the gateways that
answered. Two records with the same revision and different names, left by two
writers at once, are read as their union, which keeps secrets rather than
deleting them; the next change writes that union with a higher revision.

| In `paisans.yaml` | In a record | Meaning | `init` and `apply` | `secrets prune` |
|---|---|---|---|---|
| yes | yes | deployed | nothing | keeps |
| yes | no | not deployed yet | nothing; `apply` adds it | keeps |
| no | yes | dropped from the yaml while still deployed | warn, by name, whether or not it has secrets | skips its keys, and says why |
| no | no | removed | warn that its secrets are orphaned | removes them |

The warning for the third row says what to do:

* a site: `sites.monitor-a is deployed but paisans.yaml no longer declares
  it. Restore it, or take it out with paisans site remove, or with paisans
  site remove --force --ssh <its host> if it is gone`;
* an app: `apps.uptime is deployed but paisans.yaml no longer declares it.
  Restore it, or take it out with paisans app remove uptime`. This is the
  normal state between taking an app out of the yaml and `app remove`.

A group follows its app: one no Pocket ID app names, still in a record, is
kept and named, and `app remove` of the last app naming it takes it out.

`init` and `apply` already reach hosts. A gateway whose record cannot be read
is a warning (`could not read the deployment record on vm: <why>`), never a
refusal, and `init` on a deployment with no gateway applied yet reads none.

## `secrets prune` and a missing record

Prune reads every gateway the yaml declares. A gateway that does not answer,
or has no record, is a refusal: `secrets prune: the deployment record on vm
could not be read (<why>), so nothing says whether what paisans.yaml no longer
declares was removed or is still running. Apply the gateway first, or pass
--without-record to trust paisans.yaml alone. Nothing was changed`.

`--without-record` trusts the yaml, as prune does today. It lists every key it
would remove, and with `--execute` asks for the word `prune` at a terminal,
as `--delete-data` asks for a site's name. No flag answers it, and without a
terminal it is refused.

## A lost deployment

The flow in the site-remove-force spec keeps working, in this order:

1. The monitors are taken out of the yaml and cleaned with `site remove
   --force --ssh`. No gateway is up, so no record is changed, and the report
   says so.
2. The destroyed sites are pointed at the new hosts and applied. The new
   gateway's first `apply` writes a fresh record from the yaml, which no
   longer names the monitors.
3. `paisans secrets prune --execute` reads that record and removes the
   monitors' secrets; `--without-record` is not needed.
4. DNS is changed by hand.

## Testing

In a new package for the record (read, merge, write commands, against a fake
host): adding is a union and never removes; taking out removes only the names
given; the write is under the lock and atomic; an absent file reads as no
record, a malformed one as an error; a change starts from the newest record,
raises the revision and reaches every gateway that answers; one that does not
is reported, and caught up by the next change; equal revisions that disagree
read as their union.

In `internal/secretsgen`: the four rows above, for each of sites, apps and
groups, with records from two gateways that disagree.

In `cmd/paisans`: prune refusing with an unreadable gateway and naming
`--without-record`; `--without-record --execute` refused without a terminal;
`init` and `apply` warning for the third row and never printing a value.

In `internal/siteremove` and `internal/appremove`, against their fake worlds:
the record changed on every gateway, a forced run on an undeclared site
reporting a gateway it could not reach, and a cleaned gateway's record file
deleted.

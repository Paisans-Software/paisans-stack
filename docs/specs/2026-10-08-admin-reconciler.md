# The admin reconciler

Status: approved in session on 2026-10-08, being implemented. Removal was
approved the same day, as a second founder decision.

A community with one administrator is one lost passkey away from nobody being
able to run it. The admin reconciler reports that. It is a sidecar in the
`pocket-id` kind that keeps the `admins` group equal to Pocket ID's
administrators and fails its health check when that group has fewer than two
members, so the uptime monitor raises an incident while there is still someone
who can fix it.

## What it does

Every two hours, and once when it starts, the reconciler makes one pass against
the Pocket ID instance on its own site. A pass that could not read Pocket ID
at all is followed by another after one minute instead: the reconciler and Pocket ID
start together, and the first pass can find it still starting.

1. **Active or standby.** Pocket ID runs on every apps site and one of them is
   active (README, "Pocket ID runs on every apps site, and one of them is
   active"). The reconciler reads the standby marker the wrapper writes (see
   *Standby*). On a standby it reports `standby` and does nothing else.
2. **Read.** Every user, through `GET /api/users`, which carries each user's
   groups (`UserService.ListUsers` preloads `UserGroups`,
   `service/user_service.go:51-56` at v2.14.0), and the group named exactly
   `admins`, through `GET /api/user-groups`.
3. **Add.** Every user who is an administrator (`isAdmin`), is not
   disabled, and is not in `admins` is added to it. Two kinds of user are
   never written, in this step or the next:
   * the static API key's synthetic user, whose ID is fixed at
     `00000000-0000-0000-0000-000000000000` (`common/reserved.go:5`) and which
     is created with `IsAdmin: true` (`apikey/service.go:240-249`). It is the
     reconciler's own credential, not a person, and it is never counted either;
   * a user with an `ldapId`, whose groups an LDAP sync owns.
4. **Remove.** Every member of `admins` who is disabled or is not an
   administrator is removed from it, the same two kinds of user left alone,
   under a floor: the group keeps at least one enabled member (the synthetic
   user excluded, as in the count). Removals run after adds, in username
   order; a removal the floor forbids is skipped, logged, and named in the
   health body, so when every member is due and nobody can be added the last
   in username order stays, the same one on every pass. A pass in which an
   add failed removes nobody.
5. **Count.** The members of `admins` who are not disabled, the synthetic user
   excluded. Fewer than two is unhealthy.

### The two writes

Pocket ID v2.14.0 has no call that adds or removes one member of a group. Both
routes that change membership replace a whole list: `PUT /api/user-groups/:id/users`
replaces a group's members, and `PUT /api/users/:id/user-groups` replaces one
user's groups (`controller/user_controller.go:41`). The reconciler uses the second,
as the toolkit's `pocketid.SetUserGroups` already does, because its scope is
one user: it reads that user again with `GET /api/users/:id` immediately before
writing, and sends the groups it finds plus the `admins` group's ID (an add)
or minus it (a removal). The fresh read also re-checks the rule: a user who
changed since the list, into or out of the group or of administrator status,
is left alone.

Those are the only writes the reconciler makes. It never creates, renames or
deletes a group, never changes a user, never writes a user an LDAP sync
manages, and never removes the last enabled member of `admins`. A test fails
if any other non-GET request appears in its code paths, or if a write does
not have one of the two shapes under its rule.

### Why removals have a floor

The reconciler acts on what one read says, unattended. A group it emptied on
a wrong answer would lock every administrator out of every app that reads
`admins` at once, with nobody left inside to fix it, which is the outage the
reconciler exists to prevent. So the floor is one enabled member, whoever they
are, and the order of a pass is adds first, then removals, so a swap of the
only member for a new administrator never passes through empty. The user list
is read page by page and a read that fails at any page fails the pass, which
then writes nothing and retries in a minute. A user the list misses makes the
pass more cautious, not less: an administrator left out is one fewer counted
towards the floor, a member left out is one not removed until the next pass.

### What it reports

`GET /healthz` on the reconciler's port, published on the site's mesh address only,
serves the result of the last pass. It never starts one, so polling it costs
nothing.

| State | Status | Body |
|---|---|---|
| active, `admins` has two or more members, nobody left to add | 200 | the count |
| standby | 200 | `standby` |
| `admins` has fewer than two members | 503 | the count and the members, and any member kept by the floor |
| an administrator could not be added, or a member could not be removed | 503 | the user and Pocket ID's answer |
| no group named `admins` | 503 | `no group named admins` |
| Pocket ID did not answer, or answered with an error | 503 | the error |
| no pass has finished yet | 503 | `starting` |
| the last pass finished more than two and a half hours ago | 503 | `stale` |

The stale state catches a pass loop that has hung while the HTTP server still
answers.

`GET /livez` is Docker's healthcheck, and it is not `/healthz`: it answers 200
before the first pass and whatever the last pass found, and 503 only once the
last pass is stale. `apply`'s health gate reads Docker's health, and a new
deployment has no `admins` group until an administrator makes one, so a gate on
`/healthz` would hold the whole Pocket ID stack back on it. The uptime monitor
reads `/healthz`. A failed write is not retried until the next pass, and the reconciler
keeps no state on disk: every pass starts from what Pocket ID says.

The body is plain text, one line, so it reads well in the monitor's incident
email. It names users by username, never by email.

Every write is logged on one line: the username, the user's ID, and the
result. That log is the audit trail the approval below depends on.

## Standby

The standby wrapper (`paisans-standby.sh`) marks a standby with
`/tmp/paisans-standby` inside the app container, which the reconciler cannot see.
It now also writes the same marker to `/paisans/run/standby`, a directory bind
mounted from `/srv/paisans/<token>/<app>/run` into both containers (read only in the reconciler),
and removes both at the top of every attempt. A reconciler that finds the marker is
on a standby. One that does not find it expects Pocket ID to answer, and
reports its silence as unhealthy.

## Configuration

None. The group is always named `admins`, the interval is two hours, and the
threshold is two. A community whose apps use another group for administrators
is warned by `validate` (`admin-group-not-admins`): the reconciler would be
reconciling a group those apps do not read.

The reconciler reads its environment from the Pocket ID stack's own `.env`:
`STATIC_API_KEY`, which the toolkit already renders, and a
`PAISANS_RECONCILER_POCKET_ID` the template adds, the instance's address on the
mesh. The key never reaches a host or a stack that does not already hold it.

## Kind and image

The `pocket-id` kind gains a `reconciler` service, running
`ghcr.io/paisans-software/admin-reconciler`, built from `cmd/admin-reconciler` in this
repository by `.github/workflows/admin-reconciler-image.yml` and pinned by digest in
`internal/kinds`. The image is a static Go binary on `distroless/static`, so its
Docker healthcheck is the binary itself: `admin-reconciler healthcheck` asks the
running reconciler's `/livez` and exits 0 or 1.

The reconciler listens on port 1412, one above Pocket ID's 1411, published on the
mesh address. `render`'s port table records it, so a collision on the site is
refused like any other.

## Monitoring

The uptime kind's generated `monitors.json` gains one check per site that runs
Pocket ID: `<app> — admin reconciler (<site>)`, a GET of
`http://<site address>:1412/healthz` expecting 200. There is no public check:
the reconciler is not routed by the gateway.

## Approval

An agent needs a human's approval for every Pocket ID group change. The
reconciler is not an agent: it is a service the deployment runs, and
`docs/deployment-agent-rules.md` (*Services the deployment runs*) approves such
a service once, as a whole, rather than write by write. The founder approved
this one on 2026-10-08: Pocket ID administrators are the community's
administrators, so putting them in `admins` changes nobody's power, only which
apps recognise it. The founder approved the removal the same day, with the
floor above. Both are recorded in `docs/decisions.md`. A further kind of
write in the reconciler needs its own approval, and the reconciler gives an
agent no authority to change `admins` by hand.

## Tests

* Unit tests against a fake Pocket ID (`httptest`): a missing administrator is
  added with a fresh read before the write; the synthetic user, an LDAP user
  and a disabled user are left out; a demoted and a disabled member are
  removed with a fresh read before the write; an LDAP managed user is written
  in neither direction; the last enabled member is never removed; when every
  member is due one is kept, in username order; a group of disabled members
  is left alone; a user list that fails on its second page removes nobody and
  is retried; adds come before removals; a failed add holds removals; a
  standby does nothing; no group, a failed write, a Pocket ID that does not
  answer, fewer than two members, no pass yet and a stale pass each give 503.
* A check on the write path: every non-GET request the reconciler makes is recorded,
  and the test fails unless each one is `PUT /api/users/<id>/user-groups` whose
  body is that user's current groups plus `admins`, for an enabled non LDAP
  administrator who is outside it, or minus `admins`, for a non LDAP member
  who is disabled or not an administrator, with another enabled member still
  in the group afterwards.
* Render: the pocket-id golden output gains the reconciler service and the run
  directory; the uptime seed gains the reconciler checks; the port table lists 1412.
* An integration test against a real `pocket-id:v2.14.0` container, run only
  when Docker is available, as the Garage integration test is: it creates an
  administrator outside `admins`, runs one pass, and reads the group back;
  then demotes one administrator, runs another, and reads it back again.

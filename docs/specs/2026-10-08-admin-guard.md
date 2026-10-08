# The admin guard

Status: approved in session on 2026-10-08, being implemented.

A community with one administrator is one lost passkey away from nobody being
able to run it. The admin guard watches for that. It is a sidecar in the
`pocket-id` kind that keeps Pocket ID's administrators in the `admins` group
and fails its health check when that group has fewer than two members, so the
uptime monitor raises an incident while there is still someone who can fix it.

## What it does

Every two hours, and once when it starts, the guard makes one pass against
the Pocket ID instance on its own site. A pass that could not read Pocket ID
at all is followed by another after one minute instead: the guard and Pocket ID
start together, and the first pass can find it still starting.

1. **Active or standby.** Pocket ID runs on every apps site and one of them is
   active (README, "Pocket ID runs on every apps site, and one of them is
   active"). The guard reads the standby marker the wrapper writes (see
   *Standby*). On a standby it reports `standby` and does nothing else.
2. **Read.** Every user, through `GET /api/users`, which carries each user's
   groups (`UserService.ListUsers` preloads `UserGroups`,
   `service/user_service.go:51-56` at v2.14.0), and the group named exactly
   `admins`, through `GET /api/user-groups`.
3. **Reconcile.** Every user who is an administrator (`isAdmin`), is not
   disabled, and is not in `admins` is added to it. Two kinds of user are left
   out:
   * the static API key's synthetic user, whose ID is fixed at
     `00000000-0000-0000-0000-000000000000` (`common/reserved.go:5`) and which
     is created with `IsAdmin: true` (`apikey/service.go:240-249`). It is the
     guard's own credential, not a person, and it is never counted either;
   * a user with an `ldapId`, whose groups an LDAP sync owns.
4. **Count.** The members of `admins` who are not disabled, the synthetic user
   excluded. Fewer than two is unhealthy.

### The one write

Pocket ID v2.14.0 has no call that adds one member to a group. Both routes
that change membership replace a whole list: `PUT /api/user-groups/:id/users`
replaces a group's members, and `PUT /api/users/:id/user-groups` replaces one
user's groups (`controller/user_controller.go:41`). The guard uses the second,
as the toolkit's `pocketid.SetUserGroups` already does, because its scope is
one user: it reads that user again with `GET /api/users/:id` immediately before
writing, appends the `admins` group's ID to the groups it finds, and sends the
result.

That is the only write the guard makes. It never removes a user from any
group, never creates, renames or deletes a group, and never changes a user.
A test fails if any other non-GET request appears in its code paths.

### What it reports

`GET /healthz` on the guard's port, published on the site's mesh address only,
serves the result of the last pass. It never starts one, so polling it costs
nothing.

| State | Status | Body |
|---|---|---|
| active, `admins` has two or more members, nobody left to add | 200 | the count |
| standby | 200 | `standby` |
| `admins` has fewer than two members | 503 | the count and the members |
| an administrator could not be added | 503 | the user and Pocket ID's answer |
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
reads `/healthz`. A failed write is not retried until the next pass, and the guard
keeps no state on disk: every pass starts from what Pocket ID says.

The body is plain text, one line, so it reads well in the monitor's incident
email. It names users by username, never by email.

Every write is logged on one line: the username, the user's ID, and the
result. That log is the audit trail for the standing exception below.

## Standby

The standby wrapper (`paisans-standby.sh`) marks a standby with
`/tmp/paisans-standby` inside the app container, which the guard cannot see.
It now also writes the same marker to `/paisans/run/standby`, a directory bind
mounted from `/srv/<app>/run` into both containers (read only in the guard),
and removes both at the top of every attempt. A guard that finds the marker is
on a standby. One that does not find it expects Pocket ID to answer, and
reports its silence as unhealthy.

## Configuration

None. The group is always named `admins`, the interval is two hours, and the
threshold is two. A community whose apps use another group for administrators
is warned by `validate` (`admin-group-not-admins`): the guard would be
protecting a group those apps do not read.

The guard reads its environment from the Pocket ID stack's own `.env`:
`STATIC_API_KEY`, which the toolkit already renders, and a
`PAISANS_GUARD_POCKET_ID` the template adds, the instance's address on the
mesh. The key never reaches a host or a stack that does not already hold it.

## Kind and image

The `pocket-id` kind gains a `guard` service, running
`ghcr.io/paisans-software/admin-guard`, built from `cmd/admin-guard` in this
repository by `.github/workflows/admin-guard-image.yml` and pinned by digest in
`internal/kinds`. The image is a static Go binary on `distroless/static`, so its
Docker healthcheck is the binary itself: `admin-guard healthcheck` asks the
running guard's `/livez` and exits 0 or 1.

The guard listens on port 1412, one above Pocket ID's 1411, published on the
mesh address. `render`'s port table records it, so a collision on the site is
refused like any other.

## Monitoring

The uptime kind's generated `monitors.json` gains one check per site that runs
Pocket ID: `<app> — admin guard (<site>)`, a GET of
`http://<site address>:1412/healthz` expecting 200. There is no public check:
the guard is not routed by the gateway.

## The standing exception

Every Pocket ID group change normally needs a human's approval each time.
The guard's one write is a standing exception, granted by the founder on
2026-10-08: Pocket ID administrators are the community's administrators, so
putting them in `admins` changes nobody's power, only which apps recognise it.
It is recorded in `docs/decisions.md`, and `docs/deployment-agent-rules.md`
names it beside the tier it is an exception to.

## Tests

* Unit tests against a fake Pocket ID (`httptest`): a missing administrator is
  added with a fresh read before the write; the synthetic user, an LDAP user
  and a disabled user are left out; nobody is removed; a standby does nothing;
  no group, a failed write, a Pocket ID that does not answer, fewer than two
  members, no pass yet and a stale pass each give 503.
* A guard on the write path: every non-GET request the guard makes is recorded,
  and the test fails unless each one is `PUT /api/users/<id>/user-groups` whose
  body is that user's current groups plus `admins`.
* Render: the pocket-id golden output gains the guard service and the run
  directory; the uptime seed gains the guard checks; the port table lists 1412.
* An integration test against a real `pocket-id:v2.14.0` container, run only
  when Docker is available, as the Garage integration test is: it creates an
  administrator outside `admins`, runs one pass, and reads the group back.

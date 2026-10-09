# The visibility gate

Status: approved in session on 2026-10-08, implemented on
`feat/visibility-gate`. Founder decisions throughout; the federation part is
called out where it lands, and recorded in `docs/decisions.md`.

An app can be made readable only by people signed in to the community. Mbin
and WriteFreely both serve every page to anonymous visitors and Mbin has no
private mode the toolkit can set, so the toolkit puts the gate in front of
them at Caddy and derives everything else the app needs from its kind.

Four parts:

1. **The key**, `visibility_gate: public | member | provisional`, replacing
   `gate:`.
2. **The gate covers everything** on a gated hostname except the classes that
   carry their own authentication, which the app verifies.
3. **Each kind declares what it needs around the gate** in `internal/kinds`:
   the paths that must stay open, its inboxes, its token paths, its auto-login
   and its signed fetch setting. One generic template renders that for every
   kind.
4. **The monitor proves the gate, the edge and signed fetch** with checks of
   their own.

Two dependencies live in other repositories: a status-only health route in
the Mbin fork and in the WriteFreely fork. See *Fork health routes*.

## The key

```yaml
apps:
  talk:
    kind: mbin
    hostname: talk.example.org
    visibility_gate: member
```

| Value | Who reads the app | Gate instance |
|---|---|---|
| `public` | anyone | none |
| `member` | a signed-in member of the community | `members`, which carries the group restriction |
| `provisional` | anyone signed in, member or not | `provisional`, which admits every signed-in user |

Absent means `public`. The `gate:` key is not read: `KnownFields` refuses it as
an unknown key, and a deployment is redeployed rather than converted.

`provisional` is accepted on any gateable kind and warned about
(`visibility-gate-provisional`), because it admits every signed-in visitor,
including people who have not been accepted into the community.

`member` admits the group the gate app's `settings.members_group` names,
`members` when absent (`render.GateMembersGroup`). Nothing creates that group
at the identity provider. A member-gated app whose own member group (Eg:
Mbin's `OAUTH_OIDC_MEMBER_GROUP`) is a different name is warned about
(`visibility-gate-group-mismatch`): someone in one group and not the other is
admitted by one check and refused by the other.

## What reaches the app without a gate session

On a gated hostname, Caddy sends a request to the app without asking the gate
only when it is one of these, matched in this order:

| Class | Matched by | Who authenticates it |
|---|---|---|
| ActivityPub read | `GET` whose `Accept` names `application/activity+json` or `application/ld+json` | the app, by signed fetch; Caddy filters the answer |
| ActivityPub delivery | `POST` whose `Content-Type` names either, on the kind's `InboxPaths` | the app, which verifies the delivery's signature (WriteFreely only with an allowlist, until `authorized_fetch`; see *Known limits*) |
| Auto-login | see *Auto-login* | the gate, whose session it requires |
| Open paths | the kind's `OpenPaths` | nothing; these hold no private content |
| Token paths | the kind's `TokenPaths` **and** `Authorization: Bearer` | the app, which refuses a token it did not issue |

Everything else goes to the gate: `curl`, a JSON request with no token, a
request asking for `*/*`, a `HEAD`. With no session the gate redirects to sign
in; with a session that is not a member's, to the pending page. The redirect
carries no body from the app.

Each class is its own `handle` with a named matcher, and Caddy keeps those in
the order they are written. Every handle strips `X-Auth-Request-*` from the
inbound request first, so nothing upstream of the gate can name a user.

### ActivityPub reads are filtered

Anyone can send `Accept: application/activity+json`, and an app can answer it
with something that is not ActivityPub. Read off Mbin v1.14.0+paisans: it
picks a route by the first acceptable type (services.yaml:38), so `Accept:
text/html, application/activity+json` gets the HTML page; a route with no
ActivityPub form serves HTML whatever is asked for; and its `/api/*` read
endpoints answer anonymous callers (EntriesRetrieveApi.php:72-80,
security.yaml:148). Its signed fetch checks only its `ap_*` routes.

So the read handle passes a block into the app's own `reverse_proxy`, through
the snippet's `{block}`, that decides what comes back:

| Upstream answer | What the client gets |
|---|---|
| 2xx with `Content-Type` ActivityPub, JSON-LD or JRD | the answer, unchanged |
| 3xx | the status and `Location`, no body |
| any other 2xx | an empty 404 |
| anything else | the status, no body |

A signed peer gets what it asked for; an unsigned one gets the app's 401; a
request that reached an HTML or API route by asking for ActivityPub gets
nothing. `HEAD` is not in the class: Mbin does not check a `HEAD`'s signature
(AuthorizedFetchSubscriber.php:83), and a read needs only `GET`.

### Deliveries reach the inboxes only

A `POST` changes state before any answer could be filtered, so an ActivityPub
`Content-Type` is honoured only on the kind's inboxes. Elsewhere it goes to
the gate. Without this, anonymous `POST /api/client` with an ActivityPub
`Content-Type` would register an OAuth client on Mbin
(CreateClientApi.php:103).

### Why spoofing a header does not open anything

A spoofed `Accept` reaches the app, which refuses it unless it carries a valid
HTTP signature from an instance on its allow list, and the filter returns
nothing the app answered with that is not ActivityPub. Caddy cannot verify a
signature without a plugin, and the gateway runs no plugin whose failure to
build would stop the identity provider loading, so the app is where the check
lives. **That only holds while signed fetch is on in the app**, so the gate
value drives it (see *Signed fetch*), and the monitor proves it.

A bearer token is honoured only on a kind's `TokenPaths`. An HTML route does
not read one, and Mbin's API treats any `Authorization` that is not `Bearer`
as anonymous (OAuth2Authenticator.php:55-58), so the header alone would let a
request past the gate to be served as an anonymous visitor.

## The kind catalogue

```go
type GateSpec struct {
	Gateable    bool
	OpenPaths   []string
	InboxPaths  []string
	TokenPaths  []string
	AutoLogin   []AutoLoginRule
	SignedFetch *SignedFetch
}
```

`Gateable` is false for `synapse`, `pocket-id`, `oauth2-proxy` and `uptime`: a
Matrix client and a federating homeserver will not follow a redirect to a
passkey prompt, Pocket ID and the gate are the sign-in flow itself, and the
monitor is what an admin opens when the sites the gate signs in through are
down, so a gated monitor is unreadable at exactly the moment it exists for
and its break glass password sign-in would sit behind the sign-in it is the
fallback for. A gate on any of them is refused
(`visibility-gate-on-ungateable-kind`). Every kind has a record, and a test
fails for a kind without one.

The kind's health route is added to its open paths when it is a dedicated
route, never when it is `/`.

| Kind | Open paths | Inboxes | Token paths | Auto-login |
|---|---|---|---|---|
| `mbin` | OIDC (`/oauth/*`), the native app's chain (`/authorize /consent /token /login`), discovery (webfinger, host-meta, nodeinfo, `/i/actor`, contexts), the sign-in page's assets | `/i/inbox /f/inbox /u/*/inbox /m/*/inbox` | none | `/` and `/login` to `/oauth/oidc/connect`, unless `PHPSESSID` or `REMEMBERME` |
| `writefreely` | OIDC (`/oauth/*`), discovery (webfinger, host-meta, nodeinfo), the sign-in page's assets | `/api/collections/*/inbox` | none | `/login` always, `/` unless `wfu`, to `/oauth/generic` |
| `outline` | OIDC (`/auth/oidc`, `/auth/oidc.callback`, `/auth/redirect`, never `/auth/*`, which would open email sign-in), OAuth discovery, registration and token endpoints, `/_health` | none | `/api/*`, `/mcp` | none: its sign-in screen redirects itself when OIDC is the only provider |
| `element` | the health route | none | none | none |

Each entry is cited in `internal/kinds/gate.go` against the pinned tag.

### Why Mbin's and WriteFreely's token paths are empty

A token path is safe only if a token that reads content cannot be had without
a member's sign-in, and the app refuses everything else. Mbin's `/api/*` read
endpoints answer with no token at all, and anonymous `POST /api/client` mints
a client (CreateClientApi.php:95,122-139). WriteFreely mints tokens only from a
password sign-in, which `disable_password_auth` closes, but tokens already
issued keep working and carry no scopes (handle.go:281-292). **Mbin's mobile
apps do not work on a gated instance**, because after their WebView sign-in
they call `/api/*` with no gate session. Outline's API keys are created only
from a signed-in session (apiKeys.ts:23-27), so its API is a token path.

## Auto-login

A member who has passed the gate holds a Pocket ID session, so the app's own
sign-in completes without a prompt. Each rule redirects a request on its paths
to the app's OIDC start route when:

* it is a `GET` or `HEAD` asking for `text/html` with no `Authorization`;
* the gate's session cookie, `_oauth2_proxy`, is present, so an anonymous
  visitor still meets the gate;
* none of the rule's absent cookies is present, which is how the kind says
  "already signed in here";
* the loop breaker, `<app>_autologin`, is absent.

The redirect sets `<app>_autologin` for two minutes. It is not optional: an
app's Pocket ID client can refuse a user the gate admitted, and without it that
user would bounce between the app's sign-in page and its OIDC route forever.

Rules fire on `/` and the sign-in page only. Neither Mbin's nor WriteFreely's
OIDC start route takes a destination (OidcController.php:14-20, oauth.go:135-
166), so a deep link sent through one would land on the front page.

## The gate snippets

`gate_member` and `gate_provisional` strip `X-Auth-Request-*`, `forward_auth`
to their instance and copy the verified identity headers back. They answer a
person with a page rather than oauth2-proxy's raw status:

* **401**, no session: redirect to `https://<gate hostname>/oauth2/start?rd=`
  the original URL.
* **403**, signed in but not a member (`gate_member` only): redirect to
  `https://<gate hostname>/pending?rd=` the original URL.

Both redirects carry a fixed body, `render.GateMarker`, so the monitor can tell
the gate's redirect from an app's own.

### The pending page

The gate app's hostname serves `/pending`, a static page: the community's name
and a sentence saying the account is signed in but not yet a member, and that
approval can take up to an hour to arrive (the session's groups refresh with
its cookie). It never echoes `rd` or any other part of the request, because
Caddy placeholders are not HTML escaped, and it escapes braces in the
community's name, because Caddy expands a `{placeholder}` in a response body.

## Signed fetch

**Founder decision, 2026-10-08, and a federation policy change:** a gated app
that federates refuses every unsigned ActivityPub read, and reads only from
instances on its allow list. An empty allow list federates with nobody until an
admin adds a peer. A gated federating app whose kind records no way to refuse
an unsigned read is refused (`visibility-gate-without-signed-fetch`).

* **Mbin.** The toolkit renders `MBIN_AUTHORIZED_FETCH=true` and
  `MBIN_USE_FEDERATION_ALLOW_LIST=true` when the app is gated. Mbin reads them
  from the environment until an admin saves its settings, after which a
  database row wins (SettingsManager.php:105-106,136-163).
* **WriteFreely** checks signatures only in private mode
  (handle.go:663-690, federation_allowlist.go:493-544), which the kind renders
  on by default. A gated WriteFreely with `private: false` has no signed fetch,
  and is refused.

A rendered setting is not proof: an admin can turn either off inside the app
with nothing in the configuration to say so. The monitor's signed fetch check
is what catches that.

## The monitor

| Check | Request | Expect | Proves |
|---|---|---|---|
| gate | `GET /` with no session | a body holding the gate's marker | the edge, and the gate in the path |
| public | the kind's health route, with no session | the health route's answer | the edge reaches the app |
| signed fetch | an unsigned `GET` asking for ActivityPub, of the kind's probe | `401` | the read surface is closed |
| direct | unchanged | unchanged | the app itself |

The gate check asserts the marker rather than a 302, because a private Mbin or
WriteFreely answers `/` with a 302 to its own sign-in page, and a check on the
status alone would pass with the gate gone. The uptime fork's string check
passes on any status below 400 whose body holds the expected text.

The public check is seeded for a gated app only when its health route is in its
open paths, which is true of a dedicated route and never of `/`. Until the
forks' health routes are in the pinned images, a gated Mbin or WriteFreely has
the gate check and no public check.

The probes need no content to exist: Mbin's is `/`, which asking for
ActivityPub is `ap_instance_front` and not one of the routes signed fetch
leaves open (AuthorizedFetchSubscriber.php:52-59,148-153); WriteFreely's is
`/api/collections/<hostname>`, the instance actor, which resolves from
configuration (activitypub.go:108-131) and answers 401 unsigned.

## Fork health routes

A dedicated route in each fork, so a gated Mbin or WriteFreely can be checked
through the edge without opening its front page:

* **path** `/healthz`, a path the app does not otherwise use, reserved where
  the app lets users claim top-level names;
* **answer** `200` with an empty body when healthy, `503` with an empty body
  when a dependency is not: the database for both, and Mbin's cache as well;
* **no detail**: no version, no dependency names, no error text, so the route
  can sit in the open paths without telling anyone anything but up or down;
* **no session, no cookies, no redirect**, so it answers the same to a monitor
  as to anything else.

Each is its own branch and pull request in its fork. When a pinned image
carries the route, `internal/kinds/health.go` records it, and it is open past
the gate from then on with no other change.

## Fork settings the toolkit will render

Two more fork changes, so the app's own checks hold without an admin setting
anything by hand. Each is rendered by the toolkit once a pinned image carries
it, and until then is a known limit below.

* **Mbin `MBIN_PRIVATE_INSTANCE` from the environment.** With it on, Mbin's
  catch-all access rule refuses any request without a signed-in user
  (security.yaml:148, PrivateInstanceVoter.php), which closes anonymous
  `/api/*` reads. At v1.14.0+paisans it is read from the database only, with
  a hard-coded `false` default (SettingsManager.php:96), so nothing rendered
  can set it. The fork gives it the same environment fallback its siblings
  have; a gated Mbin renders it on, and a direct check proves an anonymous
  `/api/entries` is refused. Instance metadata and the admin and moderator
  lists stay public (security.yaml:137-147).
* **WriteFreely `authorized_fetch`**, independent of private mode: a valid
  signature on every ActivityPub read and every inbox delivery, from an
  allowlisted host when an allowlist is set. A gated WriteFreely renders it on,
  and its signed fetch record moves from private mode to this setting.

## Known limits

* **A gated WriteFreely with an empty allowlist accepts unsigned deliveries.**
  Its inbox verifies a signature only when the allowlist is non-empty
  (activitypub.go:402-409), so until `authorized_fetch` is pinned anyone can
  deliver a Follow or Like to a blog's inbox. Nothing becomes readable by it.
* **Mbin's API answers anonymous reads** that reach it, until
  `MBIN_PRIVATE_INSTANCE` is pinned. The gate keeps them from reaching it,
  except through the ActivityPub filter, which returns nothing that is not
  ActivityPub.

* **Webfinger is open to anyone**, because a peer needs it before it can sign,
  and it answers JRD rather than ActivityPub. It confirms whether a handle
  exists.
* **Outline's API answers a made-up bearer token as anonymous** on its
  optional-auth endpoints (authentication.ts:67-71), so everything Outline
  serves anonymously through its API is reachable past the gate with any
  `Authorization: Bearer` value: published shares (`shares.info`,
  `documents.info` with a share ID) and files with a public ACL, each by an
  unguessable ID, although `/s/*` itself stays behind the gate.
* **Outline's `/oauth/register` is open**, so anyone can register an OAuth
  client, rate limited (oauth/index.ts:181-200). A client alone holds nothing:
  a token still needs `/oauth/authorize`, which needs a session and stays
  behind the gate. It is open because an MCP client registers itself before
  it can sign in.
* **Auto-login checks the gate cookie's name, not its validity**, because it
  runs before the gate. A forged cookie sends a visitor to the app's OIDC start
  route, an open path anyway, and every request after that still goes through
  the gate. A signed-in non-member sent there meets the app's own Pocket ID
  client, which may refuse them, rather than the pending page.
* **`rd` is not URL encoded**, because Caddy has no placeholder that encodes
  one: a deep link with more than one query parameter loses the extras on the
  visit that crosses sign-in.

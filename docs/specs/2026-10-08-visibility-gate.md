# The visibility gate

Status: approved in session on 2026-10-08, being implemented. Founder
decisions throughout; the federation part is called out where it lands.

An app can be made readable only by people signed in to the community. Mbin
and WriteFreely both serve every page to anonymous visitors and neither has a
private mode, so the toolkit puts the gate in front of them at Caddy and
derives everything else the app needs from its kind.

This replaces the per-app `gate:` key. Four things change:

1. **The key** becomes `visibility_gate: public | member | provisional`.
2. **The gate is inverted.** It gates every request on a gated hostname except
   the classes that carry their own authentication, which the app verifies.
   It used to gate browser navigations only, which left every page readable
   to any client that did not ask for HTML.
3. **Each kind declares what it needs around the gate** in `internal/kinds`:
   the paths that must stay open, its auto-login, its signed fetch setting and
   its token paths. One generic template renders that for every kind.
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

Absent means `public`. The old `gate:` key is not read: `KnownFields` refuses
it as an unknown key, and a deployment is redeployed rather than converted.

`provisional` is accepted on any gateable kind and warned about
(`visibility-gate-provisional`), because it admits every signed-in visitor,
including people who have not been accepted into the community. It exists for
surfaces whose audience is exactly that.

## What reaches the app without a gate cookie

On a hostname whose app is gated, Caddy sends a request to the app without
asking the gate only when it is one of these:

| Class | Matched by | Who authenticates it |
|---|---|---|
| ActivityPub | `Accept` or `Content-Type` naming `application/activity+json` or `application/ld+json` | the app, by signed fetch against its allow list |
| Open paths | the kind's `OpenPaths` | nothing; these hold no private content |
| Token paths | the kind's `TokenPaths` **and** an `Authorization` header | the app, which validates the token |

Everything else goes to the gate. A request with no session is redirected to
sign in, and that includes `curl`, a JSON request with no token, and a request
asking for `*/*`. The redirect carries no body from the app.

The classes are matched in that order, then auto-login, then the gate, each in
its own `handle` with a named matcher. Caddy keeps named-matcher `handle`
blocks in the order they are written, so the order in the template is the
order of evaluation.

### Why spoofing a header does not open anything

Anyone can send `Accept: application/activity+json`. The request then reaches
the app, and the app refuses it unless it carries a valid HTTP signature from
an instance on its allow list. Caddy cannot verify a signature without a
plugin, and the gateway runs no plugin that could stop the identity provider
loading if it failed to build, so the app is where the check lives.

**That only holds while signed fetch is on in the app**, so the gate value
drives it. See *Signed fetch*.

`Authorization` is honoured only on a kind's `TokenPaths`, never on its own.
An HTML route does not read a bearer token, so an `Authorization: x` header
on `/m/anything` would otherwise be served as an anonymous visitor's page. On
a token path the app refuses a token it did not issue.

### What `OpenPaths` holds

Browser navigations and machine requests that must reach the app ungated,
because they are the sign-in itself or because a federating peer needs them
before it can sign anything:

* the app's own OIDC callback, which the browser reaches mid-login;
* a native app's OAuth chain, which runs in a WebView with no gate cookie;
* federation discovery (Eg: webfinger, host-meta, nodeinfo, the instance
  actor), which serves ActivityPub whatever the `Accept` header says;
* static assets the open pages load;
* the kind's health route, when it is a dedicated route rather than `/`.

Each entry is read off the pinned tag's source and cited beside it.

### What `TokenPaths` holds, and why Mbin's is empty

A token path is safe to leave open only if getting a token that reads content
needs a member's sign-in. A kind's `TokenPaths` stays empty until that is
shown from its source, or until the toolkit gates or disables whatever mints a
token without one.

Mbin's starts empty. Mbin has OAuth client registration and a
`client_credentials` grant, and until they are shown not to yield a reading
token without a user, leaving `/api/*` open could hand the read surface to
anyone. The cost is plain: **Mbin's mobile apps do not work on a gated
instance until this is resolved**, because after their WebView sign-in they
call `/api/*` from an HTTP client with no gate cookie. Outline's API keys are
created by a signed-in user, which makes its `/api/*` a token path.

## The kind catalogue

`internal/kinds` gains one record per kind:

```go
type GateSpec struct {
	Gateable    bool
	OpenPaths   []string
	TokenPaths  []string
	AutoLogin   []AutoLoginRule
	SignedFetch *SignedFetch
}

type AutoLoginRule struct {
	Paths         []string
	Target        string
	AbsentCookies []string
}
```

`Gateable` is false for kinds whose clients will not follow a redirect to a
passkey prompt, or that are the sign-in flow itself: `synapse`, `pocket-id`
and `oauth2-proxy`. A gate on any of them is refused
(`visibility-gate-on-ungateable-kind`), which generalises the rule that a
Matrix hostname is never gated.

`SignedFetch` is set for a kind that federates, and names the settings that
make it refuse an unsigned read. A federating kind with none recorded cannot
be gated (`visibility-gate-without-signed-fetch`), because the ActivityPub
class above would then be an open door.

## Auto-login

A member who has passed the gate holds a Pocket ID session, so the app's own
sign-in completes without a prompt and its "Log in with" button is one
pointless click. Each `AutoLoginRule` redirects a request on its `Paths` to the
app's OIDC start route when:

* it is a `GET` or `HEAD` asking for `text/html` with no `Authorization`;
* the gate's session cookie is present, so only someone the gate already let
  through is sent on, and an anonymous visitor still meets the gate;
* none of `AbsentCookies` is present, which is how the kind says "already
  signed in to the app";
* the loop breaker, `<app>_autologin`, is absent.

The redirect sets `<app>_autologin` for two minutes. It is not optional: an
app's Pocket ID client can refuse a user the gate admitted, and without it that
user would bounce between the app's sign-in page and its OIDC route forever.

Rules fire on `/` and the app's sign-in page only, never a deep link. Neither
Mbin's nor WriteFreely's OIDC start route takes a destination, so a deep link
sent through it would land on the front page having lost what was clicked.

Auto-login is rendered before the open paths. A path can be both (Eg: Mbin's
`/login`), and someone holding a gate cookie must take the redirect, while a
WebView with none falls through to the open page.

A kind with no rule keeps its button. That is a missing convenience, not a
failure.

## The gate snippets

`gate_member` and `gate_provisional` keep what the gate already did: strip
`X-Auth-Request-*` from the inbound request, `forward_auth` to the instance,
copy the verified identity headers back. They gain the two answers a person
needs instead of oauth2-proxy's raw status:

* **401**, no session: redirect to `https://<gate hostname>/oauth2/start?rd=`
  the original URL.
* **403**, signed in but not a member (`gate_member` only): redirect to
  `https://<gate hostname>/pending?rd=` the original URL.

They no longer match browser navigations only. Every request that reaches the
final `handle` is gated.

### The pending page

The gate app's own hostname serves `/pending`, a static page rendered from a
template: the community's name and a sentence saying the account is signed in
but not yet a member. It is the place a future onboarding service would
replace, behind the same path.

## Signed fetch

**Founder decision, 2026-10-08, and a federation policy change:** a gated app
that federates refuses every unsigned ActivityPub read, and reads only from
instances on its allow list. That changes who can read the instance, which is
why it is recorded as the founder's call rather than a rendering detail.

The toolkit renders the kind's `SignedFetch` settings on whenever its
`visibility_gate` is not `public`. For Mbin that is `MBIN_AUTHORIZED_FETCH` and
`MBIN_USE_FEDERATION_ALLOW_LIST`. An empty allow list means the instance
federates with nobody until an admin adds a peer.

A rendered setting is not proof. Mbin keeps settings in its database and the
database is believed to override the environment, so an admin turning signed
fetch off in the UI would reopen the read surface with nothing in the
configuration to say so. The monitor's signed fetch check is what catches
that.

## The monitor

Every gated app's checks become:

| Check | Request | Expect | Proves |
|---|---|---|---|
| gate | `GET /` with no session | `302` to sign in | the edge, and the gate in the path |
| public | the kind's health route, with no session | the health route's answer | the edge reaches the app |
| signed fetch | an unsigned ActivityPub `GET` of the kind's probe path | `401` | the read surface is closed |
| direct | unchanged | unchanged | the app itself |

The public check reaches the app only when the health route is in
`OpenPaths`, which is true of a dedicated route and never of `/`. A kind whose
health route is `/` gets no public check while gated, only the gate check,
until its fork ships a dedicated one. That is the gap the fork health routes
close.

The signed fetch check exists only for a kind with `SignedFetch`. Its probe
path is recorded with the setting, and is one the app refuses unsigned without
needing any particular content to exist.

## Fork health routes

A dedicated route in each fork, so a gated Mbin or WriteFreely can be checked
through the edge without opening its front page:

* **path** `/healthz`, a path the app does not otherwise use, reserved where
  the app lets users claim top-level names;
* **answer** `200` with an empty body when healthy, `503` with an empty body
  when a dependency is not: Postgres for both, and Mbin's cache as well;
* **no detail**: no version, no dependency names, no error text, so the route
  can sit in `OpenPaths` without telling anyone anything but up or down;
* **no session, no cookies, no redirect**, so it answers the same to a monitor
  as to anything else.

Each is its own branch and pull request in its fork. The toolkit records the
route in `internal/kinds/health.go` and adds it to the kind's `OpenPaths` when
the pinned image carries it. Until then the kind's health route stays `/`.

## What is verified from source before it is recorded

Nothing here goes into the catalogue from memory:

* each kind's `OpenPaths`, from its pinned tag's router;
* Mbin's token minting, which decides whether `TokenPaths` stays empty;
* WriteFreely's signed fetch setting, if it has one. Without one, a federating
  WriteFreely cannot be gated;
* Mbin's settings precedence between the environment and its database;
* each kind's signed fetch probe path, by running the pinned image.

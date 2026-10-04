# Toolkit decisions

One entry per settled decision about **this toolkit**. Newest last.

Decisions about a *community* — which software fills a capability, federation
policy, how that community governs itself — do not belong here. They belong in
that community's own records. This file is for choices that every adopter
inherits.

---

## 2026-09-02 — The toolkit is written in Go, as a single binary

**Decided.** Not Terraform, not Ansible, not shell.

### Why not Terraform

Terraform provisions cloud resources through provider APIs, converging a
dependency graph against a state file. This toolkit renders configuration files,
pushes them to Linux hosts, runs commands, and verifies results between stages.
Three mismatches:

* **A staged join is not a dependency graph.** "Verify the WireGuard handshake in
  both directions, and only then change etcd membership" is a procedure with a
  gate and a rollback. A graph expresses ordering, not verification-with-abort.
  The real logic would end up inside `remote-exec` provisioners, which upstream
  itself calls a last resort.
* **State becomes another artifact to keep consistent.** The backup contract
  already names four; a state file that must stay in step with the encrypted
  secrets would be a fifth, and the one that goes stale quietly.
* **Licence.** BUSL since 2023, which is an avoidable complication for something
  meant to be handed to people we have never met. OpenTofu exists if it is ever
  wanted.

Terraform *is* right for provisioning the cloud instance itself — create the
machine, firewall rules, DNS records. That should stay **optional**, because most
operators will click a button in a provider's console and requiring otherwise
raises the floor for no benefit.

### Why not Ansible

The closest fit of the alternatives, and a reasonable choice: agentless over SSH,
idempotent, good templating. Two things counted against it.

The staged operations need verification loops, conditional rollback and precise
failure messages, and those degrade quickly when expressed in YAML. And it puts
a working Python environment in front of every adopter, which is exactly the kind
of step where people stop.

### Why not shell

It matches what already exists and adds no dependency, which is a real argument.
But YAML parsing, templating and staged rollback in shell get bad fast, and this
toolkit's whole value is in the parts that would be worst.

### Why Go

**Distribution.** One static binary, no runtime, cross-compiled for macOS and
Linux. An operator downloads it and runs it. This is the same constraint that
ruled out Kubernetes: the floor for an adopter has to stay low, and every install
step is somewhere they can stop.

**The policy is the product.** Most of the design is refusals and warnings —
refuse a witness sharing a failure domain with a voter; refuse a two-voter
cluster; refuse to overwrite a locally edited `.env`; warn when pinning an app
onto the witness; verify tunnels before touching etcd membership. Each is a place
where a novice operator is caught before doing damage. That is program logic with
good error messages, not declarative configuration.

**sops and age are both Go**, so encryption and decryption can be embedded rather
than shelled out. That removes two installation steps from every workstation and
makes the secrets path harder to get wrong. *Verify the library surfaces before
relying on this.*

### What it is not

* **Not a daemon.** Every command is operator-invoked. Patroni and etcd are the
  only things that watch anything, deliberately.
* **Not an orchestrator.** It renders and pushes; Docker Compose runs things. The
  moment it starts scheduling, it is re-inventing what was already rejected.

### Consequences

* Contributors need a Go toolchain; operators need nothing.
* Templating uses the standard library, so no template engine is inherited.
* Anything shelled out to — `docker`, `wg`, `etcdctl`, `patronictl` — is a
  dependency on the *host*, and belongs in preflight.

---

## 2026-09-15 — This repository lives in the Paisans-Software organisation

**Decided.** `josephquigley/paisans-stack` became
`Paisans-Software/paisans-stack`, private, and the Go module path became
`github.com/paisans-software/paisans-stack`.

### Why

This toolkit is meant to be handed to organizers nobody here has met. A
repository under one person's account says the opposite of what the project
intends: that it is somebody's side project, that its continuity depends on one
account, and that an adopter is trusting an individual rather than a project.
An organisation can gain maintainers, survive a person losing interest, and own
the packages the toolkit tells adopters to pull.

The forks, `mbin-paisans` and `writefreely-wisp`, deliberately did not move.
Their audience is upstream maintainers who already recognise the account that
opens pull requests against them, and moving a fork under an organisation buys
nothing there while costing that recognition.

### Consequences

* GitHub redirects the old path, so open pull requests and existing clones
  survive. A redirect is a courtesy rather than a contract: a clone that
  predates the move has its remote updated rather than relying on one.
* The module path changed, which touched every import. It is renamed rather
  than left as a path that names a repository that no longer exists, because a
  module path that lies is worse than one that is inconvenient to change.
* `paisans.community`'s own `CLAUDE.md` names this repository as the one
  sanctioned fallback when its wiki is unreachable. That reference is part of a
  recovery path, so it is updated in the same change rather than left pointing
  at a redirect that a future outage would have to survive.
* Packages this project publishes now belong to the organisation, which is what
  makes `ghcr.io/paisans-software/...` a name an adopter can be asked to trust.

---

## 2026-09-15: The ACME DNS provider is declared, and its Caddy image is published

**Decided.** A deployment names its DNS provider in `acme.provider`, and pulls a
Caddy image with that provider's module compiled in from
`ghcr.io/paisans-software/caddy`. Nothing is built on a host.

### What this fixed as well as abstracted

The toolkit rendered upstream's `caddy:2-alpine` against a configuration saying
`acme_dns cloudflare`. A DNS provider in Caddy is a module compiled in with
xcaddy, and upstream's image carries none, so **the rendered gateway could not
have obtained a certificate at all.** Nobody had run it.

### Why not build on the gateway

It is what a deployment's own stack does, and it works anywhere with no registry
account. It was declined because the suggested topology puts the gateway role on
a small machine that is also the etcd witness, and etcd's stability is a
function of fsync latency. A compile there competes for exactly the resource
that placement was chosen around, and it repeats on every Caddy upgrade.

The cost is that a deployment's gateway now depends on our registry and on those
images being rebuilt. `acme.image` is what keeps that from being lock-in.

### Why one image with both modules

Switching provider is then a configuration edit with no image change, no rebuild
and no repull, which is the point of the abstraction. An adopter carries a module
they do not use, a few megabytes.

### Why the default is a digest

Every tag `caddy-dns` publishes moves, mirroring upstream, which rebuilds even a
patch tag when its base image gets a security fix. A digest is the only
reference that makes two runs of `apply` deploy the same bytes.

### Why an unknown provider is allowed

A closed set would be consistent with how application kinds are treated, and it
would leave an adopter on another DNS provider waiting for us or forking.
Instead the provider is declared with an image that carries its module, and the
module is verified at apply time by asking the binary, since what is compiled
into a binary is not a property of its name.

---

## 2026-09-15: Synapse is a resource server and MAS owns authentication

**Decided.** The `synapse` kind renders three containers, not two: the
homeserver, Matrix Authentication Service in front of it, and a database. The
identity provider is upstream of MAS rather than of the homeserver.

### What was deleted

The kind used to render an `oidc_providers` block into `homeserver.yaml`, with
the `password_config` block that accompanied it. Both are gone, and so is
`registration_shared_secret`, which the kind never rendered but which any
`synapse generate` run produces and which an operator would otherwise be left
holding.

The old form is deleted rather than kept behind a setting. Two ways to
authenticate a homeserver is two ways to get it wrong, and the second one is
not a fallback: a deployment that kept both would have a second account
creation path that bypasses the group restriction at the identity provider,
which is the whole of the access policy for a Matrix stack. A homeserver
hostname cannot be gated, because a Matrix client is not a browser and will not
follow a redirect to a passkey prompt, so there is nothing else standing in
front of it.

### Where the shape came from

From a deployment that runs it, read on 2026-09-15: its compose file, its
gateway's host blocks and the design record written when it was built. That
deployment was verified on the wire at the time it was brought up, which is
more than the toolkit's previous shape could claim, since nobody had ever run
what the kind rendered.

Nothing of that community crossed into this repository. What was taken is which
services exist, which configuration keys they carry and what order the route
matchers go in. Every value here is the fixture's example domain or a template
variable.

### The stable block, not the experimental one

The task brief asked for `experimental_features.msc3861`. Synapse v1.160.0, the
version this kind pins, documents `matrix_authentication_service` instead, and
its configuration manual at that tag no longer mentions msc3861 anywhere. The
stable block is rendered and the test asserts it. Pinning the toolkit to the
experimental key would pin it to a shape the image it pins has moved past.

### The matcher lengths in the gateway snippet are the load bearing part

`/_matrix/client/*/login`, `/logout` and `/refresh` are split off to MAS ahead
of the `/_matrix/*` catch all. What puts them ahead is their length, not their
position: Caddy sorts routes of the same directive by path matcher length,
longest first with a trailing `*` trimmed, before it ever evaluates one
(`caddyconfig/httpcaddyfile/directives.go`, `sortRoutes`, read at v2.11.4, the
version this toolkit pins). A `handle` with no path matcher sorts last, which
is what makes the bare one MAS's fallback.

An earlier version of this record said Caddy evaluates `handle` blocks in the
order written and that reversing them would send every login to the homeserver.
That is wrong, and the counterexample was already in the rendered file: the
`/.well-known/matrix/*` handle is written after both catch alls and still wins.
The snippet is written in the order it sorts into so that reading it top to
bottom matches what happens, and nothing more rests on that.

The real hazard is a matcher rather than a move: a new handle under `/_matrix`
longer than one of the login paths and overlapping it would capture those
requests. So the test asserts the property that governs, which is that each
split path outranks the catch all on length. It cannot assert what Caddy then
does with the sorted routes, because the rendered text is all it has, and its
name says so.

### The apex gets a snippet of its own

The hostname holding the `wellknown` role serves `/.well-known/matrix/server`
and `/.well-known/matrix/client` and nothing else. Routing it like the primary
would publish the entire homeserver API on the name inside every user
identifier. That name is also what `server_name` is set to when the role is
declared, because delegation documents served on a name the homeserver does not
answer to point nowhere.

### One role, two databases

MAS keeps its own database beside the homeserver's, owned by the same role.
They cannot share one, because both services define a `users` table. The
deployment this was taken from gives MAS a role of its own; the toolkit does
not, because one app holding one credential is its rule everywhere else and a
second role per app would be a change to the secrets model rather than to this
kind.

## 2026-09-30: Garage provisioning is a separate verb, keyed one per app, and the facts behind it came from running Garage rather than reading about it

### Provisioning is not apply

`apply` renders files and reconciles them against a manifest it wrote itself,
so every check it makes is "does this file match what was rendered". Garage's
cluster layout, its S3 keys and its buckets are not files. They are state
inside a running service that nothing on the workstation describes, reached
only by asking Garage itself. That needed a different kind of planner, so it
is a separate command, `paisans storage init --site <name>`, rather than a
step folded into `apply`: check what a live command reports, and plan only
what is missing, in the order Garage requires.

### One S3 key per app, not one shared key

Each object storing app gets its own Garage key, generated at `init` and
never reused, and that key is granted read/write/owner on that app's bucket
and nothing else. A shared key was the alternative and it was rejected
outright: one key handed to every app would mean any single app's `.env`,
leaked or simply read by an operator debugging it, was enough to read,
rewrite and delete every other app's objects, not just its own. Per app keys
make that blast radius one app wide instead of the whole deployment, and the
integration test added here asserts the isolation directly: a key is allowed
on its own bucket and refused on another app's.

### The media hostname is one per deployment, not one per app

`storage.media_hostname` is a single hostname, and a bucket is a path under
it, rather than a hostname per app. An app's uploads still write through a
mesh address that only this deployment's nodes can reach; the media hostname
is the one address a browser, and a federating server that will never join
the mesh, is ever told about. One hostname is also the only shape that survives
a second Garage site being added later without renaming anything a remote
server has already cached.

### `Host` is forwarded unchanged, because Outline presigns

The gateway's media snippet passes the `Host` header through unrewritten.
Outline's uploads are a presigned POST that the browser submits directly
against the media hostname, and the signature Outline computed covers the
host it signed for. Rewriting `Host` to the upstream address would make every
presigned URL Outline issues fail to verify. Mbin never presigns anything
(it writes to Garage itself, server side, over the mesh, and only the URL it
publishes is the media hostname), so this rule exists for Outline's sake, not
both apps' equally. `docs/specs/2026-09-30-garage-provisioning.md` and its
plan previously said uploads "stay off the gateway" as a blanket claim; that
was true only for Mbin; Outline's uploads cross the gateway by design, and
both documents were corrected to say so.

### The formats, the ordering, and which commands are idempotent all came from running the image

None of the following came from Garage's documentation. All of it came from
running `dxflrs/garage:v1.0.1` by hand, once, while this was built, and again,
automated, in `internal/garage/integration_test.go`:

* an S3 access key ID is the literal `GK` followed by 24 lowercase hex
  characters, and a secret key is 64 lowercase hex characters; anything else
  is refused at `key import` time with a message naming the shape;
* a fresh node's layout must be assigned and applied before any key import or
  bucket command will succeed; before that, Garage answers with "could not
  reach quorum";
* `key import` and `bucket create` both fail loudly when the target already
  exists, which is what makes "was it missing" the right question for a
  planner to ask before running either; `bucket allow` is idempotent and is
  always planned rather than checked first;
* `garage layout show` prints only the first 16 hex characters of a node's ID
  in its table rows, while `node id -q` prints the full one. The integration
  test caught a real bug this produced: matching the full ID against the
  whole combined command output never found a row that was already there,
  so every run replanned the layout step. Fixing the match to use the short
  prefix surfaced a second bug underneath it: `layout show`'s own RPC client
  logs "Connection established to \<node ID\>" for the local node it talks to,
  before it prints the table, and on a single node cluster that is always
  this node's own ID, present or not. Matching against the whole output made
  every fresh node look already provisioned. The fix narrows the match to the
  table between the `==== CURRENT CLUSTER LAYOUT ====` banner and the version
  line, which is the only part of the output that is actually about the
  layout.

The property that matters most and that no unit test can reach is that the
key ID the toolkit generates is the key ID Garage actually holds: if those
ever drift, every upload from that app fails with an opaque signature error
and nothing earlier in the sequence would say a word. The integration test
asserts it by reading the key back out of Garage with `garage key info` and
comparing it against the secrets file, not by comparing the toolkit against
itself. It also proves idempotency against the real thing: a second `Build`
after provisioning plans no `key import`, no `bucket create` and no layout
step. It is behind the `garage_integration` build tag, so the ordinary test
suite needs no Docker, and it is where this knowledge is kept honest against
whatever Garage ships next.

## 2026-10-04: A public bucket is served by Garage's web endpoint, and the gateway rewrites rather than republishes

### The S3 API has no anonymous mode at all

Garage's S3 API refuses every unauthenticated request, with
`403 Forbidden: Garage does not support anonymous access yet`, and there is no
setting, policy or grant that changes it. That is not a gap to be configured
around: a bucket policy is not the mechanism here, because the API has no
anonymous path to apply one to.

Garage's separate web endpoint is the only one that serves an object without a
credential, so the toolkit renders it as `[s3_web]` in `garage.toml` and runs
`garage bucket website --allow` on the buckets that need it. That endpoint
resolves a bucket from the request's `Host` rather than from the path, which
is what makes the gateway's job a rewrite rather than a proxy.

This is why any of it exists: a federating server fetching an image is a
machine with no account. It will not sign a request, and it will not follow a
redirect to a passkey prompt. Outline's reads are different only because
Outline presigns them for a person who is already signed in.

### The gateway rewrites, because remote instances have cached Mbin's URLs

The media hostname carries one route per public bucket. The bucket prefix is
stripped from the path and `Host` is rewritten to the vhost form the web
endpoint expects. The published URL does not change at all.

Publishing the vhost form instead was the alternative and it was rejected.
Mbin's `KBIN_STORAGE_URL` is baked into every media URL it has ever emitted,
and remote instances have recorded those URLs; nothing can recall them. A
change to the published form would break every image already federated, which
makes the published URL a one-way door for the same reason the domain is. A
rewrite at the gateway is reversible and costs one route per bucket.

This narrows, rather than contradicts, the earlier decision that `Host` is
forwarded unchanged. Everything that is not a public bucket still falls
through to the S3 API with `Host` untouched, and that is still load bearing
for exactly the reason given there: a SigV4 signature covers the host it was
signed with, so rewriting the header on the fallback would invalidate every
presigned URL Outline issues. The public bucket routes rewrite it because the
requests they carry are not signed with anything.

### Public is derived from the kind, and the declared field was rejected

`kinds.ServesObjectsPublicly` answers which kinds need this, and there is no
configuration key for it. A `public: true` field on an app was considered and
rejected: it can be set wrong in both directions, and both are bad in ways an
operator would not see. Set on Outline it would expose a private bucket to
anyone who can guess a key, with nothing failing to say so. Left off Mbin it
would silently break federated images, which looks like a remote instance's
problem rather than a local setting.

It is also derived from the kind rather than from "has a bucket", because a
kind can keep objects in S3 and serve them through its own application route,
never exposing the bucket. A future kind in that shape must not acquire a
public route just for having a bucket.

### The internal suffix resolves nowhere, on purpose

`root_domain` under `[s3_web]` is `.web.garage.internal`. It is not a real
name and there is no DNS record and no certificate for it anywhere. The web
endpoint needs a `Host` suffix to resolve a bucket from, and the gateway is
the only thing that ever sends one. A suffix under the community's own domain
would have been a name someone could point at something, and a name that
resolves is a name that can be reached; this one cannot be, from inside the
mesh or outside it.

### What keeps all of it honest

`TestAnonymousFetchReadsMbinsMediaAndNotOutlines` in
`internal/garage/integration_test.go`, behind the `garage_integration` build
tag. It starts both of the fixture's Garage nodes from their own rendered
`garage.toml`, a Caddy from the rendered `media.caddy` snippet and the
rendered image pin, provisions them through the real `garage.Build` and
`garage.Execute`, uploads an object into each bucket with the rendered
credential and a hand signed SigV4 request, and then makes three fetches
through the gateway with `Host` set to the media hostname:

* Mbin's object with no credential of any kind returns `200` and the object's
  bytes;
* Outline's object with no credential is refused with `403`, and the absence
  of the object's bytes is asserted alongside the status, because a `200`
  carrying an error document would pass a status check;
* Outline's object presigned returns `200` and the bytes, which is the
  property the fallback route exists for and the one most likely to break now
  that the routing has a branch.

Two facts about the rendered configuration fell out of running it rather than
reading it. The fixture declares replication 2, and `dxflrs/garage:v1.0.1`
refuses to apply a layout whose node count is below the replication factor, so
the rendered `garage.toml` cannot be provisioned on one node at all and the
test runs two. The per node `Build` and `Execute` reflect that: the first
site's layout apply is refused while only its own role is staged, the staged
role survives the refusal, and the second site's pass applies both at once.
Nothing in `garage.Build` stages both nodes before applying, and nothing in it
joins a node to a cluster either; a real deployment's nodes find each other
over the mesh, and the test does that join itself.

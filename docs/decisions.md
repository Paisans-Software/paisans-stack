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

Terraform *is* right for provisioning the cloud instance itself: create the
machine and its firewall rules. That should stay **optional**, because most
operators will click a button in a provider's console and requiring otherwise
raises the floor for no benefit.

DNS records used to sit in that list, left to the operator. They no longer do:
`paisans dns init` derives them from the configuration and creates them through
the DNS provider's API, because a record typed by hand is one nothing checks
against the configuration. Founder requirement. See *`dns init` creates the
records a deployment needs* in `README.md`.

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
  planner to ask before running either; `bucket allow` and `bucket website
  --allow` are idempotent, and were planned on every run until real `bucket
  info` output had been seen. It prints `Website access: true` and an
  `Authorized keys:` section of `RWO  <key ID>  <key name>` rows, possibly
  after ANSI coloured log lines, so each step is now planned only when the
  website flag is not `true` or the app's key (by ID or name) lacks one of R,
  W and O. Output that does not parse plans both and the plan says so: a
  repeated set costs nothing, a missed grant leaves the app unable to write;
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
joins a node to a cluster either. The test does that join itself, with `garage
node connect`, and that is a step the toolkit never plans: nothing it renders
carries `bootstrap_peers`, Consul discovery or Kubernetes discovery, and a
shared `rpc_secret` authenticates a peer rather than finding one. Two nodes
started from the fixture's own rendered `garage.toml` files, on the mesh
subnet, with that shared secret and no `node connect`, each list only
themselves in `garage status`.

The consequence is operational rather than theoretical. **A multi site Garage
deployment needs a manual `garage node connect` before `paisans storage init`
can converge.** Without it each node is alone, so both sites' `layout apply`
is refused for a node count below the replication factor, and `Execute` stops
at the first failing step: no key, no bucket and no website grant is ever
created. This is written down in `README.md`, in the `storage init` section,
because a decision record is not where an operator looks for an instruction.
Fixing the planner so it plans the join is deliberately out of scope here.

## 2026-10-04: The `writefreely` kind now means the writefreely-wisp fork, not upstream

### This is a breaking change for a public toolkit

`writefreely` used to mean upstream WriteFreely, which has no Postgres driver
and no S3 support: it kept its data in a SQLite file in its own data
directory, and that was the whole reason the kind could only be pinned. The
kind now renders the [writefreely-wisp](https://github.com/josephquigley/writefreely-wisp)
fork instead, which adds both. Its `config.ini` carries `[database] type =
postgres` and `[storage] type = s3`, so the same `paisans.yaml` that used
to render a SQLite file now renders a Postgres connection and an S3 bucket,
and the blog can join the cluster like any other app.

Nobody using this toolkit asked for that, and there is no way to opt out of
it short of pinning an older release of the toolkit itself: the `writefreely`
name did not change meaning for a new deployment only, it changed meaning for
every existing one the next time `paisans apply` runs. That makes it a
breaking change, and because `paisans-stack` is public and handed to people
we do not know, it is documented in the open, in `README.md`, in the section
listing what each kind means, rather than left for an adopter to discover
from a container that will not start.

### A second kind for the fork was the rejected alternative

Adding `writefreely-wisp` as its own kind, leaving `writefreely` alone, was
considered. It was rejected because the two are not a stack and a variant of
a stack in the way, say, Mbin and a possible future fork of it would be: the
wisp fork is the only WriteFreely this toolkit can run correctly going
forward, since upstream's image actively rejects the configuration an
operator would otherwise want (Postgres and S3, instead of a SQLite file one
node owns). Keeping both names would mean the toolkit ships a kind, `writefreely`,
that is known to be the wrong choice for anyone who can use the other one,
with no way for validation to say so. One name that means the better thing is
simpler to maintain and to document than two names where one is a trap.

### The image is an unreleased `develop` build, pinned by digest

No tagged release of the fork carries Postgres or S3 support yet; both exist
only on its `develop` branch. The catalogue pins
`ghcr.io/josephquigley/writefreely-wisp@sha256:4d21f45879bd98c8485eb8169ea57fbab925f0cbd5a38ac3c3bdd79901d809ea`
rather than a tag, because `develop` is a moving branch and a tag that tracked
it would silently change what an existing deployment runs on its next
`apply`. The digest was resolved against ghcr.io on 2026-10-04 with `docker
manifest inspect`, which returned a multi architecture index covering
linux/amd64 and linux/arm64, and confirmed by pulling the amd64 manifest and
checking the binary with `strings` for the `s3_secret_access_key` ini tag and
the Postgres `sslmode` field. The pin should move to a tagged release's
digest once one exists, and the comment in `internal/kinds/kinds.go` says so.

### The fork serves its own images, so its bucket stays private

The fork's `imagestore.go` streams uploads through its own `/uploads/` route
with `http.ServeContent`, from whichever store is configured, and never
emits or presigns an S3 URL. That means nothing anonymous ever needs to reach
the bucket directly, unlike Mbin's: `kinds.ServesObjectsPublicly` answers
false for `writefreely`, so the blog's bucket gets no `garage bucket website
--allow` and no route on the media hostname. The rendered `config.ini` says
this in a comment, next to `[storage]`, because that is where the next
person reading it will be standing and wondering why the blog is absent from
the media routing everything else gets.

## 2026-10-04: A deployment can pass a config key through, and the toolkit places it without interpreting it

The design is `docs/specs/2026-10-04-config-passthrough.md`. This records what
was decided and what running it changed.

### `settings` only reaches keys a template already reads

Every value in `settings` works because some template asks for it by name. A
key no template reads is silently ignored, so an adopter who wanted
WriteFreely's `app.max_blogs` or one more variable in Mbin's environment had
two options: fork the templates, or edit the rendered file on the host and have
the next `apply` refuse the whole stack over a local modification. `config` is
a second per app map for exactly that gap. The toolkit places each key in the
file the kind renders, in that file's syntax, and has no opinion about it.
`settings` keeps meaning what it meant.

### The merge runs in Go after rendering, not as a trailing block in each template

A block appended by each template was the alternative, and it fails three ways:
it cannot nest a yaml path, it cannot put an ini key inside a section that
already exists, and it cannot notice that the template already wrote the same
key. All three matter, the last most, because noticing is what makes the
collision refusal below possible. Env and ini are edited as lines rather than
parsed and re-emitted, because a full round trip would reformat a file an
operator reads.

### Ini inserts into the existing section rather than repeating it

Appending a second `[app]` at the end would have been simpler. Whether a
repeated section merges with the first or shadows it is a property of the
particular parser, and this project has been wrong about a format's behaviour
more than once by reasoning instead of running. go-ini v1.67.0, the version the
fork pins, does merge a repeated section, but inserting after the last
assignment of the section that is already there needs no assumption about it
at all, so the design does not lean on that. The key lands after the section's
last assignment rather than before the next header, so the comment that
introduces the next section stays with it.

The values go-ini would misread are refused rather than escaped, each observed
by loading the writer's own output with that library: `;` or `#` anywhere, edge
whitespace, a leading quote or backtick, a newline, and a trailing backslash,
which joins the next line onto the value and makes the key after it vanish.
`DEFAULT` is refused as a section because go-ini merges it into the lines
before any header. A section name with a dot, `[oauth.generic]`, cannot be
reached, because a key names exactly one section and one key.

### The ini insertion was run against the fork, and that found the fork's own limit

`internal/render/writefreely_integration_test.go`, behind the
`writefreely_integration` tag, boots the pinned fork image against the
`config.ini` that `render.Build` produces from the fixture, with a Postgres
reachable under the rendered `host` and carrying the rendered credentials, and
asks `writefreely settings get app.max_blogs`. It reports `3`. The same file
less only the inserted line reports `0`, which is what makes the first result
evidence rather than a constant. No line of the rendered file was changed for
the boot: the S3 endpoint is unreachable in the test and the fork logs that and
starts without uploads, and it starts without reaching the identity provider.

It has its own tag rather than `garage_integration` because it touches no
Garage and its helpers belong beside the fixture in `internal/render`.

The run also showed something the spec did not anticipate. The fork moves most
of `[app]` and `[uploads]` into its database on the first start against an empty
database, and after that the database is in force. Started once more against
that existing database with the key added, the fork logged `config.ini
disagrees with the database on app.max_blogs; the database value is in force`
and still reported `0`. The test asserts that as well, because the README
states it and a fork release that changed it should fail here rather than leave
the README wrong. A passthrough key for those sections therefore takes effect
on a new blog only. That restart is the only one observed.

What follows was read from the fork's source at `ff9dcebe`, the revision the
pinned image's label names, and was not run. `importSettings` in
`settings_runtime.go` is commented as running "at every start", so the
disagreement line is expected on every boot rather than only the one seen.
`cmd/writefreely/settings.go` has `writefreely settings set <name> <value>`,
described as "running servers apply it on their next request"; it looks like
the way to change a running blog's setting, and nobody has used it here. The
drift check covers every database-bound key present in the file, which reads
as though the template's own keys in those sections would be ignored the same
way after the first start; that is untested too. This toolkit does not try to
reconcile the two.

On the boots that were run, the rendered compose file mounts `config.ini` read
only, and the fork logged that it could not add its `settings_location` marker
to the file or strip the moved keys from it. The fork's `docker-entrypoint.sh`
says "A read-only config.ini is fine: the keys are logged and ignored", which
is the fork's own claim and was not tested further here. It was left alone.

### Yaml appends a new top level key, and re-emits only under an existing mapping

`yaml.v3`'s node API keeps comments through a parse and re-emit, but not the
blank lines between top level keys. So a key whose top level name is absent
from the rendered `homeserver.yaml` is marshalled as its own block and appended,
and the rendered text is left byte for byte as it was. Only a key under a
mapping the template already writes, such as `database.args.sslmode`, makes the
file be re-emitted. Measured on the golden `homeserver.yaml`, that drops six
blank lines and nothing else: every comment, the indentation, the quoting and
the flow sequences survive. Restoring the blank lines safely needs care around
block scalars and the loss is cosmetic, so it was not attempted. The common
Synapse keys an adopter reaches for are new top level names and take the byte
identical path.

A dot in yaml or json is always a path separator, so a new key whose own name
contains one, such as Element's `setting_defaults` key `UIFeature.feedback`,
cannot be expressed. A path that could mean an existing dotted key, such as one
under `default_server_config.m.homeserver`, is refused as ambiguous rather than
silently creating a nested `m` object beside it.

### A collision is refused, not overridden

`config-key-already-rendered` stops the build when the template already writes
the key, and names the `settings` key that owns the value when there is one.
Overriding was the alternative and was rejected for two reasons. Two sources of
truth for one value is how a deployment ends up with a setting nobody can
locate. And a passthrough that could override would route around decisions the
templates encode: `database.type = sqlite3` into a blog that belongs in the
cluster is refused because the template writes `type = postgres`. The cost is
real and stated in the README: a template owned default cannot be changed
through `config`, only by a `settings` key or a template change, both of which
are reviewable.

### A key a template leaves out on purpose is refused the same way

A collision check alone does not stop `oidc_providers` going back into a
homeserver that is supposed to be a resource server, because the synapse
template deliberately writes no identity provider and no password database:
there is nothing to collide with. The final review found that
`oidc_providers`, `oidc_config.enabled` and `cas_config.enabled` all rendered.

So a key a template omits by decision is treated as owned by that template and
refused under the same rule name, with a message saying the template leaves it
out on purpose and why. One rule name keeps the README to one entry. The
entries are limited to keys the template's own comments name as deliberately
absent, so the table records decisions rather than guesses:

* synapse: `oidc_providers`, `oidc_config`, `saml2_config`, `cas_config`,
  `jwt_config` and `password_config`. The template's header says it
  "deliberately declares no identity provider of its own and no local password
  database"; these six are Synapse's blocks for those, read in its
  configuration manual at v1.160.0. The match is on the top level segment, so
  `oidc_config.enabled` is caught and `oidc_providers_note` is not.
* writefreely: `uploads.dir`, which the template's `[uploads]` comment calls
  "deliberately absent" because uploads go to S3.

No other template names a deliberately absent key, so no other kind has
entries. `registration_shared_secret`, which the synapse kind also never
renders, is not in the table because the template's comments do not name it;
the credential name check refuses it in `validate` before rendering.

For env kinds the check covers the stack's `compose.yaml` as well. Docker
Compose v5.4.0 was run on the operator's Mac, with the version on any host not
checked, with the same variable in `env_file` and in `environment:`, in both
map and list form, and `docker compose config` showed `environment:` winning
and the `env_file` value dropped without a warning. So a
variable `compose.yaml` sets under `environment:`, or interpolates as `${VAR}`,
is refused too. The interpolation case over-refuses on purpose: the only
references today are `${POSTGRES_PASSWORD:?...}`, which the credential check
already refuses.

### The secret check is by name, and says so

`config-key-looks-like-a-secret` lowercases a key, removes `_`, `-` and `.`,
and refuses it if what is left contains `password`, `secret`, `token`, `apikey`
or `privatekey`, because `paisans.yaml` is plaintext and committed and a
passthrough map is where a token gets pasted at the end of a long day. The
separators are removed because the first version matched `apikey` and
`private_key` as written, and the final review found `SENDGRID_API_KEY` and
`privateKey` passing; the env spelling is the likeliest real case. It will not
catch a credential called anything else, and the message and the README both
say that, the same way `acme-image-is-stock-caddy` refuses the one thing it can
prove instead of claiming to be a policy. It also refuses some innocent names,
such as WriteFreely's `app.disable_password_auth`, and the README says so.

### An env key that would configure compose is refused

`apply` runs `docker compose -f /srv/<app>/compose.yaml up -d`, so the stack's
`.env` is also the file compose reads for its own settings. With `docker compose
config` on Docker Compose v5.4.0, on the operator's Mac and with no container
started, `COMPOSE_PROJECT_NAME=other` in that `.env` renamed the project from
`probe` to `other`, and `COMPOSE_PROFILES=hidden` made a service behind that
profile active. The run was made from another directory with `-f`, as `apply`
does. That `apply` would then address different containers from the ones it
started before is reasoned from the renamed project, not run.

`config-key-steers-compose` refuses an env kind's key starting `COMPOSE_` or
`DOCKER_`. `DOCKER_HOST` in the same `.env` did not redirect `docker compose
ps` in that run, while the same variable in the shell did. `DOCKER_` is refused
anyway as the docker CLI's own namespace, so the rule does not depend on
knowing which of those compose honours from a file; no application variable is
known to need that prefix.

### The refusal for a kind with no config file was dropped

The spec called for refusing `config` on a kind that renders no config file.
Every kind in the catalogue renders one, so the refusal had nothing to refuse,
and a rule no input can reach cannot be shown to fail. It
was left out rather than kept for a kind that does not exist. A future kind
without a config file has to add it back.

### What is proven, and against what

The ini merge is proven against the application that reads it, above. The env
merge's quoting is proven against `docker compose config`, below. The yaml and json
merges are proven against Go's own parsers only: the tests show the rendered
file parses and carries the value at the requested path. Synapse reads
`homeserver.yaml` with Python and Element reads `config.json` in a browser, and
nothing here runs either. That is a known gap, not a claim. One instance of it
was found by reading rather than running: Synapse 1.160.0's
`synapse/config/repository.py` raises a `ConfigError` for `url_preview_enabled`
unless a list valued `url_preview_ip_range_blacklist` is also set, and a list is
not a value `config` can carry, which is why the fixture uses
`require_auth_for_profile_requests` instead.

### Env values are quoted only where compose would rewrite them

Run with `docker compose config` on Docker Compose v5.4.0, on the operator's
Mac; the version on any host was not checked, and no container was started.
`TestEnvQuotesWhatComposeWouldOtherwiseRewrite` pins the line forms that run
chose. What compose did to a hand written `env_file` line:

| Line in the file | What compose delivered |
|---|---|
| `K=a b` | `a b` |
| `K=a #b` | `a`: a space then `#` starts a comment |
| `K=a#b` | `a#b` |
| `K=$HOME/x` | the value of `$HOME`, then `/x` |
| `K="quoted"` | `quoted`: the quotes are stripped |
| `K=trail  ` and `K= lead` | `trail` and `lead`: edge whitespace is trimmed |
| `K=a\nb` unquoted | the four characters `a\nb` |
| `K='...'` | literal, for a space, `#`, `$`, `"` and backslash |
| `K="a\nb"` | `a`, a real newline, `b` |
| `K="a\$b"`, `K="a\\b"`, `K="say \"hi\""` | `a$b`, `a\b`, `say "hi"` |

So a value is written bare, the templates' style, when it has no leading or
trailing whitespace and none of `#`, `$`, `'`, `"`, backslash, tab or newline.
Otherwise it is double quoted, with backslash, `"`, `$` and newline escaped,
the four escapes compose undid inside double quotes. A carriage return or other
control character is refused, since no form was observed to carry one.

A file written by `configmerge.Env` itself was then read back the same way,
with 24 values covering each of those cases plus empty, numbers, booleans,
`=`, a URL with `#` and `&`, and non-ASCII text. Every value compose delivered
matched the Go input, after undoing the `$$` compose prints for a literal `$`
in its output. Reading `$$` as that escape is an inference from the output
being consistently doubled, not documented behaviour that was looked up.

## 2026-10-04: Each app's objects are served on a media hostname of its own

Founder decision. The design is the amendment *one media hostname per app* in
`docs/specs/2026-10-02-serving-public-objects.md`. This supersedes two earlier
entries: *The media hostname is one per deployment, not one per app*, under
the Garage provisioning decision, and the per bucket routing under *A public
bucket is served by Garage's web endpoint*.

### A hostname can move and a path cannot

Each app that stores objects gets `<label>-media.<domain>`, a sibling of its
own hostname, and can name another under `hostnames.media`. A hostname is the
unit DNS re-points: one app's media can move to a CDN, another provider or
another Garage without touching any other app. A bucket as a path under one
shared hostname could only move with all of them. Two hostnames are also two
browser origins, so one app's user uploads cannot script against another's.

The single hostname was kept before because remote instances would have cached
URLs in that form. Checked rather than assumed: `git tag` lists nothing, and
`origin/main` carries none of the media work, so nothing has been released and
no deployment has published a URL in either shape. There was nothing to keep
working, so the old hostname was removed rather than kept as a legacy route.

### A sibling, not a child, and not a backend name

`media.talk.example.org` was rejected because a cookie the app scopes to its
own host reaches every name under it. `garage.` and `s3.` were rejected
because they name what answers rather than whose objects these are, and what
answers is exactly the part that is meant to change. Both rules are refusals
in `validate`, alongside a hostname shape check, a declared name outside the
domain, and a derived name colliding with any other hostname.

The config key is a role in the existing `hostnames` map rather than a new
field, because a role is already what selects a hostname's snippet and what
the duplicate check walks. It differs from `wellknown` in one way: it exists
whether or not it is declared.

### Public is per kind, and the blog joined Mbin

writefreely-wisp#171 makes the fork's S3 storage direct only: it requires
`[storage] image_url_base` and never serves an image from the bucket itself.
`kinds.ServesObjectsPublicly` is therefore true for `writefreely` as well as
`mbin`, and the earlier entry *The fork serves its own images, so its bucket
stays private* no longer describes the fork. Outline stays private, behind its
own media hostname on the S3 API with `Host` unchanged.

The pinned fork image predates #171 and ignores `image_url_base`, which was
observed rather than assumed: `TestRenderedPassthroughKeyIsReadByWriteFreely`
boots that image against the rendered `config.ini`, key included. Until the pin
moves, the blog streams its own images and its media hostname carries nothing.

### No gate, and headers on the public ones

Media hostnames are public to anyone with the link and never gated, including
for an app whose own hostname is gated, because ActivityPub servers hotlink the
original URLs. Public media hostnames send `Content-Security-Policy:
default-src 'none'; style-src 'unsafe-inline'; sandbox` and
`X-Content-Type-Options: nosniff`, because the apps store SVGs byte for byte
and no S3 provider can store a header on an object. Outline's sends nosniff
only: its PDF previews are an `<embed>` of the attachment, and the sandbox
would stop the browser's PDF viewer. That consequence is reasoned, not
observed in a browser.

### Outline's bucket name is checked against its media URL

Outline v1.10.0 treats the bucket name appearing anywhere in
`AWS_S3_UPLOAD_BUCKET_URL` as virtual host addressing, by substring, and then
drops the bucket from the upload path while Garage needs it there. A hostname
per app makes that reachable (bucket `docs` on `docs-media.example.org`), so it
is refused as `outline-bucket-in-media-url`. Read from Outline's source, not
run against Outline.

### What was run

`TestEachAppsMediaHostnameServesOnlyItsOwnBucket`, behind the
`garage_integration` tag, against two `dxflrs/garage:v1.0.1` nodes from their
rendered `garage.toml` and a Caddy running the three rendered media snippets.
Mbin's and the blog's objects returned 200 anonymously with exactly one copy of
each header; Outline's returned 403 anonymously and 200 presigned for
`docs-media.example.org`; Outline's key asked for on `talk-media.example.org`
returned 404. With a `header_up Host` added to Outline's snippet, the presigned
fetch failed with Garage's `Invalid signature`, so the test can fail.

The Caddy was `ghcr.io/paisans-software/caddy:2.11.4`, through the
`PAISANS_TEST_CADDY_IMAGE` override this change adds, not the rendered 2.11.6
pin: that image is in a registry that needs a login the workstation did not
have. The snippets use only stock directives, none from the modules that image
adds, but it was not the pinned bytes.

---

## 2026-10-08: The admin reconciler adds Pocket ID administrators to `admins` on its own

Founder decision. The design is `docs/specs/2026-10-08-admin-reconciler.md`.

### What was decided

The `pocket-id` kind runs an admin reconciler beside every Pocket ID instance. Every
two hours it adds each enabled, non LDAP Pocket ID administrator who is not in
the `admins` group to it, and its `/healthz` fails while `admins` has fewer
than two members. It is always on, in every deployment, and the group is
always named `admins`.

### Why an unattended write is acceptable here

An agent needs a human's approval for every Pocket ID group change. The
reconciler is not an agent but a service the deployment runs, and is approved
once, as a whole, under *Services the deployment runs* in
`docs/deployment-agent-rules.md`, added in the same change. It qualifies
because Pocket ID administrators are the community's administrators: putting
them in `admins` changes nobody's power, only which apps recognise it. The
approval is exactly that write. The reconciler never removes a member, never
changes a user and never touches a group, its code is the only thing that
keeps it so (Pocket ID's API keys carry no scopes), its tests fail on any
other request that writes, and it logs every write. It gives an agent no
authority to make the same change by hand.

### What was run

`TestRealPocketIDAdministratorIsAdded`, behind the `pocketid_integration` tag,
against `ghcr.io/pocket-id/pocket-id:v2.14.0`: one administrator in `admins`,
one in `editors` only. One pass left the second in both groups, reported two
members, and left the static API key's user, an administrator with the fixed
ID `00000000-0000-0000-0000-000000000000`, out of the group and out of the
count. The standby wrapper's integration tests were run with the shared marker
added: present on standby, gone after a stop.

The image `ghcr.io/paisans-software/admin-reconciler:0.1.0` was built locally from
`cmd/admin-reconciler/Dockerfile`; it is published by tagging `admin-reconciler-v0.1.0`,
and the kind's reference moves to its digest once it exists.


---

## 2026-10-08: Services the deployment runs are approved once, not write by write

Founder decision. The rule is *Services the deployment runs* in
`docs/deployment-agent-rules.md`.

### What was decided

The blast radius tiers govern actions an agent takes. A service the deployment
runs that makes a change those tiers gate, such as the admin reconciler's
write to `admins`, is approved once, as a whole, when a human approves its
design. That approval stands while every kind of write it makes is named in
its design and a decision record, its tests fail on any other write, and it
logs every write. Adding or widening a write is a new decision, in the tier
of the strictest write it would make.

### Why

The context differs. An agent decides at run time, from whatever it was told,
so each gated change needs a human at that moment. A service decides nothing:
what it may change is fixed in reviewed code before it runs, and the same
change happens the same way every time. Asking for approval per write would
put a human in front of a decision that was already made, and would make an
unattended service impossible.

The service grants an agent nothing. An agent making the same change by hand,
with the service's credential, or by changing what the service reads, is an
agent's action and is in the tier of the change itself.

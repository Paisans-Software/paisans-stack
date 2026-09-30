# Garage is provisioned, and the apps that store objects can serve them

Status: approved in session on 2026-09-30, not implemented.

This spec covers one change with three halves: the toolkit generates S3
credentials Garage will actually accept, a new command creates the cluster
layout, the keys and the buckets on a host, and the two applications that store
objects gain a public URL from which those objects can be served.

## Why

Three defects, found by running the pinned Garage image rather than by reading
about it. None of them is visible locally, because nothing in the toolkit has
ever talked to a Garage node.

**The generated credentials cannot work.** `secretsgen` fills
`storage.garage.access_key_id` and `storage.garage.secret_access_key` with
`password()`, which is 32 random bytes in base64. Garage refuses both:

```
Error: Invalid key format: The specified key ID is not a valid Garage key ID
(starts with `GK`, followed by 12 hex-encoded bytes)
Error: Invalid key format: The specified secret key is not a valid Garage
secret key (composed of 32 hex-encoded bytes)
```

**Nothing provisions Garage.** The toolkit renders `garage.toml` and runs the
container. It never assigns a cluster layout, never creates a bucket, and never
imports a key. A node with no layout serves nothing: every S3 request fails, and
so does any attempt to create a key, with `Could not reach quorum of 1`.

**Neither application can serve what it stores.** `KBIN_STORAGE_URL` is not
rendered at all, and Mbin builds every media URL and every Liip Imagine
thumbnail root from it. Outline is handed `http://<mesh-ip>:3900` as its bucket
URL. A browser on the internet cannot reach a mesh address, and a federating
instance that caches such a URL keeps it.

The three compound: fixing the key format alone produces a node with no layout;
provisioning alone produces buckets whose contents nobody can fetch.

## What was established by running it

Every claim below was produced by driving `dxflrs/garage:v1.0.1`, the image the
toolkit pins, and is recorded because the design depends on it.

* A key ID is `GK` followed by 24 hex characters. A secret is exactly 64 hex
  characters. Neither is negotiable and neither is what `password()` produces.
* **The layout must be applied before any key or bucket operation.** Before it,
  those commands fail on quorum rather than on their own merits.
* `garage key import` and `garage bucket create` **fail when the object already
  exists**. `garage bucket allow` is idempotent and can be repeated.
* The working sequence is `layout assign -z <zone> -c <capacity> <node-id>`,
  `layout apply --version <n>`, `key import <id> <secret> --yes -n <name>`,
  `bucket create <name>`, `bucket allow --read --write --owner <bucket> --key
  <key>`.

That third point is the one that shapes the command: a provisioning step that
is not check-then-act cannot be run twice, and "a second run reports no
changes" is the property this toolkit is built around.

## Decisions taken

**Provisioning gets its own verb rather than joining `apply`.** `apply`
renders files, compares them against a manifest and restarts containers, and
its refusal to touch a file it did not write is the whole basis for trusting it
on an adopted host. Creating a bucket is a different kind of act: it mutates a
running service's internal state, which no manifest describes and no diff can
show. Folding it into `apply` would mean a failed provision leaves `apply`
half-done with nothing to compare against. The rejected alternative was one
command for the operator to remember; the cost accepted is two.

**One key per application, not one shared key.** Today every app is handed the
same credentials, and each bucket would be granted to that key with `--owner`.
That makes Mbin's `.env` sufficient to read, rewrite and delete every document
Outline ever stored. Per-app keys make a compromised app's blast radius its own
media. The cost is one more pair of secrets per app and a longer `init` report.

**A single media hostname for the deployment**, not one per app and not a path
on each app's own hostname. One route, one certificate, every app's objects
publicly fetchable. Per-app hostnames were rejected as a certificate and DNS
entry per app for a benefit nobody needs yet; a path on each app's hostname was
rejected because the gateway would have to rewrite paths into bucket keys and
the route would collide with Mbin's own routing, which is fragile in the place
that is most expensive to change later.

The adoption design deferred Mbin's media hostname as needing multi-hostname
support and a decision of its own. That support now exists, and this is the
decision.

## Configuration

The `storage` block already exists and already carries `garage.sites` and
`garage.replication`. It gains two fields rather than a new block:

```yaml
storage:
  garage:
    sites: [home-a]
    replication: 1
    capacity: 100G
  media_hostname: media.example.org
```

`capacity` sits under `garage` because it is a property of that node's role in
the Garage cluster, beside `replication`. `media_hostname` sits above it
because it is what the deployment publishes rather than how Garage is laid out,
and because a future backend that is not Garage would keep the hostname and
lose everything under `garage`.

`storage.media_hostname` is **required once any app uses object storage and a gateway
site exists**, and is a structural error in `config` rather than a policy
refusal, because it is a missing required field rather than an incoherent
combination.

`storage.garage.capacity` is what the node advertises to Garage's layout. Garage requires a
unit suffix. It defaults to `100G` when unset, which is a number an operator
should revisit and not a number the toolkit can know.

## Secrets

`storage.garage.access_key_id` and `storage.garage.secret_access_key` are
removed. `storage.garage.admin_token` and `storage.garage.rpc_secret` stay:
they are Garage's own credentials rather than S3 ones.

**Corrected 2026-09-30.** This paragraph used to assert that both formats were
unconstrained, and that sentence is why nobody checked. Only `admin_token` is:
Garage takes it as given and a base64 password starts fine. `rpc_secret` is
parsed as a hex encoded 32 byte key, and a base64 value stops the node at
startup with `Invalid RPC secret key: expected 32 bits of entropy`. It is
therefore generated by the same hex generator as the S3 secret key, and
`render` refuses a site with a Garage role and no `rpc_secret` rather than
falling back to `admin_token`, which could only ever produce a file Garage
will not start against.

Every app whose kind uses object storage gains two generated entries:

```yaml
apps:
  talk:
    s3_access_key_id: GK...
    s3_secret_access_key: ...
```

Two new generators sit beside `password()`, producing the exact formats above.
`password()` is not reused, and the comment says why, because base64 is
precisely what Garage rejects.

`kinds` gains `UsesObjectStorage(kind)`, true for `mbin` and `outline` and
false for the rest. Synapse keeps its media on local disk by design, and the
other four store no objects at all.

## Validation

* **`garage-key-is-malformed`**: an S3 key or secret that does not match
  Garage's format. Generated values cannot trip it; a hand-edited secrets file
  can, and the failure it prevents is a provisioning run that dies halfway
  through with a message about hex encoding.
* A missing `storage.media_hostname` while an object-storing app exists is the
  structural error described above.

## Render

* The gateway renders one host block for `media_hostname`, reverse-proxying to
  Garage on port 3900 at the site holding the storage role. **It is never
  gated**: a federating server fetching an image is a machine, and it will not
  follow a redirect to a passkey prompt.
* **The `Host` header is forwarded unchanged.** This is load bearing rather
  than incidental: Outline presigns URLs, the signature covers the host that
  signed them, and rewriting the header to the upstream address invalidates
  every signature it issues. Garage treats a host it does not recognise as a
  vhost as a path-style request, which is why path-style addressing stays on
  for both apps.
* Mbin's `.env` gains `KBIN_STORAGE_URL=https://<media_hostname>/<bucket>`.
* Outline's `AWS_S3_UPLOAD_BUCKET_URL` becomes the same public base rather than
  the mesh endpoint.
* Each app's `S3_KEY`/`S3_SECRET` and `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`
  come from that app's own pair rather than the shared one.

The endpoint an app *writes* through stays internal only for Mbin, not for
both apps. Mbin writes to that endpoint itself, server side, over the mesh,
and only the URL it publishes is public, which keeps its uploads off the
gateway. Outline's server never writes bytes to S3 at all: it issues a
presigned POST that the browser submits directly against the media hostname,
so for Outline every upload, not just every read, already crosses the
gateway.

## `paisans storage init`

```
paisans storage init --site <name> [--config paisans.yaml] [--secrets secrets.enc.yaml]
                     [--ssh <destination>] [--execute]
```

It reuses `apply`'s transport, and like `apply` it writes nothing without
`--execute`: the default run reports what it would create.

1. `garage status`. If this node holds no role: `garage node id -q`, then
   `layout assign -z <site name> -c <capacity> <node id>`, then `layout apply
   --version <next>`.
2. For each app whose kind uses object storage, in a deterministic order:
   `key info` and import when absent, `bucket info` and create when absent,
   then `bucket allow --read --write --owner` unconditionally, since it is the
   one step that is safe to repeat.

It reports what it created against what was already there, and a second run
reports nothing to do.

**It never invents a credential.** The keys come from the secrets file, which
is what makes the key inside Garage and the key inside the app's `.env` the
same by construction rather than by luck.

## Tests

* Unit tests over a fake transport with canned `garage` output, covering a
  fresh node, a fully provisioned one, and a half provisioned one where the key
  exists and the bucket does not. The half provisioned case is the one that
  matters: it is what a failed first run leaves behind.
* Format tests for both generators.
* Golden tree coverage for the media host block, `KBIN_STORAGE_URL` and
  Outline's bucket URL.
* **One integration test behind a build tag**, driving the real
  `dxflrs/garage:v1.0.1` container through the whole sequence. Every fact in
  this design came from running that image and none from its documentation, so
  a test that runs it is the only thing that will notice when a future version
  changes one of them.

## What is not verified

**Outline's presigned URLs have not been exercised end to end.** The reasoning
is that a signature covers the host it was signed with, so the proxy must
forward `Host` unchanged, and Garage will then treat the request as path style.
That is read from how SigV4 works rather than observed against a running
Outline, and it is the single claim here most likely to be wrong. The
integration test above is where it gets settled.

## Out of scope

* Replication beyond a single node, and the layout changes a second site needs.
  That belongs to `site add`.
* Migrating existing media into Garage, which is Phase 2 of the adoption work
  and is a data migration rather than a toolkit feature.
* Backups of Garage's contents, which are absent for several stacks already and
  are tracked separately.

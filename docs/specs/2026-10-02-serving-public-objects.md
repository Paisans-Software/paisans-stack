# Public objects are served, and private ones stay private

Status: approved in session on 2026-10-02, not implemented.

The toolkit provisions Garage, hands each application its own credentials and
publishes a media hostname. Outline's objects are readable through it and
Mbin's are not, because Garage's S3 API has no anonymous mode at all. This
makes Mbin's media readable without making anything else readable.

## What the previous branch left

`feat/garage-provisioning` ended with a documented gap. Mbin publishes
`KBIN_STORAGE_URL` as a plain unsigned URL, a browser fetches it anonymously,
and Garage answers:

```
403 Forbidden: Garage does not support anonymous access yet
```

Outline is unaffected, because its server presigns every read and the browser's
request is therefore authenticated. So one of the two applications works today,
and the one that does not is the one whose URLs remote instances cache
permanently.

## What was established by running it

Every line below came from driving `dxflrs/garage:v1.0.1`, the image the
toolkit pins, with a real credential and real HTTP requests. None of it is from
documentation, and two earlier designs in this project were wrong because they
were.

| Request | Result |
|---|---|
| Anonymous GET, S3 API on 3900, path style | **403** |
| Anonymous GET, `s3_web` on 3902, `Host: talk-uploads.web.example.org` | **200** |
| The same, against a bucket with no `bucket website --allow` | **404** |
| Presigned GET, S3 API on 3900, `Host` forwarded unchanged | **200** |
| Anonymous GET, `s3_web`, `Host` matching a global bucket alias | **200** |

Four consequences, and the design is just these four facts arranged:

1. **`s3_web` is the only anonymous path, and it resolves a bucket from the
   request's Host** rather than from a path prefix. A path style URL therefore
   has to become a vhost style request somewhere, and the gateway is the only
   component positioned to do it.
2. **Website access is per bucket and opt in inside Garage.** A bucket nobody
   allowed answers 404 on `s3_web`, so Outline's bucket is unreachable there by
   construction rather than by our carefulness.
3. **Outline still needs the S3 API.** Its presigned reads carry a signature
   `s3_web` ignores, and they succeed on 3900 when `Host` is forwarded
   unchanged. So the gateway cannot simply point the media hostname at 3902:
   it must route per bucket.
4. **The rewritten Host never leaves the gateway.** `s3_web`'s `root_domain` is
   ours to choose, so it can be a suffix that resolves nowhere. No DNS record,
   no certificate, nothing public, and nothing for an adopter to set up.

## Decisions taken

**Which buckets are public is derived from the kind, with no configuration
key.** `kinds` gains `ServesObjectsPublicly`: true for mbin, because federation
requires remote servers to fetch media anonymously, and false for outline,
because its bucket holds document attachments.

The rejected alternative was a declared `public_objects` field with a refusal
blocking it for kinds that hold private data. That was this spec's first shape,
and it was discarded after the probe showed website access is already per
bucket and opt in: a declared flag would add a field, a refusal and a way for
an operator to talk themselves into exactly the mistake the refusal exists to
catch. There is no line to edit, so there is no line to get wrong.

The cost is stated rather than hidden: a fork whose application needs public
objects and is not Mbin has to change Go code rather than configuration. That
is the right trade while two kinds store objects and one of them is private.

**The media hostname keeps its shape.** `https://media.example.org/<bucket>/<key>`
is unchanged, which is the whole point: Mbin has already written URLs of that
form into posts, and a federating instance that cached one keeps it. The
alternative, a hostname per bucket, was rejected in the previous design and is
rejected again for the same reason, now with the added cost that changing the
published form is not recoverable after federation is real.

## Configuration

**Nothing.** No new key in `paisans.yaml`, no new secret. The media hostname and
the storage block already exist, and which buckets are public follows from the
kinds already declared.

## Render

**`garage.toml` gains an `[s3_web]` section** bound to the site's mesh address
on 3902, with `root_domain` set to a suffix that is deliberately not routable:
`.web.garage.internal`. A comment states that the suffix is internal, that
nothing resolves it, and that the gateway is the only thing that ever sends a
Host matching it.

`index` is left unset. An index document would make a prefix with no object
return something other than 404, and nothing here serves a website.

**The media host block routes per bucket.** For each app whose kind serves
objects publicly, one route matching that bucket's path prefix, which strips
the prefix and sends the request to 3902 with the Host rewritten to
`<bucket>.web.garage.internal`. Everything else on the hostname falls through
to 3900 with `Host` forwarded unchanged, which is what Outline's presigned
reads need.

The ordering is not written by hand and must not be relied on as written.
Caddy sorts same directive routes by path matcher length, longest first, which
this repository learned the hard way and recorded in `docs/decisions.md`. The
per bucket matchers are longer than the fallback, so the sort puts them first,
and the plan asserts matcher length rather than file order.

## Provisioning

`paisans storage init` gains one step per public bucket: `garage bucket website
--allow <bucket>`, planned after the bucket exists.

Whether that command is idempotent is **not yet established**. Every other step
in this planner was settled by running it, and this one must be too: the plan
requires the integration test to run provisioning twice and assert the second
run plans nothing, the same property the existing steps already hold.

Nothing is ever revoked. A bucket that stops being public is a change in the
kind's catalogue entry, which is a code change with a review, and the
provisioner is not the right place to guess that a human meant to withdraw
public access.

## What this does not change

* **Mbin's and Outline's rendered configuration.** `KBIN_STORAGE_URL` and
  `AWS_S3_UPLOAD_BUCKET_URL` keep the values they have today.
* **Uploads.** Mbin writes server side over the mesh; Outline's browser submits
  a presigned POST through the gateway. Both are unchanged, and the split is
  already documented where it is rendered.
* **The S3 API route.** It stays the fallback, with `Host` preserved, because
  that is what makes a presigned URL verify.

## Tests

* Golden coverage for the `[s3_web]` section and for the per bucket routes,
  including that Outline's bucket gets no route and no website step.
* A unit test that the provisioning plan allows website access for a public
  bucket and does not for a private one, by bucket name rather than by counting
  steps.
* **An integration test that proves the whole path**, extending the one that
  already drives a real container: provision, upload an object with the
  rendered credential, then fetch it **anonymously** through a real Caddy
  running the rendered media routing, and get 200. Then fetch an object in
  Outline's bucket the same way and get a refusal rather than the object.

That last pair is the point of this branch. The previous branch's integration
test proved the credential reaches Garage; this one has to prove a browser with
no credential at all can read Mbin's media and cannot read Outline's.

## What will still be unverified

Nothing runs Mbin or Outline here, so the claim that Mbin builds its media URLs
from `KBIN_STORAGE_URL` and that Outline presigns its reads remains read from
their source rather than observed in those applications. What this branch can
and must observe is the HTTP behaviour on both sides of the gateway, which is
where the previous design went wrong.

## Amendment, 2026-10-04: `writefreely` means the wisp fork

Founder direction in session. The fork gained Postgres and S3 support, so the
blog can join the same Patroni cluster and the same Garage node as everything
else, and the toolkit should describe that rather than the SQLite shape.

### What the fork actually does, read from its source on `develop`

**Its images are served by the application, not by Garage.** `imagestore.go`
streams them through the app's own `/uploads/` route with
`http.ServeContent`, from whichever store is configured. It never emits an S3
URL and never presigns one.

That inverts the obvious conclusion. Blog images are public to a reader, but
nothing anonymous ever reaches the object store, so **its bucket stays
private**: no website access, no gateway route, no path prefix on the media
hostname. `ServesObjectsPublicly` is false for this kind, and the only thing
that reads its bucket is the application with its own credential.

**The configuration keys are these**, with the fork's own defaults:

```
[database]
type = postgres
host, port, database, username, password
tls      # true maps to sslmode=require, false to sslmode=disable

[storage]
type = s3
s3_endpoint, s3_region, s3_bucket, s3_prefix
s3_access_key_id, s3_secret_access_key
s3_virtual_host  # default false: path style, which is what Garage needs
```

The fork validates that section at load rather than at first upload, and
refuses an endpoint carrying credentials or a path. Nothing the toolkit renders
should trip those, and the plan asserts it.

### The kind changes meaning, and that is a breaking change

`writefreely` now means the fork. It gains a `postgres` service, so clustered
placement becomes legal for it and the existing refusal stops applying on its
own, with no rule to edit.

**Stated rather than buried: an adopter running upstream's image will break.**
Upstream WriteFreely has no Postgres and no `[storage]` section, so it rejects
the configuration this kind renders. This repository is public, so that is a
breaking change for people we do not know, and it belongs in `README.md` and in
`docs/decisions.md` in those words. The rejected alternative was a second kind
for the fork, which keeps upstream working at the cost of two template sets
that mostly agree; the founder chose one kind.

### The image is pinned to an unreleased build, deliberately

There is no released wisp image with these features. `v0.20.0+wisp` carries
neither, and `develop` is 377 commits ahead of it with `softwareVer` still at
`0.20.0`, so nothing has been cut.

The default is therefore the `develop` build by digest:

```
ghcr.io/josephquigley/writefreely-wisp@sha256:4d21f45879bd98c8485eb8169ea57fbab925f0cbd5a38ac3c3bdd79901d809ea
```

Checked rather than assumed: that digest was resolved from the registry on
2026-10-04, is a multi architecture index covering amd64 and arm64, and its
binary contains the `s3_secret_access_key` ini tag and the Postgres `sslmode`
handling, so the features really are in it.

A digest names one set of bytes, which is the toolkit's actual rule, and a
branch build satisfies it. What it does not carry is a release's provenance,
so the catalogue comment says it is an unreleased build and says to bump it
when a release exists. The founder chose this over waiting on a release in
another repository.

### What this does not change

The media hostname work above is untouched. Mbin remains the only kind whose
objects are served anonymously, and the blog's bucket is private for a
different reason than Outline's: Outline presigns because its documents are
member only, and the blog never exposes its store at all.

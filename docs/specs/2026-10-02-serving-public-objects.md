# Public objects are served, and private ones stay private

Status: approved in session on 2026-10-02, not implemented.

The hostname shape below is superseded: see *Amendment, 2026-10-04: one media
hostname per app* at the end.

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

## Amendment, 2026-10-04: one media hostname per app

Founder decision in session. It supersedes the single media hostname above,
the per bucket routing under it, and this spec's rejection of a hostname per
bucket.

### What replaces it

**Each app that stores objects serves them on a hostname of its own, a sibling
of the app's.** The default is derived: the first label of the app's hostname,
then `-media`, under `community.domain`. An app at `talk.example.org` serves its
media at `talk-media.example.org`. An app can declare another under
`hostnames.media`, which is a full hostname under the deployment's domain.
`storage.media_hostname` is removed, along with the `media.caddy` snippet and
its per path routes.

The name is never a child of the app's host (`media.talk.example.org`), because
a cookie the app scopes to its host would reach it, and never a name for the
backend (`garage.`, `s3.`), because what answers it is exactly what is meant to
be able to change. Validation refuses a name that is not a hostname, a declared
name outside the domain, a name under any app hostname other than the domain
itself, and a clash with any other hostname, derived or declared.

### Why the earlier rejection no longer holds

The single hostname was kept above because Mbin had already written URLs of
that form and a federating instance that cached one keeps it. That premise was
never true of this toolkit: it has never been released, there are no tags, and
none of the media work is on `main`, so no deployment has published a URL in
either shape. The cost of changing the shape is therefore nothing now and a
migration later, which is the argument for changing it now.

What a hostname per app buys is what a path cannot:

* **It can be re-pointed one app at a time.** A hostname is the unit DNS moves.
  One app's media can go to a CDN, another provider or another Garage with a
  record change for that app and no other. A path under a shared hostname can
  only move with every other app's.
* **It isolates origins.** Two apps' user uploads on two hostnames are two
  browser origins, so a file one app stored cannot script against another
  app's media.

The cost is one DNS record per app that stores objects, which the README now
says beside the rule.

### WriteFreely is now public

The amendment above described the fork streaming images through its own
`/uploads/` route. That is no longer the fork's design. writefreely-wisp#171
(open on 2026-10-04, not yet merged) makes S3 storage direct only: with
`[storage] type = s3` the fork requires `[storage] image_url_base`, an absolute
`https://` URL with a host and no query, fragment, credentials or unclean
path; it rewrites image URLs to that base at render time; and `/uploads/*` only
answers with a redirect there. It never reads the bucket to serve an image.

So `ServesObjectsPublicly` is true for `writefreely`, its bucket gets
`garage bucket website --allow`, its media hostname goes to the web endpoint
exactly as Mbin's does, and `config.ini` renders
`image_url_base = https://<blog media host>` with no path, since the fork joins
the object key on with a slash and the gateway resolves the bucket from the
hostname.

The pinned fork image predates #171. It does not read the key, keeps streaming
images through `/uploads/` with its own credential, and so still works without
using its media hostname; that was observed by booting the pinned image against
the rendered `config.ini`. The pin has to move to a build containing #171 once
it is merged.

### Outline stays private, through the S3 API

Outline's media hostname goes to Garage's S3 API on 3900 with `Host` forwarded
unchanged, never to the web endpoint, so its presigned reads and uploads verify
exactly as before. `AWS_S3_UPLOAD_BUCKET_URL` is `https://<outline media host>`,
which Outline also uses as its S3 client's endpoint, and
`AWS_S3_FORCE_PATH_STYLE` stays true. Path style is required rather than
preferred: the media hostname is not under Garage's S3 root domain, so Garage
reads the bucket from the path, and virtual host style would sign for a
hostname nothing serves.

One upstream behaviour became a refusal. Outline's `getPublicEndpoint`
(`server/storage/files/S3Storage.ts`, read at v1.10.0) treats the bucket name
appearing anywhere in that URL as virtual host addressing and stops putting it
in the upload path. With a hostname per app, a bucket named after the app does
appear in it, so that combination is refused as `outline-bucket-in-media-url`.

### Media is public by link

No media hostname is gated or restricted by address, including the media
hostname of an app that is itself gated. ActivityPub servers hotlink the
original URLs they were given, so a gate would break every federated image.
Outline's attachments are protected by the signature on each URL, not by
anything in front of the hostname.

### Headers

Every public media hostname sends `Content-Security-Policy: default-src 'none';
style-src 'unsafe-inline'; sandbox` and `X-Content-Type-Options: nosniff`, set
by the gateway after the upstream's headers. Mbin and the blog store uploads
byte for byte, an SVG among them, and no S3 provider can store a response header
on an object.

Outline's media hostname sends nosniff only. Outline embeds PDF attachments in
the page with `<embed>`, and a browser will not run its PDF viewer in a
sandboxed document, so the policy would break PDF previews. Outline already
stores every type outside a short inline list as `Content-Disposition:
attachment`, and leaves SVG off that list because it can carry script. That the
sandbox breaks the embed is reasoned from how browsers treat sandboxed PDFs, not
observed in a browser here.

### What was run

`TestEachAppsMediaHostnameServesOnlyItsOwnBucket` replaces the earlier
integration test. Against two real `dxflrs/garage:v1.0.1` nodes and a real
Caddy running the rendered snippets: Mbin's and the blog's objects return 200
anonymously on their own hostnames, with the path as the object key and both
headers present; Outline's object is refused anonymously with 403 and returns
200 presigned for its own hostname; and Outline's key asked for on Mbin's
hostname does not return Outline's object.

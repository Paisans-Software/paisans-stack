# The ACME DNS provider is declared, and its Caddy image is published

Status: approved in session on 2026-09-15, not implemented.

This spec covers one change with two halves: a new repository that publishes a
Caddy image carrying DNS provider modules, and the toolkit changes that let a
deployment declare which provider it uses. It also records the repository move
that goes with it.

## Why

Two problems, and only one of them was the one asked about.

**The rendered gateway cannot obtain a certificate.** `infra-compose.yaml.tmpl`
runs `caddy:2-alpine` and `Caddyfile.tmpl` writes `acme_dns cloudflare`. A DNS
provider in Caddy is a separate Go module compiled in with xcaddy, and upstream's
image carries none, so Caddy fails to load that configuration. This has never
been run, which is why it was never noticed. The community's own deployment does
it correctly, in `rsync/caddy/Dockerfile`, and that knowledge did not reach the
toolkit.

**The provider is hardcoded.** "cloudflare" appears in `Caddyfile.tmpl`,
`render/site.go` and `secretsgen`, and `external.cloudflare_api_token` names one
company in a schema every adopter inherits. Every other stack choice in this
toolkit is a choice; this one was an assumption, and it is the assumption an
adopter is most likely to hit first, since certificates come before anything
works.

## Decisions taken

Each of these was a choice with a rejected alternative, and the rejection is the
part worth keeping.

**Adopters pull a published image; nothing builds on a gateway.** The rejected
alternative was rendering a Dockerfile and using compose `build:`, which is what
the community's own stack does. It works anywhere with no registry account, and
it was declined because the design puts the gateway role on a small VM that is
also the etcd witness, whose stability is a function of fsync latency. An xcaddy
build competes for exactly the resource the witness decision was about, and it
repeats on every Caddy upgrade.

The cost is stated rather than hidden: **every adopter's gateway now depends on
our registry and on us rebuilding the image.** The escape hatch below is what
keeps that from being lock-in.

**One image with both modules, not one image per provider.** Switching provider
is then a configuration edit with no image change, no rebuild and no repull,
which is the abstraction that was actually wanted. The cost is that every adopter
carries a module they do not use, a few megabytes, and that the image name does
not say which providers are in it, so its README must.

**Tags mirror upstream Caddy's own methodology.** Upstream publishes a moving
ladder, `latest`, `2`, `2.11`, `2.11.4`, and rebuilds even the patch tag when its
base image updates: `2.11.4` was republished days after release. An earlier draft
of this design claimed our tags would be immutable, which was wrong twice over.
We publish the same ladder against whatever upstream version we built from, and
all of it moves as upstream's does.

**The toolkit's default is therefore pinned by digest**, not by tag. A digest is
the only reference that makes two runs of `apply` deploy the same bytes, and
`ParseReference` already treats one as the stronger form. The tag stays in the
reference for a human to read; the digest is what pins.

**An unknown provider is allowed, with its own image.** The rejected alternative
was a closed enum like `kind`, which is consistent with how applications are
treated and would leave an adopter on Route 53 waiting for us or forking. Instead
a provider we publish no image for requires `acme.image`, and the module is
verified at apply rather than assumed.

## The `caddy-dns` repository

`Paisans-Software/caddy-dns`, public, because adopters pull from it anonymously
and a private package makes the toolkit unusable outside this community.

### Contents

A Dockerfile, a workflow, and a README that states which modules are compiled in
and that the tags mirror upstream's.

```dockerfile
ARG CADDY_VERSION
FROM caddy:${CADDY_VERSION}-builder AS builder
ARG CADDY_VERSION
RUN xcaddy build v${CADDY_VERSION} \
      --with github.com/caddy-dns/cloudflare \
      --with github.com/caddy-dns/desec

FROM caddy:${CADDY_VERSION}-alpine
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
```

The explicit `v${CADDY_VERSION}` argument matters. With no version argument
xcaddy builds the latest stable Caddy (its README: "defaults to CADDY_VERSION
env variable or latest"), so the binary would be whatever was newest at build
time rather than the version resolved for the tags.

### What triggers a build

**GitHub Actions cannot trigger on another repository's release**, so this is a
poll rather than an event. A daily scheduled job resolves upstream's current
release and builds when either the version differs from our newest image, or our
newest image is more than thirty days old. The second condition is the base-CVE
case: upstream rebuilds its own tags for that reason, and an image that only
tracked Caddy releases would sit unpatched during a quiet month.

Manual dispatch exists for forcing a rebuild.

### What gates a publish

No tag is pushed until a smoke test passes against the built image:

```
caddy version
caddy list-modules | grep -qx dns.providers.cloudflare
caddy list-modules | grep -qx dns.providers.desec
caddy validate --config <a Caddyfile using acme_dns desec>
```

The last is the one that matters. A build can succeed and still produce a binary
that cannot load the configuration this toolkit renders, and publishing that
unattended would push it to every adopter at once.

### Tags published

```
ghcr.io/paisans-software/caddy:latest
ghcr.io/paisans-software/caddy:2
ghcr.io/paisans-software/caddy:2.11
ghcr.io/paisans-software/caddy:2.11.4
```

All moving, as upstream's are. The toolkit refuses to *deploy* the floating ones
and publishes them anyway, which is consistent rather than contradictory: we
publish what upstream publishes, and we decline to point a deployment at a
reference that cannot reproduce itself.

### Where the human is

**The workflow publishes on its own and changes no deployment.** What reaches a
deployment is the pull request it opens against `paisans-stack` bumping the
default digest, merged by a person who can ask whether this Caddy release breaks
anything.

The residual failure mode, stated rather than solved: if the poll breaks it fails
quietly, and staleness is visible only to somebody who looks at the Actions log.
Alerting is not worth building at this size, but believing the automation watches
itself would be wrong.

## Toolkit changes

### Configuration

A top level `acme` block, beside `community` and `mesh`. Top level rather than
under a gateway site, because certificates are a deployment-wide fact and the
gateway role moves between machines by design.

```yaml
acme:
  provider: desec
  # image: ghcr.io/them/caddy-route53:2.9.0
```

`provider` is required once any site holds the gateway role, and unused
otherwise. `image` is optional and only meaningful for a provider the toolkit
publishes no image for.

### Secrets

`external.cloudflare_api_token` becomes `external.acme_dns_token`, rendered into
`caddy.env` as `ACME_DNS_TOKEN`. `secretsgen`'s owed entry names the declared
provider instead of Cloudflare. This is the change that removes a company's name
from a schema every adopter inherits.

There is no migration path and none is needed: nothing is deployed from this
toolkit, and the only file carrying the old key is the example.

### Render

* `Caddyfile.tmpl` writes the provider's ACME directive, which is **not one
  shape for every provider**. Cloudflare takes the token as a bare argument,
  `acme_dns cloudflare {env.ACME_DNS_TOKEN}`; deSEC requires a block with a
  `token` subdirective. Both were read from those modules' own documentation
  after a build rejected the single shape, so `internal/acme` holds the exact
  lines per provider and the template renders them.

  For a provider the toolkit publishes nothing for, the bare argument form is
  rendered, because it is the common one. A module needing subdirectives cannot
  be expressed yet: that is a known gap rather than a silent failure, because
  `apply` runs `caddy validate` before reloading and a parse error stops it.
* The Caddy image is our published digest for a supported provider, and the
  declared `acme.image` otherwise.
* `infra-compose.yaml.tmpl` keeps `image:` and gains no `build:`. No Dockerfile
  is rendered anywhere.

### Validation

Two refusals, both traceable to a rule in `README.md`:

* **`acme-provider-needs-an-image`**: the provider is not one we publish an
  image for, and no `acme.image` is declared. The message names the supported
  providers and says what to declare.
* **`acme-image-is-stock-caddy`**: the declared override is `caddy:*` or
  `docker.io/library/caddy:*`. That image certainly carries no module. The check
  is deliberately this narrow: whether a module is compiled into a binary is not
  a property of a reference string, so any other value passes rather than being
  guessed at.

A missing `acme.provider` while a gateway site exists is a structural error in
`config`, not a policy refusal, since it is a required field rather than an
incoherent combination.

### Apply

Before the existing validate-and-reload step, ask the binary what it has:

```
caddy list-modules | grep -qx dns.providers.<provider>
```

A missing module refuses the reload. This is the only check in the whole change
that is evidence rather than inference, and it covers both a wrong override image
and a provider name no module answers to.

### Images table

Caddy is an infrastructure service rather than an app kind, so its default lives
beside `spiloTag` in `internal/render`, not in the `kinds` catalogue.

`TestDefaultsArePinned` tightens: an image **we publish** must be referenced by
digest, because we control its tags and know they move. A third-party image may
still be referenced by version tag, because there we are trusting somebody else's
tag discipline either way and a digest would only add a false sense of control.

## Repository moves

`josephquigley/paisans-stack` transfers to `Paisans-Software/paisans-stack`,
staying private. The two forks, `mbin-paisans` and `writefreely-wisp`, stay on
the founder's account: their audience is upstream maintainers who already know
that name.

The transfer was carried out on 2026-09-15 with the founder's explicit
permission, given in session. GitHub keeps redirects, so open pull requests and
existing clones survive, but a redirect is a courtesy rather than a contract, and
everything naming the repository is updated in the same change:

* both local remotes, including worktrees
* the community repository's `CLAUDE.md`, which names this repository as **the
  one sanctioned Charter fallback**. That reference is part of a recovery path,
  so it cannot be left pointing at a name that no longer describes reality.
* `src/CLAUDE.md`, for the same reason
* this repository's own `README.md` where it names itself
* the Go module path, which was `github.com/josephquigley/paisans-stack` and no
  longer describes where this code lives

## Order of operations

The toolkit cannot reference a digest that does not exist, so:

1. Create `Paisans-Software/caddy-dns` with the Dockerfile, workflow and README.
2. Let the first build run. Its smoke test passing produces the first digest.
3. Change the toolkit: the `acme` block, the secrets rename, the two refusals,
   the apply gate, and the default digest from step 2.
4. Transfer `paisans-stack` to the organisation. **Done on 2026-09-15**, by the
   agent with the founder's explicit permission in session.
5. Update every reference to the old path, in the same change as step 4.

Steps 3 and 4 are independent, so a delay on the transfer does not block the
provider work.

## Records

* This repository: an entry in `docs/decisions.md`, and `README.md` updated where
  it describes certificates.
* The community's Outline: a decision record and a Decision Log line, because
  moving the repository that holds the sanctioned fallback changes this
  community's recovery path and belongs in its own history rather than only in
  the toolkit's.

## Out of scope

* The community's running `rsync/caddy/` stack, which builds its own Cloudflare
  image and is unaffected by any of this.
* `scripts/cf`, which is Cloudflare's admin API for DNS changes and has nothing
  to do with ACME.
* Any other DNS provider. Two are published; a third is a one line change to the
  Dockerfile when somebody needs it.

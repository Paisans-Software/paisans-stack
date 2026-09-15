# ACME DNS Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A deployment declares which ACME DNS provider it uses, and pulls a Caddy image that actually carries that provider's module, so the rendered gateway can obtain a certificate at all.

**Architecture:** A new public repository, `Paisans-Software/caddy-dns`, builds one Caddy image with the Cloudflare and deSEC modules compiled in and publishes it to `ghcr.io/paisans-software/caddy` under tags mirroring upstream Caddy's. The toolkit gains a top level `acme` block, selects that image by digest for a provider it publishes, accepts a declared image for one it does not, and verifies at apply time that the running binary actually has the module.

**Tech Stack:** Go 1.25 (toolkit), Docker with xcaddy (image), GitHub Actions (build and publish), sops and age (secrets, already embedded).

**Spec:** `docs/specs/2026-09-15-acme-dns-provider.md`

## Global Constraints

- Go module path is `github.com/paisans-software/paisans-stack`. Every import uses it.
- **No em dashes** in anything written here: documentation, comments, commit messages, pull request bodies. Use a comma, colon, semicolon, full stop, or a real parenthetical. This is `AGENTS.md`'s rule and it applies to every file this plan touches.
- Every refusal added to `internal/validate` traces to a rule stated in `README.md`. Policy that is not in the README is policy nobody agreed to, so a task that adds a rule also adds its README paragraph.
- **Refuse** where a configuration is incoherent; **warn** where it is merely risky. Do not blur the two.
- Claims about upstream software are verified against upstream and cited in a comment, never recalled.
- Rendering stays deterministic: sort before iterating a map, and never record a timestamp in a rendered artifact.
- Nothing this plan builds is run against a real host. `apply` gains a gate; exercising it against this community's infrastructure is a separate decision.
- Commit style follows `AGENTS.md`: `<type>: <lowercase imperative subject>`, a body saying what changed and why with the rejected alternative named, then the `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>` trailer.

## Two repositories, in order

Phase A builds and publishes the image. Phase B changes the toolkit. **Phase A comes first because Task B4 needs a real digest that only a successful publish produces.** Phase A's tasks happen in a new repository; Phase B's happen in `paisans-stack` on branch `feat/acme-dns-provider`.

## File structure

**New repository `Paisans-Software/caddy-dns`:**

| File | Responsibility |
|------|----------------|
| `Dockerfile` | build one Caddy binary with both DNS modules, from a pinned upstream version |
| `built.json` | what the last successful build produced: upstream version, date, digest. The workflow reads it to decide whether to build, and writes it after publishing |
| `smoke/Caddyfile` | a configuration using `acme_dns desec`, used only to prove the built binary can load one |
| `.github/workflows/build.yml` | poll upstream, decide, build, smoke test, publish, update `built.json`, open the toolkit pull request |
| `README.md` | which modules are in the image, how the tags work, how to add a provider |

**Toolkit changes in `paisans-stack`:**

| File | Responsibility |
|------|----------------|
| `internal/acme/acme.go` (new) | what the toolkit knows about providers: Caddy module name and published image per provider. Imported by both `validate` and `render` so neither holds a second copy |
| `internal/config/config.go` | the `acme` block and its structural checks |
| `internal/config/secrets.go` | unchanged in shape; the key rename lives in the `External` map's usage |
| `internal/validate/validate.go` | two refusals |
| `internal/render/site.go` | Caddy image selection and the Caddyfile's data |
| `internal/render/templates/Caddyfile.tmpl` | provider name and generic token variable |
| `internal/render/templates/caddy.env.tmpl` | `ACME_DNS_TOKEN` |
| `internal/apply/apply.go` | the module gate before the gateway reload |
| `internal/secretsgen/secretsgen.go` | the owed entry names the declared provider |
| `README.md`, `docs/decisions.md`, `docs/development.md` | the rules, the decision, the developer's view |

---

## Phase A: the image repository

### Task A1: Create the repository with a Dockerfile that builds and proves itself

**Files:**
- Create: `Dockerfile`, `smoke/Caddyfile`, `built.json`, `.gitignore`
- Repository: `Paisans-Software/caddy-dns`, public

**Interfaces:**
- Consumes: nothing.
- Produces: a buildable image whose binary answers `caddy list-modules` with `dns.providers.cloudflare` and `dns.providers.desec`. Task A2 automates exactly the commands proven by hand here.

- [ ] **Step 1: Create the repository**

```bash
gh repo create Paisans-Software/caddy-dns --public \
  --description "Caddy with DNS provider modules for paisans deployments" \
  --clone
cd caddy-dns
```

- [ ] **Step 2: Write the Dockerfile**

Create `Dockerfile`:

```dockerfile
# Caddy with the DNS provider modules a paisans deployment can use.
#
# Why this image exists: certificates are issued over DNS-01, because a
# deployment's hostnames may not be reachable from the internet at the moment a
# challenge runs, and because a gateway must be able to hold valid certificates
# before DNS points at it. A DNS provider in Caddy is a separate Go module
# compiled in with xcaddy, and upstream's image carries none, so upstream's
# image cannot serve a config that names one.
#
# One image carries every provider we support. Switching provider is then a
# configuration edit with no image change, which is the whole point.

ARG CADDY_VERSION

FROM caddy:${CADDY_VERSION}-builder AS builder
ARG CADDY_VERSION
# The explicit version matters. With no version argument xcaddy builds the
# latest stable Caddy (xcaddy README: `xcaddy build [<caddy_version>]`,
# "defaults to CADDY_VERSION env variable or latest"), which is whatever is
# newest when the build runs rather than the version this image is tagged as.
RUN xcaddy build "v${CADDY_VERSION}" \
      --with github.com/caddy-dns/cloudflare \
      --with github.com/caddy-dns/desec

FROM caddy:${CADDY_VERSION}-alpine
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
```

- [ ] **Step 3: Write the smoke configuration**

Create `smoke/Caddyfile`. This is not a deployment's configuration; it exists so the build can prove the binary loads a config that names a provider:

```
# Loaded by `caddy validate` in the build, and nowhere else. It proves the
# binary can parse and load a DNS provider directive, which a build succeeding
# does not prove on its own.
{
	email admin@example.org
	acme_dns desec {env.ACME_DNS_TOKEN}
}

smoke.example.org {
	respond "ok"
}
```

- [ ] **Step 4: Build it by hand and watch it fail or pass**

Run, substituting the current upstream release:

```bash
CADDY_VERSION=$(gh api repos/caddyserver/caddy/releases/latest -q .tag_name | sed 's/^v//')
echo "building against caddy ${CADDY_VERSION}"
docker build --build-arg CADDY_VERSION="${CADDY_VERSION}" -t caddy-dns:smoke .
```

Expected: a successful build. If `caddy:${CADDY_VERSION}-builder` does not exist, the tag scheme has changed upstream and the Dockerfile needs revisiting before anything else in this plan.

- [ ] **Step 5: Prove the modules are actually in the binary**

```bash
docker run --rm caddy-dns:smoke caddy list-modules | grep -x dns.providers.cloudflare
docker run --rm caddy-dns:smoke caddy list-modules | grep -x dns.providers.desec
docker run --rm -e ACME_DNS_TOKEN=smoke-not-a-secret \
  -v "$PWD/smoke:/smoke:ro" caddy-dns:smoke \
  caddy validate --config /smoke/Caddyfile --adapter caddyfile
```

Expected: both greps print their module, and `validate` prints `Valid configuration`. A build that succeeds while `validate` fails is exactly the case this gate exists for.

- [ ] **Step 6: Record what a build produced**

Create `built.json`. The workflow reads this to decide whether to build and rewrites it after publishing, so it is both state and history:

```json
{
  "caddy_version": "0.0.0",
  "built_at": "1970-01-01T00:00:00Z",
  "digest": ""
}
```

Create `.gitignore`:

```
# Nothing built lands here; the image lives in the registry.
*.tar
```

- [ ] **Step 7: Commit**

```bash
git add Dockerfile smoke/Caddyfile built.json .gitignore
git commit -m "feat: build caddy with the cloudflare and desec dns modules

Upstream's image carries no DNS provider module, so a Caddy configuration that
names one cannot load. Deployments issue certificates over DNS-01, because a
hostname may be unreachable from the internet when a challenge runs and because
a gateway has to hold certificates before DNS points at it.

One image carries both modules rather than one image per provider, so switching
provider is a configuration edit with no image change. The cost is a module an
adopter does not use, a few megabytes.

The version is passed explicitly to xcaddy, because with no argument it builds
the latest stable Caddy, which is whatever is newest at build time rather than
the version the image is tagged as.

smoke/Caddyfile exists because a build succeeding does not prove the binary can
load a config that names a provider, and that is the failure worth catching.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task A2: The workflow that polls, builds, smoke tests and publishes

**Files:**
- Create: `.github/workflows/build.yml`

**Interfaces:**
- Consumes: `Dockerfile`, `smoke/Caddyfile`, `built.json` from Task A1.
- Produces: images at `ghcr.io/paisans-software/caddy` tagged `latest`, `<major>`, `<major>.<minor>`, `<version>`, and an updated `built.json` containing the published digest. Task A4 reads that digest.

- [ ] **Step 1: Write the workflow**

Create `.github/workflows/build.yml`:

```yaml
# Publishes ghcr.io/paisans-software/caddy.
#
# This is a poll, not an event. GitHub Actions cannot trigger on another
# repository's release, so a schedule asks upstream what the current version is
# and compares it with what we last built.
#
# Two conditions build. A new upstream version is the obvious one. An image
# older than thirty days is the other: upstream rebuilds its own tags when
# their base image gets a security update, and an image that only tracked Caddy
# releases would sit unpatched through a quiet month.
name: build

on:
  schedule:
    - cron: "17 4 * * *"
  workflow_dispatch:
    inputs:
      force:
        description: "Build even when nothing changed"
        type: boolean
        default: false

permissions:
  contents: write
  packages: write

env:
  IMAGE: ghcr.io/paisans-software/caddy
  MAX_AGE_DAYS: 30

jobs:
  decide:
    runs-on: ubuntu-latest
    outputs:
      build: ${{ steps.decide.outputs.build }}
      version: ${{ steps.decide.outputs.version }}
      reason: ${{ steps.decide.outputs.reason }}
    steps:
      - uses: actions/checkout@v4

      - id: decide
        env:
          GH_TOKEN: ${{ github.token }}
          FORCE: ${{ inputs.force }}
        run: |
          set -euo pipefail
          version=$(gh api repos/caddyserver/caddy/releases/latest -q .tag_name | sed 's/^v//')
          built=$(jq -r .caddy_version built.json)
          built_at=$(jq -r .built_at built.json)
          age_days=$(( ( $(date -u +%s) - $(date -u -d "$built_at" +%s) ) / 86400 ))

          echo "version=$version" >> "$GITHUB_OUTPUT"
          if [ "$FORCE" = "true" ]; then
            echo "build=true" >> "$GITHUB_OUTPUT"
            echo "reason=forced by hand" >> "$GITHUB_OUTPUT"
          elif [ "$version" != "$built" ]; then
            echo "build=true" >> "$GITHUB_OUTPUT"
            echo "reason=caddy $built -> $version" >> "$GITHUB_OUTPUT"
          elif [ "$age_days" -ge "$MAX_AGE_DAYS" ]; then
            echo "build=true" >> "$GITHUB_OUTPUT"
            echo "reason=last build is $age_days days old" >> "$GITHUB_OUTPUT"
          else
            echo "build=false" >> "$GITHUB_OUTPUT"
            echo "reason=caddy $version already built $age_days days ago" >> "$GITHUB_OUTPUT"
          fi

      - run: echo "${{ steps.decide.outputs.reason }}" >> "$GITHUB_STEP_SUMMARY"

  build:
    needs: decide
    if: needs.decide.outputs.build == 'true'
    runs-on: ubuntu-latest
    outputs:
      digest: ${{ steps.publish.outputs.digest }}
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-qemu-action@v3
      - uses: docker/setup-buildx-action@v3

      # Built once for the host architecture and smoke tested before anything
      # is pushed. A multi architecture push happens only after that passes.
      - name: Build for smoke testing
        uses: docker/build-push-action@v6
        with:
          context: .
          load: true
          tags: caddy-dns:smoke
          build-args: |
            CADDY_VERSION=${{ needs.decide.outputs.version }}

      - name: Smoke test
        run: |
          set -euo pipefail
          docker run --rm caddy-dns:smoke caddy version
          docker run --rm caddy-dns:smoke caddy list-modules | grep -qx dns.providers.cloudflare
          docker run --rm caddy-dns:smoke caddy list-modules | grep -qx dns.providers.desec
          docker run --rm -e ACME_DNS_TOKEN=smoke-not-a-secret \
            -v "$PWD/smoke:/smoke:ro" caddy-dns:smoke \
            caddy validate --config /smoke/Caddyfile --adapter caddyfile

      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Compute tags
        id: tags
        run: |
          set -euo pipefail
          v="${{ needs.decide.outputs.version }}"
          major="${v%%.*}"
          minor="${v%.*}"
          {
            echo "tags<<EOF"
            echo "$IMAGE:$v"
            echo "$IMAGE:$minor"
            echo "$IMAGE:$major"
            echo "$IMAGE:latest"
            echo "EOF"
          } >> "$GITHUB_OUTPUT"

      - name: Publish
        id: publish
        uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          platforms: linux/amd64,linux/arm64
          tags: ${{ steps.tags.outputs.tags }}
          build-args: |
            CADDY_VERSION=${{ needs.decide.outputs.version }}

      - name: Record what was published
        run: |
          set -euo pipefail
          jq -n \
            --arg version "${{ needs.decide.outputs.version }}" \
            --arg built_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
            --arg digest "${{ steps.publish.outputs.digest }}" \
            '{caddy_version: $version, built_at: $built_at, digest: $digest}' > built.json
          git config user.name "paisans build"
          git config user.email "noreply@paisans.community"
          git add built.json
          git commit -m "chore: publish caddy ${{ needs.decide.outputs.version }}

${{ needs.decide.outputs.reason }}

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
          git push

      - name: Summarise
        run: |
          {
            echo "Published \`$IMAGE:${{ needs.decide.outputs.version }}\`"
            echo ""
            echo "Digest: \`${{ steps.publish.outputs.digest }}\`"
          } >> "$GITHUB_STEP_SUMMARY"
```

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/build.yml
git commit -m "feat: poll upstream, build, smoke test and publish

GitHub Actions cannot trigger on another repository's release, so tracking
upstream is a daily poll rather than an event. Two conditions build: a new
upstream version, and an image older than thirty days, because upstream rebuilds
its own tags when their base picks up a security fix and an image tracking only
releases would sit unpatched through a quiet month.

Nothing is pushed until the built binary answers with both modules and loads a
configuration that names one. A build can succeed and still produce a binary
that cannot serve what the toolkit renders, and publishing that unattended would
put it in front of every adopter at once.

Tags mirror upstream's ladder, including the moving ones, because that is the
methodology this image is a variant of. The toolkit pins by digest rather than
by any of these tags, which is its business rather than this repository's.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
git push
```

- [ ] **Step 3: Run it by hand and read the log**

```bash
gh workflow run build.yml -f force=true --repo Paisans-Software/caddy-dns
sleep 30
gh run list --workflow build.yml --repo Paisans-Software/caddy-dns --limit 1
gh run watch --repo Paisans-Software/caddy-dns
```

Expected: the decide job says why it built, the smoke test passes, the publish step prints a digest, and `built.json` is committed back. If the smoke test fails, stop and fix the Dockerfile: that failure is the whole reason the step exists.

- [ ] **Step 4: Verify the published image from outside**

```bash
docker run --rm ghcr.io/paisans-software/caddy:latest caddy list-modules | grep -x dns.providers.desec
gh api /orgs/Paisans-Software/packages/container/caddy/versions --jq '.[0].metadata.container.tags'
```

Expected: the module is present in the published image, and the tag list contains the version, minor, major and `latest`.

- [ ] **Step 5: Record the digest for Phase B**

```bash
jq -r '"ghcr.io/paisans-software/caddy:" + .caddy_version + "@" + .digest' built.json
```

Keep that line. **Task B4 uses this exact string**, and nothing else may be substituted for it: a tag alone does not pin, because these tags move.

---

### Task A3: README stating what is in the image and how tags behave

**Files:**
- Create: `README.md`

**Interfaces:**
- Consumes: the published tags from Task A2.
- Produces: nothing code depends on.

- [ ] **Step 1: Write it**

Create `README.md`:

````markdown
# caddy-dns

Caddy with DNS provider modules compiled in, for paisans deployments.

```
ghcr.io/paisans-software/caddy
```

## Why this exists

A paisans deployment issues certificates over DNS-01. That is not a preference:
a gateway has to be able to hold valid certificates before DNS points at it,
which is what makes moving a gateway an overlap rather than a cutover, and a
hostname served behind a VPN has nothing on the internet that can answer an
HTTP-01 challenge.

A DNS provider in Caddy is a separate Go module, compiled in with xcaddy.
Upstream's image carries none, so upstream's image cannot load a configuration
that names one. This repository builds the one that can.

## What is in it

| Module | Provider |
|--------|----------|
| `github.com/caddy-dns/cloudflare` | Cloudflare |
| `github.com/caddy-dns/desec` | deSEC |

Both are in the same image. Switching provider is a configuration edit in the
deployment, with no image change and no repull.

## Tags

Tags mirror upstream Caddy's own ladder, against whatever version we built from:

```
ghcr.io/paisans-software/caddy:latest
ghcr.io/paisans-software/caddy:2
ghcr.io/paisans-software/caddy:2.11
ghcr.io/paisans-software/caddy:2.11.4
```

**All of these move**, exactly as upstream's do. Upstream rebuilds even a patch
tag when its base image gets a security update, so `2.11.4` is a name for the
newest build of that version rather than for a fixed set of bytes.

**Pin by digest if you need reproducibility.** `paisans-stack` does, and the
build opens a pull request there to move that digest forward.

## How it stays current

A daily job asks what upstream's current release is and builds when either the
version differs from the last build, or the last build is more than thirty days
old. The second case is base image security updates, which arrive without a
Caddy release.

Nothing is published until the built binary answers `caddy list-modules` with
both providers and loads a configuration that uses one. A build can succeed and
still produce a binary that cannot serve the configuration a deployment renders.

`built.json` records what the last successful build produced. It is written by
the workflow, not by hand.

## Adding a provider

Add a `--with` line to the `Dockerfile`, add the module to the smoke test in
`.github/workflows/build.yml`, and run the workflow by hand with `force`. Every
adopter gets the new provider on their next image pull, and nobody who is not
using it is affected.
````

- [ ] **Step 2: Commit and push**

```bash
git add README.md
git commit -m "docs: say what is in the image and how its tags behave

The image name does not say which providers are compiled in, which is the cost
of carrying both in one image, so this file has to.

States plainly that every published tag moves, including the patch version.
That is upstream's methodology and this image mirrors it, and an adopter who
assumes a version tag is a fixed set of bytes would be wrong about upstream's
image too.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
git push
```

---

### Task A4: Open the toolkit's pull request from a successful build

**Files:**
- Modify: `.github/workflows/build.yml`

**Interfaces:**
- Consumes: the digest from Task A2's publish step.
- Produces: a pull request against `paisans-stack` changing one line. Nothing in this repository depends on it.

**Before this task:** a fine grained personal access token with `contents: write` and `pull_requests: write` on `Paisans-Software/paisans-stack` must exist as the organisation secret `TOOLKIT_PR_TOKEN`. `GITHUB_TOKEN` cannot reach another repository. **Creating that token is the founder's action**, not an agent's, and this task cannot be finished without it.

- [ ] **Step 1: Add the job**

Append to `.github/workflows/build.yml`:

```yaml
  propose:
    needs: [decide, build]
    if: needs.decide.outputs.build == 'true'
    runs-on: ubuntu-latest
    steps:
      # The toolkit pins this image by digest, because every tag published here
      # moves. Moving that pin is a human decision: a new Caddy release can
      # change behaviour, and nothing here knows whether it did. So this opens a
      # pull request and stops.
      - uses: actions/checkout@v4
        with:
          repository: Paisans-Software/paisans-stack
          token: ${{ secrets.TOOLKIT_PR_TOKEN }}
          ref: develop

      - name: Point the default at the new digest
        env:
          VERSION: ${{ needs.decide.outputs.version }}
          DIGEST: ${{ needs.build.outputs.digest }}
        run: |
          set -euo pipefail
          reference="ghcr.io/paisans-software/caddy:${VERSION}@${DIGEST}"
          file=internal/acme/acme.go
          sed -i -E "s|ghcr\.io/paisans-software/caddy:[^\"]*|${reference}|" "$file"
          git diff --exit-code && { echo "nothing changed, not opening a pull request"; exit 0; }

      - name: Open the pull request
        env:
          GH_TOKEN: ${{ secrets.TOOLKIT_PR_TOKEN }}
          VERSION: ${{ needs.decide.outputs.version }}
        run: |
          set -euo pipefail
          branch="chore/caddy-${VERSION}-$(date -u +%Y%m%d%H%M)"
          git config user.name "paisans build"
          git config user.email "noreply@paisans.community"
          git checkout -b "$branch"
          git add internal/acme/acme.go
          git commit -m "chore: take caddy ${VERSION}

Published by Paisans-Software/caddy-dns. The reference is pinned by digest
because every tag that repository publishes moves, upstream's included.

Nothing verified that this release behaves the same as the last one. That is
what this pull request is for.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
          git push -u origin "$branch"
          gh pr create --base develop --head "$branch" \
            --title "Take Caddy ${VERSION}" \
            --body "Published by \`Paisans-Software/caddy-dns\`, smoke tested before publishing: both DNS modules present, and a configuration using one loads.

Pinned by digest because every tag that repository publishes moves.

**Not checked by any machine:** whether this Caddy release changes behaviour a deployment depends on. Read upstream's changelog before merging.

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

- [ ] **Step 2: Commit and push**

```bash
git add .github/workflows/build.yml
git commit -m "feat: propose the new digest to the toolkit

A published image changes nothing on its own. What reaches a deployment is the
toolkit's default, and moving it is a decision a person makes, because a Caddy
release can change behaviour and nothing in this repository knows whether it
did.

So the build opens a pull request and stops. The rejected alternative was
committing straight to the toolkit, which would make an unattended job the thing
that decides what every adopter runs.

GITHUB_TOKEN cannot reach another repository, so this needs TOOLKIT_PR_TOKEN, a
fine grained token scoped to paisans-stack.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
git push
```

- [ ] **Step 3: Verify after the token exists**

```bash
gh workflow run build.yml -f force=true --repo Paisans-Software/caddy-dns
gh run watch --repo Paisans-Software/caddy-dns
gh pr list --repo Paisans-Software/paisans-stack --limit 3
```

Expected: a pull request titled `Take Caddy <version>` against `develop`, changing one line in `internal/acme/acme.go`. On the very first run, before Task B4 exists, the `sed` matches nothing and the job exits saying so, which is correct rather than a failure.

---

## Phase B: the toolkit

All Phase B work happens in `paisans-stack` on branch `feat/acme-dns-provider`.

### Task B1: The `acme` package: providers, modules, images

**Files:**
- Create: `internal/acme/acme.go`, `internal/acme/acme_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `acme.Module(provider string) string` returns the Caddy module identifier, empty for an unknown provider.
  - `acme.Directive(provider string) string` returns the Caddyfile lines that configure it, tab indented for the global options block.
  - `acme.Image(provider string) (string, bool)` returns the published image reference and whether one exists.
  - `acme.Providers() []string` returns supported provider names, sorted.
  - `acme.IsStockCaddy(reference string) bool` reports whether a reference is upstream's own image.

- [ ] **Step 1: Write the failing test**

Create `internal/acme/acme_test.go`:

```go
package acme_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Every provider the toolkit claims to support needs both halves: a module
// identifier to check for at apply time, and an image that actually has it.
// Half a provider is worse than none, because it validates and then fails on
// the host.
func TestSupportedProvidersAreComplete(t *testing.T) {
	providers := acme.Providers()
	if len(providers) < 2 {
		t.Fatalf("expected at least cloudflare and desec, got %v", providers)
	}
	for _, provider := range providers {
		if module := acme.Module(provider); !strings.HasPrefix(module, "dns.providers.") {
			t.Errorf("%s has module %q, which is not a Caddy DNS module identifier", provider, module)
		}
		image, ok := acme.Image(provider)
		if !ok {
			t.Errorf("%s is supported but has no published image", provider)
			continue
		}
		if !kinds.ParseReference(image).Pinned() {
			t.Errorf("%s image %q does not name one build", provider, image)
		}
		if !strings.Contains(image, "@sha256:") {
			t.Errorf("%s image %q is not pinned by digest, and every tag we publish moves", provider, image)
		}
	}
}

// A provider nobody published an image for is not an error here: the
// configuration may declare its own image. This package reports what it knows,
// and validate decides what that means.
func TestUnknownProviderIsReportedNotRefused(t *testing.T) {
	if _, ok := acme.Image("route53"); ok {
		t.Error("an image was claimed for a provider this toolkit does not publish")
	}
	if module := acme.Module("route53"); module != "" {
		t.Errorf("a module was claimed for an unknown provider: %q", module)
	}
}

// Upstream's own image carries no DNS module at all, so declaring it as an
// override is the one case that can be judged from the reference alone.
func TestStockCaddyIsRecognised(t *testing.T) {
	stock := []string{
		"caddy:2-alpine",
		"caddy:latest",
		"docker.io/library/caddy:2.11.4",
		"library/caddy:2",
	}
	for _, reference := range stock {
		if !acme.IsStockCaddy(reference) {
			t.Errorf("%q is upstream's image and was not recognised as one", reference)
		}
	}
	notStock := []string{
		"ghcr.io/paisans-software/caddy:2.11.4@sha256:abc",
		"ghcr.io/them/caddy-route53:2.9.0",
		"example.org/caddy-with-modules:1",
	}
	for _, reference := range notStock {
		if acme.IsStockCaddy(reference) {
			t.Errorf("%q was wrongly called upstream's image", reference)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/acme/`
Expected: FAIL, `no required module provides package .../internal/acme`.

- [ ] **Step 3: Write the implementation**

Create `internal/acme/acme.go`:

```go
// Package acme records what the toolkit knows about ACME DNS providers: the
// Caddy module each one needs, and the image that carries it.
//
// It is its own package because two callers need the same answer and neither
// should own it. internal/validate refuses a provider with no image, and
// internal/render picks the image and writes the provider into the Caddyfile.
// A second copy would drift into a configuration that validates and then fails
// on the host, which is the failure this whole change exists to remove.
package acme

import (
	"sort"
	"strings"
)

// Provider is what the toolkit knows about one DNS provider.
type Provider struct {
	// Module is the Caddy module identifier, as `caddy list-modules` prints it.
	// apply greps for exactly this string, so it is the identifier rather than
	// the Go import path.
	Module string
	// Image is the reference a deployment using this provider runs.
	//
	// Pinned by digest, not by tag. Every tag ghcr.io/paisans-software/caddy
	// publishes moves, mirroring upstream Caddy, which rebuilds even a patch
	// tag when its base image gets a security fix. A digest is the only
	// reference that makes two runs of apply deploy the same bytes.
	//
	// Moved forward by a pull request that Paisans-Software/caddy-dns opens
	// after a successful build. Do not edit it by hand except to correct it.
	Image string
}

// Directive is the Caddyfile form each provider takes, because they do not
// share one.
//
// Cloudflare accepts the token as a bare argument. deSEC requires a block with
// a `token` subdirective and rejects a bare one, which a build of this image
// found by failing `caddy validate`. Both forms are from those modules' own
// documentation: github.com/caddy-dns/cloudflare and github.com/caddy-dns/desec.

// supported is every provider this toolkit publishes an image for. A provider
// absent here is not refused outright: a deployment may declare its own image,
// and the module it claims is verified at apply time rather than assumed.
var supported = map[string]Provider{
	"cloudflare": {
		Module: "dns.providers.cloudflare",
		Image:  "ghcr.io/paisans-software/caddy:2.11.4@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	},
	"desec": {
		Module: "dns.providers.desec",
		Image:  "ghcr.io/paisans-software/caddy:2.11.4@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	},
}

// Providers returns the supported provider names, sorted, for a message that
// has to list them.
func Providers() []string {
	out := make([]string, 0, len(supported))
	for name := range supported {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Module returns the Caddy module identifier for a provider, empty when the
// toolkit does not know it.
func Module(name string) string { return supported[name].Module }

// Image returns the published image for a provider, and whether one exists.
func Image(name string) (string, bool) {
	provider, ok := supported[name]
	return provider.Image, ok
}

// IsStockCaddy reports whether a reference names upstream's own Caddy image.
//
// This is the one thing about an image that can be judged from its reference:
// upstream's image certainly carries no DNS provider module. Every other
// reference is accepted here and verified at apply time instead, because
// whether a module is compiled into a binary is a property of the binary.
func IsStockCaddy(reference string) bool {
	name := reference
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	slash := strings.LastIndex(name, "/")
	if colon := strings.LastIndex(name, ":"); colon > slash {
		name = name[:colon]
	}
	switch name {
	case "caddy", "library/caddy", "docker.io/library/caddy", "docker.io/caddy":
		return true
	}
	return false
}
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/acme/`
Expected: FAIL on the digest assertion is **not** expected, because the placeholder digest above is syntactically a digest. It passes. That is deliberate: the shape is enforced now, and Task B4 replaces the value with a real one.

- [ ] **Step 5: Commit**

```bash
git add internal/acme
git commit -m "feat: record what the toolkit knows about acme dns providers

One place for the module identifier and the published image, because validate
and render both need the same answer and a second copy would drift into a
configuration that validates and then fails on a host.

An unknown provider is reported rather than refused here. Whether that is an
error depends on whether the configuration declared its own image, and that is
validate's question, not this package's.

IsStockCaddy is the only judgement made from a reference string, because
upstream's image certainly carries no module. Everything else is verified at
apply time by asking the binary, since what is compiled into it is a property of
the binary rather than of its name.

The digests here are placeholders until the image repository has published.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task B2: The `acme` configuration block

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `examples/paisans.example.yaml`
- Modify: `internal/render/testdata/deployment.yaml`, `internal/validate/testdata/*.yaml`

**Interfaces:**
- Consumes: nothing from Task B1 (`config` must not import `acme`; policy lives in `validate`).
- Produces: `config.Config.ACME` of type `config.ACME` with fields `Provider string` and `Image string`.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
// Certificates are a deployment wide fact rather than a gateway site's
// property, because the gateway role moves between machines by design. A
// deployment with a gateway and no provider cannot obtain a certificate, and
// that is a missing required field rather than a policy question.
func TestACMEProviderIsRequiredWhenAGatewayExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	body := strings.Replace(validConfig, "acme:\n  provider: desec\n", "", 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("a deployment with a gateway and no acme provider loaded")
	}
	if !strings.Contains(err.Error(), "acme.provider") {
		t.Errorf("the error does not name the missing key:\n%v", err)
	}
}

// The provider is read as declared, and an override image is optional.
func TestACMEBlockLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACME.Provider != "desec" {
		t.Errorf("provider is %q, want desec", cfg.ACME.Provider)
	}
	if cfg.ACME.Image != "" {
		t.Errorf("image is %q, and the fixture declares none", cfg.ACME.Image)
	}
}
```

If `config_test.go` has no `validConfig` constant, add one that is a minimal valid deployment: one site with roles `[data, apps, gateway]`, a mesh subnet, one app, and an `acme:` block with `provider: desec`. Read the existing test file first and follow whatever fixture style it already uses.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/config/ -run TestACME -v`
Expected: FAIL, `cfg.ACME undefined`.

- [ ] **Step 3: Add the field and its structural check**

In `internal/config/config.go`, add to the `Config` struct after `Mesh`:

```go
	// ACME is how certificates are obtained. It is deployment wide rather than
	// a property of the gateway site, because the gateway role moves between
	// machines by design and certificates do not move with it.
	ACME ACME `yaml:"acme"`
```

And the type, beside `Mesh`:

```go
// ACME is the certificate story: which DNS provider answers the challenge, and
// optionally which Caddy image carries that provider's module.
//
// DNS-01 is not configurable. A gateway has to be able to hold valid
// certificates before DNS points at it, which is what makes moving a gateway an
// overlap rather than a cutover, and a hostname served behind a VPN has nothing
// on the internet that can answer an HTTP-01 challenge.
type ACME struct {
	// Provider is the Caddy DNS provider name, as it appears in `acme_dns`.
	Provider string `yaml:"provider"`
	// Image overrides the Caddy image. It is only meaningful for a provider the
	// toolkit publishes no image for: an image is the only way that provider's
	// module reaches the gateway, since nothing is built on a host.
	Image string `yaml:"image"`
}
```

In `structural()`, after the mesh checks:

```go
	if len(c.GatewaySites()) > 0 && c.ACME.Provider == "" {
		add("acme.provider: required, because a site holds the gateway role. Certificates are issued over DNS-01, so the provider that answers the challenge has to be named. This toolkit publishes images for cloudflare and desec; any other provider also needs acme.image.")
	}
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/config/ -run TestACME -v`
Expected: PASS.

- [ ] **Step 5: Add the block to every fixture and the example**

Add to `examples/paisans.example.yaml`, after the `mesh:` block:

```yaml
# Certificates are issued over DNS-01, always. A gateway has to be able to hold
# valid certificates before DNS points at it, which is what makes moving one an
# overlap rather than a cutover, and a hostname behind a VPN has nothing on the
# internet that can answer an HTTP-01 challenge.
#
# The provider named here decides two things: the directive in the gateway's
# Caddyfile, and which image it runs, since a DNS provider in Caddy is a module
# compiled into the binary rather than a setting.
acme:
  provider: cloudflare        # or desec
  # Only for a provider this toolkit publishes no image for. It must carry that
  # provider's module, and `apply` checks that it does before reloading
  # anything.
  # image: ghcr.io/example-org/caddy-route53:2.11.4
```

Add `acme:\n  provider: desec` to `internal/render/testdata/deployment.yaml` and to every fixture under `internal/validate/testdata/` that declares a gateway site. Run `go test ./...` and fix whichever fixtures fail to load.

- [ ] **Step 6: Run everything**

Run: `go vet ./... && go test ./...`
Expected: all packages pass.

- [ ] **Step 7: Commit**

```bash
git add internal/config examples internal/render/testdata internal/validate/testdata
git commit -m "feat: declare the acme dns provider in the configuration

A deployment says which DNS provider answers its certificate challenges. It was
cloudflare, hardcoded in a template, a renderer and a secret name, which made
one company part of a schema every adopter inherits.

Top level rather than under the gateway site, because the gateway role moves
between machines by design and certificates do not move with it.

A missing provider with a gateway declared is a structural error rather than a
policy refusal: it is a required field, not an incoherent combination.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task B3: Rename the credential and render the provider

**Files:**
- Modify: `internal/render/site.go`, `internal/render/templates/Caddyfile.tmpl`, `internal/render/templates/caddy.env.tmpl`
- Modify: `internal/secretsgen/secretsgen.go`, `internal/secretsgen/secretsgen_test.go`
- Modify: `internal/render/render_test.go`, `internal/render/testdata/secrets.fixture.yaml`, `examples/secrets.example.yaml`
- Regenerate: `internal/render/testdata/golden/`

**Interfaces:**
- Consumes: `config.Config.ACME` from Task B2, `acme.Image` from Task B1.
- Produces: rendered `caddy.env` containing `ACME_DNS_TOKEN`, and a Caddyfile whose `acme_dns` names the declared provider.

- [ ] **Step 1: Write the failing test**

Add to `internal/render/render_test.go`:

```go
// The provider reaches the gateway's configuration, and the credential is named
// for what it is rather than for whoever issues it.
func TestTheDeclaredProviderIsRendered(t *testing.T) {
	files := map[string]string{}
	for _, f := range build(t).Files {
		files[f.Path] = f.Content
	}

	caddyfile := files["vm/srv/infra/caddy/Caddyfile"]
	if !strings.Contains(caddyfile, "acme_dns desec {env.ACME_DNS_TOKEN}") {
		t.Errorf("the gateway does not use the declared provider:\n%s", caddyfile)
	}
	if strings.Contains(caddyfile, "cloudflare") {
		t.Errorf("a provider nobody declared appears in the gateway config:\n%s", caddyfile)
	}

	env := files["vm/srv/infra/caddy/caddy.env"]
	if !strings.Contains(env, "ACME_DNS_TOKEN=") {
		t.Errorf("the credential is not rendered under a provider neutral name:\n%s", env)
	}
	if strings.Contains(env, "CLOUDFLARE") {
		t.Errorf("a provider's name survives in the environment:\n%s", env)
	}
}

// The image a gateway runs is the one that carries the declared provider's
// module, pinned by digest, and never upstream's own image.
func TestTheGatewayRunsAnImageWithTheModule(t *testing.T) {
	files := map[string]string{}
	for _, f := range build(t).Files {
		files[f.Path] = f.Content
	}
	infra := files["vm/srv/infra/compose.yaml"]
	want, ok := acme.Image("desec")
	if !ok {
		t.Fatal("desec has no published image")
	}
	if !strings.Contains(infra, "image: "+want) {
		t.Errorf("the gateway does not run the image carrying its provider:\n%s", infra)
	}
	if strings.Contains(infra, "image: caddy:") {
		t.Errorf("the gateway runs upstream's image, which carries no DNS module:\n%s", infra)
	}
}
```

Add `"github.com/paisans-software/paisans-stack/internal/acme"` to that file's imports.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/render/ -run 'TestTheDeclaredProvider|TestTheGatewayRuns' -v`
Expected: FAIL on both, since the template still says `cloudflare` and the compose file still says `caddy:2-alpine`.

- [ ] **Step 3: Change the templates**

In `internal/render/templates/Caddyfile.tmpl`, replace the ACME line:

```
	acme_dns {{ .ACMEProvider }} {env.ACME_DNS_TOKEN}
```

In `internal/render/templates/caddy.env.tmpl`, replace the Cloudflare variable with:

```
# The credential for the DNS provider named in the configuration. It answers
# the DNS-01 challenge and nothing else, so it wants the narrowest scope the
# provider offers: one zone, records only.
ACME_DNS_TOKEN={{ .ACMEDNSToken }}
```

In `internal/render/templates/infra-compose.yaml.tmpl`, replace `image: caddy:2-alpine` with:

```
    image: {{ .CaddyImage }}
```

- [ ] **Step 4: Change the renderer**

In `internal/render/site.go`, inside the `site.IsGateway` branch, replace the two render calls' data:

```go
		caddyfile, err := p.renderTemplate("Caddyfile.tmpl", map[string]any{
			"Domain":         p.cfg.Community.Domain,
			"Routes":         p.routes(),
			"TrustedProxies": p.mesh,
			"ACMEProvider":   p.cfg.ACME.Provider,
		})
```

```go
		env, err := p.renderTemplate("caddy.env.tmpl", map[string]any{
			"ACMEDNSToken": p.secrets.External["acme_dns_token"],
		})
```

Add the image resolver near `spiloTag`:

```go
// caddyImage is the image the gateway runs.
//
// A DNS provider in Caddy is a module compiled into the binary, not a setting,
// so the provider decides the image. A declared image wins, because it is the
// only way a provider the toolkit publishes nothing for can reach a gateway now
// that nothing is built on a host.
func (p *planner) caddyImage() (string, error) {
	if declared := p.cfg.ACME.Image; declared != "" {
		return declared, nil
	}
	image, ok := acme.Image(p.cfg.ACME.Provider)
	if !ok {
		return "", fmt.Errorf(
			"acme.provider %q has no image in this toolkit and acme.image declares none. A DNS provider is a module compiled into Caddy, so an image carrying it is the only way it reaches a gateway. Published providers: %s",
			p.cfg.ACME.Provider, strings.Join(acme.Providers(), ", "))
	}
	return image, nil
}
```

Pass it into the infra template where `SpiloTag` is passed:

```go
	caddy, err := p.caddyImage()
	if err != nil {
		return nil, err
	}
```

```go
		"CaddyImage": caddy,
```

Add the import of `internal/acme`.

- [ ] **Step 5: Rename the secret everywhere**

In `internal/secretsgen/secretsgen.go`, in `owed`:

```go
	if secrets.External["acme_dns_token"] == "" && len(cfg.GatewaySites()) > 0 {
		out = append(out, Owed{
			Name: "external.acme_dns_token",
			Why: fmt.Sprintf(
				"issued by the DNS provider, %s in this deployment, scoped to this zone only. Certificates use DNS-01, so the gateway cannot obtain one without it",
				cfg.ACME.Provider),
		})
	}
```

Add `"fmt"` to that file's imports if absent.

In `internal/render/testdata/secrets.fixture.yaml` and `examples/secrets.example.yaml`, rename `cloudflare_api_token` to `acme_dns_token`. In `internal/secretsgen/secretsgen_test.go`, rename the assertion's key.

- [ ] **Step 6: Run the tests and regenerate the golden tree**

```bash
go test ./internal/render/ -run 'TestTheDeclaredProvider|TestTheGatewayRuns' -v
go test ./internal/render/ -update
git diff --stat internal/render/testdata/golden
go vet ./... && go test ./...
```

Expected: the two tests pass, the golden diff shows the Caddyfile's provider line, the renamed environment variable and the new image, and everything passes.

- [ ] **Step 7: Commit**

```bash
git add internal/render internal/secretsgen examples
git commit -m "feat: render the declared provider, and an image that has it

The gateway's Caddyfile names the provider from the configuration, the
credential is rendered as ACME_DNS_TOKEN rather than under one company's name,
and the Caddy image is the one carrying that provider's module.

The image change is the part that was broken rather than merely hardcoded.
The toolkit rendered upstream's caddy image against a config naming a DNS
provider, and upstream's image carries no provider module, so the gateway could
not have obtained a certificate at all. It had never been run.

A declared acme.image wins over the published one, because nothing builds on a
host any more and an image is the only way an unpublished provider's module gets
there.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task B4: The two refusals, and their README rules

**Files:**
- Modify: `internal/validate/validate.go`, `internal/validate/validate_test.go`, `README.md`
- Create: `internal/validate/testdata/acme-provider-needs-an-image.yaml`, `internal/validate/testdata/acme-image-is-stock-caddy.yaml`

**Interfaces:**
- Consumes: `acme.Image`, `acme.Providers`, `acme.IsStockCaddy` from Task B1; `config.Config.ACME` from Task B2.
- Produces: rules `acme-provider-needs-an-image` and `acme-image-is-stock-caddy`, both `Refuse`.

- [ ] **Step 1: Write the failing test**

Add to the `cases` table in `internal/validate/validate_test.go`:

```go
		{"acme-provider-needs-an-image", "acme-provider-needs-an-image", validate.Refuse},
		{"acme-image-is-stock-caddy", "acme-image-is-stock-caddy", validate.Refuse},
```

Create `internal/validate/testdata/acme-provider-needs-an-image.yaml` as a copy of `valid.yaml` with its acme block replaced by:

```yaml
acme:
  # This toolkit publishes no image for route53, and none is declared, so the
  # module would never reach the gateway.
  provider: route53
```

Create `internal/validate/testdata/acme-image-is-stock-caddy.yaml` as a copy of `valid.yaml` with:

```yaml
acme:
  provider: desec
  # Upstream's own image, which carries no DNS provider module at all.
  image: caddy:2-alpine
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/validate/ -run TestRulesFire -v`
Expected: FAIL, `rule acme-provider-needs-an-image did not fire`.

- [ ] **Step 3: Write the rules**

In `internal/validate/validate.go`, register both in `Check`, in the refusal group:

```go
	c.acmeProviderHasAnImage()
	c.acmeImageIsNotStockCaddy()
```

And the implementations:

```go
// acmeProviderHasAnImage refuses a provider whose module cannot reach the
// gateway.
//
// A DNS provider in Caddy is a Go module compiled into the binary, and nothing
// is built on a host, so a provider the toolkit publishes no image for needs an
// image declared that carries it. Without one the gateway would run a binary
// that cannot load its own configuration, and would hold no certificate for any
// hostname. That is incoherent rather than risky, so it is a refusal.
func (c *checker) acmeProviderHasAnImage() {
	if len(c.cfg.GatewaySites()) == 0 || c.cfg.ACME.Provider == "" {
		return // no gateway needs certificates; a missing provider is structural
	}
	if _, ok := acme.Image(c.cfg.ACME.Provider); ok {
		return
	}
	if c.cfg.ACME.Image != "" {
		return
	}
	c.refuse("acme-provider-needs-an-image", "acme.provider",
		"is %q, which this toolkit publishes no image for, and acme.image declares none. A DNS provider is a module compiled into Caddy rather than a setting, and nothing is built on a host, so the gateway would run a binary that cannot load its own configuration. Published providers are %s; for any other, declare acme.image with the module compiled in.",
		c.cfg.ACME.Provider, strings.Join(acme.Providers(), ", "))
}

// acmeImageIsNotStockCaddy refuses upstream's own image as an override.
//
// This is the one thing about an image that can be judged from its reference.
// Upstream's Caddy carries no DNS provider module, so it certainly cannot serve
// a configuration that names one. Every other reference is accepted here and
// verified at apply time by asking the binary, because what is compiled into a
// binary is not a property of its name.
func (c *checker) acmeImageIsNotStockCaddy() {
	if c.cfg.ACME.Image == "" || !acme.IsStockCaddy(c.cfg.ACME.Image) {
		return
	}
	c.refuse("acme-image-is-stock-caddy", "acme.image",
		"is %q, which is upstream's own Caddy image and carries no DNS provider module. Caddy would fail to load a configuration using `acme_dns %s`. Remove this key to take the image this toolkit publishes, or declare one with the module compiled in.",
		c.cfg.ACME.Image, c.cfg.ACME.Provider)
}
```

Add the `internal/acme` import.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/validate/ -v`
Expected: PASS, including the existing check that every rule names a key and gives a message longer than forty characters.

- [ ] **Step 5: Write the README rules the code traces to**

In `README.md`, in the certificates section (`### DNS-01 is what makes step 1 possible`), append:

```markdown
#### The provider is declared, and its module has to be in the binary

A DNS provider in Caddy is a Go module compiled into the binary with xcaddy, not
a setting. Upstream's image carries none, so a configuration naming a provider
cannot load on upstream's image.

```yaml
acme:
  provider: desec
```

That one field decides two things: the `acme_dns` directive in the gateway's
Caddyfile, and which image the gateway runs.

**Nothing is built on a host.** The gateway is also the etcd witness in the
suggested topology, and etcd's stability is a function of fsync latency, so a
compile there competes for exactly the resource that placement was chosen
around. Images with the modules compiled in are published at
`ghcr.io/paisans-software/caddy`, pinned here by digest because every tag that
repository publishes moves, exactly as upstream's do.

The cost is stated rather than hidden: a deployment's gateway depends on that
registry and on those images being rebuilt. The escape hatch is what keeps it
from being lock-in.

**A provider we publish no image for is supported, with an image of your own:**

```yaml
acme:
  provider: route53
  image: ghcr.io/example-org/caddy-route53:2.11.4
```

Declaring a provider with no published image and no `acme.image` is **refused**:
the module would never reach the gateway, so it would hold no certificate for
any hostname. Declaring upstream's own image is **refused** for the same reason,
and it is the only image a reference alone can be judged by.

Everything else is checked at `apply`, by asking the binary which modules it has
rather than inferring it from a name.
```

- [ ] **Step 6: Commit**

```bash
git add internal/validate README.md
git commit -m "feat: refuse a provider whose module cannot reach the gateway

Two refusals, both about the same thing: a DNS provider in Caddy is a module
compiled into the binary, and a gateway running a binary without it holds no
certificate for any hostname.

A provider this toolkit publishes no image for, with no image declared, is
refused rather than warned about, because there is no reading of it that works
now that nothing builds on a host.

Upstream's own image as an override is refused too, and it is the only image a
reference string can be judged by. Every other reference is accepted here and
checked at apply time by asking the binary, since what is compiled in is a
property of the binary rather than of its name.

The README gains the rule both trace to. Policy that is not in the README is
policy nobody agreed to.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task B5: The apply gate that asks the binary

**Files:**
- Modify: `internal/apply/apply.go`, `internal/apply/apply_test.go`

**Interfaces:**
- Consumes: `acme.Module` from Task B1.
- Produces: `apply.Plan.ACMEModule string`, set when a gateway reload is planned; `Execute` refuses to reload when the module is absent.

- [ ] **Step 1: Write the failing test**

Add to `internal/apply/apply_test.go`:

```go
// A gateway is only reloaded once its binary is known to carry the provider's
// module. This is the only check in the whole change that is evidence rather
// than inference: an image reference cannot tell you what was compiled into it.
func TestAGatewayWithoutItsProviderModuleIsNotReloaded(t *testing.T) {
	host := newHost()
	host.fail = "list-modules"

	p, err := apply.Build("vm", plan(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.ACMEModule == "" {
		t.Fatal("a gateway plan carries no module to check for")
	}

	err = apply.Execute(p, host)
	if err == nil {
		t.Fatal("a gateway was reloaded without the module its configuration needs")
	}
	if !strings.Contains(err.Error(), p.ACMEModule) {
		t.Errorf("the refusal does not name the missing module:\n%v", err)
	}
	if host.ran("caddy reload") {
		t.Error("the gateway was reloaded after the module check failed")
	}
	if host.ran("caddy validate") {
		t.Error("the configuration was validated before the binary was known to support it, which wastes the clearer error")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/apply/ -run TestAGatewayWithoutIts -v`
Expected: FAIL, `p.ACMEModule undefined`.

- [ ] **Step 3: Implement the gate**

In `internal/apply/apply.go`, add to `Plan`:

```go
	// ACMEModule is the Caddy DNS module this deployment's gateway must have,
	// as `caddy list-modules` prints it. Empty when this site runs no gateway.
	ACMEModule string
```

`Build` takes the module from the configuration. Change its signature to accept it, and update the one caller in `cmd/paisans/main.go`:

```go
func Build(site string, plan *render.Plan, acmeModule string, t Transport) (*Plan, error) {
```

Set it where `GatewayReload` is decided:

```go
	if out.GatewayReload {
		out.ACMEModule = acmeModule
	}
```

In `Execute`, before the existing validate step:

```go
	if plan.GatewayReload && plan.ACMEModule != "" {
		// Ask the binary rather than trusting the image's name. A DNS provider
		// is compiled into Caddy, so a wrong image or a provider no module
		// answers to both produce a gateway that cannot load its own
		// configuration, and neither is visible in a reference string.
		command := fmt.Sprintf(
			"docker compose -f /srv/infra/compose.yaml run --rm --entrypoint caddy caddy list-modules | grep -qx %s",
			plan.ACMEModule)
		if out, err := t.Run(command); err != nil {
			return fmt.Errorf(
				"%s: the gateway's Caddy has no %s module, so it cannot serve this configuration and was not reloaded. The image it runs was built without that provider:\n%s",
				plan.Site, plan.ACMEModule, out)
		}
	}
```

In `cmd/paisans/main.go`, pass the module:

```go
	plan, err := apply.Build(*site, rendered, acme.Module(cfg.ACME.Provider), transport)
```

Add the `internal/acme` import there.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/apply/ -v`
Expected: PASS, including the existing gateway validation test, which must still fail its reload for its own reason.

- [ ] **Step 5: Run everything**

Run: `go vet ./... && go test ./...`
Expected: all packages pass.

- [ ] **Step 6: Commit**

```bash
git add internal/apply cmd
git commit -m "feat: verify the gateway's caddy has the provider module

Before validating or reloading anything, ask the binary which DNS modules it
has. A wrong image and a provider no module answers to both produce a gateway
that cannot load its own configuration, and neither is visible in an image
reference, so this is the only check here that is evidence rather than
inference.

It runs before the configuration check on purpose. A binary without the module
fails to load the config too, and the error about a missing module says what to
fix while a parse error does not.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task B6: Take the real digest, and record the decision

**Files:**
- Modify: `internal/acme/acme.go`, `docs/decisions.md`, `docs/development.md`

**Interfaces:**
- Consumes: the digest recorded in Task A2 Step 5.
- Produces: a default that pins by digest, which every later Caddy upgrade moves through a pull request.

- [ ] **Step 1: Substitute the published digest**

Take the reference printed by Task A2 Step 5 and replace **both** placeholder values in `internal/acme/acme.go`. Both providers use the same image, because it carries both modules:

```bash
cd ../caddy-dns && reference=$(jq -r '"ghcr.io/paisans-software/caddy:" + .caddy_version + "@" + .digest' built.json) && cd -
echo "$reference"
sed -i '' -E "s|ghcr\.io/paisans-software/caddy:[^\"]*|${reference}|g" internal/acme/acme.go
grep -c "$reference" internal/acme/acme.go
```

Expected: `2`.

- [ ] **Step 2: Prove the image actually has both modules**

```bash
docker run --rm "$reference" caddy list-modules | grep -x dns.providers.cloudflare
docker run --rm "$reference" caddy list-modules | grep -x dns.providers.desec
```

Expected: both print. This is the same check `apply` makes, run once by hand against the exact digest the toolkit now ships.

- [ ] **Step 3: Run the tests**

Run: `go test ./internal/acme/ -v && go test ./...`
Expected: PASS. `TestSupportedProvidersAreComplete` now asserts against a real digest.

- [ ] **Step 4: Regenerate the golden tree**

```bash
go test ./internal/render/ -update
git diff --stat internal/render/testdata/golden
go test ./...
```

Expected: the infra compose files show the real digest.

- [ ] **Step 5: Record the decision**

Append to `docs/decisions.md`:

```markdown

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
```

- [ ] **Step 6: Update the developer notes**

In `docs/development.md`, in the `apply` gates section, add after the gateway validation paragraph:

```markdown
**Before any of that, the gateway's Caddy is asked which DNS modules it has.**
A provider is a module compiled into the binary, so a wrong image and a provider
no module answers to both produce a gateway that cannot load its own
configuration, and neither is visible in an image reference. This check runs
before the configuration check because its error says what to fix.
```

- [ ] **Step 7: Run everything and commit**

```bash
go vet ./... && go test ./...
git add internal/acme internal/render/testdata docs
git commit -m "feat: pin the published caddy image by digest

Takes the digest the image repository published, verified by hand against the
exact reference the toolkit now ships: both DNS modules present.

A digest rather than a tag because every tag that repository publishes moves,
mirroring upstream Caddy, which rebuilds even a patch tag when its base image
gets a security fix. A version tag would look like a pin and not be one.

Records the decision and what it fixed. The abstraction was asked for; the
defect it uncovered was that the rendered gateway could not have obtained a
certificate at all, because upstream's caddy image carries no DNS provider
module and nothing had ever run it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task B7: Open the pull request

**Files:** none.

- [ ] **Step 1: Verify the whole branch**

```bash
go vet ./... && go test ./...
go run ./cmd/paisans validate --config examples/paisans.example.yaml
```

Expected: every package passes, and the example validates with exactly one warning, the deliberate `pinned-app-on-witness` one.

- [ ] **Step 2: Render the example end to end**

```bash
out=$(mktemp -d)
cp examples/paisans.example.yaml "$out/paisans.yaml"
go run ./cmd/paisans init --config "$out/paisans.yaml"
go run ./cmd/paisans render --config "$out/paisans.yaml" --secrets "$out/secrets.enc.yaml" --out "$out/rendered"
grep -r "acme_dns" "$out/rendered"
grep -r "image: ghcr.io/paisans-software/caddy" "$out/rendered"
```

Expected: the provider from the example appears in the Caddyfile, and the gateway's compose file names the digest pinned image.

- [ ] **Step 3: Push and open the pull request**

```bash
git push -u origin feat/acme-dns-provider
gh pr create --base feat/init-and-apply --head feat/acme-dns-provider \
  --title "Declare the ACME DNS provider, and publish an image that has it" \
  --body "Implements \`docs/specs/2026-09-15-acme-dns-provider.md\`. Stacked on #3.

## What this fixes, not just abstracts

The toolkit rendered upstream's \`caddy:2-alpine\` against \`acme_dns cloudflare\`. A DNS provider in Caddy is a module compiled in with xcaddy and upstream's image carries none, so **the rendered gateway could not have obtained a certificate at all.** It had never been run.

## The change

- \`acme.provider\` decides the Caddyfile directive and the image, top level because the gateway role moves between machines and certificates do not move with it.
- \`external.cloudflare_api_token\` becomes \`external.acme_dns_token\`, rendered as \`ACME_DNS_TOKEN\`.
- Images come from \`Paisans-Software/caddy-dns\`, one image with both modules, pinned here by digest because every tag it publishes moves, mirroring upstream.
- Two refusals: a provider with no published image and no declared one, and upstream's own image as an override.
- \`apply\` asks the binary which modules it has before validating or reloading anything. That is the only check here that is evidence rather than inference.
- A provider we publish nothing for is supported through \`acme.image\`.

## Verification

\`go vet ./...\` and \`go test ./...\` pass. The example validates with its one deliberate warning, and \`init\` plus \`render\` produce a gateway configuration naming the declared provider and an image carrying its module. The digest was checked by hand with \`caddy list-modules\` against the exact reference shipped.

**Not run against a real host.**

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

---

## Self-review

**Spec coverage.** Image repository: Tasks A1 to A3. Poll, smoke gate and tag ladder: A2. Pull request back to the toolkit: A4. `acme` block: B2. Secret rename: B3. Provider driven Caddyfile and image: B3. Two refusals: B4. Apply gate: B5. Digest pinned default and records: B6. Repository move: already done, recorded in `docs/decisions.md` and Outline.

**Not covered, deliberately:** the `TestDefaultsArePinned` tightening described in the spec, which applies to `internal/kinds` and is superseded by `internal/acme`'s own test asserting a digest for images we publish. The spec's sentence about it describes intent that Task B1's test satisfies in a better place.

**Placeholders.** The digest in Task B1 is a syntactically valid placeholder replaced in Task B6, and both tasks say so. No step says "add error handling" or "write tests for the above".

**Type consistency.** `acme.Module`, `acme.Image`, `acme.Providers`, `acme.IsStockCaddy` are defined in B1 and used with those exact names in B3, B4 and B5. `config.ACME` with `Provider` and `Image` is defined in B2 and used in B3, B4, B5. `apply.Build` gains a parameter in B5 and its only caller is updated in the same task.

**One ordering hazard, stated rather than buried:** Task A4 depends on `internal/acme/acme.go` existing, which Task B1 creates. Running A4's verification before B1 is merged means the `sed` matches nothing, which that step already describes as correct rather than a failure.

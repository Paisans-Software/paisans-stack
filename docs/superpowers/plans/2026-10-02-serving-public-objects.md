# Serving public objects Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Mbin's media fetchable by a browser and by a federating server, without making Outline's document attachments fetchable by anyone.

**Architecture:** Garage's only anonymous read path is its `s3_web` endpoint, which resolves a bucket from the request's Host. The toolkit renders that endpoint, provisioning allows website access on public buckets only, and the gateway rewrites a path style URL into the vhost style request `s3_web` expects for those buckets while leaving everything else on the S3 API with `Host` preserved.

**Tech Stack:** Go, no new dependencies. Garage `dxflrs/garage:v1.0.1` and Caddy `ghcr.io/paisans-software/caddy:2.11.4`, both driven as real containers in the integration test.

**Spec:** `docs/specs/2026-10-02-serving-public-objects.md`

## Global Constraints

- No em dashes anywhere in code, comments, documentation or commit messages.
- Every commit message ends with exactly: `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>` and no other attribution, whatever model writes it.
- `go test ./... -count=1` and `go vet ./...` pass before every commit.
- Templates live in `internal/render/templates/` and are embedded with `//go:embed all:templates`. The `all:` prefix is required or dotfiles are skipped, and a shell glob does not match dotfiles either. Run `git check-ignore -v` on any new template and confirm it is tracked in the commit. This repository has lost template files to that trap twice.
- The golden tree at `internal/render/testdata/golden` is a specification of what an operator receives. Regenerate with `go test ./internal/render -run TestGoldenTree -update`, read `git diff` on it, and confirm every changed file is explained by the task. Never regenerate blind.
- `internal/render/testdata/deployment.yaml` must pass `validate.Check` with zero refusals; `fixture(t)` enforces it and every render test goes through that helper.
- Never claim in a commit message or report that you verified something you did not run yourself. Where you reason instead, say so in those words.

## Garage and Caddy behaviour, captured from the pinned images

Produced by running `dxflrs/garage:v1.0.1` with a real credential and real HTTP requests. Tasks 2, 3 and 4 depend on these. Do not invent variations.

| Request | Result |
|---|---|
| Anonymous GET, S3 API on 3900, path style | `403` with body `Garage does not support anonymous access yet` |
| Anonymous GET, `s3_web` on 3902, `Host: talk-uploads.web.example.org`, path `/a/pic.png` | `200`, object body |
| The same against a bucket with no `bucket website --allow` | `404`, body `Not found` |
| `s3_web`, a key that does not exist in an allowed bucket | `404`, body `API error: Key not found` |
| Presigned GET, S3 API on 3900, `Host` forwarded unchanged | `200`, object body |

`garage bucket website --allow talk-uploads` prints `Website access allowed for talk-uploads`.

Caddy 2.11.4 sorts same directive routes by path matcher length, longest first, so a `/<bucket>/*` matcher is evaluated before a bare `handle`. File order is not what decides this, which this repository already recorded in `docs/decisions.md`.

---

## File Structure

**Modified:**
- `internal/kinds/kinds.go`: `ServesObjectsPublicly`.
- `internal/render/templates/garage.toml.tmpl`: the `[s3_web]` section.
- `internal/render/templates/media.caddy.snippet.tmpl`: per bucket routes plus the S3 API fallback.
- `internal/render/site.go` and `internal/render/appview.go`: the data those routes need.
- `internal/garage/provision.go`: the website step.
- `internal/garage/provision_test.go`: its unit coverage.
- `internal/garage/integration_test.go`: the anonymous fetch, through a real Caddy.
- `README.md`: remove the limitation, describe what now happens.
- `docs/decisions.md`: the record.

---

### Task 1: A kind says whether its objects are public, and Garage gains the endpoint that can serve them

**Files:**
- Modify: `internal/kinds/kinds.go`
- Modify: `internal/render/templates/garage.toml.tmpl`
- Test: `internal/kinds/kinds_test.go`, `internal/render/render_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `kinds.ServesObjectsPublicly(kind config.Kind) bool`, true for mbin only; an `[s3_web]` section in every Garage site's rendered `garage.toml`.

- [ ] **Step 1: Write the failing test**

In `internal/render/render_test.go`:

```go
// Garage's S3 API has no anonymous mode at all, so the only way a browser can
// fetch an object is the s3_web endpoint. Its root_domain is a suffix nothing
// resolves: the gateway is the only thing that ever sends a Host matching it,
// and it never leaves the gateway, so there is no DNS record and no
// certificate for an adopter to set up.
func TestGarageServesAWebEndpointOnAnInternalSuffix(t *testing.T) {
	tree := build(t)
	conf := tree.file(t, "home-a/srv/infra/garage/garage.toml")
	if !strings.Contains(conf, "[s3_web]") {
		t.Errorf("no s3_web section, so nothing can read an object anonymously:\n%s", conf)
	}
	if !strings.Contains(conf, `root_domain = ".web.garage.internal"`) {
		t.Errorf("the web root domain should be the internal suffix, got:\n%s", conf)
	}
	if !strings.Contains(conf, ":3902") {
		t.Error("s3_web should bind 3902")
	}
	if strings.Contains(conf, "index =") {
		t.Error("no index document: a prefix with no object must 404 rather than return something else")
	}
}
```

Use whatever helper this file already has for reading a rendered file; `tree.file` is illustrative, so match the surrounding tests.

In `internal/kinds/kinds_test.go`:

```go
// Mbin's media must be fetchable by a federating server with no credential,
// which is why its bucket is public. Outline's bucket holds document
// attachments and must never be.
func TestOnlyMbinServesObjectsPublicly(t *testing.T) {
	for _, kind := range config.Kinds() {
		want := kind == config.KindMbin
		if got := kinds.ServesObjectsPublicly(kind); got != want {
			t.Errorf("%s: ServesObjectsPublicly is %v, want %v", kind, got, want)
		}
	}
	if kinds.ServesObjectsPublicly(config.KindOutline) {
		t.Fatal("outline's bucket holds document attachments and must never be world readable")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/kinds ./internal/render -run 'TestOnlyMbinServesObjectsPublicly|TestGarageServesAWebEndpointOnAnInternalSuffix' -v`
Expected: FAIL, `undefined: kinds.ServesObjectsPublicly`, and no `[s3_web]` in the rendered file.

- [ ] **Step 3: Add the predicate**

In `internal/kinds/kinds.go`, beside `UsesObjectStorage`:

```go
// ServesObjectsPublicly reports whether a kind's objects must be readable
// without a credential.
//
// Mbin's are: a federating server fetching an image is a machine with no
// account here, and a remote instance caches the URL it was given. Outline's
// are not, because its bucket holds the attachments of documents that are
// readable only to members, and its server presigns every read it issues.
//
// This is deliberately not a configuration key. Garage's website access is per
// bucket and opt in, so the only way Outline's bucket becomes world readable
// is a change to this function, which is a code change with a review rather
// than a line somebody edits at two in the morning.
func ServesObjectsPublicly(kind config.Kind) bool {
	return kind == config.KindMbin
}
```

- [ ] **Step 4: Render the endpoint**

In `internal/render/templates/garage.toml.tmpl`, after the `[s3_api]` section:

```
[s3_web]
# The only way an object is readable without a credential. Garage's S3 API
# refuses every unauthenticated request outright, so Mbin's media would answer
# 403 to a browser without this endpoint.
#
# root_domain is a suffix that resolves nowhere on purpose. This endpoint
# resolves a bucket from the request's Host, and the gateway is the only thing
# that ever sends one: it rewrites a path style media URL into the vhost form
# this expects. Nothing outside the mesh ever sends a Host matching it, so
# there is no DNS record and no certificate here.
#
# No index document. A prefix with no object must be a 404.
bind_addr = "{{ .Site.Address }}:3902"
root_domain = ".web.garage.internal"
```

- [ ] **Step 5: Run the tests, then regenerate the golden tree**

Run: `go test ./internal/kinds ./internal/render -run 'TestOnlyMbinServesObjectsPublicly|TestGarageServesAWebEndpointOnAnInternalSuffix' -v`
Expected: PASS.

Then `go test ./internal/render -run TestGoldenTree -update`, then `git diff internal/render/testdata/golden`. Expected: `garage.toml` on each Garage site, and the manifests. Anything else is a finding: stop and say so.

**One existing test will now matter.** The previous branch added coverage that boots the real image against the rendered `garage.toml`. Run it: `go test -tags garage_integration ./internal/garage -count=1`. A malformed `[s3_web]` section will stop Garage starting, and that test is what tells you. Put its output in your report.

- [ ] **Step 6: Commit**

```bash
go test ./... -count=1 && go vet ./...
git add internal/kinds internal/render
git commit -m "feat: render the endpoint that can serve an object anonymously

$(cat <<'MSG'
Garage's S3 API refuses every unauthenticated request, so Mbin's media answers
403 to a browser today. Its s3_web endpoint is the only anonymous path, and it
resolves a bucket from the request's Host.

root_domain is an internal suffix that resolves nowhere. The gateway is the
only thing that ever sends a Host matching it, so this adds no DNS record and
no certificate.

Which kinds serve objects publicly is a function rather than a configuration
key, so the only way Outline's bucket becomes world readable is a code change
with a review.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 2: The gateway rewrites a public bucket's path into the request Garage wants

**Files:**
- Modify: `internal/render/templates/media.caddy.snippet.tmpl`
- Modify: `internal/render/site.go`, `internal/render/appview.go`
- Test: `internal/render/render_test.go`

**Interfaces:**
- Consumes: `kinds.ServesObjectsPublicly` from Task 1.
- Produces: a rendered `media.caddy` carrying one route per public bucket plus the S3 API fallback. The view needs, per public bucket, its name and the internal vhost `<bucket>.web.garage.internal`.

- [ ] **Step 1: Write the failing test**

```go
// A public bucket's objects reach the web endpoint, which needs a vhost style
// Host. Everything else stays on the S3 API with Host untouched, because that
// is what makes Outline's presigned URLs verify.
//
// Caddy sorts same directive routes by path matcher length, longest first, so
// this asserts matcher length rather than the order the lines happen to be
// written in. The file order is not what decides it.
func TestOnlyAPublicBucketIsRewrittenToTheWebEndpoint(t *testing.T) {
	tree := build(t)
	snippet := tree.file(t, "vm/srv/infra/caddy/snippets/media.caddy")

	if !strings.Contains(snippet, "handle /talk-uploads/*") {
		t.Errorf("mbin's bucket should have its own route:\n%s", snippet)
	}
	if !strings.Contains(snippet, "uri strip_prefix /talk-uploads") {
		t.Error("the bucket prefix must be stripped: the web endpoint takes the key as the path")
	}
	if !strings.Contains(snippet, "header_up Host talk-uploads.web.garage.internal") {
		t.Error("the web endpoint resolves the bucket from Host, so Host must be rewritten")
	}
	if !strings.Contains(snippet, ":3902") {
		t.Error("a public bucket's reads go to the web endpoint")
	}

	// Outline's bucket must not appear at all. Its attachments are private and
	// its reads are presigned, so they belong on the S3 API with the fallback.
	if strings.Contains(snippet, "docs-uploads") {
		t.Errorf("outline's bucket must not be routed to the anonymous endpoint:\n%s", snippet)
	}

	// The fallback keeps Host, which is the whole reason presigned URLs verify.
	if !strings.Contains(snippet, ":3900") {
		t.Error("the fallback should reach the S3 API")
	}
	if strings.Contains(snippet, "header_up Host {upstream_hostport}") {
		t.Error("the fallback must not rewrite Host")
	}

	// Matcher length is what Caddy sorts on, so prove the public route's
	// matcher is longer than the fallback's rather than trusting file order.
	if len("/talk-uploads/*") <= 0 {
		t.Fatal("unreachable, kept so the intent of the assertion below is clear")
	}
}
```

Replace `talk-uploads` and `docs-uploads` with whatever the fixture's Mbin and Outline buckets are actually called; read `internal/render/testdata/deployment.yaml` and the existing golden `.env` files rather than assuming. Delete the final placeholder assertion and instead assert, against the rendered Caddyfile or snippet, that the bare `handle` has no path matcher while the bucket route does, which is what makes Caddy's sort put the bucket route first.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/render -run TestOnlyAPublicBucketIsRewrittenToTheWebEndpoint -v`
Expected: FAIL, the snippet is a single `reverse_proxy` to 3900 today.

- [ ] **Step 3: Give the view what it needs**

In `internal/render/appview.go` or `site.go`, whichever already assembles the media snippet's data, add a list of public buckets. Each entry carries the bucket name and the internal vhost. Build it from `cfg.AppNames()` so the order is deterministic, including only apps where both `kinds.UsesObjectStorage` and `kinds.ServesObjectsPublicly` are true, and using the same bucket name the app's own S3 values use so the two cannot drift.

- [ ] **Step 4: Render the routes**

Rewrite `internal/render/templates/media.caddy.snippet.tmpl`:

```
# Rendered by paisans. Do not edit: `paisans apply` overwrites this file.
#
# Object storage, served to browsers and to federating servers.
#
# NEVER GATE THIS HOSTNAME. A federating server fetching an image is a machine,
# and it will not follow a redirect to a passkey prompt.
#
# Two upstreams, and which one a request takes is decided by its bucket.
#
# A public bucket goes to Garage's web endpoint on 3902. That endpoint is the
# only way Garage serves an object with no credential, and it resolves the
# bucket from the request's Host, so the prefix is stripped from the path and
# the Host is rewritten to an internal name that resolves nowhere.
#
# Everything else goes to the S3 API on 3900 with Host forwarded unchanged.
# That is load bearing rather than tidy: Outline presigns its reads, the
# signature covers the host it was signed with, and rewriting this header
# invalidates every URL Outline issues.
#
# Caddy sorts these by path matcher length, longest first, so the bucket routes
# are evaluated before the fallback no matter what order they are written in.
{{- range .PublicBuckets }}
handle /{{ .Bucket }}/* {
	uri strip_prefix /{{ .Bucket }}
	reverse_proxy {{ $.GarageAddress }}:3902 {
		header_up Host {{ .Vhost }}
	}
}
{{- end }}
handle {
	reverse_proxy {{ .GarageAddress }}:3900
}
```

Match the field names to whatever you called them in step 3.

- [ ] **Step 5: Run the test, regenerate, read the diff**

Run: `go test ./internal/render -run TestOnlyAPublicBucketIsRewrittenToTheWebEndpoint -v`
Expected: PASS.

Then `go test ./internal/render -run TestGoldenTree -update` and `git diff internal/render/testdata/golden`. Expected: the gateway's `media.caddy` and the manifest. If any app's `.env` changed, something is wrong: the published URL form must not move, because Mbin has already written URLs of that shape into federated posts.

- [ ] **Step 6: Commit**

```bash
go test ./... -count=1 && go vet ./...
git add internal/render
git commit -m "feat: route a public bucket to the endpoint that can serve it

$(cat <<'MSG'
The media hostname now has two upstreams and the bucket decides which. A public
bucket's objects go to Garage's web endpoint with the prefix stripped and the
Host rewritten to an internal name, because that endpoint resolves a bucket
from Host and is the only one that serves an object without a credential.

Everything else stays on the S3 API with Host forwarded unchanged, which is
what makes Outline's presigned URLs verify.

The published URL form does not change. Mbin has written URLs of that shape
into federated posts and remote instances keep them.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 3: Provisioning allows website access, on public buckets only

**Files:**
- Modify: `internal/garage/provision.go`
- Test: `internal/garage/provision_test.go`

**Interfaces:**
- Consumes: `kinds.ServesObjectsPublicly` from Task 1.
- Produces: one additional planned step per public bucket, `garage bucket website --allow <bucket>`, after that bucket exists.

- [ ] **Step 1: Write the failing tests**

```go
// Website access is what makes an object readable with no credential, and it
// is per bucket. Mbin's bucket needs it. Outline's must never have it, because
// its bucket holds the attachments of documents only members can read.
func TestOnlyAPublicBucketIsAllowedWebsiteAccess(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "Connection established to 51494feb5444d466\n==== CURRENT CLUSTER LAYOUT ====\nID  Tags  Zone  Capacity\n51494feb5444d466  []  home-a  100.0 GB\n\nCurrent cluster layout version: 1\n"},
		"node id -q":  {out: "51494feb5444d466aaaabbbbccccddddeeeeffff00001111222233334444abcd@10.44.0.1:3901\n"},
		"key info":    {out: "Key name: talk\nKey ID: GK00112233445566778899aabb\n"},
		"bucket info": {out: "Bucket: cfc236316d4a81858f84f84c287f5a0d\nSize: 0 B\nObjects: 0\n"},
	}}

	plan, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}

	var website []string
	for _, s := range plan.Steps {
		if strings.Contains(s.Command, "bucket website") {
			website = append(website, s.Command)
		}
	}
	joined := strings.Join(website, "\n")
	if !strings.Contains(joined, "bucket website --allow talk-uploads") {
		t.Errorf("mbin's bucket needs website access or its media stays unreadable, got:\n%s", joined)
	}
	if strings.Contains(joined, "docs-uploads") {
		t.Errorf("outline's bucket must never be world readable, got:\n%s", joined)
	}
}
```

Use the fixture's real bucket names. Reuse the existing `fakeTransport`, `fixtureConfig` and `fixtureSecrets` helpers in that file rather than adding new ones, and note that the `layout show` fixture above carries the RPC preamble on purpose: the suite learned that the hard way.

Add a second test asserting the website step is planned **after** the bucket create step for the same bucket, by comparing indices. Garage cannot allow website access on a bucket that does not exist, and the existing ordering test is the model for how to assert this.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/garage -run TestOnlyAPublicBucketIsAllowedWebsiteAccess -v`
Expected: FAIL, no `bucket website` step is planned.

- [ ] **Step 3: Plan the step**

In `internal/garage/provision.go`, in the per app loop, after the bucket create step and the `bucket allow` grant, add for an app whose kind serves objects publicly:

```go
// Website access is what lets a request with no credential read this bucket.
// It is planned unconditionally, like the grant above, because it is a set
// rather than a create: see the note on idempotency in this task.
//
// Nothing here ever revokes it. A bucket that stops being public is a change
// to the kinds catalogue, which is a code change with a review, and a
// provisioner that guessed a human meant to withdraw public access would be
// guessing about the one thing in this package that cannot be undone quietly.
```

Write the step out in full rather than leaving the comment.

**Idempotency is not established and you must establish it.** Every other step in this planner was settled by running the real thing. Run `garage bucket website --allow` twice against a real container and record what the second run says. If it errors, the step must be check-then-act like `key import` and `bucket create`, and you need a marker from `bucket info` to detect it. If it succeeds, it is a set like `bucket allow` and can be planned unconditionally. Put the actual output in your report either way, and make the code match what you observed rather than what this plan guessed.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/garage -count=1 -v`
Expected: the new tests pass and the existing ones still do, including the second run planning nothing new if you took the check-then-act route.

- [ ] **Step 5: Commit**

```bash
go test ./... -count=1 && go vet ./...
git add internal/garage
git commit -m "feat: allow website access on public buckets only

$(cat <<'MSG'
Website access is what makes an object readable with no credential, and Garage
scopes it per bucket. Mbin's bucket gets it because federation requires a
machine with no account to fetch media. Outline's does not, and the planner has
no way to express that it should: the decision comes from the kind.

Nothing revokes it. Withdrawing public access is a change to the kinds
catalogue, reviewed like any other code change, rather than something a
provisioner infers.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 4: A browser with no credential reads Mbin's media and cannot read Outline's

**Files:**
- Modify: `internal/garage/integration_test.go`
- Modify: `README.md`
- Modify: `docs/decisions.md`

**Interfaces:**
- Consumes: everything above.
- Produces: nothing other code depends on.

This task is the reason the branch exists. Everything before it is machinery whose correctness is argued; this is where it is observed.

- [ ] **Step 1: Extend the integration test**

In `internal/garage/integration_test.go`, behind the existing `garage_integration` build tag, add a test that:

1. Starts `dxflrs/garage:v1.0.1` against the **rendered** `garage.toml`, reusing the helper the previous branch added for exactly this, so the `[s3_web]` section under test is the one an operator receives.
2. Provisions it by running the real plan through `garage.Build` and `garage.Execute`.
3. Uploads an object into the Mbin bucket with the **rendered credential**, signing a real SigV4 request rather than shelling out to a client that is not a dependency of this repository.
4. Starts `ghcr.io/paisans-software/caddy:2.11.4` with the **rendered** `media.caddy` snippet and a minimal Caddyfile that serves the media hostname, on the same Docker network as Garage.
5. **Fetches the object anonymously through Caddy**, with `Host` set to the media hostname and no credential of any kind, and asserts `200` and the object's bytes.
6. Uploads an object into Outline's bucket the same way, fetches it anonymously through Caddy, and asserts it is **refused** rather than returned. Assert on the absence of the object's bytes as well as the status, because a 200 carrying an error document would otherwise pass.
7. Fetches a presigned URL for Outline's object through Caddy and asserts `200`, which is the property the fallback route exists for and the one most likely to break when the routing gains a branch.

Register cleanup for both containers and the network with `t.Cleanup`, and skip with a clear message when `docker` is absent.

- [ ] **Step 2: Run it**

Run: `go test -tags garage_integration ./internal/garage -v -count=1`
Expected: PASS. Put the real output in your report, including the status codes for all three fetches.

Then `docker ps -a` and confirm nothing survives.

**If the anonymous fetch does not return 200, stop and report it loudly rather than adjusting the test until it passes.** The design rests on behaviour I probed by hand; if the probe was wrong about anything, this is where it surfaces, and the plan being wrong is a better outcome than a test shaped to agree with it.

- [ ] **Step 3: Correct the README**

`README.md` currently documents the limitation this branch removes: that Garage's S3 API has no anonymous reads, that Mbin's media does not work, and that the fix is scoped separately. Replace it with what now happens: a public bucket's objects are served through Garage's web endpoint, the gateway rewrites the request, private buckets are never routed there, and which kinds are public is a property of the kind rather than a setting.

Keep the sentence that a federating server fetching an image is a machine with no account, because that is why any of this exists.

- [ ] **Step 4: Record the decision**

Append to `docs/decisions.md`, in the established style: that Garage's S3 API has no anonymous mode and the web endpoint is the only one that does; that the gateway rewrites path style to vhost style rather than changing the published URL, because remote instances cache Mbin's URLs; that public is derived from the kind with no configuration key, and the declared field was rejected; and that the internal suffix resolves nowhere on purpose.

State that the whole path is covered by an integration test that fetches anonymously through a real Caddy and a real Garage, and name it.

- [ ] **Step 5: Commit**

```bash
go test ./... -count=1 && go vet ./... && go test -tags garage_integration ./internal/garage -count=1
git add internal/garage README.md docs/decisions.md
git commit -m "test: fetch Mbin's media anonymously, and fail to fetch Outline's

$(cat <<'MSG'
The branch's claim is that a browser with no credential can read Mbin's media
and cannot read Outline's. This observes it: a real Garage behind a real Caddy,
the rendered configuration for both, an object uploaded with the rendered
credential, and three fetches. The anonymous read of the public bucket returns
the object, the anonymous read of the private bucket is refused, and a
presigned read of the private bucket still succeeds through the fallback.

That last one is the property most likely to break now that the routing has a
branch, and nothing cheaper than this test would notice.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Self-review

**Spec coverage.** `ServesObjectsPublicly` and the `[s3_web]` section: Task 1. The per bucket routes, the prefix strip, the Host rewrite and the preserved fallback: Task 2. The website step and its ordering: Task 3. The anonymous fetch, the private refusal, the presigned fallback, the README and the record: Task 4. The spec's "nothing changes in configuration" is covered negatively by Task 2's step 5, which fails the task if any app's `.env` moves.

**The spec's one open question is assigned.** Whether `bucket website --allow` is idempotent is unestablished, and Task 3 step 3 requires the implementer to run it twice against a real container and shape the code to what it sees rather than to what this plan guessed.

**Placeholders.** Task 2's step 1 contains a deliberately useless final assertion with instructions to replace it, which is a seam rather than a gap: the real assertion depends on how the fixture names its buckets, and the step says to read the fixture instead of guessing. No step says "add error handling".

**Type consistency.** `kinds.ServesObjectsPublicly` is declared in Task 1 and used by name in Tasks 2 and 3. `PublicBuckets`, with `Bucket` and `Vhost`, is introduced in Task 2 and used only there; Task 2 step 4 says to match the template to the names chosen in step 3 rather than assuming these. `garage.Build` and `garage.Execute` keep the signatures the previous branch established.

**One risk the plan cannot remove.** Task 4 depends on the pinned Caddy image accepting `uri strip_prefix` inside a `handle` block with a `header_up Host` override, which I have not run. If Caddy rejects that shape, Task 4 fails loudly rather than silently, which is the correct place for that discovery.

---

## Amendment, 2026-10-04: `writefreely` becomes the wisp fork

Tasks 5 and 6 implement the spec's amendment of the same date. They are
independent of Tasks 1 to 4 except where noted: nothing here touches the media
hostname, the `s3_web` endpoint or the gateway routing, because the fork serves
its own images and never exposes its bucket.

### What this change contradicts, found before dispatching

Four existing tests assert the truth this amendment reverses, and one refusal
loses its only fixture. Each must be updated deliberately rather than
discovered by a red suite:

| Location | Asserts today | After |
|---|---|---|
| `internal/kinds/kinds_test.go:21` | `UsesObjectStorage(writefreely)` is false | true |
| `internal/render/render_test.go:230` | no file under `blog/` contains `postgresql://`, "which it has never supported" | its config carries a Postgres host, so the assertion inverts |
| `internal/secretsgen/secretsgen_test.go:137` | WriteFreely gets no `database_password` | it gets one |
| `internal/render/render_test.go:545` | lists the kinds shipping a template set | unchanged, writefreely still ships one |
| `internal/validate/testdata/cluster-placement-without-a-cluster.yaml:56` | uses writefreely as the kind with no Postgres, to trip that refusal | **the fixture stops tripping the rule and must switch kinds** |

That last row is the one that matters. The rule refuses `placement: cluster`
for a kind with no Postgres service, and writefreely was its example. After
this change writefreely has one, so the fixture validates cleanly and the rule
has no coverage at all. The fixture must move to a kind that still has no
Postgres: `element` or `oauth2-proxy`. Check which of those the fixture can use
without tripping a different rule, and say in the commit message which you
chose and why.

---

### Task 5: The kind gains Postgres and object storage, and the fork's image

**Files:**
- Modify: `internal/kinds/kinds.go`
- Modify: `internal/kinds/kinds_test.go`
- Modify: `internal/validate/testdata/cluster-placement-without-a-cluster.yaml`
- Test: `internal/kinds/kinds_test.go`, `internal/validate/validate_test.go`

**Interfaces:**
- Consumes: `kinds.ServesObjectsPublicly` from Task 1, which must return **false** for writefreely.
- Produces: `writefreely` with a `postgres` service, `UsesPostgres` and `UsesObjectStorage` true, `ServesObjectsPublicly` false, and the fork's image as its default.

- [ ] **Step 1: Write the failing test**

```go
// The wisp fork supports Postgres and S3, so the blog joins the same cluster
// and the same object store as everything else. Its objects are not public:
// the fork streams images through its own /uploads/ route rather than emitting
// an S3 URL, so nothing anonymous ever reaches its bucket.
func TestWriteFreelyUsesPostgresAndPrivateObjectStorage(t *testing.T) {
	if !kinds.UsesPostgres(config.KindWriteFreely) {
		t.Error("the wisp fork keeps its data in Postgres")
	}
	if !kinds.UsesObjectStorage(config.KindWriteFreely) {
		t.Error("the wisp fork keeps uploaded images in S3")
	}
	if kinds.ServesObjectsPublicly(config.KindWriteFreely) {
		t.Fatal("the fork serves its own images, so its bucket must never be world readable")
	}
	var sawPostgres bool
	for _, s := range kinds.Services(config.KindWriteFreely) {
		if s.Name == kinds.PostgresService {
			sawPostgres = true
		}
	}
	if !sawPostgres {
		t.Error("a pinned blog runs its own Postgres beside itself, so the kind needs that service")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/kinds -run TestWriteFreelyUsesPostgresAndPrivateObjectStorage -v`
Expected: FAIL on every assertion.

- [ ] **Step 3: Change the catalogue entry**

Replace the comment that says WriteFreely has never supported Postgres. The
entry gains a `postgres` service alongside `app`, exactly as mbin and outline
have one, and its app image becomes:

```
ghcr.io/josephquigley/writefreely-wisp@sha256:4d21f45879bd98c8485eb8169ea57fbab925f0cbd5a38ac3c3bdd79901d809ea
```

The comment must say three things, because each is a fact somebody will
otherwise have to rediscover: that this kind means the wisp fork rather than
upstream WriteFreely, that upstream's image will reject the configuration this
kind renders, and that the digest is a `develop` build because no release
carries Postgres or S3 yet, with a note to bump it when one does.

Do not take the digest on trust. Resolve it yourself and confirm it is a
multi architecture index covering amd64 and arm64, and that its binary
contains the `s3_secret_access_key` ini tag. Put the commands and their output
in your report. If the digest no longer resolves or lacks the features, stop
and report that rather than pinning something else.

- [ ] **Step 4: Update the three tests that assert the old truth**

`internal/kinds/kinds_test.go:21` flips `config.KindWriteFreely` to true in the
`UsesObjectStorage` table. Add writefreely to whatever table covers
`UsesPostgres`, and to the `ServesObjectsPublicly` coverage from Task 1 as
false.

`internal/secretsgen/secretsgen_test.go:137` asserts WriteFreely gets no
`database_password`. It now gets one. Invert the assertion and reword the
comment, which currently says it has never supported Postgres.

`internal/render/render_test.go:230` asserts no file under `blog/` contains
`postgresql://`. Task 6 renders the Postgres connection, so this assertion
belongs to Task 6's step where the rendering lands. In this task, leave it
alone: it still passes, because nothing renders a connection yet.

- [ ] **Step 5: Move the refusal's fixture to a kind that still has no Postgres**

`internal/validate/testdata/cluster-placement-without-a-cluster.yaml` uses
writefreely to trip `cluster-placement-without-a-cluster`, and the comment in
it says WriteFreely has no Postgres so there is no cluster for it to join.
After step 3 that is false and the fixture validates cleanly, which silently
removes the rule's only coverage.

Switch it to `element` or `oauth2-proxy`, whichever does not trip a different
rule, and update the comment to say why that kind has no cluster to join. Run
the validate suite and confirm the fixture still produces exactly one finding,
and that it is still `cluster-placement-without-a-cluster`.

- [ ] **Step 6: Run everything and commit**

```bash
go test ./... -count=1 && go vet ./...
git add internal/kinds internal/validate
git commit -m "feat: writefreely means the wisp fork, with Postgres and S3

$(cat <<'MSG'
The fork supports both, so the blog joins the same Patroni cluster and the same
Garage node as everything else rather than running a SQLite file nothing else
can reach.

Its objects stay private. The fork streams images through its own /uploads/
route from whichever store is configured and never emits or presigns an S3 URL,
so nothing anonymous reaches its bucket and it needs no website access.

This breaks an adopter running upstream's image, which has no Postgres and no
[storage] section and will reject what this kind renders. The README and the
decision record say so in those words.

The image is a develop build by digest because no release carries these
features. The digest was resolved and its binary checked rather than trusted.

cluster-placement-without-a-cluster used writefreely as its example of a kind
with no Postgres. That is no longer true, so the fixture moved to a kind that
still has none, or the rule would have kept passing with no coverage.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 6: Render the fork's Postgres and S3 configuration

**Files:**
- Modify: `internal/render/templates/writefreely/config.ini.secret.tmpl`
- Modify: `internal/render/templates/writefreely/compose.yaml.tmpl`
- Modify: `internal/render/render_test.go`
- Modify: `README.md`, `docs/decisions.md`, `examples/paisans.example.yaml`
- Test: `internal/render/render_test.go`

**Interfaces:**
- Consumes: everything from Task 5, plus the per app S3 credentials and `DSN` the app view already provides for mbin and outline.
- Produces: a `config.ini` whose `[database]` and `[storage]` sections match the fork's own keys.

- [ ] **Step 1: Write the failing test**

```go
// The fork reads these keys and validates them at load rather than at the
// first upload, so a wrong key name is a container that will not start.
// Checked against config/config.go and config/storage.go on the fork's
// develop branch.
func TestTheBlogReadsPostgresAndS3(t *testing.T) {
	tree := build(t)
	conf := tree.file(t, "home-a/srv/blog/config.ini")

	for _, want := range []string{
		"type = postgres",
		"type = s3",
		"s3_endpoint = http://10.44.0.1:3900",
		"s3_bucket = blog-uploads",
		"s3_access_key_id = ",
		"s3_secret_access_key = ",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config.ini is missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "type = sqlite3") {
		t.Error("the fork keeps its data in Postgres, so the SQLite section should be gone")
	}
	if strings.Contains(conf, "s3_virtual_host = true") {
		t.Error("Garage needs path style addressing, which is the fork's default")
	}
}
```

Read the fixture before fixing the expected endpoint and bucket: the endpoint
is whatever `garageEndpointHost` yields for the fixture and the bucket is the
app's own name plus `-uploads` unless a setting overrides it. Assert the
endpoint is the same internal address Mbin writes through rather than a
hardcoded value, the way Task 3 of the previous branch did.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/render -run TestTheBlogReadsPostgresAndS3 -v`
Expected: FAIL, the template still renders `type = sqlite3` and no storage
section.

- [ ] **Step 3: Render the two sections**

Replace the `[database]` section in
`internal/render/templates/writefreely/config.ini.secret.tmpl`:

```
[database]
; Postgres, the same cluster everything else uses. A pinned blog talks to the
; Postgres container beside it; a clustered one talks to the local HAProxy,
; which is what .DSN already resolves for every other kind.
;
; tls is false because the connection does not leave the host: the fork maps
; true to sslmode=require and false to sslmode=disable, so this is the
; difference between requiring TLS to a local socket and not.
type = postgres
host = {{ .DBHost }}
port = {{ .DBPort }}
database = {{ .DBName }}
username = {{ .DBUser }}
password = {{ .DBPassword }}
tls = false
```

and add, after `[uploads]` if there is one or beside it:

```
[storage]
; Uploaded images live in Garage rather than on the container's disk, so a
; clustered blog does not pin itself to one node by its own uploads.
;
; This bucket is NOT public, and that is not an oversight. The fork streams
; images through its own /uploads/ route with http.ServeContent, from whichever
; store is configured, and never emits or presigns an S3 URL. Nothing anonymous
; ever reaches this bucket, so it gets no website access and no route on the
; media hostname.
;
; s3_virtual_host is left at its default, which is path style, because that is
; what Garage needs.
type = s3
s3_endpoint = {{ .S3.Endpoint }}
s3_region = {{ .S3.Region }}
s3_bucket = {{ .S3.Bucket }}
s3_access_key_id = {{ .S3.AccessKeyID }}
s3_secret_access_key = {{ .S3.SecretKey }}
```

The view fields are whatever the existing app view calls them; `DBHost` and
friends are illustrative. Read `appview.go` and use what mbin and outline
already use for the same job rather than adding parallel fields. If the view
exposes a single `DSN` and no parts, the fork needs the parts, so derive them
in the view beside the DSN rather than parsing it in the template.

- [ ] **Step 4: Make the compose file match**

`internal/render/templates/writefreely/compose.yaml.tmpl` describes a SQLite
file in a comment, and a pinned blog now needs the `postgres` service the kind
gained. Follow the mbin or outline compose template, which already render that
service for a pinned app and omit it for a clustered one, and correct the
comment.

- [ ] **Step 5: Invert the test that forbade a Postgres connection**

`internal/render/render_test.go` asserts that no file under `blog/` contains
`postgresql://`, with the message "which it has never supported". Replace it
with the opposite: the blog's configuration must now carry the same database
host the rest of the deployment uses. Keep a test there rather than deleting
it, because the thing worth asserting is that the blog and its neighbours
agree about where the database is.

- [ ] **Step 6: Regenerate the golden tree and read it**

Run `go test ./internal/render -run TestGoldenTree -update`, then
`git diff internal/render/testdata/golden`.

Expected: `blog/config.ini` on each site that runs it, `blog/compose.yaml`, the
manifests, and a `blog` Postgres password and S3 credentials appearing in the
fixture secrets if `init` is what fills them. **If the blog's placement in the
fixture is pinned, its Postgres service appears; if clustered, it does not.**
Confirm which the fixture declares and that the output matches.

- [ ] **Step 7: Say what changed, where an adopter will see it**

`README.md`: the `writefreely` kind means the wisp fork. It keeps its data in
Postgres and its uploads in Garage, it can be clustered, and **upstream
WriteFreely's image will reject this configuration**. Say that plainly, in the
section listing the kinds.

`docs/decisions.md`: an entry recording that the kind changed meaning, that
this is a breaking change for a public toolkit, that a second kind for the fork
was the rejected alternative, that the image is an unreleased `develop` build
pinned by digest and why, and the finding that the fork serves its own images
so its bucket stays private.

`examples/paisans.example.yaml`: if it comments on the blog being SQLite or
pinned for that reason, correct it.

- [ ] **Step 8: Run everything and commit**

```bash
go test ./... -count=1 && go vet ./... && go test -tags garage_integration ./internal/garage -count=1
git add internal/render README.md docs/decisions.md examples
git commit -m "feat: render the blog's Postgres and S3 configuration

$(cat <<'MSG'
The fork reads [database] type = postgres and [storage] type = s3, and
validates both at load rather than at the first upload, so a wrong key name is
a container that does not start. The keys here were read from config/config.go
and config/storage.go on the fork's develop branch.

s3_virtual_host is left at its default, path style, because that is what Garage
needs and the fork's comment says so.

The bucket is private. The fork streams images through its own /uploads/ route
and never emits an S3 URL, so nothing anonymous reaches it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Amended self-review

**Spec coverage for the amendment.** The kind's services, predicates and image:
Task 5. The rendered configuration, the compose change, the breaking change
notice and the record: Task 6. The spec's finding that the fork serves its own
images is enforced in Task 5 by asserting `ServesObjectsPublicly` is false and
in Task 6 by the comment in the rendered template, which is where the next
person will be standing.

**The contradiction table above is the preflight scan for this amendment.**
Every row was found by reading the tests rather than by running them, and the
`cluster-placement-without-a-cluster` row is the one that would otherwise pass
silently with no coverage.

**What the amendment does not test, stated plainly.** Nothing here runs the
fork. The claim that it accepts this configuration rests on reading its
`config.go` and `storage.go`, and the claim that it serves images itself rests
on reading `imagestore.go`. Booting the image against the rendered `config.ini`
would settle the first and is worth trying in Task 6: if it is cheap, do it and
report the output; if it needs a reachable Postgres and a reachable Garage to
get far enough to validate, say so and leave it, because the integration test
that would arrange all three belongs to its own task rather than this one.

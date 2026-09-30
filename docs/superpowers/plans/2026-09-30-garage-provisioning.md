# Garage provisioning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Garage usable by the deployment: credentials in the format Garage accepts, a command that creates the layout, keys and buckets on a host, and a public URL from which Mbin's and Outline's objects can actually be fetched.

**Architecture:** Three layers, already separated in this repository and kept separate here. `internal/secretsgen` learns two new generators and per-app S3 credentials. `internal/config`, `internal/validate` and `internal/render` learn the media hostname and wire each app to its own key. A new `internal/garage` package plans and executes provisioning over the `apply` package's existing `Transport`, and `cmd/paisans` exposes it as `paisans storage init`.

**Tech Stack:** Go, no new dependencies. Garage `dxflrs/garage:v1.0.1`, driven through its CLI over ssh.

**Spec:** `docs/specs/2026-09-30-garage-provisioning.md`

## Global Constraints

- No em dashes anywhere in code, comments, documentation or commit messages.
- Every commit message ends with exactly: `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`
- `go test ./... -count=1` and `go vet ./...` pass before every commit.
- Templates live in `internal/render/templates/` and are embedded with `//go:embed all:templates`. The `all:` prefix is required or dotfiles are skipped. Run `git check-ignore -v` on every new template and confirm it is tracked in the commit: a template git ignores renders locally and is missing on a fresh checkout. A shell glob such as `templates/synapse/*.tmpl` does not match dotfiles either, which has cost this repository time twice.
- The golden tree at `internal/render/testdata/golden` is a specification of what an operator receives, not a byproduct. Regenerate with `go test ./internal/render -run TestRender -update`, then read `git diff` on it and confirm every changed file is explained by the task. Never regenerate blind.
- `internal/render/testdata/deployment.yaml` must pass `validate.Check` with zero refusals. `fixture(t)` enforces this and every render test goes through it.
- A Garage access key ID is `GK` followed by exactly 24 lowercase hex characters. A Garage secret key is exactly 64 lowercase hex characters. Both were established by running `dxflrs/garage:v1.0.1`; neither is what `password()` produces.
- Never claim in a commit message or a report that you verified something you did not run yourself. Where you reason to a conclusion instead, say so in those words.

## Garage command output, captured from the pinned image

These exact strings were produced by running `dxflrs/garage:v1.0.1`. Tasks 4 and 6 depend on them. Do not invent variations.

Unprovisioned node, `garage layout show`:

```
==== CURRENT CLUSTER LAYOUT ====
No nodes currently have a role in the cluster.
See `garage status` to view available nodes.

Current cluster layout version: 0
```

After `layout assign -z home-a -c 100G <id>` and `layout apply --version 1`:

```
==== CURRENT CLUSTER LAYOUT ====
ID                Tags  Zone    Capacity  Usable capacity
51494feb5444d466        home-a  100.0 GB  100.0 GB (100.0%)

Zone redundancy: maximum

Current cluster layout version: 1
```

`garage node id -q` prints `<64 hex characters>@<address>`. The part before `@` is the node ID that `layout assign` takes.

Absent key, `garage key info <name>`, non zero exit: `Error: 0 matching keys`
Present key: `Key name: talk`, then `Key ID: GKd3cd033a5fac46ab1aa37efe`
Absent bucket, `garage bucket info <name>`, non zero exit: `Error: Bucket not found / several matching buckets: nosuch`
Present bucket: `Bucket: cfc236316d4a...` followed by `Size:`, `Objects:`

Ordering is forced: before a layout is applied, `key import` fails with `Error: Internal error: Remote error: Could not reach quorum of 1 (sets=Some(1)). 0 of 0 request succeeded, others returned errors: []`. `key import` and `bucket create` both fail when the object already exists; `bucket allow` is idempotent.

---

## File Structure

**Created:**
- `internal/garage/provision.go`: plans and executes provisioning. Pure logic plus `Transport` calls, no CLI concerns.
- `internal/garage/provision_test.go`: unit tests over a fake transport.
- `internal/garage/integration_test.go`: build tag `garage_integration`, drives the real container.
- `internal/render/templates/media.caddy.snippet.tmpl`: the gateway's media host routing.

**Modified:**
- `internal/config/config.go`: `Garage.Capacity`, `Storage.MediaHostname`, the structural error.
- `internal/config/secrets.go`: remove the two shared S3 fields from `GarageSecrets`.
- `internal/kinds/kinds.go`: `UsesObjectStorage`.
- `internal/secretsgen/secretsgen.go`: two generators, per-app S3 keys, owed text.
- `internal/validate/validate.go`: `garage-key-is-malformed`.
- `internal/render/appview.go`: per-app S3 credentials and the public bucket URL.
- `internal/render/site.go`: the media host block.
- `internal/render/templates/Caddyfile.tmpl`: import the media snippet.
- `internal/render/templates/mbin/.env.secret.tmpl`: `KBIN_STORAGE_URL`.
- `internal/render/templates/outline/.env.secret.tmpl`: public bucket URL.
- `cmd/paisans/main.go`: the `storage init` verb and usage text.
- `README.md`, `docs/decisions.md`, `examples/paisans.example.yaml`, `examples/secrets.example.yaml`.

---

### Task 1: Garage-format credentials, generated per app

**Files:**
- Modify: `internal/kinds/kinds.go`
- Modify: `internal/secretsgen/secretsgen.go`
- Modify: `internal/config/secrets.go`
- Test: `internal/secretsgen/secretsgen_test.go`, `internal/kinds/kinds_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `kinds.UsesObjectStorage(kind config.Kind) bool`; secrets keys `s3_access_key_id` and `s3_secret_access_key` under `apps.<name>`; unexported `garageKeyID() (string, error)` and `garageSecretKey() (string, error)` in `secretsgen`.

- [ ] **Step 1: Write the failing test for the generators' formats**

In `internal/secretsgen/secretsgen_test.go`:

```go
// Garage refuses anything else, and it refuses it at provisioning time on a
// host rather than here, which is the worst place to discover a format.
// Established by running dxflrs/garage:v1.0.1: "The specified key ID is not a
// valid Garage key ID (starts with `GK`, followed by 12 hex-encoded bytes)".
func TestGeneratedS3CredentialsMatchGaragesFormat(t *testing.T) {
	cfg := loadFixtureConfig(t)
	secrets := &config.Secrets{}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatalf("filling secrets: %v", err)
	}
	keyID := regexp.MustCompile(`^GK[0-9a-f]{24}$`)
	secret := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		if !kinds.UsesObjectStorage(app.Kind) {
			if _, ok := secrets.Apps[name]["s3_access_key_id"]; ok {
				t.Errorf("%s is a %s and stores no objects, but was given an S3 key", name, app.Kind)
			}
			continue
		}
		id, _ := secrets.Apps[name]["s3_access_key_id"].(string)
		if !keyID.MatchString(id) {
			t.Errorf("%s's s3_access_key_id is %q, which Garage will refuse", name, id)
		}
		sec, _ := secrets.Apps[name]["s3_secret_access_key"].(string)
		if !secret.MatchString(sec) {
			t.Errorf("%s's s3_secret_access_key is not 32 hex encoded bytes", name)
		}
	}
}
```

If `loadFixtureConfig` does not already exist in that package's tests, load `../render/testdata/deployment.yaml` with `config.Load` the way the neighbouring tests do.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/secretsgen -run TestGeneratedS3CredentialsMatchGaragesFormat -v`
Expected: FAIL, `undefined: kinds.UsesObjectStorage`.

- [ ] **Step 3: Add `UsesObjectStorage`**

In `internal/kinds/kinds.go`, beside `UsesPostgres`:

```go
// UsesObjectStorage reports whether a kind keeps uploads in S3 rather than on
// local disk.
//
// Synapse is deliberately absent. Its media store is a directory the
// homeserver owns, which is also why the synapse kind is pinned to one node.
func UsesObjectStorage(kind config.Kind) bool {
	switch kind {
	case config.KindMbin, config.KindOutline:
		return true
	default:
		return false
	}
}
```

- [ ] **Step 4: Add the two generators**

In `internal/secretsgen/secretsgen.go`, beside `password()`:

```go
// garageKeyID returns an S3 access key ID in the only shape Garage accepts:
// the literal "GK" followed by 12 hex encoded bytes. `password()` is not
// reused here, and base64 is exactly why: Garage rejects it outright with
// "The specified key ID is not a valid Garage key ID".
func garageKeyID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return "GK" + hex.EncodeToString(raw), nil
}

// garageSecretKey returns an S3 secret key as 32 hex encoded bytes, which is
// the only shape Garage accepts.
func garageSecretKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
```

Add `encoding/hex` to the imports.

- [ ] **Step 5: Generate them per app**

`appSecretKeys` returns names that are all filled by `password()`, so these two cannot go through it. Find where `appSecretKeys` results are filled and add, for each app where `kinds.UsesObjectStorage(app.Kind)` is true, an `s3_access_key_id` from `garageKeyID()` and an `s3_secret_access_key` from `garageSecretKey()`, each filled only when absent and each recorded through the same `note(created, ...)` path the other app secrets use. Follow the existing shape rather than inventing a second one.

- [ ] **Step 6: Remove the shared pair**

In `internal/config/secrets.go`, delete `AccessKeyID` and `SecretAccessKey` from `GarageSecrets`, leaving `AdminToken` and `RPCSecret`. In `secretsgen.go`, remove `storage.garage.access_key_id` and `storage.garage.secret_access_key` from the generated list. Build will fail where `appview.go` reads them; Task 3 owns that and a temporary compile error here is expected only if you stop mid task, so finish the task.

Update the comment on `GarageSecrets` to say that these are Garage's own credentials and that S3 credentials are per app.

- [ ] **Step 7: Run the tests**

Run: `go test ./... -count=1` and `go vet ./...`
Expected: the new test passes. If `appview.go` does not compile, add the minimal read of the app's own key now rather than leaving the tree broken; Task 3 refines the rest.

- [ ] **Step 8: Commit**

```bash
git add internal/kinds internal/secretsgen internal/config
git commit -m "feat: generate S3 credentials Garage will accept

$(cat <<'MSG'
secretsgen filled storage.garage.access_key_id and secret_access_key with
password(), which is base64. Garage refuses both: a key ID is GK plus 12 hex
encoded bytes and a secret is 32 hex encoded bytes, checked by running
dxflrs/garage:v1.0.1, the image this toolkit pins. The credentials the toolkit
handed Mbin and Outline could never have worked.

They are now per app rather than shared. One key for every app meant each
bucket granted to it with --owner, so Mbin's .env would have been enough to
read, rewrite and delete every document Outline ever stored.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 2: The media hostname and the capacity, declared and checked

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/validate/validate.go`
- Create: `internal/validate/testdata/garage-key-is-malformed.yaml`
- Test: `internal/config/config_test.go`, `internal/validate/validate_test.go`

**Interfaces:**
- Consumes: `kinds.UsesObjectStorage` from Task 1.
- Produces: `cfg.Storage.MediaHostname string`, `cfg.Storage.Garage.Capacity string` (defaulting to `100G`), and the refusal rule `garage-key-is-malformed`.

- [ ] **Step 1: Write the failing structural test**

In `internal/config/config_test.go`:

```go
// An app that stores objects and no hostname to serve them from is a
// configuration that renders a working uploader and a URL nobody can fetch.
func TestObjectStorageNeedsAMediaHostname(t *testing.T) {
	_, err := config.Load(filepath.Join("testdata", "storage-without-a-media-hostname.yaml"))
	if err == nil {
		t.Fatal("expected a configuration with an object storing app and no storage.media_hostname to be refused")
	}
	if !strings.Contains(err.Error(), "storage.media_hostname") {
		t.Errorf("the error should name the missing field, got: %v", err)
	}
}
```

Create `internal/config/testdata/storage-without-a-media-hostname.yaml` as the smallest configuration with a gateway site and one `outline` app, with `storage.garage.sites` set and no `media_hostname`. Copy the shape from a neighbouring fixture in that directory rather than writing one from scratch.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/config -run TestObjectStorageNeedsAMediaHostname -v`
Expected: FAIL, because `config.Load` accepts the file.

- [ ] **Step 3: Add the fields and the check**

In `internal/config/config.go`:

```go
type Storage struct {
	Garage Garage `yaml:"garage"`
	// MediaHostname is where objects are served from. It is one hostname for
	// the deployment rather than one per app: a bucket is a path under it.
	//
	// It is required once any app stores objects, because the endpoint an app
	// writes through is a mesh address and a browser cannot reach one. A
	// federating instance that caches such a URL keeps it.
	MediaHostname string `yaml:"media_hostname"`
}

type Garage struct {
	Sites       []string `yaml:"sites"`
	Replication int      `yaml:"replication"`
	// Capacity is what this node advertises to Garage's layout. Garage
	// requires a unit suffix, as in 100G. It is not derived from the disk
	// because the toolkit cannot know how much of that disk is meant for
	// objects.
	Capacity string `yaml:"capacity"`
}
```

Default `Capacity` to `100G` where the other defaults are applied. In the same place the other structural errors are collected, add: when any app's kind reports `kinds.UsesObjectStorage` and `Storage.MediaHostname` is empty, `add("storage.media_hostname is required: %s stores objects and the URL an app publishes must be one a browser can reach", name)`.

If `internal/config` cannot import `internal/kinds` without a cycle, put the check in `internal/validate` as a refusal named `object-storage-without-a-media-hostname` instead, add it to `README.md` beside the other rules, and say in the commit message which way you went and why.

- [ ] **Step 4: Write the failing refusal test**

In `internal/validate/validate_test.go`, add a case to the existing table that loads `testdata/garage-key-is-malformed.yaml` and expects exactly one finding, `garage-key-is-malformed`, at REFUSE. Follow the shape of the `gated-matrix-hostname` case already there.

The fixture needs an app with a hand written S3 key that is not in Garage's format. Since secrets are a separate file from the configuration, check how the neighbouring refusal fixtures that involve secrets are loaded and follow that; if `validate.Check` takes only a configuration, this rule belongs where the secrets are read instead. In that case implement it as a refusal in `secretsgen`'s load path, keep the name `garage-key-is-malformed`, and say so in the commit message.

- [ ] **Step 5: Implement the refusal**

```go
// garageKeyIsMalformed refuses an S3 credential Garage will not accept.
//
// Generated credentials cannot trip this. A hand edited secrets file can, and
// the failure it prevents is a provisioning run that dies halfway through with
// a message about hex encoding, after it has already imported some keys.
func (c *checker) garageKeyIsMalformed() {
	keyID := regexp.MustCompile(`^GK[0-9a-f]{24}$`)
	secret := regexp.MustCompile(`^[0-9a-f]{64}$`)
	// ... for each app's s3_access_key_id and s3_secret_access_key that is
	// set, refuse when it does not match, naming the app, the field and the
	// expected shape.
}
```

Write the loop out in full rather than leaving the comment. Wire it into `Check` beside the other rules and add the rule to `README.md` where the refusals are listed.

- [ ] **Step 6: Run the tests**

Run: `go test ./... -count=1` and `go vet ./...`
Expected: both new tests pass, everything else still green.

- [ ] **Step 7: Commit**

```bash
git add internal/config internal/validate README.md
git commit -m "feat: declare where objects are served from, and refuse a key Garage will not take

$(cat <<'MSG'
storage.media_hostname is required once any app stores objects. Without it the
toolkit renders an app that uploads successfully and publishes a URL pointing
at a mesh address, which no browser can reach and which a federating instance
caches permanently.

storage.garage.capacity is what the node advertises to Garage's layout, with a
unit suffix because Garage requires one.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 3: Apps use their own key, and publish a URL that resolves

**Files:**
- Modify: `internal/render/appview.go`
- Modify: `internal/render/site.go`
- Create: `internal/render/templates/media.caddy.snippet.tmpl`
- Modify: `internal/render/templates/Caddyfile.tmpl`
- Modify: `internal/render/templates/mbin/.env.secret.tmpl`
- Modify: `internal/render/templates/outline/.env.secret.tmpl`
- Modify: `internal/render/testdata/deployment.yaml`, `internal/render/testdata/secrets.fixture.yaml`
- Test: `internal/render/render_test.go`

**Interfaces:**
- Consumes: `kinds.UsesObjectStorage`, the per-app secrets from Task 1, `cfg.Storage.MediaHostname` from Task 2.
- Produces: `s3Values.PublicBase string` on the app view; a rendered `srv/infra/caddy/snippets/media.caddy` on the gateway.

- [ ] **Step 1: Write the failing test**

```go
// The endpoint an app writes through is internal. The URL it publishes is not,
// and the two are different strings for a reason: uploads must not cross the
// gateway, and a media URL must resolve for a browser and for a federating
// server that will never join this mesh.
func TestObjectStorageIsPublishedOnTheMediaHostname(t *testing.T) {
	tree := build(t)

	env := tree.file(t, "home-a/srv/talk/.env")
	if !strings.Contains(env, "KBIN_STORAGE_URL=https://media.example.org/talk-uploads") {
		t.Errorf("Mbin builds every media URL and every thumbnail root from KBIN_STORAGE_URL, got:\n%s", env)
	}
	if !strings.Contains(env, "S3_ENDPOINT=http://10.44.0.1:3900") {
		t.Error("uploads should still go direct to Garage rather than through the gateway")
	}

	outline := tree.file(t, "home-a/srv/docs/.env")
	if !strings.Contains(outline, "AWS_S3_UPLOAD_BUCKET_URL=https://media.example.org") {
		t.Errorf("Outline publishes attachment URLs from its bucket URL, got:\n%s", outline)
	}

	caddyfile := tree.file(t, "vm/srv/infra/caddy/Caddyfile")
	block := hostBlock(t, caddyfile, "media.example.org")
	if strings.Contains(block, "import gate_") {
		t.Error("the media hostname must never be gated: a federating server fetching an image is a machine")
	}

	snippet := tree.file(t, "vm/srv/infra/caddy/snippets/media.caddy")
	if !strings.Contains(snippet, "3900") {
		t.Errorf("the media snippet should reach Garage on 3900, got:\n%s", snippet)
	}
	if strings.Contains(snippet, "header_up Host") {
		t.Error("Host must be forwarded unchanged or Outline's presigned URLs stop verifying")
	}
}
```

Adjust the site and app names to the fixture's own (`talk`, and whatever the Outline app is called there). Reuse the existing `build(t)`, `tree.file` and `hostBlock` helpers rather than adding new ones.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/render -run TestObjectStorageIsPublishedOnTheMediaHostname -v`
Expected: FAIL, because `KBIN_STORAGE_URL` appears nowhere.

- [ ] **Step 3: Add the fixture values**

In `internal/render/testdata/deployment.yaml`, add `media_hostname: media.example.org` under `storage`, and `capacity: 100G` under `storage.garage`. In `internal/render/testdata/secrets.fixture.yaml`, remove the two shared Garage S3 entries and add, under each object storing app, an `s3_access_key_id` in the form `GK` plus 24 hex characters and an `s3_secret_access_key` of 64 hex characters. They are fixtures: make them obviously fake but correctly shaped, for example `GK00000000000000000000talk` is wrong because it is not hex, while `GK0000000000000000000ta1k` is also wrong. Use real hex.

- [ ] **Step 4: Wire the app view**

In `internal/render/appview.go`, replace the shared credential reads with the app's own, and add the public base:

```go
v.S3 = s3Values{
	// Uploads go direct to Garage over the mesh. Routing them through the
	// gateway would put every byte of every upload through the one machine
	// that also terminates TLS for everything else.
	Endpoint:       fmt.Sprintf("http://%s:3900", garageEndpointHost(p)),
	// PublicBase is what an app writes into a page, a feed or a federated
	// post. It has to resolve for a browser and for a server that will never
	// join this mesh.
	PublicBase:     "https://" + p.cfg.Storage.MediaHostname,
	AccessKeyID:    appSecretString(planned.Name, "s3_access_key_id"),
	SecretKey:      appSecretString(planned.Name, "s3_secret_access_key"),
	Bucket:         v.Setting("s3_bucket", planned.Name+"-uploads"),
	Region:         v.Setting("s3_region", "garage"),
	ForcePathStyle: v.SettingBool("s3_force_path_style", true),
}
```

Use whatever accessor this file already has for per-app secrets rather than adding `appSecretString` if one exists. Add `PublicBase` to the `s3Values` struct with a comment.

- [ ] **Step 5: Render the two URLs**

In `internal/render/templates/mbin/.env.secret.tmpl`, beneath the existing S3 block:

```
# Mbin builds every media URL and every Liip Imagine thumbnail root from this,
# so it is the public base rather than the endpoint uploads are written to. A
# federating instance caches what it finds here.
KBIN_STORAGE_URL={{ .S3.PublicBase }}/{{ .S3.Bucket }}
```

In `internal/render/templates/outline/.env.secret.tmpl`, change `AWS_S3_UPLOAD_BUCKET_URL` to `{{ .S3.PublicBase }}` and add a comment saying that the value is public because Outline hands a browser a redirect to it.

- [ ] **Step 6: Render the media host block**

Create `internal/render/templates/media.caddy.snippet.tmpl`:

```
# Rendered by paisans. Do not edit: `paisans apply` overwrites this file.
#
# Object storage, served to browsers and to federating servers. Uploads do not
# come this way: an app writes to Garage over the mesh, and only what it
# publishes is served here.
#
# NEVER GATE THIS HOSTNAME. A federating server fetching an image is a machine,
# and it will not follow a redirect to a passkey prompt.
#
# Host is forwarded unchanged, and that is load bearing rather than tidy.
# Outline presigns URLs, the signature covers the host it was signed with, and
# rewriting this header to the upstream address invalidates every signature it
# issues. Garage treats a host it does not recognise as a vhost as a path style
# request, which is why path style addressing stays on for every app.
reverse_proxy {{ .GarageAddress }}:3900
```

In `internal/render/site.go`, render it to `srv/infra/caddy/snippets/media.caddy` on the gateway site when `cfg.Storage.MediaHostname` is set, following how the gate snippets are rendered. In `Caddyfile.tmpl`, emit a host block for the media hostname that imports it, with no gate import, beside the existing per app blocks.

- [ ] **Step 7: Run the test, then regenerate the golden tree**

Run: `go test ./internal/render -run TestObjectStorageIsPublishedOnTheMediaHostname -v`
Expected: PASS.

Then: `go test ./internal/render -run TestRender -update`, then `git diff internal/render/testdata/golden` and read it. Expected changes: `.env` for each object storing app, the new `media.caddy` snippet, the gateway's `Caddyfile`, and the manifest. Anything else is a finding: stop and say so.

- [ ] **Step 8: Run everything and commit**

```bash
go test ./... -count=1 && go vet ./...
git add internal/render
git commit -m "feat: serve object storage on a media hostname

$(cat <<'MSG'
KBIN_STORAGE_URL was never rendered, and Mbin builds every media URL and every
Liip Imagine thumbnail root from it. Outline was handed the mesh endpoint as
its bucket URL, so it would have redirected browsers to an address they cannot
reach and federating instances would have cached it.

Uploads still go direct to Garage over the mesh. Only the published URL is
public, which keeps every uploaded byte off the gateway.

Host is forwarded unchanged because Outline presigns URLs and the signature
covers the host that signed them.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 4: The provisioning planner

**Files:**
- Create: `internal/garage/provision.go`
- Create: `internal/garage/provision_test.go`

**Interfaces:**
- Consumes: `apply.Transport` (interface: `Run(string) (string, error)`, `ReadFile`, `WriteFile`, `Describe() string`), `kinds.UsesObjectStorage`, per-app secrets from Task 1, `cfg.Storage.Garage.Capacity` from Task 2.
- Produces:
  ```go
  type Step struct {
      Describe string // one line, shown to the operator
      Command  string // the garage command, exactly as it will run
  }
  type Plan struct {
      Site    string
      Steps   []Step   // only what is missing
      Present []string // what was already there, for the report
  }
  func Build(site string, cfg *config.Config, secrets *config.Secrets, t Transport) (*Plan, error)
  func Execute(plan *Plan, t Transport) error
  type Transport interface { Run(command string) (string, error); Describe() string }
  ```

Define `garage.Transport` as its own two method interface rather than importing `apply.Transport`, so this package does not depend on the applier. `apply.SSHTransport` satisfies both.

- [ ] **Step 1: Write the failing tests over a fake transport**

In `internal/garage/provision_test.go`:

```go
// fakeTransport answers canned output per command prefix, so the whole planner
// is testable without a host or a container.
type fakeTransport struct {
	responses map[string]response // keyed by a substring of the command
	ran       []string
}

type response struct {
	out string
	err error
}

func (f *fakeTransport) Run(command string) (string, error) {
	f.ran = append(f.ran, command)
	for key, r := range f.responses {
		if strings.Contains(command, key) {
			return r.out, r.err
		}
	}
	return "", nil
}

func (f *fakeTransport) Describe() string { return "fake" }

// A fresh node needs the layout first. Before one is applied Garage answers
// every key and bucket command with "Could not reach quorum of 1", so a plan
// that ordered them the other way would fail on a real host and pass here
// unless the order is asserted.
func TestAFreshNodeIsLaidOutBeforeAnyKeyIsImported(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "==== CURRENT CLUSTER LAYOUT ====\nNo nodes currently have a role in the cluster.\n\nCurrent cluster layout version: 0\n"},
		"node id -q":  {out: "51494feb5444d466aaaabbbbccccddddeeeeffff00001111222233334444abcd@127.0.0.1:3901\n"},
		"key info":    {out: "Error: 0 matching keys", err: errors.New("exit status 1")},
		"bucket info": {out: "Error: Bucket not found / several matching buckets: talk-uploads", err: errors.New("exit status 1")},
	}}

	plan, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}

	var commands []string
	for _, s := range plan.Steps {
		commands = append(commands, s.Command)
	}
	joined := strings.Join(commands, "\n")
	assign := indexOfContaining(commands, "layout assign")
	apply := indexOfContaining(commands, "layout apply")
	importKey := indexOfContaining(commands, "key import")
	if assign < 0 || apply < 0 || importKey < 0 {
		t.Fatalf("a fresh node needs all three, got:\n%s", joined)
	}
	if !(assign < apply && apply < importKey) {
		t.Errorf("layout must be applied before any key is imported, got:\n%s", joined)
	}
	if !strings.Contains(joined, "layout apply --version 1") {
		t.Errorf("a layout at version 0 is applied as version 1, got:\n%s", joined)
	}
	if !strings.Contains(joined, "-c 100G") {
		t.Errorf("capacity comes from the configuration and Garage requires a unit suffix, got:\n%s", joined)
	}
}

// key import and bucket create both fail when the object exists, so a second
// run must not plan them. bucket allow is idempotent and is always planned.
func TestAProvisionedNodePlansNothingButTheGrant(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "==== CURRENT CLUSTER LAYOUT ====\nID  Tags  Zone  Capacity\nabc  []  home-a  100.0 GB\n\nCurrent cluster layout version: 1\n"},
		"key info":    {out: "Key name: talk\nKey ID: GK00112233445566778899aabb\n"},
		"bucket info": {out: "Bucket: cfc236316d4a81858f84f84c287f5a0d\nSize: 0 B\nObjects: 0\n"},
	}}

	plan, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}
	for _, s := range plan.Steps {
		if strings.Contains(s.Command, "key import") || strings.Contains(s.Command, "bucket create") || strings.Contains(s.Command, "layout") {
			t.Errorf("nothing was missing, so this should not have been planned: %s", s.Command)
		}
	}
	if len(plan.Present) == 0 {
		t.Error("a fully provisioned node should report what it found rather than looking like it did nothing")
	}
}

// The half provisioned node is what a failed first run leaves behind, and it
// is the case a check-then-act planner exists for.
func TestAHalfProvisionedNodePlansOnlyWhatIsMissing(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"layout show": {out: "Current cluster layout version: 1\n"},
		"key info":    {out: "Key name: talk\nKey ID: GK00112233445566778899aabb\n"},
		"bucket info": {out: "Error: Bucket not found / several matching buckets: talk-uploads", err: errors.New("exit status 1")},
	}}

	plan, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport)
	if err != nil {
		t.Fatalf("building the plan: %v", err)
	}
	joined := ""
	for _, s := range plan.Steps {
		joined += s.Command + "\n"
	}
	if strings.Contains(joined, "key import") {
		t.Errorf("the key was already there and importing it again fails, got:\n%s", joined)
	}
	if !strings.Contains(joined, "bucket create") {
		t.Errorf("the bucket was missing and should be planned, got:\n%s", joined)
	}
}

// A transport failure is not an absent object. Treating every non zero exit as
// "not there yet" would plan an import against a node that is simply
// unreachable, and then run it.
func TestAnUnreachableNodeIsAnErrorRatherThanAnEmptyPlan(t *testing.T) {
	transport := &fakeTransport{responses: map[string]response{
		"": {out: "ssh: connect to host home-a port 22: Connection refused", err: errors.New("exit status 255")},
	}}

	if _, err := garage.Build("home-a", fixtureConfig(t), fixtureSecrets(t), transport); err == nil {
		t.Fatal("expected a connection failure to be reported rather than treated as an unprovisioned node")
	}
}
```

Write `indexOfContaining`, `fixtureConfig` and `fixtureSecrets` as small helpers in the test file. `fixtureConfig` loads `../render/testdata/deployment.yaml`; `fixtureSecrets` loads `../render/testdata/secrets.fixture.yaml`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/garage -v`
Expected: FAIL, the package does not exist.

- [ ] **Step 3: Write the planner**

Create `internal/garage/provision.go`. The package comment states why this is not part of `apply`: `apply` renders files and compares them against a manifest, while this mutates a running service's internal state that no manifest describes.

The command prefix is `docker compose -f /srv/infra/compose.yaml exec -T garage /garage`, matching how `apply` reaches into the infra stack. Define it once as a constant.

Detection, each by exit status **and** a marker in the output, because an unreachable host also exits non zero:

```go
// absent reports whether a check command means "this object does not exist
// yet", as opposed to "the node could not be reached". Garage names both cases
// on stderr and the marker is the only thing that tells them apart: a bare non
// zero exit would make an unreachable host look like an empty cluster, and the
// planner would then cheerfully plan an import against it.
func absent(out string, err error, marker string) (bool, error) {
	if err == nil {
		return false, nil
	}
	if strings.Contains(out, marker) {
		return true, nil
	}
	return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
}
```

Markers: `0 matching keys` for a key, `Bucket not found` for a bucket.

Layout: run `layout show`, read the line `Current cluster layout version: N`. When `N` is 0, plan `node id -q`, then `layout assign -z <site> -c <capacity> <node id>`, then `layout apply --version 1`. The node ID is everything before the `@`. When `N` is greater than 0, plan nothing for the layout and record it in `Present`.

Because the node ID is only known at execution time on a fresh node, either run `node id -q` during `Build` (it reads nothing and changes nothing) or make the step carry a small closure. Running it during `Build` is simpler and is what the test above assumes.

Per app, in `cfg.AppNames()` order so the plan is deterministic: skip unless `kinds.UsesObjectStorage`; check the key by its ID, plan `key import <id> <secret> --yes -n <app>` when absent; check the bucket by name, plan `bucket create <bucket>` when absent; always plan `bucket allow --read --write --owner <bucket> --key <id>`.

`Execute` runs each step's command in order and fails on the first error, naming the step.

**The secret appears in a command line.** Say so in a comment: it is visible in `ps` on the host for the moment the command runs. Note that the alternative, writing it to a file first, trades that for a secret at rest on the host, and that `apply` already writes rendered secrets there, so this is not the weakest link. Record it rather than leaving it unremarked.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/garage -v`
Expected: all four pass.

- [ ] **Step 5: Run everything and commit**

```bash
go test ./... -count=1 && go vet ./...
git add internal/garage
git commit -m "feat: plan Garage provisioning, check-then-act

$(cat <<'MSG'
Every step is checked before it is planned, because Garage's own behaviour
requires it: key import and bucket create both fail when the object already
exists, while bucket allow is idempotent. A planner that did not check could
run exactly once.

Ordering is forced too. Before a layout is applied Garage answers key and
bucket commands with "Could not reach quorum of 1", so the layout is planned
first and a test asserts that order rather than the commands' presence.

An absent object is distinguished from an unreachable host by a marker in the
output, not by the exit status alone. Both exit non zero, and treating the
second as the first would plan an import against a node nobody can reach.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 5: `paisans storage init`

**Files:**
- Modify: `cmd/paisans/main.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `garage.Build`, `garage.Execute`, `garage.Plan` from Task 4; `apply.SSHTransport`.
- Produces: the `storage init` verb.

- [ ] **Step 1: Add the verb**

In `main()`, add a case for `storage` that requires a second word, `init`, and rejects anything else with a message naming what exists. Write `runStorageInit(args []string) error` modelled on `runApply`: the same `--config`, `--secrets`, `--site`, `--ssh`, `--sudo` and `--execute` flags, the same site lookup, the same validate-and-refuse gate, the same unencrypted secrets warning.

Without `--execute` it prints the plan and stops. With it, it runs `garage.Execute` and then prints what was created.

- [ ] **Step 2: Update the usage text**

```
  storage init --site <name> [--config paisans.yaml] [--secrets secrets.enc.yaml]
               [--ssh <destination>] [--execute]
```

and in the Commands list:

```
  storage    Provision object storage on a site: the cluster layout, each
             app's key, and its bucket. Creates only what is missing.
             Writes nothing without --execute.
```

The closing sentence of the usage text says "Only apply reaches a host, and only with --execute." That is now false. Change it to name both commands.

- [ ] **Step 3: Document it**

In `README.md`, add a short section after the one describing `apply`: what the command does, that it is separate from `apply` because it mutates a running service rather than a file, that it is safe to run again, and that it must run after the infrastructure stack is up because Garage has to be reachable.

Say plainly that `paisans apply` alone leaves object storage unusable, since that is the failure an adopter would otherwise meet as an application error.

- [ ] **Step 4: Verify by hand**

```bash
go run ./cmd/paisans storage init --site home-a --config internal/render/testdata/deployment.yaml --secrets internal/render/testdata/secrets.fixture.yaml --ssh nonexistent.invalid
```

Expected: it fails reaching the host, and the message names the host rather than printing an empty plan. That is the `TestAnUnreachableNodeIsAnErrorRatherThanAnEmptyPlan` behaviour, seen from the outside.

Also run `go run ./cmd/paisans --help` and confirm the new verb appears and that the "only apply reaches a host" sentence is gone.

- [ ] **Step 5: Run everything and commit**

```bash
go test ./... -count=1 && go vet ./...
git add cmd/paisans README.md
git commit -m "feat: add paisans storage init

$(cat <<'MSG'
A separate verb rather than a flag on apply. apply renders files, compares them
against a manifest and restarts containers, and its refusal to touch a file it
did not write is the basis for trusting it on an adopted host. Creating a
bucket mutates state no manifest describes, so a failed provision inside apply
would leave it half done with nothing to compare against.

The usage text said "Only apply reaches a host". That is no longer true and now
names both.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 6: An integration test against the real Garage, and the records

**Files:**
- Create: `internal/garage/integration_test.go`
- Modify: `docs/decisions.md`
- Modify: `examples/paisans.example.yaml`, `examples/secrets.example.yaml`
- Modify: `README.md`

**Interfaces:**
- Consumes: everything above.
- Produces: nothing other tasks rely on.

- [ ] **Step 1: Write the integration test**

`internal/garage/integration_test.go`, first line `//go:build garage_integration`.

It requires `docker`, and skips with a clear message when it is absent. It:

1. Writes a `garage.toml` to `t.TempDir()` with `replication_factor = 1`, an `rpc_secret` of 64 hex characters, and the bind addresses on 127.0.0.1.
2. Starts `dxflrs/garage:v1.0.1` with that file mounted, and registers `t.Cleanup` to remove the container.
3. Waits for `garage status` to answer rather than sleeping a fixed time.
4. Builds a plan against a `dockerTransport` whose `Run` shells `docker exec <name> ...`, executes it, and asserts:
   - the layout is applied,
   - `garage key info <id>` finds each app's key **with the ID from the secrets file**, which is the property that matters: the key inside Garage and the key inside the app's `.env` must be the same or every upload fails with a signature error,
   - `garage bucket info <bucket>` finds each bucket,
   - the key is allowed on its own bucket and **not** on another app's bucket.
5. Runs `Build` a second time and asserts the plan contains no `key import`, no `bucket create` and no `layout` step. This is the idempotency claim, proven against the real thing rather than against canned strings.

The command prefix inside this test is `/garage`, not the compose form, since it execs the container directly.

- [ ] **Step 2: Run it**

Run: `go test -tags garage_integration ./internal/garage -v -count=1`
Expected: PASS. It pulls the image on first run.

Then run `go test ./... -count=1` with no tag and confirm it is skipped, so the ordinary suite stays free of a Docker dependency.

- [ ] **Step 3: Record the decision**

Append to `docs/decisions.md`, in the style of the entries already there: that provisioning is a separate verb and why, that keys are per app and what the shared key would have allowed, that the media hostname is one per deployment, and that `Host` is forwarded unchanged because of presigned URLs.

State that the formats and the ordering were established by running `dxflrs/garage:v1.0.1` and name the integration test as where that knowledge is kept honest.

- [ ] **Step 4: Update the examples**

`examples/paisans.example.yaml` gains `storage.media_hostname` and `storage.garage.capacity`, with comments in the file's existing voice.

`examples/secrets.example.yaml` loses the two shared Garage S3 entries and gains per app `s3_access_key_id` and `s3_secret_access_key` for each object storing app, correctly shaped. If a test asserts the examples agree with what `secretsgen` produces, it will catch a mistake here; run the full suite and believe it.

- [ ] **Step 5: Run everything and commit**

```bash
go test ./... -count=1 && go vet ./... && go test -tags garage_integration ./internal/garage -count=1
git add internal/garage docs/decisions.md examples README.md
git commit -m "test: drive the real Garage through the whole provisioning sequence

$(cat <<'MSG'
Every fact this design rests on came from running dxflrs/garage:v1.0.1 and none
of it from documentation: the key and secret formats, the ordering, and which
commands are idempotent. A test that runs the image is the only thing that will
notice when a future version changes one of them.

It asserts the property that matters most and that unit tests cannot reach: the
key inside Garage is the key inside the app's .env. If those ever drift, every
upload fails with a signature error and nothing before this test would say so.

Behind a build tag, so the ordinary suite needs no Docker.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Self-review

**Spec coverage.** Formats and per-app keys: Task 1. `media_hostname`, `capacity`, `garage-key-is-malformed`: Task 2. Media host block, `Host` unchanged, `KBIN_STORAGE_URL`, Outline's bucket URL, per-app credentials in the render: Task 3. `UsesObjectStorage`: Task 1. The command: Tasks 4 and 5. Integration test and records: Task 6. The spec's "not verified" note about Outline's presigned URLs is discharged by Task 6's assertion that the key in Garage matches the key in the `.env`, and the `Host` behaviour is asserted in Task 3 as the absence of a rewrite. **Gap accepted and stated here:** nothing in this plan runs Outline itself, so the presigning claim remains reasoned rather than observed. It is called out in Task 3's test comment and in the spec.

**Placeholders.** Task 2 steps 3 and 4 name two forks in the road (an import cycle, and where secrets are reachable from) rather than pretending the shape of the existing code is known; each fork says what to do either way and requires the choice be recorded in the commit message. Task 4 step 3 leaves the loop bodies to the implementer but specifies every command string, marker and order. No step says "add error handling".

**Type consistency.** `s3Values.PublicBase` is introduced in Task 3 and used only there. `garage.Transport`, `garage.Plan`, `garage.Step`, `garage.Build` and `garage.Execute` are declared in Task 4's Interfaces block and used with the same names in Tasks 5 and 6. Secret key names `s3_access_key_id` and `s3_secret_access_key` are identical in Tasks 1, 2, 3, 4 and 6.

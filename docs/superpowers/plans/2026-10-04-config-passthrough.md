# Config passthrough Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a deployment set a configuration key the toolkit has never heard of, in the file its application actually reads, without forking a template or editing a file on the host.

**Architecture:** A per app `config` map carries dotted keys. `kinds` says which file and which format each kind's configuration is. A new `internal/configmerge` package merges keys into already rendered output, one function per format. `render` calls it after the template runs and fails loudly when the template already wrote the key. `validate` refuses the cases that are knowable without rendering.

**Tech Stack:** Go, no new dependencies. `yaml.v3` is already direct and `encoding/json` is stdlib; env and ini are handled by small line oriented writers rather than a round trip.

**Spec:** `docs/specs/2026-10-04-config-passthrough.md`

## Branch note

This branch is stacked on `feat/media-serving`, which is complete and awaiting a merge decision. It must merge after that one. If `feat/media-serving` changes before merging, rebase rather than merging develop into this branch, so the history stays readable.

## Global Constraints

- No em dashes anywhere in code, comments, documentation or commit messages.
- Every commit message ends with exactly: `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>` and no other attribution, whatever model writes it.
- `go test ./... -count=1` and `go vet ./...` pass before every commit.
- The golden tree at `internal/render/testdata/golden` is a specification of what an operator receives. Regenerate with `go test ./internal/render -run TestGoldenTree -update`, read `git diff` on it, and confirm every changed file is explained. Never regenerate blind.
- `internal/render/testdata/deployment.yaml` must pass `validate.Check` with zero refusals; `fixture(t)` enforces it.
- Templates are embedded with `//go:embed all:templates`; the `all:` prefix is required or dotfiles are skipped, and a shell glob does not match dotfiles either. `TestEveryRequiredComposeVariableIsSetInTheStacksEnv` and the template set test are the guards; do not weaken them.
- Never claim in a commit message or report that you verified something you did not run yourself. Where you reason instead, say so in those words.

## The decision this plan settles that the spec left implicit

**A collision with a key the template already wrote is a render error, not a validate refusal.** `validate.Check` takes a configuration and never renders, so it cannot know what a template emits. `render` can. So:

- `validate` refuses what is knowable statically: a dotted key on an env kind, a credential shaped key name, and a `config` block on a kind that renders no config file.
- `render.Build` returns an error named `config-key-already-rendered` when a passthrough key collides with one the template wrote. `cmd/paisans` already reports a `Build` error, and `render`, `apply` and `storage init` all go through it, so every path that would write the file refuses it.

Keep the refusal names identical in both places so an operator searching the README finds one rule rather than two.

## File Structure

**Created:**
- `internal/configmerge/configmerge.go`: one exported function per format. No knowledge of kinds, apps or templates.
- `internal/configmerge/configmerge_test.go`: table tests per format, including comment preservation.
- `internal/validate/testdata/config-key-is-nested-in-an-env-file.yaml`
- `internal/validate/testdata/config-key-looks-like-a-secret.yaml`
- `internal/validate/testdata/config-for-a-kind-with-no-config-file.yaml`

**Modified:**
- `internal/config/config.go`: the `Config map[string]any` field on `App`.
- `internal/kinds/kinds.go`: `ConfigFile(kind)` and `ConfigFormat(kind)`.
- `internal/validate/validate.go`: three refusals.
- `internal/render/site.go` or wherever a rendered file is finalised: the merge call and the collision error.
- `internal/render/render_test.go`: golden coverage per format.
- `internal/render/testdata/deployment.yaml`: one passthrough key per format.
- `README.md`, `docs/decisions.md`, `examples/paisans.example.yaml`.

---

### Task 1: A kind says where its configuration lives, and the obvious mistakes are refused

**Files:**
- Modify: `internal/config/config.go`, `internal/kinds/kinds.go`, `internal/validate/validate.go`, `README.md`
- Create: the three validate fixtures listed above
- Test: `internal/kinds/kinds_test.go`, `internal/validate/validate_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  ```go
  // config
  type App struct { /* ... */ Config map[string]any `yaml:"config"` }

  // kinds
  type ConfigFormat string
  const (
      ConfigNone ConfigFormat = ""
      ConfigEnv  ConfigFormat = "env"
      ConfigINI  ConfigFormat = "ini"
      ConfigYAML ConfigFormat = "yaml"
      ConfigJSON ConfigFormat = "json"
  )
  func ConfigFormat(kind config.Kind) ConfigFormat
  func ConfigFile(kind config.Kind) string   // the rendered path, relative to the stack: ".env", "config.ini", "homeserver.yaml", "config.json"
  ```

- [ ] **Step 1: Write the failing test for the format table**

```go
// Each kind reads its configuration from a different file in a different
// syntax, so a passthrough key means something different per kind. This is a
// fact about the applications rather than a choice, and getting it wrong puts
// a key in a file nothing reads.
func TestEachKindsConfigFileAndFormat(t *testing.T) {
	cases := map[config.Kind]struct {
		format kinds.ConfigFormat
		file   string
	}{
		config.KindMbin:        {kinds.ConfigEnv, ".env"},
		config.KindOutline:     {kinds.ConfigEnv, ".env"},
		config.KindPocketID:    {kinds.ConfigEnv, ".env"},
		config.KindOAuth2Proxy: {kinds.ConfigEnv, ".env"},
		config.KindWriteFreely: {kinds.ConfigINI, "config.ini"},
		config.KindSynapse:     {kinds.ConfigYAML, "homeserver.yaml"},
		config.KindElement:     {kinds.ConfigJSON, "config.json"},
	}
	for _, kind := range config.Kinds() {
		want, known := cases[kind]
		if !known {
			t.Errorf("%s is not in this table, so nobody decided where its configuration lives", kind)
			continue
		}
		if got := kinds.ConfigFormat(kind); got != want.format {
			t.Errorf("%s: format is %q, want %q", kind, got, want.format)
		}
		if got := kinds.ConfigFile(kind); got != want.file {
			t.Errorf("%s: file is %q, want %q", kind, got, want.file)
		}
	}
}
```

Enumerating `config.Kinds()` rather than listing is deliberate: a kind added later fails this test until somebody decides, which is the behaviour `TestDefaultsArePinned` already has.

**Check the rendered file names against the templates before trusting this table.** Read `internal/render/templates/<kind>/` and the code that names rendered files; writefreely's template is `config.ini.secret.tmpl` and the rendered name drops `.secret`. If any name here is wrong, fix the table and say so in your report.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/kinds -run TestEachKindsConfigFileAndFormat -v`
Expected: FAIL, `undefined: kinds.ConfigFormat`.

- [ ] **Step 3: Add the field and the table**

`App` gains:

```go
// Config is passed through to the file this app's kind reads, in that file's
// own syntax, without the toolkit interpreting the key.
//
// It is not `settings`. A setting is an input the toolkit reasons about: it
// reads it, sometimes validates it, and decides things with it. A config key
// is one the toolkit has no opinion about and only places.
//
// A key the kind's template already writes is refused rather than overridden,
// because two sources of truth for one value is how a deployment ends up with
// a setting nobody can locate.
Config map[string]any `yaml:"config"`
```

`kinds` gains the two functions, with a comment on the table saying that the
file names are the rendered names rather than the template names.

- [ ] **Step 4: Write the three failing refusal tests**

Add three cases to the validate table, each loading its own fixture and expecting exactly one finding at REFUSE:

- `config-key-is-nested-in-an-env-file`: an mbin app with `config: {a.b: 1}`. The message says env has no nesting, names the key, and says a dot is a typo here rather than a path.
- `config-key-looks-like-a-secret`: any app with `config: {app.api_token: xyz}`. The message names `secrets.enc.yaml` and says `paisans.yaml` is plaintext and meant to be committed.
- `config-for-a-kind-with-no-config-file`: an oauth2-proxy app is wrong for this one, since it reads an env file. Use a kind whose `ConfigFormat` is `ConfigNone` if one exists; **if every kind has a config file, say so in your report and drop this fixture and its rule rather than inventing a kind to refuse.** Do not add a kind to make a rule fire.

Build each fixture from a neighbouring one so it differs in one thing only.

- [ ] **Step 5: Implement the refusals**

Write them out in full, in the shape the neighbouring rules use. The secret check matches, case insensitively, a key whose final segment or whole name contains `password`, `secret`, `token`, `apikey` or `private_key`. The message must say the check is by name and will not catch a credential named something else, because a narrow check that admits its narrowness is the house style here.

Add all three to `README.md` beside the other rules.

- [ ] **Step 6: Run everything and commit**

```bash
go test ./... -count=1 && go vet ./...
git add internal/config internal/kinds internal/validate README.md
git commit -m "feat: declare where each kind's configuration lives, and refuse the obvious mistakes

$(cat <<'MSG'
A passthrough key means something different per kind because the applications
do not share a format, so kinds now says which file and which syntax each one
reads.

Three refusals are knowable without rendering: a dotted key on an env file,
where a dot is a typo rather than a path; a credential shaped key name, since
paisans.yaml is plaintext and secrets.enc.yaml exists for the other thing; and
a config block on a kind that reads no config file. The collision case needs
rendered output and lands with the merge.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 2: The merge, one function per format, knowing nothing about kinds

**Files:**
- Create: `internal/configmerge/configmerge.go`, `internal/configmerge/configmerge_test.go`

**Interfaces:**
- Consumes: nothing. This package imports no other package of ours.
- Produces:
  ```go
  // Each takes the rendered file and the operator's keys, and returns the
  // merged file. Keys are sorted so output is deterministic. A key already
  // present in the rendered content is reported through Collision rather than
  // overwritten.
  func Env(rendered string, keys map[string]any) (string, error)
  func INI(rendered string, keys map[string]any) (string, error)
  func YAML(rendered string, keys map[string]any) (string, error)
  func JSON(rendered string, keys map[string]any) (string, error)

  // Collision is returned when the rendered file already carries a key.
  type Collision struct { Key string }
  func (c *Collision) Error() string
  ```

- [ ] **Step 1: Write the failing tests**

Four groups. Write them all before implementing any.

```go
// Env is line oriented and has no nesting. The block is labelled so a reader
// of the rendered file knows where the values came from.
func TestEnvAppendsALabelledBlock(t *testing.T) {
	rendered := "# Rendered by paisans. Do not edit.\nDATABASE_URL=postgres://x\n"
	out, err := configmerge.Env(rendered, map[string]any{
		"KBIN_META_TITLE": "A place",
		"KBIN_META_DESC":  "Talk here",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "DATABASE_URL=postgres://x") {
		t.Error("the rendered content must survive")
	}
	if !strings.Contains(out, "KBIN_META_DESC=Talk here") || !strings.Contains(out, "KBIN_META_TITLE=A place") {
		t.Errorf("both keys should be present:\n%s", out)
	}
	if strings.Index(out, "KBIN_META_DESC") > strings.Index(out, "KBIN_META_TITLE") {
		t.Error("keys should be sorted, so two runs render the same bytes")
	}
}

// A key the template already wrote is a collision rather than an override.
func TestEnvRefusesAKeyTheTemplateAlreadyWrote(t *testing.T) {
	_, err := configmerge.Env("DATABASE_URL=postgres://x\n", map[string]any{"DATABASE_URL": "postgres://y"})
	var collision *configmerge.Collision
	if !errors.As(err, &collision) {
		t.Fatalf("want a Collision, got %v", err)
	}
	if collision.Key != "DATABASE_URL" {
		t.Errorf("the collision should name the key, got %q", collision.Key)
	}
}

// Ini inserts into the section that is already there rather than appending a
// second one with the same name, because whether a repeated section merges or
// shadows is a property of whichever parser the application uses.
func TestINIInsertsIntoTheExistingSection(t *testing.T) {
	rendered := "; Rendered by paisans.\n\n[app]\n; what the site is called\nsite_name = Example\n\n[database]\ntype = postgres\n"
	out, err := configmerge.INI(rendered, map[string]any{"app.max_blogs": 3})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "[app]") != 1 {
		t.Errorf("the section must not be repeated:\n%s", out)
	}
	appBlock := out[strings.Index(out, "[app]"):strings.Index(out, "[database]")]
	if !strings.Contains(appBlock, "max_blogs = 3") {
		t.Errorf("the key belongs inside [app]:\n%s", out)
	}
	if !strings.Contains(out, "; what the site is called") {
		t.Error("comments in the rendered file must survive: the file explains itself")
	}
}

// A section the rendered file does not have is created, once.
func TestINICreatesAMissingSection(t *testing.T) {
	out, err := configmerge.INI("[app]\nsite_name = Example\n", map[string]any{"email.enabled": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "[email]") != 1 {
		t.Errorf("want one new section:\n%s", out)
	}
	if !strings.Contains(out, "enabled = true") {
		t.Errorf("want the key:\n%s", out)
	}
}

// Yaml nests, to any depth, and the result must parse.
func TestYAMLSetsANestedPath(t *testing.T) {
	out, err := configmerge.YAML("server_name: example.org\n", map[string]any{
		"retention.enabled":    true,
		"url_preview_enabled":  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the merged file must parse: %v\n%s", err, out)
	}
	if got["server_name"] != "example.org" {
		t.Error("the rendered content must survive")
	}
	if got["url_preview_enabled"] != true {
		t.Error("a top level key should be set")
	}
	retention, ok := got["retention"].(map[string]any)
	if !ok || retention["enabled"] != true {
		t.Errorf("a dotted key should nest:\n%s", out)
	}
}

// Json is the same shape as yaml and must also still parse.
func TestJSONSetsANestedPath(t *testing.T) {
	out, err := configmerge.JSON(`{"default_server_config":{"m.homeserver":{"base_url":"https://chat.example.org"}}}`, map[string]any{
		"show_labs_settings":         true,
		"setting_defaults.use_system_theme": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the merged file must parse: %v\n%s", err, out)
	}
	if got["show_labs_settings"] != true {
		t.Error("a top level key should be set")
	}
	defaults, ok := got["setting_defaults"].(map[string]any)
	if !ok || defaults["use_system_theme"] != false {
		t.Errorf("a dotted key should nest:\n%s", out)
	}
	if _, ok := got["default_server_config"]; !ok {
		t.Error("the rendered content must survive")
	}
}

// A collision at depth is still a collision.
func TestYAMLRefusesAPathTheTemplateAlreadyWrote(t *testing.T) {
	_, err := configmerge.YAML("database:\n  name: synapse\n", map[string]any{"database.name": "other"})
	var collision *configmerge.Collision
	if !errors.As(err, &collision) {
		t.Fatalf("want a Collision, got %v", err)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/configmerge -v`
Expected: FAIL, the package does not exist.

- [ ] **Step 3: Implement the four functions**

Write the package. Its doc comment says why env and ini are line oriented rather than a parse and re-emit: both files are read by humans, a round trip would reformat them, and the insertion is small enough to be obvious.

Guidance, not code to transcribe:
* Sort keys before emitting, every format, so two runs produce the same bytes. The golden tree depends on it.
* **Env**: detect a collision by scanning for a line whose key before `=` matches, ignoring leading whitespace and `export `. Append a block with a one line comment saying these came from `config` in the deployment declaration.
* **Ini**: walk lines tracking the current section. A collision is a key already present in the section the operator is targeting. Insert before the blank line that ends the section, or at the end of the section's last line, so the file keeps its shape. Create a missing section at the end.
* **Yaml**: unmarshal into `yaml.Node` rather than `map[string]any` if you can keep comments that way; if that proves awkward, unmarshal to a map, set, and re-emit, and say in the report that yaml comments are not preserved and why. The rendered `homeserver.yaml` carries long explanatory comments and losing them is a real cost, so make the call deliberately and record it.
* **Json**: unmarshal to `map[string]any`, set the path, re-emit indented. Json has no comments, so nothing is lost.
* A value is a scalar. A map or list in a `config` value is an error naming the key; nesting is expressed by the dotted key, not by yaml structure.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/configmerge -count=1 -v`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
go test ./... -count=1 && go vet ./...
git add internal/configmerge
git commit -m "feat: merge passthrough keys into rendered configuration

$(cat <<'MSG'
One function per format, importing nothing of ours, so the merge is testable
without a kind, an app or a template.

Env and ini are line oriented rather than parsed and re-emitted, because both
files are read by people and a round trip would reformat them. Ini inserts
into the section that already exists instead of appending a second one with
the same name, since whether a repeated section merges or shadows belongs to
whichever parser the application uses.

A key the rendered file already carries is a Collision rather than an
overwrite. Keys are sorted so two runs render the same bytes.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 3: Render calls the merge, and a collision stops the build

**Files:**
- Modify: `internal/render/site.go` or wherever a rendered file is finalised, plus `internal/render/appview.go` if the keys need to reach it
- Modify: `internal/render/testdata/deployment.yaml`
- Test: `internal/render/render_test.go`

**Interfaces:**
- Consumes: `kinds.ConfigFormat`, `kinds.ConfigFile`, the four `configmerge` functions, `App.Config`.
- Produces: rendered config files carrying passthrough keys, and a `Build` error for a collision.

- [ ] **Step 1: Write the failing test**

```go
// A passthrough key reaches the file its kind actually reads, in that file's
// syntax. One key per format, so the rendered output for each is visible in
// the tree an operator receives.
func TestAPassthroughKeyReachesTheRenderedFile(t *testing.T) {
	tree := build(t)

	env := tree.file(t, "home-a/srv/talk/.env")
	if !strings.Contains(env, "KBIN_META_TITLE=A place to talk") {
		t.Errorf("mbin reads an environment:\n%s", env)
	}

	ini := tree.file(t, "home-a/srv/blog/config.ini")
	appBlock := ini[strings.Index(ini, "[app]"):]
	if !strings.Contains(appBlock, "max_blogs = 3") {
		t.Errorf("the blog's key belongs inside [app]:\n%s", ini)
	}

	home := tree.file(t, "vm/srv/chat/homeserver.yaml")
	if !strings.Contains(home, "url_preview_enabled: true") {
		t.Errorf("synapse reads yaml:\n%s", home)
	}

	elem := tree.file(t, "home-b/srv/web/config.json")
	if !strings.Contains(elem, `"show_labs_settings": true`) {
		t.Errorf("element reads json:\n%s", elem)
	}
}

// The collision cannot be caught by validate, which never renders, so Build
// has to refuse it. Every command that would write the file goes through
// Build, so refusing there refuses all of them.
func TestBuildRefusesAKeyTheTemplateAlreadyWrote(t *testing.T) {
	cfg := fixture(t)
	app := cfg.Apps["blog"]
	app.Config = map[string]any{"database.type": "mysql"}
	cfg.Apps["blog"] = app

	secrets := loadFixtureSecrets(t)
	if _, err := render.Build(cfg, secrets); err == nil {
		t.Fatal("a key the template already writes must stop the build")
	} else if !strings.Contains(err.Error(), "config-key-already-rendered") {
		t.Errorf("the error should name the rule, got: %v", err)
	}
}
```

Match the helper names and the `Build` signature to what the package actually
has; read it rather than assuming. The site and app names come from the
fixture, so read those too.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/render -run 'TestAPassthroughKeyReachesTheRenderedFile|TestBuildRefusesAKeyTheTemplateAlreadyWrote' -v`
Expected: FAIL, nothing merges and no key appears.

- [ ] **Step 3: Add the fixture keys**

In `internal/render/testdata/deployment.yaml`, add one passthrough key per format, to the apps the test reads: `KBIN_META_TITLE` on the mbin app, `app.max_blogs` on the blog, `url_preview_enabled` on chat, `show_labs_settings` on the element app. Pick keys those applications really have; the three named here were read from their own configuration, and if you change one, read the application rather than guessing.

- [ ] **Step 4: Call the merge**

Find where a rendered file becomes a `File` in the plan. For the file whose name matches `kinds.ConfigFile(kind)`, pass the rendered content and the app's `Config` through the matching `configmerge` function. A `*configmerge.Collision` becomes an error naming `config-key-already-rendered`, the app, the file and the key.

Do not merge into any other file. A passthrough key must not reach `compose.yaml`, a Caddy snippet or the secrets, and a test asserting that is worth more than a comment saying it.

- [ ] **Step 5: Regenerate the golden tree and read it**

Run `go test ./internal/render -run TestGoldenTree -update`, then `git diff internal/render/testdata/golden`. Expected: the four config files that gained a key, and the manifests. **A passthrough key appearing in any other file is a finding: stop and report it.**

- [ ] **Step 6: Commit**

```bash
go test ./... -count=1 && go vet ./...
git add internal/render
git commit -m "feat: pass a declared config key through to the rendered file

$(cat <<'MSG'
The key reaches the file its kind reads and no other. A key in compose.yaml, a
Caddy snippet or the secrets would be a different and worse feature, so a test
asserts it does not get there.

A collision with a key the template already wrote stops Build rather than
validate, because validate takes a configuration and never renders, so it
cannot know what a template emits. Every command that writes a file goes
through Build, so one refusal covers render, apply and storage init.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

### Task 4: Prove the ini insertion takes effect, and document the mechanism

**Files:**
- Modify: `internal/garage/integration_test.go` or a new `internal/render/integration_test.go` behind the same build tag, whichever fits the existing container helpers better
- Modify: `README.md`, `docs/decisions.md`, `examples/paisans.example.yaml`

**Interfaces:**
- Consumes: everything above.
- Produces: nothing other code depends on.

- [ ] **Step 1: Prove the ini key takes effect against real software**

Every format claim in the last two branches that was reasoned rather than run
needed correcting, and ini is the one with the most room to be wrong: a key in
the right section textually is not the same as a key the application reads.

Behind the existing `garage_integration` build tag, or a new tag if that one's
helpers do not fit, boot the pinned WriteFreely fork against a **rendered**
`config.ini` carrying a passthrough key, with a reachable Postgres, and
confirm the application reports the value. `writefreely settings get <key>` is
how the previous branch read settings back. Put the actual output in your
report.

If the value does not take effect, **stop and report it loudly**. It would mean
the insertion is textually right and functionally wrong, which is the whole
reason this step exists.

- [ ] **Step 2: Document it where an adopter will look**

`README.md` gains a section near the one describing `settings`, saying:
- what `config` is, with one example per format;
- that the toolkit does not interpret the key;
- that a key the toolkit already renders is refused, and that the way to change
  such a value is a `settings` key or a template change rather than an
  override, with the reason;
- that `paisans.yaml` is plaintext, so a credential belongs in
  `secrets.enc.yaml`, and that the check for that is by name and narrow.

The distinction between `settings` and `config` has to be legible to somebody
who has never read the spec, because the two look alike and choosing wrong is
silent in one direction.

- [ ] **Step 3: Record the decision**

`docs/decisions.md`, in the established style: that passthrough exists because
`settings` only works for keys a template already reads; that the merge happens
after rendering in Go rather than as a trailing block in each template, with
the three reasons; that ini inserts into the existing section rather than
repeating it, and why that avoids an assumption about a parser; that a
collision is refused rather than overridden, and what overriding would have
routed around; and that the secret check is by name and admits it.

- [ ] **Step 4: Put one in the example**

`examples/paisans.example.yaml` gains one `config` key with a comment
distinguishing it from the `settings` block above it. One is enough: the README
carries the rest, and an example that demonstrates every format teaches less
than one that shows the difference between the two mechanisms.

- [ ] **Step 5: Run everything and commit**

```bash
go test ./... -count=1 && go vet ./... && go test -tags garage_integration ./internal/... -count=1
git add internal README.md docs/decisions.md examples
git commit -m "test: prove a passthrough ini key is read by the application

$(cat <<'MSG'
A key in the right section textually is not the same as a key the application
reads, and every format claim on the last two branches that was reasoned rather
than run needed correcting. This boots the pinned fork against a rendered
config.ini carrying a passthrough key and reads the value back.

The README now distinguishes settings from config for somebody who has never
read the spec, because the two look alike and choosing wrong is silent in one
direction.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
MSG
)"
```

---

## Self-review

**Spec coverage.** The `config` map, the four formats, and the statically knowable refusals: Task 1. The merge and the collision: Task 2. Reaching the rendered file and refusing a collision at build: Task 3. The real software check and the records: Task 4. The spec's "apply still owns every rendered file" is covered negatively by Task 3's assertion that a key reaches no other file.

**Two things the plan deliberately leaves open, each with instructions.** Whether a kind exists whose `ConfigFormat` is `ConfigNone` is unknown to me: every kind may render a config file, in which case Task 1's third refusal has nothing to refuse and the step says to drop it and report rather than invent a kind. And whether yaml comments survive depends on whether `yaml.Node` is workable for this, which Task 2 asks the implementer to decide deliberately and record, because `homeserver.yaml`'s comments are load bearing for the next reader.

**Placeholders.** Task 2's step 3 gives guidance rather than code for four functions, with the exact behaviour pinned by the tests in step 1. That is the right division here: the tests are the specification and transcribing four parsers into a plan would make it longer without making it clearer.

**Type consistency.** `kinds.ConfigFormat` and `kinds.ConfigFile` are declared in Task 1 and used in Task 3. `configmerge.Env`, `INI`, `YAML`, `JSON` and `Collision` are declared in Task 2 and used in Task 3. `App.Config` is declared in Task 1 and read in Task 3. The refusal name `config-key-already-rendered` appears in Tasks 3 and 4 and nowhere else, because it is a render error rather than a validate rule.

**One risk the plan cannot remove.** Nothing here runs Synapse or Element, so the yaml and json merges are proven to parse and to carry the value, not to be read correctly by those applications. The spec says so, and the ini check is where the class of error is caught cheapest.

# The deployment record: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** each gateway keeps `/var/lib/paisans/deployed.<token>.json`, the names of the sites, apps and Pocket ID groups deployed; `apply` adds to it, the removal commands take from it, and `secrets prune` removes only what the yaml does not declare and no record lists.

**Architecture:** a new package `internal/deployrecord` reads and writes the record over a `registry.Runner`, under the registry's lock with a compare-and-swap on the file's hash. `secretsgen.Orphans` takes the union of the gateways' records. The commands reach the gateways through the replaceable `registryHost`; `site remove` forgets the site in its stage 2 and deletes a cleaned gateway's record in stage 3.

**Tech Stack:** Go; the fake hosts already in `cmd/paisans` (`initFake`, `registryHost`) and `internal/siteremove` (`world`).

**Spec:** `docs/specs/2026-10-09-deployment-record.md`. It builds on `docs/specs/2026-10-09-site-remove-force.md` (`secrets prune`, `site remove --force`).

## Global Constraints

- **Public repo:** documentation addresses and fixture names only (`home-a`, `home-b`, `vm`, `watch`).
- **No rejected alternatives** in docs, comments, commits or PRs. **No migration code.**
- **Names only** in the record: no secret, no address. Warnings and refusals name keys, never values.
- **`apply` only adds** to the record; only `site remove`, `site remove --force` on an undeclared site, and `app remove` take out.
- **Readers take the union** of every gateway's record.
- **`init` and `apply`:** an unreadable record is a warning, never a refusal; a gateway with no record yet is silent.
- **`secrets prune`:** any gateway unreadable or without a record is a refusal naming `--without-record`; `--without-record --execute` asks for the word `prune` at a terminal and has no flag answering it.
- **Commits** end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. After every task `go test ./...` and `go vet ./...` pass.

## Review Focus

1. **Two writers at once** (two operators applying two gateways' sites, or an apply racing a removal). Expected: the second write is refused with "the deployment record changed while it was read", never a lost update. Pinned in Task 1, `TestAWriteRefusesAChangedRecord`.
2. **A record listing a site whose secrets are gone already.** Expected: the warning still names it (the spec's third row is regardless of secrets). Pinned in Task 2, `TestDroppedIsRegardlessOfSecrets`.
3. **Gateways that disagree** (one lists `monitor-a`, the other not). Expected: prune keeps `monitor-a`'s secrets. Pinned in Task 2, `TestOrphansKeepWhatAnyRecordLists`.
4. **A gateway that answers but has no record** (applied before this feature). Expected: `init`/`apply` silent; prune refuses and names `--without-record`. Pinned in Task 3, `TestPruneRefusesWithoutARecord`.
5. **`app remove` when the app is already gone from every host** (plan empty) but a record still lists it. Expected: the record is still cleaned. Pinned in Task 6, `TestForgetAppCleansTheRecordWhenNothingElseIsLeft`.

---

### Task 1: `internal/deployrecord`

**Files:**
- Create: `internal/deployrecord/record.go`, `internal/deployrecord/record_test.go`

**Interfaces:**
- Produces:
  - `type Record struct { Version int; Sites, Apps, PocketIDGroups []string }` (json `version`, `sites`, `apps`, `pocket_id_groups`)
  - `func Path(d deployment.Deployment) string`
  - `func FromConfig(cfg *config.Config) Record`
  - `func (r Record) Lists(kind, name string) bool`, kind one of `"sites"`, `"apps"`, `"pocket_id_groups"`
  - `func Union(rs ...Record) Record`
  - `func Read(t registry.Runner, d deployment.Deployment) (Record, bool, error)`
  - `func Add(t registry.Runner, d deployment.Deployment, names Record) (bool, error)`
  - `func Forget(t registry.Runner, d deployment.Deployment, names Record) (bool, error)`
  - `func RemoveCommand(d deployment.Deployment) string`
  - `const ChangedMarker = "paisans: the deployment record changed while it was read"`

- [ ] **Step 1: Write the failing tests**

```go
package deployrecord_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
)

var dep = deployment.Deployment{ID: "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}

// fakeHost runs the record's write command as the shell would: refuse on a
// changed hash, else replace the file.
type fakeHost struct{ files map[string]string }

var (
	pathRe = regexp.MustCompile(`f='([^']+)'`)
	sumRe  = regexp.MustCompile(`\[ "\$cur" = '([^']*)' \]`)
	dataRe = regexp.MustCompile(`printf %s '([^']*)' \| base64 -d`)
)

func (h *fakeHost) Describe() string { return "ubuntu@vm.example.org" }
func (h *fakeHost) ReadFile(p string) (string, bool, error) {
	c, ok := h.files[p]
	return c, ok, nil
}
func (h *fakeHost) Run(command string) (string, error) {
	path := pathRe.FindStringSubmatch(command)[1]
	cur := "none"
	if c, ok := h.files[path]; ok {
		s := sha256.Sum256([]byte(c))
		cur = hex.EncodeToString(s[:])
	}
	if sumRe.FindStringSubmatch(command)[1] != cur {
		return deployrecord.ChangedMarker + "\n", errors.New("exit status 1")
	}
	data, _ := base64.StdEncoding.DecodeString(dataRe.FindStringSubmatch(command)[1])
	h.files[path] = string(data)
	return "", nil
}

func TestAddIsAUnionAndForgetTakesOnlyWhatIsNamed(t *testing.T) {
	h := &fakeHost{files: map[string]string{}}
	if changed, err := deployrecord.Add(h, dep, deployrecord.Record{Sites: []string{"vm", "home-a"}, Apps: []string{"talk"}}); err != nil || !changed {
		t.Fatalf("first add: %v %v", changed, err)
	}
	if _, err := deployrecord.Add(h, dep, deployrecord.Record{Sites: []string{"home-b"}}); err != nil {
		t.Fatal(err)
	}
	r, found, err := deployrecord.Read(h, dep)
	if err != nil || !found || strings.Join(r.Sites, ",") != "home-a,home-b,vm" || !r.Lists("apps", "talk") {
		t.Fatalf("%+v %v %v", r, found, err)
	}
	if changed, _ := deployrecord.Add(h, dep, deployrecord.Record{Sites: []string{"vm"}}); changed {
		t.Error("adding what is listed wrote the file")
	}
	if _, err := deployrecord.Forget(h, dep, deployrecord.Record{Sites: []string{"home-b"}}); err != nil {
		t.Fatal(err)
	}
	r, _, _ = deployrecord.Read(h, dep)
	if strings.Join(r.Sites, ",") != "home-a,vm" || !r.Lists("apps", "talk") {
		t.Errorf("after forget: %+v", r)
	}
}

func TestForgetWithNoRecordWritesNothing(t *testing.T) {
	h := &fakeHost{files: map[string]string{}}
	if changed, err := deployrecord.Forget(h, dep, deployrecord.Record{Sites: []string{"x"}}); err != nil || changed || len(h.files) != 0 {
		t.Errorf("%v %v %v", changed, err, h.files)
	}
}

func TestAWriteRefusesAChangedRecord(t *testing.T) {
	h := &fakeHost{files: map[string]string{}}
	racing := &racer{fakeHost: h}
	_, err := deployrecord.Add(racing, dep, deployrecord.Record{Sites: []string{"vm"}})
	if err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.files[deployrecord.Path(dep)], "home-b") {
		t.Error("the other writer's record was overwritten")
	}
}

// racer is a host where another writer lands between the read and the write.
type racer struct{ *fakeHost }

func (r *racer) ReadFile(p string) (string, bool, error) {
	c, ok, err := r.fakeHost.ReadFile(p)
	r.files[p] = `{"version":1,"sites":["home-b"],"apps":[],"pocket_id_groups":[]}` + "\n"
	return c, ok, err
}

func TestAMalformedRecordIsAnError(t *testing.T) {
	h := &fakeHost{files: map[string]string{deployrecord.Path(dep): "{"}}
	if _, _, err := deployrecord.Read(h, dep); err == nil {
		t.Error("read a malformed record")
	}
}

func TestUnion(t *testing.T) {
	u := deployrecord.Union(deployrecord.Record{Sites: []string{"a"}}, deployrecord.Record{Sites: []string{"b", "a"}, Apps: []string{"x"}})
	if strings.Join(u.Sites, ",") != "a,b" || !u.Lists("apps", "x") {
		t.Errorf("%+v", u)
	}
}

func TestThePathCarriesTheToken(t *testing.T) {
	if got := deployrecord.Path(dep); got != "/var/lib/paisans/deployed.f2a9.json" {
		t.Errorf("got %s", got)
	}
}
```

Also a `FromConfig` test, against the fixture:

```go
func TestFromConfig(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	r := deployrecord.FromConfig(cfg)
	if strings.Join(r.Sites, ",") != "home-a,home-b,vm,watch" || !r.Lists("pocket_id_groups", "members") || !r.Lists("apps", "talk") {
		t.Errorf("%+v", r)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/deployrecord/`
Expected: FAIL to compile, the package does not exist.

- [ ] **Step 3: Implement `internal/deployrecord/record.go`**

```go
// Package deployrecord is the record each gateway keeps of what the
// deployment has deployed: its sites, apps and Pocket ID groups, by name.
// apply adds to it and only the removal commands take from it, so a
// paisans.yaml edited by mistake cannot shrink it, and `secrets prune` removes
// only what the yaml does not declare and no record lists
// (docs/specs/2026-10-09-deployment-record.md).
package deployrecord

// Version is the record's layout.
const Version = 1

// ChangedMarker is what the write prints when the record changed between
// the read and the write: another command wrote it, and running again reads
// that.
const ChangedMarker = "paisans: the deployment record changed while it was read"

type Record struct {
	Version        int      `json:"version"`
	Sites          []string `json:"sites"`
	Apps           []string `json:"apps"`
	PocketIDGroups []string `json:"pocket_id_groups"`
}

// Path is the record on a gateway, named by the deployment's token, which is
// what proves it this deployment's.
func Path(d deployment.Deployment) string {
	return registry.Dir + "/deployed." + d.Token() + ".json"
}

// FromConfig is everything cfg declares.
func FromConfig(cfg *config.Config) Record {
	r := Record{Sites: cfg.SiteNames(), Apps: cfg.AppNames()}
	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Kind == config.KindPocketID {
			r.PocketIDGroups = append(r.PocketIDGroups, kinds.PocketIDSignupGroups(cfg.Apps[name].Settings)...)
		}
	}
	return normal(r)
}

func (r Record) list(kind string) []string {
	switch kind {
	case "sites":
		return r.Sites
	case "apps":
		return r.Apps
	case "pocket_id_groups":
		return r.PocketIDGroups
	}
	return nil
}

// Lists reports whether the record names name under kind: sites, apps or
// pocket_id_groups, the secrets file's own keys.
func (r Record) Lists(kind, name string) bool {
	for _, n := range r.list(kind) {
		if n == name {
			return true
		}
	}
	return false
}

// Union is every name any of rs lists.
func Union(rs ...Record) Record {
	var out Record
	for _, r := range rs {
		out.Sites = append(out.Sites, r.Sites...)
		out.Apps = append(out.Apps, r.Apps...)
		out.PocketIDGroups = append(out.PocketIDGroups, r.PocketIDGroups...)
	}
	return normal(out)
}

func minus(r, names Record) Record {
	drop := func(list, gone []string) []string {
		var out []string
		for _, n := range list {
			if !slices.Contains(gone, n) {
				out = append(out, n)
			}
		}
		return out
	}
	return normal(Record{Sites: drop(r.Sites, names.Sites), Apps: drop(r.Apps, names.Apps), PocketIDGroups: drop(r.PocketIDGroups, names.PocketIDGroups)})
}

// normal sorts each list, drops repeats, and makes absent lists empty, so
// that equal records encode the same.
func normal(r Record) Record {
	clean := func(list []string) []string {
		out := []string{}
		for _, n := range list {
			if n != "" && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
		sort.Strings(out)
		return out
	}
	return Record{Version: Version, Sites: clean(r.Sites), Apps: clean(r.Apps), PocketIDGroups: clean(r.PocketIDGroups)}
}

func encode(r Record) string {
	data, _ := json.Marshal(normal(r))
	return string(data) + "\n"
}

func parse(content string) (Record, error) {
	var r Record
	if err := json.Unmarshal([]byte(content), &r); err != nil {
		return Record{}, err
	}
	if r.Version != Version {
		return Record{}, fmt.Errorf("version %d, and this toolkit reads version %d", r.Version, Version)
	}
	return normal(r), nil
}

// Read reads the record on the gateway t reaches. found is false for a
// gateway with none, which is not an error.
func Read(t registry.Runner, d deployment.Deployment) (Record, bool, error) {
	r, _, found, err := read(t, d)
	return r, found, err
}

func read(t registry.Runner, d deployment.Deployment) (Record, string, bool, error) {
	content, found, err := t.ReadFile(Path(d))
	if err != nil {
		return Record{}, "", false, fmt.Errorf("%s: reading %s: %w", t.Describe(), Path(d), err)
	}
	if !found {
		return normal(Record{}), "", false, nil
	}
	r, err := parse(content)
	if err != nil {
		return Record{}, "", false, fmt.Errorf("%s: %s is not a deployment record: %w", t.Describe(), Path(d), err)
	}
	return r, content, true, nil
}

// Add adds names to the record, creating it. It reports whether it wrote.
func Add(t registry.Runner, d deployment.Deployment, names Record) (bool, error) {
	cur, raw, found, err := read(t, d)
	if err != nil {
		return false, err
	}
	next := Union(cur, names)
	if found && encode(next) == encode(cur) {
		return false, nil
	}
	return true, write(t, d, raw, found, next)
}

// Forget takes names out of the record. A gateway with none is left
// without one.
func Forget(t registry.Runner, d deployment.Deployment, names Record) (bool, error) {
	cur, raw, found, err := read(t, d)
	if err != nil || !found {
		return false, err
	}
	next := minus(cur, names)
	if encode(next) == encode(cur) {
		return false, nil
	}
	return true, write(t, d, raw, found, next)
}

// write replaces the record with next, under the registry's lock, only if
// the file still hashes to what was read: a record another command changed
// meanwhile is refused rather than overwritten.
func write(t registry.Runner, d deployment.Deployment, raw string, found bool, next Record) error {
	want := "none"
	if found {
		sum := sha256.Sum256([]byte(raw))
		want = hex.EncodeToString(sum[:])
	}
	out, err := t.Run(writeCommand(d, want, encode(next)))
	if err != nil {
		if strings.Contains(out, ChangedMarker) {
			return fmt.Errorf("%s: %s. Run the command again", t.Describe(), ChangedMarker)
		}
		return fmt.Errorf("%s: writing %s: %w: %s", t.Describe(), Path(d), err, strings.TrimSpace(out))
	}
	return nil
}

func writeCommand(d deployment.Deployment, want, content string) string {
	return strings.Join([]string{
		"set -e",
		"umask 077",
		"mkdir -p " + registry.Dir,
		"chmod 700 " + registry.Dir,
		"exec 9>>" + registry.LockPath,
		fmt.Sprintf("flock -w 60 9 || { echo 'paisans: another command holds %s'; exit 1; }", registry.LockPath),
		"f=" + quote(Path(d)),
		"cur=none",
		`if [ -f "$f" ]; then cur=$(sha256sum "$f" | cut -d' ' -f1); fi`,
		`[ "$cur" = ` + quote(want) + ` ] || { echo ` + quote(ChangedMarker) + `; exit 1; }`,
		"tmp=$(mktemp " + registry.Dir + "/.deployed.XXXXXX)",
		`trap 'rm -f "$tmp"' EXIT`,
		"printf %s " + quote(base64.StdEncoding.EncodeToString([]byte(content))) + ` | base64 -d > "$tmp"`,
		`chmod 600 "$tmp"`,
		`mv "$tmp" "$f"`,
	}, "; ")
}

// RemoveCommand deletes the record, for cleaning a gateway host.
func RemoveCommand(d deployment.Deployment) string { return "rm -f -- " + quote(Path(d)) }

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
```

Imports: `crypto/sha256`, `encoding/base64`, `encoding/hex`, `encoding/json`, `fmt`, `slices`, `sort`, `strings`, and `config`, `deployment`, `kinds`, `registry`. Check `kinds` does not import `registry` (`go list -deps ./internal/kinds | grep registry`); there is no cycle either way, since `registry` imports neither.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/deployrecord/ && go vet ./internal/deployrecord/`
Expected: PASS.

- [ ] **Step 5: Commit**

`git add internal/deployrecord && git commit -m "feat: the deployment record each gateway keeps"` with the trailer.

---

### Task 2: orphans read the record

**Files:**
- Modify: `internal/secretsgen/orphans.go`, `internal/secretsgen/orphans_test.go`
- Modify: callers of `secretsgen.Orphans` in `cmd/paisans/secrets.go` (pass `nil` for now)

**Interfaces:**
- Consumes: `deployrecord.Record`, `Lists` (Task 1).
- Produces:
  - `func Orphans(cfg *config.Config, secrets *config.Secrets, deployed *deployrecord.Record) []Orphan`: as today, minus every key whose name `deployed` lists. `nil` means no record: the yaml alone.
  - `func Dropped(cfg *config.Config, deployed *deployrecord.Record) []Orphan`: every name `deployed` lists that the yaml does not declare, keyed `sites.<n>`, `apps.<n>`, `pocket_id_groups.<n>`, with `Why` = `is deployed but paisans.yaml no longer declares it` and `Leaves` = what to do (below). Empty for `nil`.

- [ ] **Step 1: Write the failing tests**

```go
func TestOrphansKeepWhatAnyRecordLists(t *testing.T) {
	cfg := &config.Config{Sites: map[string]config.Site{"home-a": {}}, Apps: map[string]config.App{}}
	s := &config.Secrets{
		Sites:          map[string]config.SiteSecrets{"home-a": {}, "monitor-a": {}, "monitor-b": {}},
		Apps:           map[string]map[string]any{"uptime": {}},
		OIDCClients:    map[string]config.OIDCClient{"uptime": {}},
		PocketIDGroups: map[string]string{"old": "1"},
	}
	one := deployrecord.Record{Sites: []string{"home-a", "monitor-a"}, Apps: []string{"uptime"}}
	two := deployrecord.Record{Sites: []string{"home-a"}, PocketIDGroups: []string{"old"}}
	u := deployrecord.Union(one, two)
	var keys []string
	for _, o := range secretsgen.Orphans(cfg, s, &u) {
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, ",") != "sites.monitor-b" {
		t.Errorf("got %v, want only sites.monitor-b", keys)
	}
	if n := len(secretsgen.Orphans(cfg, s, nil)); n != 5 {
		t.Errorf("without a record: %d orphans, want 5", n)
	}
}

func TestDroppedIsRegardlessOfSecrets(t *testing.T) {
	cfg := &config.Config{Sites: map[string]config.Site{"home-a": {}}, Apps: map[string]config.App{}}
	rec := deployrecord.Record{Sites: []string{"home-a", "monitor-a"}, Apps: []string{"uptime"}, PocketIDGroups: []string{"old"}}
	var keys []string
	for _, o := range secretsgen.Dropped(cfg, &rec) {
		keys = append(keys, o.Key)
		if !strings.Contains(o.Why, "still deployed") && !strings.Contains(o.Why, "is deployed") {
			t.Errorf("%s: %q", o.Key, o.Why)
		}
		if o.Leaves == "" {
			t.Errorf("%s says nothing about what to do", o.Key)
		}
	}
	if strings.Join(keys, ",") != "apps.uptime,pocket_id_groups.old,sites.monitor-a" {
		t.Errorf("got %v", keys)
	}
	if len(secretsgen.Dropped(cfg, nil)) != 0 {
		t.Error("no record dropped something")
	}
}
```

Update `TestOrphans` and `TestNoOrphansWhenTheSecretsMatch` to pass `nil`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/secretsgen/ -run 'Orphans|Dropped'`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

In `Orphans`, add the parameter and, before each `append`, skip a name the record lists:

```go
	held := func(kind, name string) bool { return deployed != nil && deployed.Lists(kind, name) }
```

`sites` checks `held("sites", name)`; `apps` and `oidc_clients` check `held("apps", name)`; `pocket_id_groups` checks `held("pocket_id_groups", group)`.

```go
// Dropped is every name the record lists that paisans.yaml does not declare:
// something taken out of the yaml while it still runs, by mistake or before
// its removal command. It is reported whether or not it has secrets.
func Dropped(cfg *config.Config, deployed *deployrecord.Record) []Orphan {
	if deployed == nil {
		return nil
	}
	const why = "is deployed but paisans.yaml no longer declares it"
	declared := deployrecord.FromConfig(cfg)
	var out []Orphan
	for _, n := range deployed.Sites {
		if !declared.Lists("sites", n) {
			out = append(out, Orphan{Key: "sites." + n, Why: why, Leaves: "restore it, or take it out with `paisans site remove " + n + "`, or `paisans site remove " + n + " --force --ssh <its host>` if its host is gone"})
		}
	}
	for _, n := range deployed.Apps {
		if !declared.Lists("apps", n) {
			out = append(out, Orphan{Key: "apps." + n, Why: why, Leaves: "restore it, or take it out with `paisans app remove " + n + "`"})
		}
	}
	for _, n := range deployed.PocketIDGroups {
		if !declared.Lists("pocket_id_groups", n) {
			out = append(out, Orphan{Key: "pocket_id_groups." + n, Why: why, Leaves: "restore an app naming it in signup_default_groups, or `paisans app remove` the app that named it"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
```

In `cmd/paisans/secrets.go`, pass `nil` at both `Orphans` calls.

- [ ] **Step 4: Run tests**

Run: `go test ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

`git commit -m "feat: orphaned secrets keep what a deployment record lists"`.

---

### Task 3: prune and the warnings read the gateways

**Files:**
- Create: `cmd/paisans/record.go` (`deploymentRecord`, `warnSecrets`)
- Modify: `cmd/paisans/secrets.go` (`runSecretsPrune(args, stdin, stdout)`, `--without-record`, `--sudo`; `runSecrets` passes `os.Stdin, os.Stdout`; `warnOrphans` replaced by `warnSecrets`)
- Modify: `cmd/paisans/main.go` (`runInit` reads the record through `initHosts`; `runApply` reads it after its transport exists)
- Test: `cmd/paisans/secrets_test.go`, `cmd/paisans/init_test.go`

**Interfaces:**
- Consumes: Tasks 1-2.
- Produces:
  - `var errNoRecord = errors.New("no deployment record: the gateway has not been applied since the record existed")`
  - `func deploymentRecord(cfg *config.Config, hosts func(gateway string) registry.Runner) (deployrecord.Record, map[string]error)`: union of the gateways' records; `missing[gw]` is the read error, or `errNoRecord`.
  - `func warnSecrets(r ui.Reporter, cfg *config.Config, secrets *config.Secrets, deployed *deployrecord.Record)`: one `r.Warn` per orphan and per dropped name.
  - `func recordForWarnings(r ui.Reporter, cfg *config.Config, hosts func(string) registry.Runner) *deployrecord.Record`: reads; warns for each unreadable gateway except `errNoRecord`; returns `nil` when no gateway has a record.

- [ ] **Step 1: Write the failing tests**

In `secrets_test.go`, a fake `registryHost` serving records:

```go
// withRecords makes every gateway answer with the record given for it; a
// gateway absent from records has none, and one named in down does not
// answer.
func withRecords(t *testing.T, records map[string]string, down ...string) {
	t.Helper()
	saved := registryHost
	registryHost = func(name string, site config.Site, destination string, sudo bool) registry.Runner {
		return &initFake{name: name, down: slices.Contains(down, name), files: map[string]string{deployrecord.Path(fixtureDeployment()): records[name]}}
	}
	t.Cleanup(func() { registryHost = saved })
}

func fixtureDeployment() deployment.Deployment {
	cfg, _ := config.Load(fixtureConfig())
	return cfg.Deployment()
}

func TestPruneRefusesWithoutARecord(t *testing.T) {
	configPath, secretsPath := writeFixtureSecrets(t, func(s *config.Secrets) { s.Sites["monitor-a"] = config.SiteSecrets{WireGuardPrivateKey: "x"} })
	withRecords(t, map[string]string{})
	var err error
	captureStdout(t, func() { err = runSecretsPrune([]string{"--config", configPath, "--secrets", secretsPath}, strings.NewReader(""), &bytes.Buffer{}) })
	if err == nil || !strings.Contains(err.Error(), "--without-record") || !strings.Contains(err.Error(), "vm") {
		t.Errorf("err = %v", err)
	}
}

func TestPruneKeepsWhatTheRecordLists(t *testing.T) {
	configPath, secretsPath := writeFixtureSecrets(t, func(s *config.Secrets) {
		s.Sites["monitor-a"] = config.SiteSecrets{WireGuardPrivateKey: "x"}
		s.Sites["monitor-b"] = config.SiteSecrets{WireGuardPrivateKey: "y"}
	})
	withRecords(t, map[string]string{"vm": `{"version":1,"sites":["home-a","home-b","vm","watch","monitor-a"],"apps":[],"pocket_id_groups":[]}`})
	out := captureStdout(t, func() {
		if err := runSecretsPrune([]string{"--config", configPath, "--secrets", secretsPath, "--execute"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	})
	s, _ := config.LoadSecrets(secretsPath)
	if _, ok := s.Sites["monitor-a"]; !ok {
		t.Error("a site the record lists lost its secrets")
	}
	if _, ok := s.Sites["monitor-b"]; ok {
		t.Error("sites.monitor-b, in no record, was kept")
	}
	if !strings.Contains(out, "sites.monitor-a is deployed") {
		t.Errorf("prune does not say why it kept monitor-a:\n%s", out)
	}
}

func TestPruneWithoutRecordAsksAtATerminal(t *testing.T) {
	configPath, secretsPath := writeFixtureSecrets(t, func(s *config.Secrets) { s.Sites["monitor-a"] = config.SiteSecrets{WireGuardPrivateKey: "x"} })
	withRecords(t, map[string]string{}, "vm")
	var err error
	captureStdout(t, func() {
		err = runSecretsPrune([]string{"--config", configPath, "--secrets", secretsPath, "--without-record", "--execute"}, strings.NewReader("prune\n"), &bytes.Buffer{})
	})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("err = %v", err)
	}
	if s, _ := config.LoadSecrets(secretsPath); s.Sites["monitor-a"].WireGuardPrivateKey == "" {
		t.Error("the file was changed")
	}
}
```

`initFake` gains `files map[string]string`, read by `ReadFile` before the registry case:

```go
	if c, ok := f.files[path]; ok && c != "" {
		return c, true, nil
	}
```

Update `TestSecretsPruneListsThenRemovesOnlyOrphans` and `TestSecretsPruneNeverDecryptsTheFile` to call `withRecords(t, map[string]string{"vm": <a record listing the fixture's sites, apps and groups>})` and the new signature. A helper builds that record:

```go
func fixtureRecord() string {
	cfg, _ := config.Load(fixtureConfig())
	data, _ := json.Marshal(deployrecord.FromConfig(cfg))
	return string(data)
}
```

In `init_test.go`, `TestInitWarnsAboutOrphanedSecrets` gives `vm` a record (`sites["vm"].files = map[string]string{deployrecord.Path(...): fixtureRecord()}`), and a new test:

```go
// A site the record lists that paisans.yaml no longer declares is warned
// about, whether or not it has secrets.
func TestInitWarnsAboutADroppedSite(t *testing.T) {
	sites := fakeSites()
	path, out := initWorld(t, nil, sites, nil)
	cfg, _ := config.Load(path)
	sites["vm"].files = map[string]string{deployrecord.Path(cfg.Deployment()): `{"version":1,"sites":["home-a","home-b","vm","watch","monitor-a"],"apps":[],"pocket_id_groups":[]}`}
	if err := runInitQuietly(t, path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Lines(), "sites.monitor-a is deployed but paisans.yaml no longer declares it") {
		t.Errorf("no warning:\n%s", out.Lines())
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cmd/paisans/ -run 'Prune|InitWarns'`
Expected: FAIL to compile (`runSecretsPrune` arity, `initFake.files`).

- [ ] **Step 3: Implement `cmd/paisans/record.go`**

```go
var errNoRecord = errors.New("it has no deployment record, which its next apply writes")

// deploymentRecord is the union of every gateway's deployment record, and,
// for each gateway that could not give one, why.
func deploymentRecord(cfg *config.Config, hosts func(string) registry.Runner) (deployrecord.Record, map[string]error) {
	missing := map[string]error{}
	var records []deployrecord.Record
	for _, gw := range cfg.GatewaySites() {
		rec, found, err := deployrecord.Read(hosts(gw), cfg.Deployment())
		switch {
		case err != nil:
			missing[gw] = err
		case !found:
			missing[gw] = errNoRecord
		default:
			records = append(records, rec)
		}
	}
	return deployrecord.Union(records...), missing
}

// recordForWarnings is the record init and apply warn against. A gateway
// with no record yet is silent; one that cannot be read is a warning. With
// no record at all the yaml alone is used.
func recordForWarnings(r ui.Reporter, cfg *config.Config, hosts func(string) registry.Runner) *deployrecord.Record {
	rec, missing := deploymentRecord(cfg, hosts)
	read := len(cfg.GatewaySites()) - len(missing)
	for _, gw := range sortedKeys(missing) {
		if !errors.Is(missing[gw], errNoRecord) {
			r.Warn("could not read the deployment record on "+gw, missing[gw].Error())
		}
	}
	if read == 0 {
		return nil
	}
	return &rec
}

// warnSecrets warns once per orphaned secret and once per name the record
// lists that paisans.yaml no longer declares. Never a refusal.
func warnSecrets(r ui.Reporter, cfg *config.Config, secrets *config.Secrets, deployed *deployrecord.Record) {
	for _, o := range secretsgen.Dropped(cfg, deployed) {
		r.Warn(o.Key+" "+o.Why, o.Leaves+".")
	}
	for _, o := range secretsgen.Orphans(cfg, secrets, deployed) {
		r.Warn("secrets: "+o.Key+" "+o.Why, "`paisans secrets prune` removes it.")
	}
}
```

`sortedKeys` is a small helper (`maps.Keys` + `slices.Sorted`). Delete `warnOrphans`.

In `runSecretsPrune(args []string, stdin io.Reader, stdout io.Writer)`: add `withoutRecord := fs.Bool("without-record", false, "trust paisans.yaml alone, when no gateway's deployment record can be read; asks for the word prune at a terminal")` and `sudo := fs.Bool("sudo", true, "read the gateways' records through sudo, since /var/lib/paisans is root's")`. After loading secrets:

```go
	var deployed *deployrecord.Record
	if !*withoutRecord {
		if len(cfg.GatewaySites()) == 0 {
			return fmt.Errorf("secrets prune: %s declares no gateway, so there is no deployment record to read. Pass --without-record to trust paisans.yaml alone. Nothing was changed", *configPath)
		}
		rec, missing := deploymentRecord(cfg, func(gw string) registry.Runner { return registryHost(gw, cfg.Sites[gw], "", *sudo) })
		if len(missing) > 0 {
			gw := sortedKeys(missing)[0]
			return fmt.Errorf("secrets prune: the deployment record on %s could not be read (%v), so nothing says whether what paisans.yaml no longer declares was removed or is still running. Apply the gateway first, or pass --without-record to trust paisans.yaml alone. Nothing was changed", gw, missing[gw])
		}
		deployed = &rec
	}
	for _, o := range secretsgen.Dropped(cfg, deployed) {
		r.Note(o.Key+" "+o.Why+"; its secrets are kept", o.Leaves)
	}
	orphans := secretsgen.Orphans(cfg, secrets, deployed)
```

and, before writing with `--execute`:

```go
	if *withoutRecord {
		if err := confirmWord(stdin, stdout, "prune", fmt.Sprintf("This removes the %d secret(s) above for good, trusting paisans.yaml alone.", len(orphans))); err != nil {
			return err
		}
	}
```

`confirmWord` asks for any word; `confirmSiteFor` becomes a call to it with the site's name, prefixing `site remove: ` to its errors:

```go
// confirmWord says what is about to happen and asks for word at the
// terminal, refusing anything else. There is no flag to answer it.
func confirmWord(stdin io.Reader, stdout io.Writer, word, what string) error {
	if !stdinIsTerminal(stdin) {
		return fmt.Errorf("it asks for the word %s at a terminal, and stdin is not one. Run it from an interactive shell. Nothing was changed", word)
	}
	fmt.Fprintf(stdout, "\n%s Type %s to go on: ", what, word)
	answer, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && answer == "" {
		return fmt.Errorf("no answer read. Nothing was changed")
	}
	if strings.TrimSpace(answer) != word {
		return fmt.Errorf("%q is not %s. Nothing was changed", strings.TrimSpace(answer), word)
	}
	return nil
}
```


`runInit`: the hosts map `initHosts(cfg, *sudo)` passed to `settleMesh` is kept in a variable; after the secrets load, replace `warnOrphans(r, cfg, secrets)` with `warnSecrets(r, cfg, secrets, recordForWarnings(r, cfg, func(gw string) registry.Runner { return hosts[gw] }))`. If the config was re-loaded after the mesh roll, rebuild nothing: the hosts are by site name and unchanged.

`runApply`: remove the `warnOrphans` call; after `transport := siteTransport(...)`, add:

```go
	warnSecrets(r, cfg, secrets, recordForWarnings(r, cfg, func(gw string) registry.Runner {
		if gw == *site {
			return transport
		}
		return registryHost(gw, cfg.Sites[gw], "", *sudo)
	}))
```

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/paisans/ ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

`git commit -m "feat: secrets prune and the warnings read every gateway's deployment record"`.

---

### Task 4: `apply` adds to the record on a gateway

**Files:**
- Modify: `cmd/paisans/record.go` (`recordApplied`), `cmd/paisans/main.go` (`runApply`, after the standby check)
- Test: `cmd/paisans/record_test.go`

**Interfaces:**
- Produces: `func recordApplied(r ui.Reporter, cfg *config.Config, site string, t registry.Runner) error`: on a gateway site, `deployrecord.Add(t, d, deployrecord.FromConfig(cfg))` under a step `record the deployment on <site>`; on any other site, nothing.

- [ ] **Step 1: Write the failing test**

```go
func TestApplyRecordsTheDeploymentOnAGateway(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	gw := &recordFake{files: map[string]string{}}
	if err := recordApplied(&ui.Recorder{}, cfg, "vm", gw); err != nil {
		t.Fatal(err)
	}
	rec, found, _ := deployrecord.Read(gw, cfg.Deployment())
	if !found || !rec.Lists("sites", "home-b") || !rec.Lists("apps", "talk") {
		t.Errorf("%+v %v", rec, found)
	}
	other := &recordFake{files: map[string]string{}}
	if err := recordApplied(&ui.Recorder{}, cfg, "home-b", other); err != nil || len(other.files) != 0 || len(other.ran) != 0 {
		t.Errorf("a non gateway was written: %v %v", err, other.files)
	}
}
```

`recordFake` in `record_test.go` is Task 1's `fakeHost` (same three regexps), plus `ran []string`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/paisans/ -run ApplyRecords`
Expected: FAIL, `undefined: recordApplied`.

- [ ] **Step 3: Implement**

```go
// recordApplied adds what paisans.yaml declares to the deployment record on
// a gateway, after its apply: apply only ever adds, so a yaml edited by
// mistake cannot take a running site's protection away.
func recordApplied(r ui.Reporter, cfg *config.Config, site string, t registry.Runner) error {
	if !cfg.Sites[site].Has(config.RoleGateway) {
		return nil
	}
	s := r.Step("record the deployment on " + site)
	changed, err := deployrecord.Add(t, cfg.Deployment(), deployrecord.FromConfig(cfg))
	if err != nil {
		s.Fail(err)
		return fmt.Errorf("%w. The apply itself finished; run it again to record it", err)
	}
	s.Done(map[bool]string{true: "updated", false: "up to date"}[changed])
	return nil
}
```

In `runApply`, after the `checkStandby` call and before `clients.result()`: `if err := recordApplied(r, cfg, *site, transport); err != nil { return err }`.

- [ ] **Step 4: Run tests** — `go test ./... && go vet ./...`, PASS.

- [ ] **Step 5: Commit** — `git commit -m "feat: apply records the deployment on a gateway"`.

---

### Task 5: `site remove` forgets the site, and cleaning a gateway deletes its record

**Files:**
- Modify: `internal/siteremove/cluster.go` (stage 2), `internal/siteremove/host.go` (stage 3: `hostPlan.record`, a step, `runHost`)
- Modify: `internal/siteremove/world_test.go` (a case running the record's write command)
- Test: `internal/siteremove/record_test.go`

**Interfaces:**
- Consumes: `deployrecord.Read`, `Forget`, `Path`, `RemoveCommand` (Task 1).
- Produces: stage 2 steps with verb `forget`, one per remaining gateway whose record lists the site; stage 3 step `delete` titled `delete the deployment record`.

- [ ] **Step 1: The fake runs the write**

In `host.Run`, before the `default`:

```go
	case strings.Contains(command, "/.deployed."):
		path := regexp.MustCompile(`f='([^']+)'`).FindStringSubmatch(command)[1]
		data, _ := base64.StdEncoding.DecodeString(regexp.MustCompile(`printf %s '([^']*)' \| base64 -d`).FindStringSubmatch(command)[1])
		h.files[path] = string(data)
		return "", nil
	case strings.HasPrefix(command, "rm -f -- '/var/lib/paisans/deployed."):
		delete(h.files, strings.TrimSuffix(strings.TrimPrefix(command, "rm -f -- '"), "'"))
		return "", nil
```

- [ ] **Step 2: Write the failing tests**

```go
func recordListing(sites ...string) string {
	return `{"version":1,"sites":["` + strings.Join(sites, `","`) + `"],"apps":[],"pocket_id_groups":[]}` + "\n"
}

// The remaining gateway's record no longer lists the removed site.
func TestSiteRemoveForgetsTheSiteOnEveryGateway(t *testing.T) {
	w := setup(t)
	vm := w.hosts["vm"]
	vm.files[deployrecord.Path(dep)] = recordListing("home-a", "home-b", "vm", "watch")
	p := w.mustBuild("home-b", siteremove.Options{})
	if !hasStep(p, 2, "vm", "forget", "home-b") {
		t.Fatalf("no forget planned:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(vm.files[deployrecord.Path(dep)], "home-b") {
		t.Error("vm's record still lists home-b")
	}
}

// Cleaning a gateway's host deletes its record with the rest.
func TestCleaningAGatewayDeletesItsRecord(t *testing.T) {
	w := setup(t)
	vm := w.hosts["vm"]
	vm.files[deployrecord.Path(dep)] = recordListing("vm")
	dest, _ := config.ParseDestination("ubuntu@192.0.2.10")
	p, err := siteremove.BuildForced(w.cfg.WithoutSite("vm"), w.secrets, "vm", dest, vm, siteremove.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasStepIn(stageNamed(p, "clean the host"), "vm", "delete", deployrecord.Path(dep)) {
		t.Fatalf("no record deletion:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := vm.files[deployrecord.Path(dep)]; ok {
		t.Error("the record is still on the host")
	}
}
```

- [ ] **Step 3: Run them to verify they fail** — `go test ./internal/siteremove/ -run 'Forgets|DeletesItsRecord'`, FAIL.

- [ ] **Step 4: Implement**

In `buildCluster`, after the `for _, name := range p.end.SiteNames()` loop:

```go
	var forgets []string
	for _, gw := range p.end.GatewaySites() {
		rec, found, err := deployrecord.Read(p.transports[gw], p.dep())
		if err != nil {
			return nil, fmt.Errorf("site remove %s: %w", p.Site, err)
		}
		if found && rec.Lists("sites", p.Site) {
			forgets = append(forgets, gw)
			st.Steps = append(st.Steps, Step{Site: gw, Verb: "forget", Title: "forget " + p.Site + " in the deployment record on " + gw, Text: fmt.Sprintf("%s out of %s, so secrets prune may remove its secrets", p.Site, deployrecord.Path(p.dep()))})
		}
	}
```

and at the end of `st.run`, before `return nil`:

```go
		for _, gw := range forgets {
			p.work("forget " + p.Site + " in the deployment record on " + gw)
			if _, err := deployrecord.Forget(p.transports[gw], p.dep(), deployrecord.Record{Sites: []string{p.Site}}); err != nil {
				return err
			}
		}
```

In `hostPlan`, add `record bool`. In `buildHost`, after the units: `if _, found, err := t.ReadFile(deployrecord.Path(d)); err != nil { return nil, ... } else { hp.record = found }`. In `hostSteps`, after the files: `if hp.record { add("delete", "delete the deployment record", "%s, this deployment's record of what it deployed", deployrecord.Path(d)) }`. In `runHost`, after deleting the files: `if hp.record { p.work("delete the deployment record"); if err := run("deleting the deployment record", deployrecord.RemoveCommand(d)); err != nil { return err } }`.

- [ ] **Step 5: Run tests** — `go test ./... && go vet ./...`, PASS.

- [ ] **Step 6: Commit** — `git commit -m "feat: site remove forgets the site in every gateway's record"`.

---

### Task 6: `site remove --force` and `app remove` forget what they removed

**Files:**
- Modify: `cmd/paisans/record.go` (`forgetInRecords`), `cmd/paisans/siteremove.go`, `cmd/paisans/appremove.go`
- Test: `cmd/paisans/record_test.go`

**Interfaces:**
- Produces: `func forgetInRecords(r ui.Reporter, cfg *config.Config, names deployrecord.Record, what string, execute, sudo bool)`: on every gateway the yaml declares, through `registryHost`. Without execute it lists `forget <what> in the deployment record on <gw>` items for the gateways whose record lists any of `names`; with execute it forgets them. A gateway that cannot be read or written is a warning (`the deployment record on vm still lists <what>`), never an error: the removal itself is done.
- `func appRecordNames(cfg *config.Config, app string, deployed deployrecord.Record) deployrecord.Record`: the app, plus each group `deployed` lists that no Pocket ID app in `cfg` names.

- [ ] **Step 1: Write the failing tests**

```go
func TestForgetInRecordsReachesEveryGatewayAndWarnsForOneDown(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	vm := &recordFake{files: map[string]string{deployrecord.Path(cfg.Deployment()): `{"version":1,"sites":["vm","monitor-a"],"apps":[],"pocket_id_groups":[]}`}}
	saved := registryHost
	registryHost = func(name string, _ config.Site, _ string, _ bool) registry.Runner { return vm }
	t.Cleanup(func() { registryHost = saved })
	rec := &ui.Recorder{}
	forgetInRecords(rec, cfg, deployrecord.Record{Sites: []string{"monitor-a"}}, "monitor-a", true, true)
	if strings.Contains(vm.files[deployrecord.Path(cfg.Deployment())], "monitor-a") {
		t.Error("vm still lists monitor-a")
	}
	vm.down = true
	rec = &ui.Recorder{}
	forgetInRecords(rec, cfg, deployrecord.Record{Sites: []string{"monitor-b"}}, "monitor-b", true, true)
	if !strings.Contains(rec.Lines(), "still lists monitor-b") {
		t.Errorf("no warning:\n%s", rec.Lines())
	}
}

func TestForgetAppCleansTheRecordWhenNothingElseIsLeft(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	deployed := deployrecord.Record{Apps: []string{"talk", "uptime"}, PocketIDGroups: []string{"members", "retired"}}
	names := appRecordNames(cfg, "uptime", deployed)
	if strings.Join(names.Apps, ",") != "uptime" || strings.Join(names.PocketIDGroups, ",") != "retired" {
		t.Errorf("%+v", names)
	}
}
```

`recordFake` gains `down bool`: `ReadFile` and `Run` return an ssh timeout error when set.

- [ ] **Step 2: Run them to verify they fail** — FAIL, undefined.

- [ ] **Step 3: Implement**

```go
// forgetInRecords takes names out of the deployment record on every gateway
// paisans.yaml declares. The removal they follow is done whether or not this
// reaches every gateway, so a gateway it cannot reach is a warning: its
// record keeps the secrets, which a later run of the same removal frees.
func forgetInRecords(r ui.Reporter, cfg *config.Config, names deployrecord.Record, what string, execute, sudo bool) {
	d := cfg.Deployment()
	for _, gw := range cfg.GatewaySites() {
		t := registryHost(gw, cfg.Sites[gw], "", sudo)
		rec, found, err := deployrecord.Read(t, d)
		if err != nil {
			r.Warn("the deployment record on "+gw+" still lists "+what, err.Error())
			continue
		}
		if !found || !listsAny(rec, names) {
			continue
		}
		title := "forget " + what + " in the deployment record on " + gw
		if !execute {
			r.Item(title)
			continue
		}
		s := r.Step(title)
		if _, err := deployrecord.Forget(t, d, names); err != nil {
			s.Fail(err)
			r.Warn("the deployment record on "+gw+" still lists "+what, err.Error()+". Run the same command again to take it out")
			continue
		}
		s.Done("")
	}
}

func listsAny(rec, names deployrecord.Record) bool {
	for _, kind := range []string{"sites", "apps", "pocket_id_groups"} {
		for _, n := range names.List(kind) {
			if rec.Lists(kind, n) {
				return true
			}
		}
	}
	return false
}

// appRecordNames is what app remove takes out of a record: the app, and each
// group the record lists that no Pocket ID app paisans.yaml declares names.
func appRecordNames(cfg *config.Config, app string, deployed deployrecord.Record) deployrecord.Record {
	declared := deployrecord.FromConfig(cfg)
	names := deployrecord.Record{Apps: []string{app}}
	for _, g := range deployed.PocketIDGroups {
		if !declared.Lists("pocket_id_groups", g) {
			names.PocketIDGroups = append(names.PocketIDGroups, g)
		}
	}
	return names
}
```

This needs `Record.List(kind) []string` exported in `internal/deployrecord` (rename `list`).

In `runSiteRemoveForced`, when the site is not declared: call `forgetInRecords(r, cfg, deployrecord.Record{Sites: []string{site}}, site, a.execute, a.sudo)` before `forcedNothingToDo` (so an empty host still clears the record), after `Execute` succeeds on the execute path, and with `execute=false` in the dry run. Concretely: dry run (`!a.execute`) calls it once after `plan.Show`; execute calls it once after `Execute` or, for an empty plan, before returning from `forcedNothingToDo`.

In `runAppRemove`: read the union with `deploymentRecord(cfg, ...)` (ignore `missing`; `forgetInRecords` warns per gateway), compute `names := appRecordNames(cfg, app, rec)`, and call `forgetInRecords(r, cfg, names, app, *execute, *sudo)` in three places: the `plan.Empty()` branch (before its `Result`), the dry-run branch, and after `runner.Execute` succeeds.

- [ ] **Step 4: Run tests** — `go test ./... && go vet ./...`, PASS.

- [ ] **Step 5: Commit** — `git commit -m "feat: site remove --force and app remove forget what they removed"`.

---

### Task 7: README

**Files:** `README.md`: the secrets prune paragraph (*Secrets nothing declares are pruned*), the `--force` subsection's lost-deployment list, and the `site remove` stage 2 and stage 3 rows.

- [ ] **Step 1:** Document the record (path, contents, writers, union, warnings) beside `secrets prune`; `--without-record` and its typed `prune`; stage 2 forgetting the site on remaining gateways; stage 3 deleting a gateway's record; the lost-deployment flow step 2 to 3 order (the gateway's first apply writes the record before prune).
- [ ] **Step 2:** `go run ./cmd/paisans secrets prune --help 2>&1 | grep -- -without-record`; PASS.
- [ ] **Step 3:** `git commit -m "docs: the deployment record"`.

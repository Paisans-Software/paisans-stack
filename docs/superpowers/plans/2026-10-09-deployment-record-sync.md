# The deployment record syncs across gateways: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** every change to the deployment record starts from the newest gateway record, raises its revision and is written to every gateway that answers; readers take the newest record; a gateway that missed changes is caught up by the next one.

**Architecture:** `internal/deployrecord` gains `Revision` and `UpdatedAt`, `Newest`, and `Update`, which reads every gateway, applies a change to the newest record and writes the result to each gateway that answered, reporting the ones it missed. The commands' readers switch from the union to `Newest`; their writers (`recordApplied`, `forgetInRecords`, `site remove` stage 2) switch to `Update` over every gateway.

**Tech Stack:** Go; the fakes from the previous plan (`recordFake`, the `world`'s record case, `withRecords`).

**Spec:** `docs/specs/2026-10-09-deployment-record.md` (sections *The record*, *Who writes it*, *Who reads it*, as revised in 5d072cf).

## Global Constraints

- **Ordering is by `revision`**, never by `updated_at`; `updated_at` is RFC 3339 UTC from the writer's clock, for people.
- **Equal revisions that disagree read as their union.**
- **A gateway that does not answer, or whose write is refused, is a warning**, never a failure, for every writer. `secrets prune` still refuses unless every gateway answers.
- **No change, no raise:** when the names are unchanged, the revision stays and only gateways holding a different record are written.
- **No migration code:** a record without `revision` reads as revision 0, which is the field's zero value, not a conversion.
- **Commits** end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. After every task `go test ./...` and `go vet ./...` pass.

## Review Focus

1. **A gateway down during a `site remove`, back later.** Expected: readers ignore its stale listing (the newer record wins), and the next writer replaces it. Pinned in Task 1, `TestUpdateCatchesUpAStaleGateway`, and Task 2, `TestPruneTakesTheNewestRecord`.
2. **Two records at the same revision with different names.** Expected: read as their union; the next change writes it at a higher revision. Pinned in Task 1, `TestEqualRevisionsReadAsTheirUnion`.
3. **Every gateway down during a write.** Expected: warnings only, the command finishes, nothing written. Pinned in Task 1, `TestUpdateWithEveryGatewayDownWritesNothing`.
4. **A malformed record on one gateway.** Expected: `Update` stops with `ErrMalformed` before writing anywhere, since it cannot know what that record held. Pinned in Task 1, `TestUpdateStopsAtAMalformedRecord`.
5. **A no-op change with all gateways current.** Expected: no write at all. Pinned in Task 1, `TestUpdateWithNothingToChangeWritesNothing`.

---

### Task 1: `deployrecord.Update` and `Newest`

**Files:** `internal/deployrecord/record.go`, `internal/deployrecord/record_test.go`

**Interfaces:**
- Produces:
  - `Record.Revision int` (json `revision`), `Record.UpdatedAt string` (json `updated_at`)
  - `func Newest(rs ...Record) Record`
  - `type Result struct { Changed bool; Wrote []string; Missed map[string]error }`
  - `func Update(hosts map[string]registry.Runner, d deployment.Deployment, change func(Record) Record, now time.Time) (Result, error)`
  - `func Adding(names Record) func(Record) Record`, `func Forgetting(names Record) func(Record) Record`
  - `func Gather(hosts map[string]registry.Runner, d deployment.Deployment) (newest Record, found int, missing map[string]error)`, where `missing[gw]` is the read error, or `ErrNoRecord` for a gateway with none
  - `var ErrNoRecord`
  - `Add` and `Forget` stay, as `Update` over one host.

- [ ] **Step 1: tests** (in `record_test.go`, beside the existing ones; the `fakeHost` there gains `down bool`):

```go
func gateways(hs ...*fakeHost) map[string]registry.Runner {
	out := map[string]registry.Runner{}
	for i, h := range hs {
		out[fmt.Sprintf("gw%d", i+1)] = h
	}
	return out
}

var now = time.Date(2026, 10, 9, 18, 40, 0, 0, time.UTC)

func TestUpdateCatchesUpAStaleGateway(t *testing.T) {
	fresh := &fakeHost{files: map[string]string{deployrecord.Path(dep): `{"version":1,"revision":5,"sites":["vm"],"apps":[],"pocket_id_groups":[]}`}}
	stale := &fakeHost{files: map[string]string{deployrecord.Path(dep): `{"version":1,"revision":3,"sites":["vm","monitor-a"],"apps":[],"pocket_id_groups":[]}`}}
	res, err := deployrecord.Update(gateways(fresh, stale), dep, deployrecord.Adding(deployrecord.Record{}), now)
	if err != nil || res.Changed || strings.Join(res.Wrote, ",") != "gw2" {
		t.Fatalf("%+v %v", res, err)
	}
	r, _, _ := deployrecord.Read(stale, dep)
	if r.Revision != 5 || r.Lists("sites", "monitor-a") {
		t.Errorf("stale gateway not caught up: %+v", r)
	}
}

func TestAChangeStartsFromTheNewestAndRaisesTheRevision(t *testing.T) {
	fresh := &fakeHost{files: map[string]string{deployrecord.Path(dep): `{"version":1,"revision":5,"sites":["vm"],"apps":[],"pocket_id_groups":[]}`}}
	stale := &fakeHost{files: map[string]string{deployrecord.Path(dep): `{"version":1,"revision":3,"sites":["vm","monitor-a"],"apps":[],"pocket_id_groups":[]}`}}
	res, err := deployrecord.Update(gateways(fresh, stale), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"home-a"}}), now)
	if err != nil || !res.Changed || len(res.Wrote) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, h := range []*fakeHost{fresh, stale} {
		r, _, _ := deployrecord.Read(h, dep)
		if r.Revision != 6 || r.UpdatedAt != "2026-10-09T18:40:00Z" || strings.Join(r.Sites, ",") != "home-a,vm" {
			t.Errorf("%+v", r)
		}
	}
}

func TestUpdateReportsAGatewayItMissed(t *testing.T) {
	up := &fakeHost{files: map[string]string{}}
	down := &fakeHost{files: map[string]string{}, down: true}
	res, err := deployrecord.Update(gateways(up, down), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"vm"}}), now)
	if err != nil || res.Missed["gw2"] == nil || strings.Join(res.Wrote, ",") != "gw1" {
		t.Errorf("%+v %v", res, err)
	}
}

func TestUpdateWithEveryGatewayDownWritesNothing(t *testing.T) {
	down := &fakeHost{files: map[string]string{}, down: true}
	res, err := deployrecord.Update(gateways(down), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"vm"}}), now)
	if err != nil || len(res.Wrote) != 0 || res.Missed["gw1"] == nil {
		t.Errorf("%+v %v", res, err)
	}
}

func TestUpdateStopsAtAMalformedRecord(t *testing.T) {
	ok := &fakeHost{files: map[string]string{}}
	bad := &fakeHost{files: map[string]string{deployrecord.Path(dep): "{"}}
	_, err := deployrecord.Update(gateways(ok, bad), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"vm"}}), now)
	if !errors.Is(err, deployrecord.ErrMalformed) || len(ok.files) != 0 {
		t.Errorf("err %v, wrote %v", err, ok.files)
	}
}

func TestUpdateWithNothingToChangeWritesNothing(t *testing.T) {
	rec := `{"version":1,"revision":2,"updated_at":"x","sites":["vm"],"apps":[],"pocket_id_groups":[]}` + "\n"
	a := &fakeHost{files: map[string]string{deployrecord.Path(dep): rec}}
	b := &fakeHost{files: map[string]string{deployrecord.Path(dep): rec}}
	res, err := deployrecord.Update(gateways(a, b), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"vm"}}), now)
	if err != nil || res.Changed || len(res.Wrote) != 0 {
		t.Errorf("%+v %v", res, err)
	}
}

func TestEqualRevisionsReadAsTheirUnion(t *testing.T) {
	n := deployrecord.Newest(
		deployrecord.Record{Revision: 4, Sites: []string{"a"}},
		deployrecord.Record{Revision: 4, Sites: []string{"b"}},
		deployrecord.Record{Revision: 3, Sites: []string{"c"}},
	)
	if n.Revision != 4 || strings.Join(n.Sites, ",") != "a,b" {
		t.Errorf("%+v", n)
	}
}
```

- [ ] **Step 2:** run, expect FAIL to compile.

- [ ] **Step 3: implement.** `normal` carries `Revision` and `UpdatedAt` through. `Newest` picks the highest revision and unions the records at it, keeping the revision and the latest `UpdatedAt` string. `Gather` reads each host through `read` (sorted by gateway name), returns the newest of those found, how many were found, and `missing`. `Update`:

```go
func Update(hosts map[string]registry.Runner, d deployment.Deployment, change func(Record) Record, now time.Time) (Result, error) {
	res := Result{Missed: map[string]error{}}
	type seen struct {
		rec   Record
		raw   string
		found bool
	}
	read := map[string]seen{}
	var found []Record
	for _, gw := range slices.Sorted(maps.Keys(hosts)) {
		rec, raw, ok, err := readOne(hosts[gw], d)
		switch {
		case errors.Is(err, ErrMalformed):
			return res, err
		case err != nil:
			res.Missed[gw] = err
			continue
		}
		read[gw] = seen{rec, raw, ok}
		if ok {
			found = append(found, rec)
		}
	}
	base := Newest(found...)
	target := normal(change(base))
	if sameNames(target, base) {
		target = base
	} else {
		target.Revision = base.Revision + 1
		target.UpdatedAt = now.UTC().Format(time.RFC3339)
		res.Changed = true
	}
	want := encode(target)
	for _, gw := range slices.Sorted(maps.Keys(read)) {
		s := read[gw]
		if s.found && s.raw == want {
			continue
		}
		if err := write(hosts[gw], d, s.raw, s.found, target); err != nil {
			res.Missed[gw] = err
			continue
		}
		res.Wrote = append(res.Wrote, gw)
	}
	return res, nil
}
```

`readOne` is today's `read`. `sameNames` compares the three lists. Compare `s.raw == want` by encoding; a record written by this code encodes byte for byte the same. `Add`/`Forget` become `Update(map[string]registry.Runner{"": t}, d, Adding(names)/Forgetting(names), time.Now())` returning `res.Changed` and the first `Missed` error. `fakeHost.down` makes `ReadFile` and `Run` fail.

- [ ] **Step 4:** `go test ./internal/deployrecord/ && go vet ./...`, PASS. **Step 5:** commit `feat: the deployment record syncs across gateways by revision`.

---

### Task 2: readers take the newest record

**Files:** `cmd/paisans/record.go` (`deploymentRecord` built on `deployrecord.Gather`), `cmd/paisans/secrets_test.go`

- [ ] **Step 1: test**

```go
// A gateway that missed a removal still lists the site; the newer record on
// the other gateway does not, and it decides.
func TestPruneTakesTheNewestRecord(t *testing.T) {
	configPath, secretsPath := writeFixtureSecrets(t, func(s *config.Secrets) { s.Sites["monitor-a"] = config.SiteSecrets{WireGuardPrivateKey: "x"} })
	withTwoGateways(t, configPath)
	withRecords(t, map[string]string{
		"vm":  `{"version":1,"revision":9,"sites":["home-a","home-b","vm","vm2","watch"],"apps":[],"pocket_id_groups":[]}`,
		"vm2": `{"version":1,"revision":8,"sites":["home-a","home-b","vm","vm2","watch","monitor-a"],"apps":[],"pocket_id_groups":[]}`,
	})
	captureStdout(t, func() {
		if err := runSecretsPrune([]string{"--config", configPath, "--secrets", secretsPath, "--execute"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	})
	if s, _ := config.LoadSecrets(secretsPath); s.Sites["monitor-a"].WireGuardPrivateKey != "" {
		t.Error("the stale gateway's listing kept monitor-a")
	}
}
```

`withTwoGateways` appends a second gateway site `vm2` to the copied `paisans.yaml` (roles `[gateway]`, an address in the fixture's mesh, an ssh section with a documentation address) so that `cfg.GatewaySites()` is `vm, vm2`. If validation refuses two gateways in the fixture, give `vm2` what it asks for; the test only needs `GatewaySites` to name both.

- [ ] **Step 2-4:** FAIL, then `deploymentRecord` returns `deployrecord.Gather(...)`'s newest and missing (`errNoRecord` becomes `deployrecord.ErrNoRecord`), PASS. `TestOrphansKeepWhatAnyRecordLists` in secretsgen stays (it tests `Orphans` on a given record). **Step 5:** commit `feat: readers take the newest deployment record`.

---

### Task 3: writers reach every gateway

**Files:** `cmd/paisans/record.go` (`recordApplied`, `forgetInRecords`, `reportMissed`), `cmd/paisans/main.go` (`runApply` passes every gateway), `internal/siteremove/cluster.go` (stage 2 uses `Update`), tests in `cmd/paisans/record_test.go` and `internal/siteremove/record_test.go`

**Interfaces:**
- `func recordApplied(r ui.Reporter, cfg *config.Config, site string, hosts func(string) registry.Runner) error`
- `func reportMissed(r ui.Reporter, res deployrecord.Result, what string)`: one `r.Warn("<gw> missed this change to the deployment record", "<err>. It is brought up to date the next time a command that writes the record reaches it")` per missed gateway.

- [ ] **Step 1: tests**

```go
// apply of a gateway writes every gateway it reaches, and reports one down.
func TestApplyRecordsOnEveryGatewayAndReportsOneDown(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	vm := &recordFake{files: map[string]string{}}
	down := &recordFake{files: map[string]string{}, down: true}
	rec := &ui.Recorder{}
	hosts := func(gw string) registry.Runner { return map[string]registry.Runner{"vm": vm, "vm2": down}[gw] }
	if err := recordApplied(rec, withGateway(cfg, "vm2"), "vm", hosts); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := deployrecord.Read(vm, cfg.Deployment()); !found {
		t.Error("vm was not written")
	}
	if !strings.Contains(rec.Lines(), "vm2 missed this change") {
		t.Errorf("no warning:\n%s", rec.Lines())
	}
}
```

`withGateway(cfg, name)` returns a copy of `cfg` with a site `name` holding `[gateway]` (a test helper in `record_test.go`). Update `TestApplyRecordsTheDeploymentOnAGateway` and `TestApplyNamesTheWayOutOfAMalformedRecord` to the new signature.

In `internal/siteremove/record_test.go`:

```go
// A gateway that missed the removal is written with the newest record when
// the removal runs, so it no longer lists the site.
func TestSiteRemoveWritesTheNewestRecordToEveryGateway(t *testing.T) {
	w := setup(t)
	vm := w.hosts["vm"]
	vm.files[deployrecord.Path(dep)] = `{"version":1,"revision":2,"sites":["home-a","home-b","vm","watch"],"apps":[],"pocket_id_groups":[]}` + "\n"
	p := w.mustBuild("home-b", siteremove.Options{})
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	r, _, _ := deployrecord.Read(vm, dep)
	if r.Revision != 3 || r.Lists("sites", "home-b") {
		t.Errorf("%+v", r)
	}
}
```

- [ ] **Step 2:** FAIL. **Step 3: implement.**
  - `recordApplied` runs only for a gateway `site`; it builds `map[gw]Runner` from `cfg.GatewaySites()` through `hosts`, calls `deployrecord.Update(..., deployrecord.Adding(deployrecord.FromConfig(cfg)), time.Now())`, keeps the `ErrMalformed` message, and `reportMissed`s. A missed gateway is not an error.
  - `runApply` passes `func(gw string) registry.Runner { if gw == *site { return transport }; return registryHost(gw, cfg.Sites[gw], "", *sudo) }`.
  - `forgetInRecords`: `Gather` for the dry run (an item when the newest lists any of `names`, returning `left`); with execute, one step `forget <what> in the deployment record` calling `Update(..., Forgetting(names), time.Now())` and `reportMissed`.
  - Stage 2: plan one step `forget <site> in the deployment record` (site `""`, shown under its own title) when `Gather` over `p.end.GatewaySites()` finds the site in the newest record or any gateway out of date; run `Update(..., Forgetting({Sites: [site]}), now())`, where `now` is a package variable defaulting to `time.Now` (tests may leave it). Missed gateways become `p.Notes` lines through a `recordMissed(gw, why)` in `remains.go`, added to `WorstCaseRemains`.

- [ ] **Step 4:** `go test ./... && go vet ./...`, PASS. **Step 5:** commit `feat: changes to the deployment record reach every gateway`.

---

### Task 4: README

- [ ] In *What is deployed is recorded on each gateway*: the revision and `updated_at`, the newest record deciding, equal revisions read as their union, every change written to every gateway that answers, a missed gateway warned about and caught up by the next writer. `secrets prune` unchanged. Commit `docs: the deployment record syncs across gateways`.

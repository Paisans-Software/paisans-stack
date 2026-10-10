# The deployment record tags every add: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** a name is deployed while it has an add no removal saw; `apply` re-adds every declared name with a new tag; a removal records the add tags it saw; a write made while every gateway answered compacts the record.

**Architecture:** inside `internal/deployrecord`, `entry` becomes `{adds, removed []string}` of tags and the record loses `revision`. `merge` unions the tag sets per name; `Change.apply` adds a fresh tag per added name (always) and moves every add tag of a forgotten deployed name into `removed`; `compact` keeps one live tag per deployed name and drops the rest, run only when no gateway was missed while reading. `Union`, `Add`, `Forget` and `Read` become unexported, and the tests that used them read through `Gather`. A write refused because another command changed the record wraps a new `ErrChanged`, which `reportMissed` words as "run the command again".

**Tech Stack:** Go.

**Spec:** `docs/specs/2026-10-09-deployment-record.md`, as revised in 035114f.

## Global Constraints

- **Deployed:** some tag in `adds` is not in `removed`.
- **Tags:** 16 hex characters from `crypto/rand`, through a package variable `newTag` tests can replace.
- **`Adding` always makes a new tag** for every name it names, deployed or not.
- **`Forgetting`** moves every add tag of a deployed name into `removed`; a name not deployed is left alone.
- **Compaction** happens only when every host in the `Update` call was read without error; it keeps the smallest live tag of each deployed name, and drops names not deployed.
- **No revision field.** `updated_at` stays, for people.
- **No migration code.** The added/removed layout is unreleased and reads as malformed.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`; `go test ./...` and `go vet ./...` pass after each task.

## Review Focus

1. **Re-add on a gateway that missed the removal.** A and B list `x`; B down, `x` removed on A; A down, `x` re-added on B; both up. Expected: `x` deployed. Pinned in Task 1, `TestAReAddOnAGatewayThatMissedTheRemovalStays`.
2. **The split, produced through `Update`:** A and B; A down, `y` added on B; B down, `m1` removed on A; both up. Expected: `y` deployed, `m1` not, and after the next write both gateways hold the same compacted record. Pinned in Task 1, `TestASplitThroughUpdateKeepsEveryAddAndEveryRemoval`.
3. **A lone gateway's adds.** Expected: reach the other gateway at the next change. Pinned in Task 1, `TestALoneGatewaysAddsReachTheOthers`.
4. **No compaction with a gateway down.** Expected: removed tags kept, so the down gateway's stale add stays cancelled when it returns. Pinned in Task 1, `TestNoCompactionWhileAGatewayIsDown`.
5. **A refused write.** Expected: `ErrChanged`, and the warning says to run the command again. Pinned in Task 2, `TestAChangedRecordSaysRunAgain`.

---

### Task 1: tagged entries in `internal/deployrecord`

**Files:** `internal/deployrecord/record.go`, `internal/deployrecord/record_test.go`, `internal/secretsgen/orphans_test.go` (stop using `deployrecord.Union`)

- [ ] **Step 1: tests.** Replace the stored-layout helper `stored` and the tests built on it with tests that produce every state through `Update`, toggling `fakeHost.down`:

```go
func view(t *testing.T, hs ...*fakeHost) deployrecord.Record {
	t.Helper()
	r, _, missing := deployrecord.Gather(gateways(hs...), dep)
	for gw, err := range missing {
		if !errors.Is(err, deployrecord.ErrNoRecord) {
			t.Fatalf("%s: %v", gw, err)
		}
	}
	return r
}

func change(t *testing.T, ch deployrecord.Change, hs ...*fakeHost) deployrecord.Result {
	t.Helper()
	res, err := deployrecord.Update(gateways(hs...), dep, ch, now)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func sites(names ...string) deployrecord.Record { return deployrecord.Record{Sites: names} }

func TestAReAddOnAGatewayThatMissedTheRemovalStays(t *testing.T) {
	a, b := &fakeHost{files: map[string]string{}}, &fakeHost{files: map[string]string{}}
	change(t, deployrecord.Adding(sites("vm", "x")), a, b)
	b.down = true
	change(t, deployrecord.Forgetting(sites("x")), a, b)
	a.down, b.down = true, false
	change(t, deployrecord.Adding(sites("vm", "x")), a, b)
	a.down = false
	if r := view(t, a, b); !r.Lists("sites", "x") {
		t.Errorf("x lost: %v", r.Sites)
	}
}

func TestASplitThroughUpdateKeepsEveryAddAndEveryRemoval(t *testing.T) {
	a, b := &fakeHost{files: map[string]string{}}, &fakeHost{files: map[string]string{}}
	change(t, deployrecord.Adding(sites("vm", "m1")), a, b)
	a.down = true
	change(t, deployrecord.Adding(sites("y")), a, b)
	a.down, b.down = false, true
	change(t, deployrecord.Forgetting(sites("m1")), a, b)
	b.down = false
	if r := view(t, a, b); strings.Join(r.Sites, ",") != "vm,y" {
		t.Fatalf("merged %v", r.Sites)
	}
	change(t, deployrecord.Forgetting(sites()), a, b)
	if a.files[deployrecord.Path(dep)] != b.files[deployrecord.Path(dep)] {
		t.Error("the gateways differ after a write both answered")
	}
	if strings.Contains(a.files[deployrecord.Path(dep)], `"m1"`) {
		t.Error("not compacted: m1 is still in the file")
	}
}

func TestALoneGatewaysAddsReachTheOthers(t *testing.T) {
	a, c := &fakeHost{files: map[string]string{}}, &fakeHost{files: map[string]string{}}
	change(t, deployrecord.Adding(sites("vm")), a)
	a.down = true
	change(t, deployrecord.Adding(sites("vm2")), a, c)
	a.down = false
	change(t, deployrecord.Forgetting(sites()), a, c)
	if r := view(t, a); strings.Join(r.Sites, ",") != "vm,vm2" {
		t.Errorf("a holds %v", r.Sites)
	}
}

func TestNoCompactionWhileAGatewayIsDown(t *testing.T) {
	a, b := &fakeHost{files: map[string]string{}}, &fakeHost{files: map[string]string{}}
	change(t, deployrecord.Adding(sites("vm", "x")), a, b)
	b.down = true
	change(t, deployrecord.Forgetting(sites("x")), a, b)
	if !strings.Contains(a.files[deployrecord.Path(dep)], `"x"`) {
		t.Fatal("x's removal was compacted away while b was down")
	}
	b.down = false
	if r := view(t, a, b); r.Lists("sites", "x") {
		t.Error("b's stale add brought x back")
	}
}
```

Rewrite the other tests to the new API: `Add(h, ...)` becomes `change(t, deployrecord.Adding(...), h)` and `Read(h, dep)` becomes `view(t, h)`; `TestTheListLayoutIsMalformed` gains a case for the added/removed layout; `TestEqualRevisions…`, `TestATieKeepsTheName` and `TestANameAddedAgainAfterItsRemoval` go (ties and revisions are gone; re-adding is Focus 1). In `secretsgen`'s `TestOrphansKeepWhatAnyRecordLists`, build the record directly instead of with `deployrecord.Union`.

- [ ] **Step 2:** FAIL. **Step 3:** implement per the Global Constraints. `Encode(r)` gives each name one tag, `"t-" + name`. `Result.Changed` is whether the deployed names changed. **Step 4:** `go test ./internal/... && go vet ./...`, PASS (the command tests may still fail until Task 2). **Step 5:** commit with Task 2 if needed.

### Task 2: callers, wording, README

**Files:** `cmd/paisans/record.go`, `cmd/paisans/*_test.go`, `internal/siteremove/{cluster.go,record_test.go}`, `README.md`

- [ ] `reportMissed`: an `ErrChanged` miss says `<gw>'s deployment record was changed by another command meanwhile; run this command again`; any other miss keeps the catch-up wording. Test:

```go
func TestAChangedRecordSaysRunAgain(t *testing.T) {
	rec := &ui.Recorder{}
	reportMissed(rec, deployrecord.Result{Missed: map[string]error{"vm": fmt.Errorf("ubuntu@vm.example.org: %w", deployrecord.ErrChanged)}})
	if !strings.Contains(rec.Lines(), "run this command again") || strings.Contains(rec.Lines(), "brought up to date") {
		t.Errorf("%s", rec.Lines())
	}
}
```

- [ ] `recordResult`: `Changed` → `updated`; written without a change → `written to <gws>`; nothing written → `up to date`.
- [ ] Rename leftovers: `newest` variables to `deployed`/`merged`; comments that say "newest"; tests `TestPruneTakesTheNewestRecord` → `TestPruneMergesTheGatewaysRecords`, `TestAChangeStartsFromTheNewestAndRaisesTheRevision` (goes with Task 1), `TestSiteRemoveWritesTheNewestRecordToEveryGateway` → `TestSiteRemoveTakesTheSiteOutOnEveryGateway`. `TestPruneMergesTheGatewaysRecords` produces its two records through `deployrecord.Update` on two `recordFake`s instead of literals.
- [ ] Replace `deployrecord.Read` in command and siteremove tests with a `Gather` over the one host.
- [ ] README: the record's paragraph, tags instead of revisions, compaction, the run-again warning.
- [ ] `go test ./... && go vet ./...`, PASS; commit.

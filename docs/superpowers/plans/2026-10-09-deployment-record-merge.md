# The deployment record merges name by name: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** the stored record keeps, per name, the revision of its last add and last removal; readers and writers merge the gateways' records name by name, so no add one gateway alone saw and no removal one gateway alone saw is lost.

**Architecture:** inside `internal/deployrecord`, a stored `doc` (maps of name to `{added, removed}`) replaces the stored lists. `Record`, the lists of deployed names with `Revision`, stays the type every caller reads, so `secretsgen` and the commands do not change. `Update` takes a `Change` (`Adding`/`Forgetting` keep their names and argument) and applies it to the merge. Test fixtures that wrote the list layout write the new one through an exported `Encode`.

**Tech Stack:** Go.

**Spec:** `docs/specs/2026-10-09-deployment-record.md`, as revised in e702edd.

## Global Constraints

- **Deployed** means `added > 0 && added >= removed`; a tie keeps the name.
- **Merge** is per name the maximum `added` and the maximum `removed`; `revision` and `updated_at` are the maxima.
- **A change** takes `merged.revision + 1` for every event it makes, and makes none for a name already in the state it asks for; no event, no new revision.
- **Writes** go to every gateway that answered whose parsed record encodes differently from the merge (that is the catch-up); `Missed` as before; a malformed record stops everything before a write.
- **No migration code:** the layout is unreleased; a list-layout file is malformed.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`; `go test ./...` and `go vet ./...` pass after each task.

## Review Focus

1. **The reviewer's split:** A and B at r1; B alone adds Y (r2); A alone removes m1, m2 (r3, r4). Expected: the merge lists Y and neither monitor, and a catch-up writes that to both. Pinned in Task 1, `TestASplitKeepsEveryAddAndEveryRemoval`.
2. **A new gateway applied while the others were down.** Expected: its adds reach the others at the next change. Pinned in Task 1, `TestAGatewayAppliedAloneIsNotIgnored`.
3. **An add and a removal of one name at the same revision** (two writers at once). Expected: deployed. Pinned in Task 1, `TestATieKeepsTheName`.
4. **A name removed, then added again.** Expected: deployed, and a later removal takes it out. Pinned in Task 1, `TestANameAddedAgainAfterItsRemoval`.
5. **A list-layout file.** Expected: `ErrMalformed`. Pinned in Task 1, `TestTheListLayoutIsMalformed`.

---

### Task 1: per-name records in `internal/deployrecord`

**Files:** `internal/deployrecord/record.go`, `internal/deployrecord/record_test.go`

**Interfaces:**
- Produces:
  - `type Change struct` (unexported fields); `func Adding(names Record) Change`; `func Forgetting(names Record) Change`
  - `func Update(hosts map[string]registry.Runner, d deployment.Deployment, ch Change, now time.Time) (Result, error)`
  - `func Encode(r Record) string`: a stored record in which every name of `r` was added at `r.Revision` (1 when 0), for tests and nothing else that writes.
  - `Read`, `Gather`, `Add`, `Forget`, `FromConfig`, `Lists`, `List`, `Union`, `Path`, `RemoveCommand`, `ErrMalformed`, `ErrNoRecord` keep their signatures; `Newest` is removed.

- [ ] **Step 1: tests.** Rewrite the fixtures in the existing tests that wrote the list layout to `deployrecord.Encode(deployrecord.Record{Revision: n, Sites: ...})`, or to the stored layout where a removal is needed, and replace `TestEqualRevisionsReadAsTheirUnion` with:

```go
func stored(revision int, sites map[string][2]int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"version":1,"revision":%d,"updated_at":"","sites":{`, revision)
	first := true
	for _, n := range slices.Sorted(maps.Keys(sites)) {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, `%q:{"added":%d,"removed":%d}`, n, sites[n][0], sites[n][1])
	}
	b.WriteString(`},"apps":{},"pocket_id_groups":{}}` + "\n")
	return b.String()
}

func TestASplitKeepsEveryAddAndEveryRemoval(t *testing.T) {
	a := &fakeHost{files: map[string]string{deployrecord.Path(dep): stored(4, map[string][2]int{"vm": {1, 0}, "m1": {1, 3}, "m2": {1, 4}})}}
	b := &fakeHost{files: map[string]string{deployrecord.Path(dep): stored(2, map[string][2]int{"vm": {1, 0}, "m1": {1, 0}, "m2": {1, 0}, "y": {2, 0}})}}
	r, _, _ := deployrecord.Gather(gateways(a, b), dep)
	if strings.Join(r.Sites, ",") != "vm,y" {
		t.Fatalf("merged %v", r.Sites)
	}
	if _, err := deployrecord.Update(gateways(a, b), dep, deployrecord.Adding(deployrecord.Record{}), now); err != nil {
		t.Fatal(err)
	}
	for _, h := range []*fakeHost{a, b} {
		got, _, _ := deployrecord.Read(h, dep)
		if strings.Join(got.Sites, ",") != "vm,y" || got.Revision != 4 {
			t.Errorf("caught up to %+v", got)
		}
	}
}

func TestAGatewayAppliedAloneIsNotIgnored(t *testing.T) {
	old := &fakeHost{files: map[string]string{deployrecord.Path(dep): stored(9, map[string][2]int{"vm": {1, 0}})}}
	fresh := &fakeHost{files: map[string]string{deployrecord.Path(dep): stored(1, map[string][2]int{"vm2": {1, 0}})}}
	r, _, _ := deployrecord.Gather(gateways(old, fresh), dep)
	if strings.Join(r.Sites, ",") != "vm,vm2" {
		t.Errorf("merged %v", r.Sites)
	}
}

func TestATieKeepsTheName(t *testing.T) {
	a := &fakeHost{files: map[string]string{deployrecord.Path(dep): stored(5, map[string][2]int{"x": {1, 5}})}}
	b := &fakeHost{files: map[string]string{deployrecord.Path(dep): stored(5, map[string][2]int{"x": {5, 0}})}}
	if r, _, _ := deployrecord.Gather(gateways(a, b), dep); !r.Lists("sites", "x") {
		t.Error("a tie dropped the name")
	}
}

func TestANameAddedAgainAfterItsRemoval(t *testing.T) {
	h := &fakeHost{files: map[string]string{deployrecord.Path(dep): stored(3, map[string][2]int{"x": {1, 3}})}}
	if _, err := deployrecord.Update(gateways(h), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"x"}}), now); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := deployrecord.Read(h, dep); !r.Lists("sites", "x") || r.Revision != 4 {
		t.Fatalf("%+v", r)
	}
	if _, err := deployrecord.Update(gateways(h), dep, deployrecord.Forgetting(deployrecord.Record{Sites: []string{"x"}}), now); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := deployrecord.Read(h, dep); r.Lists("sites", "x") || r.Revision != 5 {
		t.Errorf("%+v", r)
	}
}

func TestTheListLayoutIsMalformed(t *testing.T) {
	h := &fakeHost{files: map[string]string{deployrecord.Path(dep): `{"version":1,"sites":["vm"],"apps":[],"pocket_id_groups":[]}`}}
	if _, _, err := deployrecord.Read(h, dep); !errors.Is(err, deployrecord.ErrMalformed) {
		t.Errorf("err = %v", err)
	}
}
```

- [ ] **Step 2:** run, FAIL.
- [ ] **Step 3: implement.**

```go
type entry struct {
	Added   int `json:"added,omitempty"`
	Removed int `json:"removed,omitempty"`
}

func (e entry) deployed() bool { return e.Added > 0 && e.Added >= e.Removed }

// doc is the record as stored: every name with its last add and removal.
type doc struct {
	Version        int              `json:"version"`
	Revision       int              `json:"revision"`
	UpdatedAt      string           `json:"updated_at"`
	Sites          map[string]entry `json:"sites"`
	Apps           map[string]entry `json:"apps"`
	PocketIDGroups map[string]entry `json:"pocket_id_groups"`
}
```

`doc.kind(kind) map[string]entry`; `view(doc) Record` lists the deployed names; `merge(docs ...doc) doc`; `(m *doc) apply(ch Change, rev int) bool` makes the events; `encodeDoc` marshals with empty maps for absent kinds (Go sorts map keys, so equal docs encode the same); `parseDoc` requires `Version == 1` and the map layout (`json.Unmarshal` of a list into a map is an error, which is `ErrMalformed`). `read` returns the doc; `Gather` returns `view(merge(found...))`; `Update` merges, applies at `merged.Revision+1` and stamps `updated_at` when anything changed, and writes `encodeDoc(merged)` to each answering gateway whose parsed doc encodes differently, skipping all writes when nothing was found and nothing changed. `Read` returns `view(doc)`. `Encode(r)` builds a doc with each name `{added: rev}`.

- [ ] **Step 4:** `go test ./internal/deployrecord/`, PASS; then `go test ./...`: the command and siteremove tests that wrote list-layout fixtures fail as malformed, which Task 2 fixes. Commit with Task 2 if the tree does not pass alone.

### Task 2: fixtures, and the README

**Files:** `cmd/paisans/{secrets,init,record,siteremove}_test.go`, `internal/siteremove/record_test.go`, `README.md`

- [ ] Replace every list-layout record literal with `deployrecord.Encode(deployrecord.Record{...})` (`fixtureRecord()` returns `deployrecord.Encode(deployrecord.FromConfig(cfg))`; `recordListing` in siteremove returns `deployrecord.Encode(deployrecord.Record{Sites: sites})`; `TestPruneTakesTheNewestRecord` writes vm at revision 9 without `monitor-a` but with a removal of it, `{"monitor-a": {added 1, removed 9}}`, since a record that merely lacks a name no longer outvotes one that lists it, and vm2 at revision 8 listing it).
- [ ] README: the record keeps each name's last add and removal; readers and writers merge name by name; a removed name stays in the file; a tie keeps the name.
- [ ] `go test ./... && go vet ./...`, PASS; commit `feat: the deployment record merges name by name`.

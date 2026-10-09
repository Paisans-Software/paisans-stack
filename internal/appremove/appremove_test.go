package appremove

import (
	"github.com/paisans-software/paisans-stack/internal/ui"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
)

const (
	docsEnv     = "/srv/paisans/f2a9/docs/.env"
	docsCompose = "/srv/paisans/f2a9/docs/compose.yaml"
	docsUpload  = "/srv/paisans/f2a9/docs/data/upload-1"
	talkEnv     = "/srv/paisans/f2a9/talk/.env"
	docsRoute   = "/srv/paisans/f2a9/infra/caddy/snippets/docs.caddy"
	docsMedia   = "/srv/paisans/f2a9/infra/caddy/snippets/docs-media.caddy"
	talkRoute   = "/srv/paisans/f2a9/infra/caddy/snippets/talk.caddy"
)

// world is docs taken out of the fixture's configuration and applied
// everywhere since: its stack on home-a with an upload it wrote, its routes
// on the gateway, vm, and talk, still declared, beside both.
func world(t *testing.T) (*config.Config, map[string]*host) {
	t.Helper()
	cfg := fixture(t)
	delete(cfg.Apps, "docs")
	a, vm := newHost("home-a"), newHost("vm")
	a.files[docsEnv] = "SECRET_KEY=x\n"
	a.files[docsCompose] = "services: {}\n"
	a.files[docsUpload] = "an upload"
	a.files[talkEnv] = "talk\n"
	a.containers = []hostcheck.Container{
		{Name: "paisans-f2a9-docs-app-1", Deployment: ours, Project: "paisans-f2a9-docs", PID: 10},
		{Name: "paisans-f2a9-docs-redis-1", Deployment: ours, Project: "paisans-f2a9-docs"},
		{Name: "paisans-f2a9-talk-app-1", Deployment: ours, Project: "paisans-f2a9-talk", PID: 11},
		// Somebody else's container that happens to carry the project's
		// name, and another deployment's: neither is docs'.
		{Name: "docs-by-hand", Project: "paisans-f2a9-docs", PID: 12},
		{Name: "paisans-0c1d-docs-app-1", Deployment: theirs, Project: "paisans-f2a9-docs", PID: 13},
	}
	a.networks = []hostcheck.Network{
		{Name: "paisans-f2a9-docs_default", Deployment: ours, Project: "paisans-f2a9-docs"},
		{Name: "docs_default", Project: "paisans-f2a9-docs"},
	}
	a.volumes = []hostcheck.Volume{{Name: "paisans-f2a9-docs_cache", Deployment: ours, Project: "paisans-f2a9-docs"}}
	a.manifest(t, a.entry(docsEnv, true), a.entry(docsCompose, true), a.entry(talkEnv, false))
	vm.files[docsRoute] = "docs route\n"
	vm.files[docsMedia] = "docs media route\n"
	vm.files[talkRoute] = "talk route\n"
	vm.manifest(t, vm.entry(docsRoute, true), vm.entry(docsMedia, true), vm.entry(talkRoute, false))
	return cfg, map[string]*host{"home-a": a, "home-b": newHost("home-b"), "vm": vm, "watch": newHost("watch")}
}

func probe(t *testing.T, cfg *config.Config, app string, hosts map[string]*host, deleteData bool) Input {
	t.Helper()
	in := Input{DeleteData: deleteData}
	for _, site := range cfg.SiteNames() {
		in.Sites = append(in.Sites, state(t, cfg, app, site, hosts[site]))
	}
	return in
}

func executor(hosts map[string]*host) *Executor {
	m := map[string]Host{}
	for k, v := range hosts {
		m[k] = v
	}
	return &Executor{Hosts: m}
}

func TestRefusedBeforeAnyHost(t *testing.T) {
	cfg := fixture(t)
	for app, want := range map[string]string{
		"talk":   "still in paisans.yaml",
		"infra":  "infrastructure stack",
		"../etc": "not an app's name",
		"":       "not an app's name",
		"Docs":   "not an app's name",
	} {
		err := Refusal(cfg, app)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Refusal(%q) = %v, want %q", app, err, want)
		}
	}
	delete(cfg.Apps, "docs")
	if err := Refusal(cfg, "docs"); err != nil {
		t.Errorf("an app out of the configuration is refused: %v", err)
	}
}

// A route on the gateway still unmarked means no whole apply has run there
// since docs left, so the gateway may still route to it.
func TestAnUnmarkedEntryRefuses(t *testing.T) {
	cfg, hosts := world(t)
	vm := hosts["vm"]
	vm.manifest(t, vm.entry(docsRoute, false), vm.entry(docsMedia, true), vm.entry(talkRoute, false))
	_, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err == nil || !strings.Contains(err.Error(), "vm: "+docsRoute) || !strings.Contains(err.Error(), "paisans apply") {
		t.Fatalf("Build = %v", err)
	}
	if strings.Contains(err.Error(), "talk") {
		t.Errorf("a declared app's file was read as docs': %v", err)
	}
}

func TestNothingFoundAnywhere(t *testing.T) {
	cfg := fixture(t)
	delete(cfg.Apps, "docs")
	hosts := map[string]*host{"home-a": newHost("home-a"), "home-b": newHost("home-b"), "vm": newHost("vm"), "watch": newHost("watch")}
	p, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Errorf("an empty world planned %+v", p)
	}
}

func TestThePlanTakesOnlyWhatIsProvablyTheApps(t *testing.T) {
	cfg, hosts := world(t)
	p, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err != nil {
		t.Fatal(err)
	}
	rec := &ui.Recorder{Verbose_: true}
	p.Show(rec)
	printed := rec.Lines()
	for _, want := range []string{
		"item: remove containers and networks",
		"detail: stop paisans-f2a9-docs: 2 container(s), 1 running: paisans-f2a9-docs-app-1, paisans-f2a9-docs-redis-1",
		"detail: remove network paisans-f2a9-docs_default",
		"item: remove files",
		"detail: " + docsEnv,
		"detail: " + docsRoute,
		"detail: " + docsMedia,
		"item: keep /srv/paisans/f2a9/docs",
		"detail: /srv/paisans/f2a9/docs: data, written by the app",
		"item: keep volumes",
		"detail: volume paisans-f2a9-docs_cache. --delete-data deletes it",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("plan lacks %q:\n%s", want, printed)
		}
	}
	for _, never := range []string{"docs-by-hand", "paisans-0c1d", "network docs_default", "talk"} {
		if strings.Contains(printed, never) {
			t.Errorf("plan names %q:\n%s", never, printed)
		}
	}
}

// The whole removal, then the same command again: the second run changes
// nothing and finds only the data it kept.
func TestRemovalResumesAndRepeats(t *testing.T) {
	cfg, hosts := world(t)
	p, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err != nil {
		t.Fatal(err)
	}
	if err := executor(hosts).Execute(p); err != nil {
		t.Fatal(err)
	}
	a, vm := hosts["home-a"], hosts["vm"]
	for _, gone := range []string{docsEnv, docsCompose} {
		if _, ok := a.files[gone]; ok {
			t.Errorf("%s is still there", gone)
		}
	}
	if _, ok := a.files[docsUpload]; !ok {
		t.Error("the app's upload was deleted without --delete-data")
	}
	if _, ok := vm.files[docsRoute]; ok {
		t.Error("the route is still there")
	}
	if got := a.manifestFiles(t); len(got) != 1 || got[0].Path != strings.TrimPrefix(talkEnv, "/") {
		t.Errorf("home-a's manifest = %+v, want talk's entry alone", got)
	}
	if got := vm.manifestFiles(t); len(got) != 1 || got[0].Path != strings.TrimPrefix(talkRoute, "/") {
		t.Errorf("vm's manifest = %+v, want talk's route alone", got)
	}
	names := map[string]bool{}
	for _, c := range a.containers {
		names[c.Name] = true
	}
	if names["paisans-f2a9-docs-app-1"] || !names["docs-by-hand"] || !names["paisans-0c1d-docs-app-1"] || !names["paisans-f2a9-talk-app-1"] {
		t.Errorf("containers left: %v", names)
	}
	if len(a.volumes) != 1 {
		t.Errorf("a named volume went without --delete-data")
	}

	// Stopped after the containers and before the files: the same command
	// picks up there.
	again, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err != nil {
		t.Fatal(err)
	}
	before := len(a.files) + len(vm.files)
	if err := executor(hosts).Execute(again); err != nil {
		t.Fatal(err)
	}
	if len(a.files)+len(vm.files) != before {
		t.Error("a second run changed files")
	}
	for _, s := range again.Sites {
		if s.Site == "home-a" && (len(s.Files) > 0 || len(s.Containers) > 0 || !s.Dir.Exists) {
			t.Errorf("second plan on home-a: %+v", s)
		}
		if s.Site == "vm" && !s.Empty() {
			t.Errorf("second plan on vm: %+v", s)
		}
	}
}

// An interrupted file step leaves files removed and their entries still
// recorded. The next run drops those entries as gone.
func TestAnEntryWhoseFileIsGoneIsDropped(t *testing.T) {
	cfg, hosts := world(t)
	delete(hosts["vm"].files, docsRoute)
	p, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err != nil {
		t.Fatal(err)
	}
	if err := executor(hosts).Execute(p); err != nil {
		t.Fatal(err)
	}
	if got := hosts["vm"].manifestFiles(t); len(got) != 1 {
		t.Errorf("vm's manifest = %+v", got)
	}
}

func TestAFileEditedOnTheHostIsKept(t *testing.T) {
	cfg, hosts := world(t)
	a := hosts["home-a"]
	a.files[docsEnv] = "SECRET_KEY=edited\n"
	p, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, true))
	if err != nil {
		t.Fatal(err)
	}
	rec := &ui.Recorder{Verbose_: true}
	p.Show(rec)
	if !rec.Has("detail", docsEnv+" and its manifest entry: edited on the host") ||
		!rec.Has("detail", "/srv/paisans/f2a9/docs and everything in it, because a file in it was edited") {
		t.Errorf("plan:\n%s", rec.Lines())
	}
	ex := executor(hosts)
	if err := ex.Execute(p); err != nil {
		t.Fatal(err)
	}
	if a.files[docsEnv] != "SECRET_KEY=edited\n" || a.files[docsUpload] == "" {
		t.Error("the edited file, or the directory holding it, was deleted")
	}
	entries := a.manifestFiles(t)
	if len(entries) != 2 || entries[0].Path != strings.TrimPrefix(docsEnv, "/") {
		t.Errorf("manifest = %+v, want the edited file's entry kept", entries)
	}
	if !strings.Contains(strings.Join(ex.Kept, "\n"), docsEnv+", edited on the host") {
		t.Errorf("kept = %v", ex.Kept)
	}
}

func TestDeleteDataTakesTheDirectoryAndVolumes(t *testing.T) {
	cfg, hosts := world(t)
	p, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := executor(hosts).Execute(p); err != nil {
		t.Fatal(err)
	}
	a := hosts["home-a"]
	if _, ok := a.files[docsUpload]; ok {
		t.Error("the upload survived --delete-data")
	}
	if len(a.volumes) != 0 {
		t.Errorf("volumes left: %+v", a.volumes)
	}
	if _, ok := a.files[talkEnv]; !ok {
		t.Error("talk's file went with docs")
	}
	if !strings.Contains(strings.Join(a.sent, "\n"), "docker rm -v") {
		t.Error("anonymous volumes were not removed with the containers")
	}
}

func TestTheClientIsDeletedOnlyByItsRecordedID(t *testing.T) {
	docs := &pocketid.OIDCClient{ID: "c-1", Name: "docs"}
	other := &pocketid.OIDCClient{ID: "c-2", Name: "docs"}
	renamed := &pocketid.OIDCClient{ID: "c-1", Name: "wiki"}
	for name, tc := range map[string]struct {
		st     ClientState
		delete string
		kept   int
	}{
		"recorded and named":   {ClientState{Provider: "auth", RecordedID: "c-1", ByID: docs, ByName: docs}, "c-1", 0},
		"named, not recorded":  {ClientState{Provider: "auth", ByName: other}, "", 1},
		"recorded, other name": {ClientState{Provider: "auth", RecordedID: "c-1", ByID: renamed}, "", 1},
		"recorded, gone":       {ClientState{Provider: "auth", RecordedID: "c-1", ByName: other}, "", 1},
		"no pocket-id":         {ClientState{RecordedID: "c-1"}, "", 0},
	} {
		cp := planClient("docs", tc.st)
		got := ""
		if cp.Delete != nil {
			got = cp.Delete.ID
		}
		if got != tc.delete || len(cp.Kept) != tc.kept {
			t.Errorf("%s: delete %q kept %v", name, got, cp.Kept)
		}
		for _, k := range cp.Kept {
			if !strings.Contains(k, "Delete it as a Pocket ID administrator") {
				t.Errorf("%s: kept without saying how to delete it: %s", name, k)
			}
		}
	}
}

type fakeClients struct{ deleted []string }

func (f *fakeClients) OIDCClientByID(string) (*pocketid.OIDCClient, error) { return nil, nil }
func (f *fakeClients) FindOIDCClient(string) (*pocketid.OIDCClient, error) { return nil, nil }
func (f *fakeClients) DeleteOIDCClient(id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// --delete-data: the database and role apply made, and the bucket and keys
// storage init made, each proven before it goes.
func TestDeleteDataDropsTheDatabaseAndBucket(t *testing.T) {
	cfg, hosts := world(t)
	a := hosts["home-a"]
	a.db = &database{leader: "home-a", dbs: map[string]string{"docs": "docs", "talk": "talk"}, roles: map[string]bool{"docs": true, "talk": true}}
	a.garage = &garageNode{
		keys: map[string]bool{"GK000000000000000000000001": true, "GK000000000000000000000002": true},
		buckets: map[string]*bucket{
			"docs-uploads": {id: "aaaaaaaaaaaaaaaa1111", keys: []string{"GK000000000000000000000001"}, objects: []string{"a", "b/c", "d", "e"}},
			"shared":       {id: "bbbbbbbbbbbbbbbb2222", keys: []string{"GK000000000000000000000001", "GK000000000000000000000002"}},
		},
	}
	secrets := &config.Secrets{Apps: map[string]map[string]any{
		"docs": {"s3_access_key_id": "GK000000000000000000000001", "s3_secret_access_key": "SECRET-docs"},
		"talk": {"s3_access_key_id": "GK000000000000000000000002", "s3_secret_access_key": "SECRET-talk"},
	}}
	hm := map[string]Host{}
	for k, v := range hosts {
		hm[k] = v
	}
	in := probe(t, cfg, "docs", hosts, true)
	var err error
	if in.Database, err = ProbeDatabase(cfg, "docs", hm); err != nil {
		t.Fatal(err)
	}
	if in.Storage, err = ProbeStorage(cfg, secrets, "docs", a); err != nil {
		t.Fatal(err)
	}
	in.Client = ClientState{Provider: "auth", Site: "home-a", RecordedID: "c-1", ByID: &pocketid.OIDCClient{ID: "c-1", Name: "docs"}}
	p, err := Build(cfg, "docs", in)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Database.DropDatabase || !p.Database.DropRole || len(p.Storage.Buckets) != 1 || p.Storage.Buckets[0].Name != "docs-uploads" {
		t.Fatalf("plan: db %+v storage %+v", p.Database, p.Storage)
	}
	if !strings.Contains(strings.Join(p.Storage.Kept, "\n"), "bucket shared: also granted to key(s) GK000000000000000000000002") {
		t.Errorf("kept = %v", p.Storage.Kept)
	}
	clients := &fakeClients{}
	ex := executor(hosts)
	ex.Clients = clients
	if err := ex.Execute(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.db.dbs["docs"]; ok || a.db.roles["docs"] || a.db.dbs["talk"] == "" {
		t.Errorf("databases %v roles %v", a.db.dbs, a.db.roles)
	}
	if _, ok := a.garage.buckets["docs-uploads"]; ok || a.garage.keys["GK000000000000000000000001"] || a.garage.buckets["shared"] == nil || !a.garage.keys["GK000000000000000000000002"] {
		t.Errorf("garage: buckets %v keys %v", a.garage.buckets, a.garage.keys)
	}
	if len(clients.deleted) != 1 || clients.deleted[0] != "c-1" {
		t.Errorf("deleted clients %v", clients.deleted)
	}
	for _, s := range a.sent {
		if strings.Contains(s, "SECRET") {
			t.Errorf("a secret reached a command line: %s", s)
		}
	}

	// The same again: everything is gone, so nothing is planned.
	in = probe(t, cfg, "docs", hosts, true)
	in.Database, _ = ProbeDatabase(cfg, "docs", hm)
	in.Storage, _ = ProbeStorage(cfg, secrets, "docs", a)
	again, err := Build(cfg, "docs", in)
	if err != nil {
		t.Fatal(err)
	}
	if again.Database.DropDatabase || again.Database.DropRole || len(again.Storage.Buckets) > 0 || len(again.Storage.Keys) > 0 {
		t.Errorf("second plan: db %+v storage %+v", again.Database, again.Storage)
	}
}

// A database of the app's name owned by another role, or one a declared
// app also uses, is not provably the removed app's.
func TestADatabaseIsDroppedOnlyWithItsOwnRole(t *testing.T) {
	cfg := fixture(t)
	delete(cfg.Apps, "docs")
	if dp := planDatabase(cfg, DatabaseState{Leader: "home-a", Name: "docs", Database: true, Owner: "postgres", Role: true}); dp.DropDatabase || dp.DropRole || len(dp.Kept) != 2 {
		t.Errorf("foreign owner: %+v", dp)
	}
	if dp := planDatabase(cfg, DatabaseState{Leader: "home-a", Name: "talk", Database: true, Owner: "talk", Role: true}); dp.DropDatabase || len(dp.Kept) != 1 {
		t.Errorf("a declared app's database: %+v", dp)
	}
	if dp := planDatabase(cfg, DatabaseState{Leader: "home-a", Name: "docs", Role: true}); !dp.DropRole || dp.DropDatabase {
		t.Errorf("a role left alone by a stopped run: %+v", dp)
	}
}

func TestKeyInfoIsReadWithoutItsSecret(t *testing.T) {
	out := "Key name: docs\nKey ID: GK1\nSecret key: abc\nCan create buckets: false\n\nKey-specific bucket aliases:\n\nAuthorized buckets:\n  RWO  docs-uploads    aaaaaaaaaaaaaaaa\n  R                    bbbbbbbbbbbbbbbb\n"
	h := &scripted{out: out}
	kb, err := garage.ReadKeyBuckets(h, fixture(t).Deployment(), "GK1")
	if err != nil || len(kb.Buckets) != 2 || kb.Buckets[1] != "bbbbbbbbbbbbbbbb" {
		t.Errorf("ReadKeyBuckets = %+v, %v", kb, err)
	}
	h.fail = true
	if _, err := garage.ReadKeyBuckets(h, fixture(t).Deployment(), "GK1"); err == nil || strings.Contains(err.Error(), "abc") {
		t.Errorf("a failure: %v", err)
	}
}

type scripted struct {
	out  string
	fail bool
}

func (s *scripted) Run(string) (string, error) {
	if s.fail {
		return s.out, errExit
	}
	return s.out, nil
}
func (s *scripted) Describe() string { return "home-a" }

var errExit = &exitError{}

type exitError struct{}

func (*exitError) Error() string { return "exit status 1" }

// A removal reports a section per site and a step per thing it does there,
// and a failure marks the step that failed.
func TestTheExecutorReportsEachStep(t *testing.T) {
	cfg, hosts := world(t)
	p, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err != nil {
		t.Fatal(err)
	}
	rec := &ui.Recorder{}
	ex := executor(hosts)
	ex.Report = rec
	if err := ex.Execute(p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ kind, text string }{
		{"section", "home-a"},
		{"done", "remove containers and networks"},
		{"done", "remove files"},
	} {
		if !rec.Has(want.kind, want.text) {
			t.Errorf("no %s %q:\n%s", want.kind, want.text, rec.Lines())
		}
	}
	for _, e := range rec.Events {
		if e.Kind == "fail" {
			t.Errorf("a clean removal failed %q", e.Text)
		}
	}
}

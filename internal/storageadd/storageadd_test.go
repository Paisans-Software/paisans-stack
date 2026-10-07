package storageadd_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/storageadd"
)

// growing is the shape storage add was built for, a deployment growing from
// one Garage node: home-a has run Garage alone at replication 1, with every
// app provisioned, and home-b has just been applied at the configuration's
// replication 2, so its node runs alone with an empty layout.
func growing(t *testing.T) *world {
	t.Helper()
	cfg, secrets := fixture(t)
	cfg.Storage.Garage.Consistency = "dangerous"
	w := newWorld(t, cfg, secrets)
	w.provisioned(1, "home-a")
	w.deployGarage("home-b", 2)
	return w
}

func stageNames(p *storageadd.Plan) []string {
	var out []string
	for _, st := range p.Stages {
		out = append(out, st.Name)
	}
	return out
}

func printed(p *storageadd.Plan) string {
	var b bytes.Buffer
	p.Print(&b)
	return b.String()
}

// Garage cannot change the factor in place, and the reset it documents is
// unsupported, so a plan that needs it refuses at the first gate unless the
// operator asked for it, having changed nothing.
func TestAFactorChangeNeedsTheFlag(t *testing.T) {
	w := growing(t)
	p := w.build(storageadd.Options{})
	if got := strings.Join(stageNames(p), ","); got != "nodes,settle,reset,connect,layout,sync,provision,media routes,smoke" {
		t.Fatalf("stages: %s", got)
	}
	err := storageadd.Execute(p)
	if err == nil || !strings.Contains(err.Error(), "--change-replication") {
		t.Fatalf("expected a refusal naming --change-replication, got %v", err)
	}
	if !strings.Contains(err.Error(), "home-a at 1") {
		t.Errorf("the refusal does not say which node is at which factor:\n%v", err)
	}
	for _, h := range w.hosts {
		for _, c := range h.commands {
			if strings.Contains(c, "stop garage") || strings.Contains(c, "mv -n") || strings.Contains(c, "node connect") {
				t.Fatalf("a refused plan ran %q on %s", c, h.name)
			}
		}
	}
}

// The whole join from that shape: reset, connect, one layout version,
// a sync that has to be waited for, then provisioning found present, the
// media routes and the probe. The first run exits at the sync with
// ErrWaiting, and a later run resumes and finishes.
func TestAResetJoinWaitsAndResumes(t *testing.T) {
	w := growing(t)
	// Metadata still moving at the first read; the blocks are covered by
	// TestWaitPollsThroughTheSync.
	w.migrate = 1

	p := w.build(storageadd.Options{ChangeReplication: true})
	out := printed(p)
	for _, want := range []string{"set aside", "cluster_layout.rf1", "garage layout apply --version 1", "record every bucket"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not show %q:\n%s", want, out)
		}
	}
	if !p.Pending() {
		t.Fatal("a plan with a reset to run is not pending")
	}

	err := storageadd.Execute(p)
	if !errors.Is(err, storageadd.ErrWaiting) {
		t.Fatalf("expected the run to stop waiting on the sync, got %v", err)
	}
	if !strings.Contains(err.Error(), "stage 6 (sync)") {
		t.Errorf("the wait does not name the sync stage:\n%v", err)
	}

	a, b := w.hosts["home-a"], w.hosts["home-b"]
	if !a.asides[storageadd.LayoutFile+".rf1"] {
		t.Error("home-a's replication 1 layout was not set aside")
	}
	if a.factor() != 2 || b.factor() != 2 {
		t.Errorf("garage.toml factors after the reset: home-a %d, home-b %d", a.factor(), b.factor())
	}
	if zoneOf(a, a) != "home-a" || zoneOf(a, b) != "home-b" || a.version != 1 {
		t.Errorf("the layout after the reset is version %d with roles %v", a.version, a.roles)
	}
	if _, ok := a.files[storageadd.CountsFile]; !ok {
		t.Error("the object counts were not recorded on the anchor's host before the stop")
	}
	assertStoppedBeforeAnyRewrite(t, w)

	// A later run: every factor matches, nothing is stopped again, and it
	// carries on from the sync.
	p = w.build(storageadd.Options{})
	if got := strings.Join(stageNames(p), ","); strings.Contains(got, "reset") {
		t.Fatalf("a finished reset was planned again: %s", got)
	}
	before := len(a.commands)
	if err := storageadd.Execute(p); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	for _, c := range a.commands[before:] {
		if strings.Contains(c, "stop garage") || strings.Contains(c, "layout apply") {
			t.Errorf("the resumed run repeated %q", c)
		}
	}
	if _, ok := a.files[storageadd.CountsFile]; ok {
		t.Error("the counts file outlived the gate that compares it")
	}
	if len(w.objects) != 0 {
		t.Errorf("the probe was left behind: %v", w.objects)
	}

	// And once more: nothing left to do, every gate still checked.
	p = w.build(storageadd.Options{})
	if p.Pending() {
		t.Errorf("a finished cluster still plans work:\n%s", printed(p))
	}
	if err := storageadd.Execute(p); err != nil {
		t.Fatalf("a run against a finished cluster: %v", err)
	}
}

// A node that meets a peer at a higher factor exits, so no node may start at
// the new factor while another still runs at the old one: every stop comes
// before every garage.toml write and every start.
func assertStoppedBeforeAnyRewrite(t *testing.T, w *world) {
	t.Helper()
	a := w.hosts["home-a"]
	stop, start := -1, -1
	for i, c := range a.commands {
		if strings.Contains(c, "stop garage") && stop < 0 {
			stop = i
		}
		if strings.Contains(c, "up -d garage") && start < 0 {
			start = i
		}
	}
	if stop < 0 || start < 0 || stop > start {
		t.Errorf("home-a was not stopped before it was started again (stop %d, start %d)", stop, start)
	}
}

// --wait polls a waiting gate instead of exiting.
func TestWaitPollsThroughTheSync(t *testing.T) {
	w := growing(t)
	w.migrate, w.resync = 2, 3
	p := w.build(storageadd.Options{ChangeReplication: true, Wait: 30_000_000_000})
	if err := storageadd.Execute(p); err != nil {
		t.Fatalf("a run told to wait should have waited through the sync: %v", err)
	}
}

// A reset interrupted after every garage.toml was rewritten but before every
// node was started leaves no factor that differs. The counts file is what
// says it is unfinished, and the next run starts the nodes and carries on.
func TestAnInterruptedResetResumes(t *testing.T) {
	w := growing(t)
	w.failOnce = "up -d garage"
	p := w.build(storageadd.Options{ChangeReplication: true})
	err := storageadd.Execute(p)
	if err == nil || errors.Is(err, storageadd.ErrWaiting) {
		t.Fatalf("expected the failed start to stop the run, got %v", err)
	}
	if w.hosts["home-a"].running {
		t.Fatal("the test meant home-a's start to fail")
	}

	p = w.build(storageadd.Options{})
	names := strings.Join(stageNames(p), ",")
	if !strings.Contains(names, "reset") {
		t.Fatalf("an unfinished reset was not resumed: %s", names)
	}
	if err := storageadd.Execute(p); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	if !w.hosts["home-a"].running {
		t.Error("home-a was never started again")
	}
}

// The first listed site serves media and takes every app's writes, and a
// node with no role answers every bucket as missing, so a site with no role
// listed ahead of one that has a role is refused.
func TestARoleLessSiteMayNotBeListedFirst(t *testing.T) {
	cfg, secrets := fixture(t)
	cfg.Storage.Garage.Sites = []string{"home-b", "home-a"}
	cfg.Storage.Garage.Replication = 1
	w := newWorld(t, cfg, secrets)
	w.provisioned(1, "home-a")
	w.deployGarage("home-b", 1)

	err := storageadd.Execute(w.build(storageadd.Options{}))
	if err == nil || !strings.Contains(err.Error(), "Move home-b to the end") {
		t.Fatalf("expected the order refusal, got %v", err)
	}
}

// A site that has not been applied has no garage.toml, and storage add does
// not write one: apply does, and starts Garage with it.
func TestASiteWithoutGarageIsSentToApply(t *testing.T) {
	cfg, secrets := fixture(t)
	w := newWorld(t, cfg, secrets)
	w.provisioned(2, "home-a")
	delete(w.hosts["home-b"].files, storageadd.GarageToml)

	err := storageadd.Execute(w.build(storageadd.Options{}))
	if err == nil || !strings.Contains(err.Error(), "paisans apply --site home-b") {
		t.Fatalf("expected to be sent to apply, got %v", err)
	}
}

// A plain join, with no reset: home-b has been applied at the cluster's own
// factor and only needs connecting and a role.
func TestAJoinWithoutAReset(t *testing.T) {
	cfg, secrets := fixture(t)
	cfg.Storage.Garage.Replication = 1
	w := newWorld(t, cfg, secrets)
	w.provisioned(1, "home-a")
	w.deployGarage("home-b", 1)

	p := w.build(storageadd.Options{})
	if got := strings.Join(stageNames(p), ","); strings.Contains(got, "reset") || strings.Contains(got, "settle") {
		t.Fatalf("a join at the same factor planned a reset: %s", got)
	}
	if err := storageadd.Execute(p); err != nil {
		t.Fatal(err)
	}
	a, b := w.hosts["home-a"], w.hosts["home-b"]
	if zoneOf(b, b) != "home-b" || a.version != 2 {
		t.Errorf("home-b has no role in a layout at version 2: version %d, roles %v", a.version, a.roles)
	}
	for _, h := range w.hosts {
		for _, c := range h.commands {
			if strings.Contains(c, "stop garage") {
				t.Errorf("a join at the same factor stopped Garage on %s", h.name)
			}
		}
	}
}

// The probe is written with an app's own key, which travels to curl on
// stdin, and a failure that echoes stdin must not carry it into an error.
func TestAFailingProbeDoesNotPrintTheKey(t *testing.T) {
	cfg, secrets := fixture(t)
	w := newWorld(t, cfg, secrets)
	w.provisioned(2, "home-a", "home-b")
	w.failOnce = "curl -fsS --max-time 20 -K -"

	err := storageadd.Execute(w.build(storageadd.Options{}))
	if err == nil {
		t.Fatal("expected the probe's failure to stop the run")
	}
	for _, app := range cfg.AppNames() {
		if s, ok := secrets.Apps[app]["s3_secret_access_key"].(string); ok && s != "" && strings.Contains(err.Error(), s) {
			t.Fatalf("the error carries %s's S3 secret:\n%v", app, err)
		}
	}
	for _, h := range w.hosts {
		for _, c := range h.commands {
			for _, app := range cfg.AppNames() {
				if s, ok := secrets.Apps[app]["s3_secret_access_key"].(string); ok && s != "" && strings.Contains(c, s) {
					t.Fatalf("a command line on %s carries %s's S3 secret: %s", h.name, app, c)
				}
			}
		}
	}
}

// After a reset, a bucket holding fewer objects than it did before is not
// passed: the counts converge through table sync, so it waits, and says
// where the previous layouts are.
func TestFewerObjectsAfterAResetWaits(t *testing.T) {
	w := growing(t)
	p := w.build(storageadd.Options{ChangeReplication: true})
	// The counts are recorded at the start of the reset; lose objects after.
	w.failOnce = "stop garage"
	if err := storageadd.Execute(p); err == nil {
		t.Fatal("the test meant the stop to fail")
	}
	for b := range w.buckets {
		w.buckets[b] = 3
	}
	err := storageadd.Execute(w.build(storageadd.Options{ChangeReplication: true}))
	if !errors.Is(err, storageadd.ErrWaiting) || !strings.Contains(err.Error(), "held 7 before the reset") {
		t.Fatalf("expected a wait on the object counts, got %v", err)
	}
}

// Each node takes its own capacity, and changing one later reassigns that
// node alone, in one new layout version. A change smaller than layout show's
// rounding is not a change.
func TestCapacitiesAreAssignedAndResized(t *testing.T) {
	cfg, secrets := fixture(t)
	cfg.Storage.Garage.Replication = 1
	cfg.Storage.Garage.Capacities = map[string]string{"home-b": "2T"}
	w := newWorld(t, cfg, secrets)
	w.provisioned(1, "home-a")
	w.deployGarage("home-b", 1)

	if err := storageadd.Execute(w.build(storageadd.Options{})); err != nil {
		t.Fatal(err)
	}
	a, b := w.hosts["home-a"], w.hosts["home-b"]
	if a.roles[b.short()] != "home-b 2.0 TB" || a.roles[a.short()] != "home-a 100.0 GB" {
		t.Fatalf("roles after the join: %v", a.roles)
	}

	// Within the rounding: nothing to do.
	cfg.Storage.Garage.Capacities["home-b"] = "2010G"
	if p := w.build(storageadd.Options{}); p.Pending() {
		t.Errorf("a change hidden by layout show's rounding planned a rebalance:\n%s", printed(p))
	}

	// A real change: home-b alone is reassigned, in one version.
	cfg.Storage.Garage.Capacities["home-b"] = "500G"
	p := w.build(storageadd.Options{})
	if !strings.Contains(printed(p), "resize") {
		t.Fatalf("a capacity change was not planned:\n%s", printed(p))
	}
	before := a.version
	if err := storageadd.Execute(p); err != nil {
		t.Fatal(err)
	}
	if a.version != before+1 || a.roles[b.short()] != "home-b 500.0 GB" || a.roles[a.short()] != "home-a 100.0 GB" {
		t.Errorf("after the resize: version %d (was %d), roles %v", a.version, before, a.roles)
	}
}

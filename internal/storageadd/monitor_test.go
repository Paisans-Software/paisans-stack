package storageadd_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/storageadd"
)

const seed = "/srv/paisans/f2a9/status/monitors.json"

// The last stage reseeds the monitor: a monitor whose seed was rendered
// before a site existed is given the one rendered now, and restarted on it.
// Here watch's seed is the one an apply before home-b wrote, so it lacks
// home-b's ping.
func TestTheJoinReseedsTheMonitorLast(t *testing.T) {
	cfg, secrets := fixture(t)
	cfg.Storage.Garage.Replication = 1
	w := newWorld(t, cfg, secrets)
	w.provisioned(1, "home-a")
	w.deployGarage("home-b", 1)
	watch := w.hosts["watch"]
	stale := strings.Replace(watch.files[seed], `"home-b — ping"`, `"home-x — ping"`, 1)
	if stale == watch.files[seed] {
		t.Fatal("the fixture's seed does not ping home-b")
	}
	watch.files[seed] = stale
	watch.recordManifest()

	p := w.build(storageadd.Options{})
	last := p.Stages[len(p.Stages)-1]
	if last.Name != "monitor" || !strings.Contains(printed(p), "update    watch: "+seed) || !strings.Contains(printed(p), "restart   watch: status") {
		t.Fatalf("the last stage is not the monitor's reseed:\n%s", printed(p))
	}
	if !p.Pending() {
		t.Error("a stale seed does not make the plan pending")
	}
	if err := storageadd.Execute(p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(watch.files[seed], `"home-b — ping"`) {
		t.Errorf("the monitor was not reseeded:\n%s", watch.files[seed])
	}
	if watch.ran("/srv/paisans/f2a9/status/compose.yaml restart") != 1 {
		t.Error("the monitor was not restarted on the new seed")
	}
	again := w.build(storageadd.Options{})
	if again.Pending() {
		t.Errorf("a finished join still plans work:\n%s", printed(again))
	}
}

// A reseed that fails stops at its stage, with the layout applied and the
// apply that finishes the monitor by hand.
func TestAFailedReseedNamesTheApply(t *testing.T) {
	cfg, secrets := fixture(t)
	cfg.Storage.Garage.Replication = 1
	w := newWorld(t, cfg, secrets)
	w.provisioned(1, "home-a")
	w.deployGarage("home-b", 1)
	watch := w.hosts["watch"]
	watch.files[seed] = strings.Replace(watch.files[seed], `"home-b — ping"`, `"home-x — ping"`, 1)
	watch.recordManifest()
	w.failOnce = "/srv/paisans/f2a9/status/compose.yaml restart"

	err := storageadd.Execute(w.build(storageadd.Options{}))
	if err == nil || !strings.Contains(err.Error(), "(monitor)") || !strings.Contains(err.Error(), "paisans apply --site watch --only status --execute") {
		t.Fatalf("want the monitor stage to fail naming the apply, got %v", err)
	}
	if zoneOf(w.hosts["home-b"], w.hosts["home-b"]) != "home-b" {
		t.Error("the join was not done before the monitor stage")
	}
}

// A deployment with no monitor site skips the stage.
func TestNoMonitorSiteSkipsTheReseed(t *testing.T) {
	cfg, secrets := fixture(t)
	cfg.Storage.Garage.Replication = 1
	delete(cfg.Sites, "watch")
	delete(cfg.Apps, "status")
	w := newWorld(t, cfg, secrets)
	w.provisioned(1, "home-a")
	w.deployGarage("home-b", 1)
	p := w.build(storageadd.Options{})
	last := p.Stages[len(p.Stages)-1]
	if last.Name != "monitor" || len(last.Steps) != 0 || !strings.Contains(last.Gate, "no site holds the monitor role") {
		t.Fatalf("the stage is not skipped: %q %v", last.Gate, last.Steps)
	}
	if err := storageadd.Execute(p); err != nil {
		t.Fatal(err)
	}
}

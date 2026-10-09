package appremove

import (
	"errors"
	"github.com/paisans-software/paisans-stack/internal/ui"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
)

const seed = "/srv/paisans/f2a9/status/monitors.json"

// watched is world with the monitor site applied while docs was still
// declared, so its seed carries docs' checks.
func watched(t *testing.T) (*config.Config, *config.Secrets, map[string]*host) {
	t.Helper()
	cfg, hosts := world(t)
	secrets, err := config.LoadSecrets(filepath.Join("..", "render", "testdata", "secrets.fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := render.Build(fixture(t), secrets)
	if err != nil {
		t.Fatal(err)
	}
	watch := hosts["watch"]
	var entries []render.ManifestFile
	for _, f := range before.Files {
		rel, ok := strings.CutPrefix(f.Path, "watch/")
		if !ok || rel == render.ManifestName || !strings.HasPrefix(rel, "srv/paisans/f2a9/status/") {
			continue
		}
		watch.files["/"+rel] = f.Content
		entries = append(entries, watch.entry("/"+rel, false))
	}
	watch.manifest(t, entries...)
	if !strings.Contains(watch.files[seed], "docs") {
		t.Fatal("the seed rendered with docs does not check it")
	}
	return cfg, secrets, hosts
}

func monitors(t *testing.T, cfg *config.Config, secrets *config.Secrets, hosts map[string]*host, p *Plan) {
	t.Helper()
	rendered, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	transports := map[string]apply.Transport{}
	for name, h := range hosts {
		transports[name] = h
	}
	if err := p.PlanMonitors(cfg, rendered, "", transports); err != nil {
		t.Fatal(err)
	}
}

// The monitor is reseeded last: once docs is off every site, watch's seed no
// longer checks it, and the monitor restarts on the new file. A second run
// finds the seed current and plans nothing for the monitor.
func TestTheRemovalReseedsTheMonitorLast(t *testing.T) {
	cfg, secrets, hosts := watched(t)
	p, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err != nil {
		t.Fatal(err)
	}
	monitors(t, cfg, secrets, hosts, p)
	rec := &ui.Recorder{Verbose_: true}
	p.Show(rec)
	if !rec.Has("section", "watch (the monitor)") || !rec.Has("detail", seed) {
		t.Fatalf("the plan does not show the reseed:\n%s", rec.Lines())
	}
	if err := executor(hosts).Execute(p); err != nil {
		t.Fatal(err)
	}
	watch := hosts["watch"]
	if strings.Contains(watch.files[seed], "docs") {
		t.Errorf("the monitor still checks docs:\n%s", watch.files[seed])
	}
	restarted := 0
	for _, c := range watch.sent {
		if strings.HasSuffix(c, "/status/compose.yaml restart") {
			restarted++
		}
	}
	if restarted != 1 {
		t.Errorf("the monitor was restarted %d time(s)", restarted)
	}
	for _, c := range hosts["home-a"].sent {
		if strings.Contains(c, "restart") {
			t.Errorf("a site other than the monitor's was restarted: %s", c)
		}
	}

	again, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err != nil {
		t.Fatal(err)
	}
	monitors(t, cfg, secrets, hosts, again)
	if len(again.Monitors) != 1 || again.Monitors[0].Pending() {
		t.Error("a reseeded monitor still plans work")
	}
	if err := executor(hosts).Execute(again); err != nil {
		t.Errorf("a second run fails the monitor's gate: %v", err)
	}
}

// A failed reseed comes after everything of the app's is gone, and names
// the apply that finishes it.
func TestAFailedReseedNamesTheApply(t *testing.T) {
	cfg, secrets, hosts := watched(t)
	hosts["watch"].fail = "/status/compose.yaml restart"
	p, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err != nil {
		t.Fatal(err)
	}
	monitors(t, cfg, secrets, hosts, p)
	err = executor(hosts).Execute(p)
	var me *MonitorError
	if !errors.As(err, &me) || !strings.Contains(err.Error(), "paisans apply --site watch --only status --execute") {
		t.Fatalf("want the monitor's failure naming the apply, got %v", err)
	}
	if _, ok := hosts["home-a"].files[docsEnv]; ok {
		t.Error("the app's files were not removed before the monitor")
	}
}

// A deployment with no monitor site plans no reseed.
func TestNoMonitorSitePlansNoReseed(t *testing.T) {
	cfg, secrets, hosts := watched(t)
	delete(cfg.Sites, "watch")
	delete(cfg.Apps, "status")
	delete(hosts, "watch")
	p, err := Build(cfg, "docs", probe(t, cfg, "docs", hosts, false))
	if err != nil {
		t.Fatal(err)
	}
	monitors(t, cfg, secrets, hosts, p)
	if len(p.Monitors) != 0 {
		t.Errorf("monitors planned: %d", len(p.Monitors))
	}
}

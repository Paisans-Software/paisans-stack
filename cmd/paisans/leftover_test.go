package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// apply's plan output ends with a "left over" section naming this
// deployment's stacks and files the site no longer renders, saying apply
// leaves them and naming `paisans app remove` for each app they belong to. A whole plan shows what it is about to mark; a
// partial one shows the manifest's marks. Nothing left over prints nothing.
func TestTheApplyPlanListsLeftovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(freshSite), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &config.Secrets{Version: 1}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatal(err)
	}
	rendered, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	id := cfg.Deployment().ID
	inv := &hostcheck.Inventory{
		Containers: []hostcheck.Container{
			{Name: "paisans-f2a9-talk-app-1", Deployment: id, Project: "paisans-f2a9-talk", PID: 1},
			{Name: "paisans-f2a9-docs-app-1", Deployment: id, Project: "paisans-f2a9-docs", PID: 2},
		},
		ManifestFiles: []render.ManifestFile{{Path: "srv/paisans/f2a9/docs/.env", Leftover: true, LeftoverSince: "2026-10-01T00:00:00Z"}},
	}

	whole, err := apply.Build("home-a", rendered, acme.Module(cfg.ACME.Provider), emptyHost{})
	if err != nil {
		t.Fatal(err)
	}
	rec := &ui.Recorder{Verbose_: true}
	if err := printLeftovers(rec, cfg, "home-a", inv, whole); err != nil {
		t.Fatal(err)
	}
	if !warned(rec, "left over: stack docs", "stack docs (compose project paisans-f2a9-docs, 1 of 1 running): paisans-f2a9-docs-app-1") ||
		!warned(rec, "`paisans app remove docs`", "") ||
		!rec.Has("detail", "left over: this deployment's, no longer rendered for home-a. apply leaves each one in place.") ||
		!rec.Has("detail", "`paisans app remove docs` takes docs's off every site") ||
		strings.Contains(rec.Lines(), "talk") {
		t.Errorf("whole plan:\n%s", rec.Lines())
	}
	if strings.Contains(rec.Lines(), "docs/.env") {
		t.Errorf("a whole plan showed the old manifest's mark, not its own:\n%s", rec.Lines())
	}

	partial, err := apply.Build("home-a", rendered, acme.Module(cfg.ACME.Provider), emptyHost{}, apply.Only("talk"))
	if err != nil {
		t.Fatal(err)
	}
	rec = &ui.Recorder{Verbose_: true}
	if err := printLeftovers(rec, cfg, "home-a", inv, partial); err != nil {
		t.Fatal(err)
	}
	if !warned(rec, "left over: file /srv/paisans/f2a9/docs/.env", "file /srv/paisans/f2a9/docs/.env, left over since 2026-10-01T00:00:00Z") ||
		!warned(rec, "`paisans app remove docs`", "") {
		t.Errorf("partial plan:\n%s", rec.Lines())
	}

	rec = &ui.Recorder{Verbose_: true}
	if err := printLeftovers(rec, cfg, "home-a", &hostcheck.Inventory{}, whole); err != nil || len(rec.Events) != 0 {
		t.Errorf("nothing left over printed %q, %v", rec.Lines(), err)
	}
}

// warned reports whether rec holds a warning whose hint contains hint and
// whose detail contains detail.
func warned(rec *ui.Recorder, hint, detail string) bool {
	for _, e := range rec.Events {
		if e.Kind == "warn" && strings.Contains(e.Text, hint) && strings.Contains(e.Extra, detail) {
			return true
		}
	}
	return false
}

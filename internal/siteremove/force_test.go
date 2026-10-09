package siteremove_test

import (
	"os"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

func forced(t *testing.T, w *world, cfg *config.Config, site string, dest config.Destination, o siteremove.Options) *siteremove.Plan {
	t.Helper()
	p, err := siteremove.BuildForced(cfg, w.secrets, site, dest, w.hosts[site], o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// None of the cluster's refusals stop --force: home-a has apps pinned to it
// and etcd is unhealthy. Only home-a is reached, and paisans.yaml is not
// touched.
func TestForcedIgnoresTheClustersRefusals(t *testing.T) {
	w := setup(t)
	w.etcdDown["home-b"] = true
	before, err := os.ReadFile(w.configPath)
	if err != nil {
		t.Fatal(err)
	}
	p := forced(t, w, w.cfg, "home-a", w.cfg.Sites["home-a"].Destination(), siteremove.Options{})
	if len(p.Stages) != 1 || stageNamed(p, "clean the host") == nil {
		t.Fatalf("stages:\n%s", printed(p))
	}
	if !p.Current {
		t.Error("home-a's declared host is not marked as its own")
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	for _, h := range w.hosts {
		if h.name != "home-a" && len(h.commands) > 0 {
			t.Errorf("%s was reached: %v", h.name, h.commands)
		}
	}
	after, _ := os.ReadFile(w.configPath)
	if string(after) != string(before) {
		t.Error("--force edited paisans.yaml")
	}
}

// The host stage of a forced plan is the unforced plan's host stage.
func TestForcedHostStageMatchesTheFullRemoval(t *testing.T) {
	w := setup(t)
	full := w.mustBuild("home-b", siteremove.Options{})
	f := forced(t, w, w.cfg, "home-b", w.cfg.Sites["home-b"].Destination(), siteremove.Options{})
	var a []siteremove.Step
	for _, s := range stageNamed(full, "clean the host").Steps {
		if s.Verb != "note" {
			a = append(a, s)
		}
	}
	var b []siteremove.Step
	for _, s := range stageNamed(f, "clean the host").Steps {
		if s.Verb != "note" {
			b = append(b, s)
		}
	}
	if len(a) != len(b) {
		t.Fatalf("full:\n%s\nforced:\n%s", printed(full), printed(f))
	}
	for i := range a {
		if a[i].Text != b[i].Text {
			t.Errorf("step %d: %q vs %q", i, a[i].Text, b[i].Text)
		}
	}
}

// A site no longer declared is cleaned through any destination, its roles
// read from the registry entry on the host.
func TestForcedUndeclaredGatewayHandsOverCaddy(t *testing.T) {
	w := setup(t)
	vm := w.hosts["vm"]
	vm.files[render.HostSitesDir+"/blog.caddy"] = "blog.example.org { respond 200 }\n"
	dest, _ := config.ParseDestination("ubuntu@192.0.2.10")
	p := forced(t, w, w.cfg.WithoutSite("vm"), "vm", dest, siteremove.Options{})
	if p.Current {
		t.Error("an undeclared site's host is marked as its own")
	}
	if !hasStepIn(stageNamed(p, "clean the host"), "vm", "hand over", "Caddy") {
		t.Fatalf("no hand over:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
}

// A host with nothing of this deployment's on it plans nothing.
func TestForcedOnACleanHostPlansNothing(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	b.containers, b.networks, b.volumes, b.rules = nil, nil, nil, nil
	b.files = map[string]string{registry.Path: encode(t, registry.Registry{Version: registry.Version, Deployments: map[string]registry.Entry{}})}
	b.wgUp = false
	dest, _ := config.ParseDestination("ubuntu@192.0.2.11")
	p := forced(t, w, w.cfg, "home-b", dest, siteremove.Options{})
	if n := len(stageNamed(p, "clean the host").Steps); n != 0 {
		t.Fatalf("%d step(s) planned:\n%s", n, printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
}

// The plan for the declared site's own host says what the cluster loses.
func TestForcedOwnHostSaysWhatTheClusterLoses(t *testing.T) {
	w := setup(t)
	p := forced(t, w, w.cfg, "home-b", w.cfg.Sites["home-b"].Destination(), siteremove.Options{})
	if !strings.Contains(printed(p), "still declared") {
		t.Errorf("no warning:\n%s", printed(p))
	}
}

func TestForcedRefusesHostGone(t *testing.T) {
	w := setup(t)
	_, err := siteremove.BuildForced(w.cfg, w.secrets, "home-b", w.cfg.Sites["home-b"].Destination(), nil, siteremove.Options{HostGone: true})
	if err == nil || !strings.Contains(err.Error(), "Drop one of them") {
		t.Errorf("err = %v", err)
	}
}

// A --ssh that reaches another declared site's host is refused: it would
// take a live site down under another site's name.
func TestForcedRefusesAnotherDeclaredSitesHost(t *testing.T) {
	w := setup(t)
	dest, _ := config.ParseDestination("ubuntu@192.0.2.99")
	_, err := siteremove.BuildForced(w.cfg, w.secrets, "monitor-x", dest, w.hosts["home-b"], siteremove.Options{})
	if err == nil || !strings.Contains(err.Error(), "home-b") {
		t.Fatalf("err = %v", err)
	}
	if n := w.hosts["home-b"].ran("docker"); n != 0 {
		t.Errorf("home-b was changed: %d docker command(s)", n)
	}
}

// The site's own host is known by its registry entry, however --ssh spells
// it.
func TestForcedKnowsTheSitesOwnHostByItsRegistryEntry(t *testing.T) {
	w := setup(t)
	dest, _ := config.ParseDestination("root@192.0.2.12:2222")
	p := forced(t, w, w.cfg, "home-b", dest, siteremove.Options{})
	if !p.Current {
		t.Error("home-b's host reached by another spelling is not marked as its own")
	}
}

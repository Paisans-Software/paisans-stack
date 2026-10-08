package render_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// monitorConfig is the fixture with the watch monitor's ingress block set,
// nil for the default mode paisans.
func monitorConfig(t *testing.T, ingress *config.Ingress) *config.Config {
	t.Helper()
	cfg := fixture(t)
	watch := cfg.Sites["watch"]
	watch.Ingress = ingress
	cfg.Sites["watch"] = watch
	if refusals := validate.Check(cfg).Refusals(); len(refusals) > 0 {
		t.Fatalf("the monitor configuration is refused: %v", refusals)
	}
	return cfg
}

func mustBuild(t *testing.T, cfg *config.Config) *render.Plan {
	t.Helper()
	plan, err := render.Build(cfg, fixtureSecrets(t))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestAPaisansMonitorClaimsEightyAndFourFortyThree(t *testing.T) {
	cfg := monitorConfig(t, nil)
	found := map[int]bool{}
	for _, l := range render.SiteListeners(cfg, "watch") {
		if l.Owner == "Caddy" && l.Address == "" && l.Proto == "tcp" && l.Key == "sites.watch.roles (monitor)" {
			found[l.Port] = true
		}
	}
	if !found[80] || !found[443] {
		t.Fatalf("%v", render.SiteListeners(cfg, "watch"))
	}
}

func TestAnExternalListenIsClaimed(t *testing.T) {
	cfg := monitorConfig(t, &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"})
	var found bool
	for _, l := range render.SiteListeners(cfg, "watch") {
		if l.Port == 80 || l.Port == 443 {
			t.Errorf("an external monitor claims %s", l)
		}
		if l.Address == "127.0.0.1" && l.Port == 8480 && l.Proto == "tcp" && l.Key == "sites.watch.ingress.listen" && l.Owner == "app status (uptime)" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no claim for listen: %v", render.SiteListeners(cfg, "watch"))
	}
	// On a site that also holds data, a listen on Postgres's loopback port
	// is a collision validate reports, not a container that fails to start.
	site := cfg.Sites["watch"]
	site.Roles = append(site.Roles, config.RoleData)
	site.Ingress.Listen = "127.0.0.1:5432"
	cfg.Sites["watch"] = site
	if !validate.Check(cfg).Has("port-collision") {
		t.Fatal("a listen on 5432 beside Postgres is not a collision")
	}
}

// ServedBy names the monitor that serves an app pinned to it; every other
// app, pinned or clustered, is the gateway's.
func TestServedBy(t *testing.T) {
	cfg := fixture(t)
	if site, ok := render.ServedBy(cfg, "status"); !ok || site != "watch" {
		t.Fatalf("status: %s %v", site, ok)
	}
	for _, app := range []string{"talk", "chat", "blog"} {
		if site, ok := render.ServedBy(cfg, app); ok {
			t.Errorf("%s is served by monitor %s", app, site)
		}
	}
}

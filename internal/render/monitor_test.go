package render_test

import (
	"strings"
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

// The monitor is served from its own machine, so the gateway carries neither
// its host block nor its snippet, and the monitor's Caddy carries only it.
func TestMonitorAppsLeaveTheGatewayCaddyfile(t *testing.T) {
	files := planFiles(build(t))
	gateway := files["vm/srv/infra/caddy/Caddyfile"]
	if strings.Contains(gateway, "status.example.org") {
		t.Fatalf("the gateway routes the monitor:\n%s", gateway)
	}
	if _, ok := files["vm/srv/infra/caddy/snippets/status.caddy"]; ok {
		t.Fatal("the gateway carries the monitor's snippet")
	}
	if !strings.Contains(gateway, "talk.example.org {") {
		t.Fatal("the gateway lost an ordinary app")
	}
	monitor := files["watch/srv/infra/caddy/Caddyfile"]
	if !strings.Contains(monitor, "status.example.org {") || strings.Contains(monitor, "talk.example.org") {
		t.Fatalf("the monitor's Caddyfile:\n%s", monitor)
	}
	for path := range files {
		if strings.HasPrefix(path, "watch/srv/infra/caddy/snippets/") && path != "watch/srv/infra/caddy/snippets/status.caddy" {
			t.Errorf("the monitor carries %s", path)
		}
	}
	snippet := files["watch/srv/infra/caddy/snippets/status.caddy"]
	if !strings.Contains(snippet, "reverse_proxy 10.44.0.4:3001") {
		t.Fatalf("the monitor's snippet does not reach the app on its own mesh address:\n%s", snippet)
	}
	// Its Caddy is on the same machine as the app, so a dead upstream is not
	// a site that died while the edge stayed up.
	if strings.Contains(snippet, "upstream_single") {
		t.Errorf("the monitor's own app imports upstream_single:\n%s", snippet)
	}
}

func TestAPaisansMonitorRunsTheGatewaysCaddy(t *testing.T) {
	files := planFiles(build(t))
	image := func(path string) string {
		compose := files[path]
		i := strings.Index(compose, "\n  caddy:\n")
		if i < 0 {
			return ""
		}
		block := compose[i:]
		j := strings.Index(block, "image: ")
		return strings.SplitN(block[j:], "\n", 2)[0]
	}
	if image("watch/srv/infra/compose.yaml") == "" || image("watch/srv/infra/compose.yaml") != image("vm/srv/infra/compose.yaml") {
		t.Fatalf("monitor %q, gateway %q", image("watch/srv/infra/compose.yaml"), image("vm/srv/infra/compose.yaml"))
	}
	compose := files["watch/srv/infra/compose.yaml"]
	if !strings.Contains(compose, "network_mode: host") || strings.Contains(compose, "etcd:") || strings.Contains(compose, "patroni:") {
		t.Fatalf("the monitor's infra stack:\n%s", compose)
	}
	if !strings.Contains(files["watch/srv/infra/caddy/Caddyfile"], "acme_dns desec") {
		t.Fatal("the monitor's Caddy does not use DNS-01")
	}
	if !strings.Contains(files["watch/srv/infra/caddy/caddy.env"], "ACME_DNS_TOKEN=") {
		t.Fatal("no token for the monitor's Caddy")
	}
}

func TestAnExternalMonitorRunsNoCaddy(t *testing.T) {
	cfg := monitorConfig(t, &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"})
	files := planFiles(mustBuild(t, cfg))
	for path := range files {
		if strings.HasPrefix(path, "watch/srv/infra/") {
			t.Errorf("an external monitor renders %s", path)
		}
	}
	if strings.Contains(files["vm/srv/infra/caddy/Caddyfile"], "status.example.org") {
		t.Error("the gateway routes an external monitor's app")
	}
}

// A gated app on a paisans monitor needs the gate's named snippets there too,
// and an ungated one does not carry them.
func TestAMonitorCarriesTheGateOnlyWhenItsAppIsGated(t *testing.T) {
	files := planFiles(build(t))
	if strings.Contains(files["watch/srv/infra/caddy/Caddyfile"], "-gates.caddy") {
		t.Error("an ungated monitor imports the gate snippets")
	}
	cfg := fixture(t)
	status := cfg.Apps["status"]
	status.Gate = "members"
	cfg.Apps["status"] = status
	files = planFiles(mustBuild(t, cfg))
	if !strings.Contains(files["watch/srv/infra/caddy/Caddyfile"], "import /etc/caddy/snippets/gate-gates.caddy") || files["watch/srv/infra/caddy/snippets/gate-gates.caddy"] == "" {
		t.Fatalf("a gated monitor lacks the gate snippets:\n%s", files["watch/srv/infra/caddy/Caddyfile"])
	}
}

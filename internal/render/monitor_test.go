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
	gateway := files["vm/srv/paisans/f2a9/infra/caddy/Caddyfile"]
	if strings.Contains(gateway, "status.example.org") {
		t.Fatalf("the gateway routes the monitor:\n%s", gateway)
	}
	if _, ok := files["vm/srv/paisans/f2a9/infra/caddy/snippets/status.caddy"]; ok {
		t.Fatal("the gateway carries the monitor's snippet")
	}
	if !strings.Contains(gateway, "talk.example.org {") {
		t.Fatal("the gateway lost an ordinary app")
	}
	monitor := files["watch/srv/paisans/f2a9/infra/caddy/Caddyfile"]
	if !strings.Contains(monitor, "status.example.org {") || strings.Contains(monitor, "talk.example.org") {
		t.Fatalf("the monitor's Caddyfile:\n%s", monitor)
	}
	for path := range files {
		if strings.HasPrefix(path, "watch/srv/paisans/f2a9/infra/caddy/snippets/") && path != "watch/srv/paisans/f2a9/infra/caddy/snippets/status.caddy" {
			t.Errorf("the monitor carries %s", path)
		}
	}
	snippet := files["watch/srv/paisans/f2a9/infra/caddy/snippets/status.caddy"]
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
	if image("watch/srv/paisans/f2a9/infra/compose.yaml") == "" || image("watch/srv/paisans/f2a9/infra/compose.yaml") != image("vm/srv/paisans/f2a9/infra/compose.yaml") {
		t.Fatalf("monitor %q, gateway %q", image("watch/srv/paisans/f2a9/infra/compose.yaml"), image("vm/srv/paisans/f2a9/infra/compose.yaml"))
	}
	compose := files["watch/srv/paisans/f2a9/infra/compose.yaml"]
	if !strings.Contains(compose, "network_mode: host") || strings.Contains(compose, "etcd:") || strings.Contains(compose, "patroni:") {
		t.Fatalf("the monitor's infra stack:\n%s", compose)
	}
	if !strings.Contains(files["watch/srv/paisans/f2a9/infra/caddy/Caddyfile"], "acme_dns desec") {
		t.Fatal("the monitor's Caddy does not use DNS-01")
	}
	if !strings.Contains(files["watch/srv/paisans/f2a9/infra/caddy/caddy.env"], "ACME_DNS_TOKEN=") {
		t.Fatal("no token for the monitor's Caddy")
	}
}

func TestAnExternalMonitorRunsNoCaddy(t *testing.T) {
	cfg := monitorConfig(t, &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"})
	files := planFiles(mustBuild(t, cfg))
	for path := range files {
		if strings.HasPrefix(path, "watch/srv/paisans/f2a9/infra/") {
			t.Errorf("an external monitor renders %s", path)
		}
	}
	if strings.Contains(files["vm/srv/paisans/f2a9/infra/caddy/Caddyfile"], "status.example.org") {
		t.Error("the gateway routes an external monitor's app")
	}
}

// A gated app on a paisans monitor needs the gate's named snippets there too,
// and an ungated one does not carry them.
func TestAMonitorCarriesTheGateOnlyWhenItsAppIsGated(t *testing.T) {
	files := planFiles(build(t))
	if strings.Contains(files["watch/srv/paisans/f2a9/infra/caddy/Caddyfile"], "-gates.caddy") {
		t.Error("an ungated monitor imports the gate snippets")
	}
	cfg := fixture(t)
	status := cfg.Apps["status"]
	status.Gate = "members"
	cfg.Apps["status"] = status
	files = planFiles(mustBuild(t, cfg))
	if !strings.Contains(files["watch/srv/paisans/f2a9/infra/caddy/Caddyfile"], "import /etc/caddy/snippets/gate-gates.caddy") || files["watch/srv/paisans/f2a9/infra/caddy/snippets/gate-gates.caddy"] == "" {
		t.Fatalf("a gated monitor lacks the gate snippets:\n%s", files["watch/srv/paisans/f2a9/infra/caddy/Caddyfile"])
	}
}

// TRUST_PROXY names exactly where the proxy in front of the monitor connects
// from, because the login rate limiter is keyed on the client address and
// only a trusted proxy's X-Forwarded-For is read. The monitor's own Caddy
// dials the app on the site's mesh address from the host itself. A port
// Docker publishes on 127.0.0.1 reaches the container from its compose
// network's gateway, which the template pins. A LAN or mesh listen keeps
// the proxy's own address, which is known only to lie in that network.
func TestTrustProxyFollowsWhereTheProxyConnectsFrom(t *testing.T) {
	for _, tc := range []struct {
		ingress *config.Ingress
		want    string
	}{
		{nil, "TRUST_PROXY=10.44.0.4/32"},
		{&config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"}, "TRUST_PROXY=10.255.255.1/32"},
		{&config.Ingress{Mode: config.IngressExternal, Listen: "192.168.1.20:8480"}, "TRUST_PROXY=192.168.0.0/16"},
		{&config.Ingress{Mode: config.IngressExternal, Listen: "172.20.0.5:8480"}, "TRUST_PROXY=172.16.0.0/12"},
		{&config.Ingress{Mode: config.IngressExternal, Listen: "100.101.102.103:8480"}, "TRUST_PROXY=100.64.0.0/10"},
		{&config.Ingress{Mode: config.IngressExternal, Listen: "10.44.0.4:8480"}, "TRUST_PROXY=10.44.0.0/24"},
	} {
		env := planFiles(mustBuild(t, monitorConfig(t, tc.ingress)))["watch/srv/paisans/f2a9/status/.env"]
		if !strings.Contains(env, "\n"+tc.want+"\n") || !strings.Contains(env, "\nPUBLIC_BASE_URL=https://status.example.org\n") {
			t.Errorf("%+v:\n%s", tc.ingress, env)
		}
	}
}

// In mode external the app is published on listen alone: nothing dials its
// mesh address, since the gateway does not route it and the monitor does not
// check its own container. Its compose network is pinned, so the address the
// web server's connections arrive from is known. Mode paisans publishes on the
// mesh address, where its Caddy reaches it, and pins nothing.
func TestAnExternalAppIsPublishedOnListenOnly(t *testing.T) {
	compose := planFiles(mustBuild(t, monitorConfig(t, &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"})))["watch/srv/paisans/f2a9/status/compose.yaml"]
	for _, want := range []string{`"127.0.0.1:8480:3001"`, "subnet: 10.255.255.0/29", "gateway: 10.255.255.1"} {
		if !strings.Contains(compose, want) {
			t.Errorf("missing %s:\n%s", want, compose)
		}
	}
	if strings.Contains(compose, "10.44.0.4:3001") {
		t.Errorf("an external app is still published on the mesh:\n%s", compose)
	}
	compose = planFiles(build(t))["watch/srv/paisans/f2a9/status/compose.yaml"]
	if strings.Contains(compose, "8480") || strings.Contains(compose, "ipam") || strings.Count(compose, ":3001:3001") != 1 {
		t.Errorf("mode paisans publishes on the mesh address alone:\n%s", compose)
	}
}

// The pinned network is a claim like a port: the host check refuses a host
// where something else already uses it.
func TestAnExternalMonitorClaimsItsNetwork(t *testing.T) {
	cfg := monitorConfig(t, &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"})
	got := render.SiteNetworks(cfg, "watch")
	if len(got) != 1 || got[0].Subnet != "10.255.255.0/29" || got[0].Key != "sites.watch.ingress" || got[0].Project != "paisans-f2a9-status" {
		t.Fatalf("%+v", got)
	}
	for _, l := range render.SiteListeners(cfg, "watch") {
		if l.Address == "10.44.0.4" && l.Port == 3001 {
			t.Errorf("an external app still claims its mesh publish: %s", l)
		}
	}
	if got := render.SiteNetworks(monitorConfig(t, nil), "watch"); len(got) != 0 {
		t.Errorf("mode paisans claims %+v", got)
	}
}

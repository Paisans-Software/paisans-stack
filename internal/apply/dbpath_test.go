package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
)

// databaseApps is the fixture's cluster placement apps that use Postgres.
func databaseApps(t *testing.T) apply.Option {
	t.Helper()
	return apply.DatabaseApps(apply.ClusterDatabaseApps(fixtureConfig(t))...)
}

func stacks(p *apply.Plan) []string {
	var out []string
	for _, a := range p.Actions {
		out = append(out, a.Stack)
	}
	return out
}

func actionOn(p *apply.Plan, stack string) (apply.Action, bool) {
	for _, a := range p.Actions {
		if a.Stack == stack {
			return a, true
		}
	}
	return apply.Action{}, false
}

func indexOf(cmds []string, sub string) int {
	for i, c := range cmds {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

func TestTheClusterDatabaseAppsAreClusterPlacedAndUsePostgres(t *testing.T) {
	cfg := fixtureConfig(t)
	if got := strings.Join(apply.ClusterDatabaseApps(cfg), ","); got != "auth,docs,talk" {
		t.Errorf("got %s: blog and chat have their own Postgres, gate and web use none", got)
	}
}

// A new haproxy.cfg restarts HAProxy, which closes every app's connection to
// the database, so each database app is restarted after the infrastructure
// stack, its gate and the bootstrap, and checked.
func TestAHAProxyChangeRestartsTheDatabaseApps(t *testing.T) {
	host := appliedHost(t)
	p, err := apply.Build("home-a", planChanging(t, "srv/paisans/f2a9/infra/haproxy/haproxy.cfg"), acmeModule(t), host, databaseApps(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stacks(p), ","); got != "infra,auth,docs,talk" {
		t.Fatalf("actions %s", got)
	}
	for _, app := range []string{"auth", "docs", "talk"} {
		a, _ := actionOn(p, app)
		if a.Recreate || !strings.HasPrefix(a.Reason, "its database path changed (HAProxy/Patroni moved)") {
			t.Errorf("%s: %s, %s", app, a.Command(apply.Fixture), a.Reason)
		}
	}
	cfg := fixtureConfig(t)
	secrets, err := config.LoadSecrets("../render/testdata/secrets.fixture.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b, err := apply.Databases(cfg, secrets, "home-a")
	if err != nil {
		t.Fatal(err)
	}
	p.WithDatabases(b)
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	cmds := host.commands
	infra := indexOf(cmds, "/srv/paisans/f2a9/infra/compose.yaml restart haproxy")
	infraGate := indexOf(cmds, "/srv/paisans/f2a9/infra/compose.yaml ps --all")
	bootstrap := indexOf(cmds, ":8008/cluster")
	if infra < 0 || infraGate < infra || bootstrap < infraGate {
		t.Fatalf("infra %d, its gate %d, bootstrap %d:\n%s", infra, infraGate, bootstrap, strings.Join(cmds, "\n"))
	}
	for _, app := range []string{"auth", "docs", "talk"} {
		restart := indexOf(cmds, "/srv/paisans/f2a9/"+app+"/compose.yaml restart")
		gate := indexOf(cmds, "/srv/paisans/f2a9/"+app+"/compose.yaml ps --all")
		if restart < bootstrap || gate < restart {
			t.Errorf("%s: restart %d, gate %d, bootstrap %d", app, restart, gate, bootstrap)
		}
	}
	if host.ran("/srv/paisans/f2a9/blog/compose.yaml restart") {
		t.Error("blog has its own Postgres and was restarted")
	}
}

// A new patroni.env recreates Patroni, which on the primary moves it.
func TestAPatroniChangeRestartsTheDatabaseApps(t *testing.T) {
	host := appliedHost(t)
	p, err := apply.Build("home-a", planChanging(t, "srv/paisans/f2a9/infra/patroni.env"), acmeModule(t), host, databaseApps(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stacks(p), ","); got != "infra,auth,docs,talk" {
		t.Errorf("actions %s", got)
	}
}

// Garage, etcd and Caddy are not on the database path, and an action that
// moves only them restarts no app.
func TestAChangeOffTheDatabasePathRestartsNoApp(t *testing.T) {
	host := appliedHost(t)

	garage, err := apply.Build("home-a", planChanging(t, "srv/paisans/f2a9/infra/garage/garage.toml"), acmeModule(t), host, databaseApps(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stacks(garage), ","); got != "infra" {
		t.Errorf("a garage.toml change acts on %s", got)
	}

	etcd := planWith(t, func(cfg *config.Config) { cfg.Etcd.HeartbeatMS = 300 })
	p, err := apply.Build("home-a", etcd, acmeModule(t), host, databaseApps(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stacks(p), ","); got != "infra" {
		t.Errorf("an etcd only compose change acts on %s", got)
	}
	if a, _ := actionOn(p, "infra"); !a.Recreate {
		t.Errorf("the etcd change was expected to recreate, and plans %s", a.Command(apply.Fixture))
	}
}

// On a site that is both a gateway and an apps site, a routing change
// restarts Caddy and nothing on the database path.
func TestACaddyChangeRestartsNoApp(t *testing.T) {
	gatewayApps := func(cfg *config.Config) {
		site := cfg.Sites["home-a"]
		site.Roles = append(site.Roles, config.RoleGateway)
		cfg.Sites["home-a"] = site
	}
	host := newHost()
	first, err := apply.Build("home-a", planWith(t, gatewayApps), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}
	moved := planWith(t, func(cfg *config.Config) {
		gatewayApps(cfg)
		app := cfg.Apps["blog"]
		app.Hostname = "words.example.org"
		cfg.Apps["blog"] = app
	})
	p, err := apply.Build("home-a", moved, acmeModule(t), host, databaseApps(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range p.Actions {
		if strings.Contains(a.Reason, "database path") {
			t.Errorf("a routing change restarts %s", a.Stack)
		}
	}
	// The routing change is the gateway's reload, and no infra action.
	if a, ok := actionOn(p, "infra"); ok {
		t.Errorf("infra plans %q as well as the reload", a.Command(apply.Fixture))
	}
	if !p.GatewayReload {
		t.Error("a routing change planned no reload")
	}
}

// An app with an action of its own keeps that one action, with the reason
// added, rather than being restarted twice.
func TestAnAppWithItsOwnActionIsNotActedOnTwice(t *testing.T) {
	host := appliedHost(t)
	rendered := planChanging(t, "srv/paisans/f2a9/infra/haproxy/haproxy.cfg", "srv/paisans/f2a9/talk/.env")
	p, err := apply.Build("home-a", rendered, acmeModule(t), host, databaseApps(t))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range p.Actions {
		if a.Stack == "talk" {
			n++
			if !a.Recreate || !strings.Contains(a.Reason, "database path changed") {
				t.Errorf("talk: %s, %s", a.Command(apply.Fixture), a.Reason)
			}
		}
	}
	if n != 1 {
		t.Errorf("talk is acted on %d times", n)
	}
}

// --only without infra moves nothing under the apps. --only infra restarts
// HAProxy, so the apps are restarted with it.
func TestOnlyAndTheDatabasePath(t *testing.T) {
	host := appliedHost(t)
	rendered := planChanging(t, "srv/paisans/f2a9/infra/haproxy/haproxy.cfg")
	talk, err := apply.Build("home-a", rendered, acmeModule(t), host, databaseApps(t), apply.Only("talk"), apply.Recreate("talk"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stacks(talk), ","); got != "talk" {
		t.Errorf("--only talk acts on %s", got)
	}
	if a, _ := actionOn(talk, "talk"); strings.Contains(a.Reason, "database path") {
		t.Error("--only talk claims a database path change it did not make")
	}
	infra, err := apply.Build("home-a", rendered, acmeModule(t), host, databaseApps(t), apply.Only("infra"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stacks(infra), ","); got != "infra,auth,docs,talk" {
		t.Errorf("--only infra acts on %s", got)
	}
}

package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
)

const wg0 = "etc/wireguard/wg0.conf"

// appliedHost is home-a after a whole first apply of the fixture, with wg0 up.
func appliedHost(t *testing.T) *fakeHost {
	t.Helper()
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	host.commands = nil
	return host
}

// withNewPeer is the fixture with home-b given an endpoint, which changes the
// peer list in home-a's wg0.conf.
func withNewPeer(t *testing.T) func(*config.Config) {
	return func(cfg *config.Config) {
		site := cfg.Sites["home-b"]
		site.Endpoint = "198.51.100.20:51820"
		cfg.Sites["home-b"] = site
	}
}

// A scoped apply writes its one file, syncs the mesh, acts on no stack, and
// leaves the manifest able to recognise every other file as ours.
func TestAScopedApplyMovesOneFile(t *testing.T) {
	host := appliedHost(t)
	rendered := planWith(t, withNewPeer(t))

	p, err := apply.Build("home-a", rendered, acmeModule(t), host, apply.Scope(wg0))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 1 || p.Changes[0].Kind != apply.Update {
		t.Fatalf("want one update, got %v", p.Changes)
	}
	if p.WireGuard != apply.WireGuardSync || len(p.Actions) != 0 || p.GatewayChanging {
		t.Fatalf("a scoped mesh change planned %v %v %v", p.WireGuard, p.Actions, p.GatewayChanging)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if !host.ran("wg syncconf") {
		t.Error("the new peer was not handed to wg0")
	}
	if host.ran("docker compose") {
		t.Errorf("a scoped apply acted on a stack: %v", host.commands)
	}

	whole, err := apply.Build("home-a", rendered, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if c := whole.Conflicts(); len(c) != 0 {
		t.Fatalf("a whole apply after a scoped one sees conflicts: %v", c)
	}
	for _, c := range whole.Changes {
		if strings.HasSuffix(c.Path, "wg0.conf") && c.Kind != apply.Unchanged {
			t.Errorf("wg0.conf is %s after the scoped apply wrote it", c.Kind)
		}
	}
}

// Rolling back restores the old file and the old record, and syncs the mesh
// again, so the next apply sees the old file as ours.
func TestAScopedApplyRollsBack(t *testing.T) {
	host := appliedHost(t)
	before := host.files["/"+wg0]
	p, err := apply.Build("home-a", planWith(t, withNewPeer(t)), acmeModule(t), host, apply.Scope(wg0))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	host.commands = nil
	if err := apply.Rollback(p, host); err != nil {
		t.Fatal(err)
	}
	if host.files["/"+wg0] != before {
		t.Error("wg0.conf was not restored")
	}
	if !host.ran("wg syncconf") {
		t.Error("the restored peers were not handed to wg0")
	}
	again, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Conflicts()) != 0 || len(again.Writes()) != 0 {
		t.Errorf("after rollback the original render is not what the host holds: %v", again.Changes)
	}
}

func TestAScopeNamingNoRenderedFileIsRefused(t *testing.T) {
	host := appliedHost(t)
	if _, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Scope("srv/paisans/f2a9/nothing/here")); err == nil {
		t.Error("a scope naming nothing rendered was accepted")
	}
}

package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// planChangingOn is planChanging for any site.
func planChangingOn(t *testing.T, site string, rels ...string) *render.Plan {
	t.Helper()
	changed := plan(t)
	for _, rel := range rels {
		found := false
		for i, file := range changed.Files {
			if file.Path == site+"/"+rel {
				changed.Files[i].Content = file.Content + "# a later change\n"
				found = true
			}
		}
		if !found {
			t.Fatalf("%s renders no %s", site, rel)
		}
	}
	return changed
}

func (h *fakeHost) count(substring string) int {
	n := 0
	for _, command := range h.commands {
		if strings.Contains(command, substring) {
			n++
		}
	}
	return n
}

// A routing change on a running gateway is one reload and nothing else. It
// used to be a reload and then a restart of Caddy, from the infra action the
// same files also produced: two actions for one change, the second of them
// dropping every open connection.
func TestARoutingChangeIsOneReloadAndNoRestart(t *testing.T) {
	host := applied(t, "vm")
	host.running = true
	p, err := apply.Build("vm", planChangingOn(t, "vm", "srv/paisans/f2a9/infra/caddy/Caddyfile", "srv/paisans/f2a9/infra/caddy/snippets/blog.caddy"), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range p.Actions {
		if a.Stack == "infra" {
			t.Errorf("a routing change plans %q as well as the reload", a.Command(apply.Fixture))
		}
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if n := host.count("caddy reload"); n != 1 {
		t.Errorf("reloaded %d times, want once: %v", n, host.commands)
	}
	if host.ran("compose.yaml restart") || host.ran("compose.yaml up -d") {
		t.Errorf("a routing change also restarted or recreated: %v", host.commands)
	}
}

// A stopped gateway has nothing to reload, and a routing change is no stack
// action, so it is started rather than left down.
func TestARoutingChangeStartsAStoppedGateway(t *testing.T) {
	host := applied(t, "vm")
	host.running = false
	p, err := apply.Build("vm", planChangingOn(t, "vm", "srv/paisans/f2a9/infra/caddy/Caddyfile"), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if !host.ran("compose.yaml up -d caddy") || host.ran("caddy reload") {
		t.Errorf("a stopped gateway was not started, or was reloaded: %v", host.commands)
	}
}

// When the infrastructure stack is recreated in the same apply, Caddy gets
// exactly one action: none more if `up -d` replaced it, since it started on
// the new routing, and a reload after `up -d` if it was left in place. A
// compose or environment change alone, with no routing change, is the
// recreate and never a reload.
func TestARoutingChangeBesideARecreateIsOneAction(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rels      []string
		recreates []string
		reloads   int
	}{
		{"caddy replaced", []string{"srv/paisans/f2a9/infra/caddy/Caddyfile", "srv/paisans/f2a9/infra/caddy/caddy.env"}, []string{"caddy"}, 0},
		{"caddy left in place", []string{"srv/paisans/f2a9/infra/caddy/Caddyfile", "srv/paisans/f2a9/infra/compose.yaml"}, []string{"etcd"}, 1},
		{"no routing change", []string{"srv/paisans/f2a9/infra/compose.yaml"}, []string{"caddy"}, 0},
	} {
		base := applied(t, "vm")
		base.running = true
		host := newContainerHost(base, tc.recreates...)
		p, err := apply.Build("vm", planChangingOn(t, "vm", tc.rels...), acmeModule(t), host)
		if err != nil {
			t.Fatal(err)
		}
		if err := apply.Execute(p, host); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if n := host.count("caddy reload"); n != tc.reloads {
			t.Errorf("%s: reloaded %d times, want %d: %v", tc.name, n, tc.reloads, host.commands)
		}
		if host.ran("restart caddy") {
			t.Errorf("%s: Caddy was restarted: %v", tc.name, host.commands)
		}
		if tc.reloads == 1 && host.indexOf("caddy reload") < host.indexOf("compose.yaml up -d") {
			t.Errorf("%s: reloaded before `up -d`, which may then replace it anyway: %v", tc.name, host.commands)
		}
	}
}

package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// planChanging is the fixture's render with a comment appended to each named
// file of home-a, given relative to the site's root.
func planChanging(t *testing.T, rels ...string) *render.Plan {
	t.Helper()
	changed := plan(t)
	for _, rel := range rels {
		found := false
		for i, file := range changed.Files {
			if file.Path == "home-a/"+rel {
				changed.Files[i].Content = file.Content + "# a later change\n"
				found = true
			}
		}
		if !found {
			t.Fatalf("home-a renders no %s", rel)
		}
	}
	return changed
}

func infraAction(t *testing.T, p *apply.Plan) apply.Action {
	t.Helper()
	for _, a := range p.Actions {
		if a.Stack == "infra" {
			return a
		}
	}
	t.Fatalf("no action on infra: %v", p.Actions)
	return apply.Action{}
}

// A new garage.toml restarts Garage and nothing else. Restarting the whole
// infrastructure project for it restarted Patroni, a failover on the primary,
// and HAProxy, which drops every app's database connection.
func TestABindMountedInfraFileRestartsOnlyItsService(t *testing.T) {
	for _, tc := range []struct {
		rels []string
		want string
	}{
		{[]string{"srv/infra/garage/garage.toml"}, "docker compose -f /srv/infra/compose.yaml restart garage"},
		{[]string{"srv/infra/haproxy/haproxy.cfg"}, "docker compose -f /srv/infra/compose.yaml restart haproxy"},
		{[]string{"srv/infra/garage/garage.toml", "srv/infra/haproxy/haproxy.cfg"}, "docker compose -f /srv/infra/compose.yaml restart garage haproxy"},
	} {
		host := appliedHost(t)
		p, err := apply.Build("home-a", planChanging(t, tc.rels...), acmeModule(t), host)
		if err != nil {
			t.Fatal(err)
		}
		a := infraAction(t, p)
		if a.Recreate || a.Command() != tc.want {
			t.Errorf("%v: infra runs %q, want %q", tc.rels, a.Command(), tc.want)
		}
		if !strings.Contains(a.Reason, "only") {
			t.Errorf("%v: the reason does not say the restart is narrowed: %s", tc.rels, a.Reason)
		}
		if err := apply.Execute(p, host); err != nil {
			t.Fatal(err)
		}
		if !host.ran(tc.want) {
			t.Errorf("%v: %q never ran: %v", tc.rels, tc.want, host.commands)
		}
	}
}

// An environment change still recreates, which Compose limits to the
// services whose configuration changed.
func TestAnInfraEnvironmentChangeStillRecreates(t *testing.T) {
	host := appliedHost(t)
	p, err := apply.Build("home-a", planChanging(t, "srv/infra/patroni.env", "srv/infra/garage/garage.toml"), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if a := infraAction(t, p); !a.Recreate || len(a.Services) != 0 {
		t.Errorf("patroni.env changed and infra plans %q", a.Command())
	}
}

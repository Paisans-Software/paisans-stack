package apply_test

import (
	"fmt"
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
		{[]string{"srv/paisans/f2a9/infra/garage/garage.toml"}, "docker compose -f /srv/paisans/f2a9/infra/compose.yaml restart garage"},
		{[]string{"srv/paisans/f2a9/infra/haproxy/haproxy.cfg"}, "docker compose -f /srv/paisans/f2a9/infra/compose.yaml restart haproxy"},
		{[]string{"srv/paisans/f2a9/infra/garage/garage.toml", "srv/paisans/f2a9/infra/haproxy/haproxy.cfg"}, "docker compose -f /srv/paisans/f2a9/infra/compose.yaml restart garage haproxy"},
	} {
		host := appliedHost(t)
		p, err := apply.Build("home-a", planChanging(t, tc.rels...), acmeModule(t), host)
		if err != nil {
			t.Fatal(err)
		}
		a := infraAction(t, p)
		if a.Recreate || a.Command(apply.Fixture) != tc.want {
			t.Errorf("%v: infra runs %q, want %q", tc.rels, a.Command(apply.Fixture), tc.want)
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
	p, err := apply.Build("home-a", planChanging(t, "srv/paisans/f2a9/infra/patroni.env", "srv/paisans/f2a9/infra/garage/garage.toml"), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if a := infraAction(t, p); !a.Recreate || len(a.Services) != 0 {
		t.Errorf("patroni.env changed and infra plans %q", a.Command(apply.Fixture))
	}
}

// containerHost is a fakeHost whose infrastructure stack has containers with
// IDs: `up -d` replaces those of the services in recreates and leaves the
// rest, the way Compose replaces only a container whose configuration
// changed.
type containerHost struct {
	*fakeHost
	ids       map[string]string
	recreates map[string]bool
	gen       int
}

func newContainerHost(h *fakeHost, recreates ...string) *containerHost {
	c := &containerHost{fakeHost: h, ids: map[string]string{}, recreates: map[string]bool{}}
	for _, s := range []string{"caddy", "etcd", "garage", "haproxy", "patroni"} {
		c.ids[s] = s + "-0"
	}
	for _, s := range recreates {
		c.recreates[s] = true
	}
	return c
}

func (c *containerHost) Run(command string) (string, error) {
	const compose = "docker compose -f /srv/paisans/f2a9/infra/compose.yaml "
	switch command {
	case compose + "ps --all --format json":
		c.commands = append(c.commands, command)
		var b strings.Builder
		for _, s := range []string{"caddy", "etcd", "garage", "haproxy", "patroni"} {
			fmt.Fprintf(&b, `{"ID":%q,"Service":%q,"State":"running","Health":""}`+"\n", c.ids[s], s)
		}
		return b.String(), nil
	case compose + "up -d":
		c.gen++
		for s := range c.recreates {
			c.ids[s] = fmt.Sprintf("%s-%d", s, c.gen)
		}
	}
	return c.fakeHost.Run(command)
}

// A haproxy.cfg changed in the same apply as a patroni.env: the env file
// makes infra an `up -d`, which replaces Patroni and leaves HAProxy running
// on the old file, since a bind mounted file is not configuration to Compose.
// HAProxy is restarted after it, and so is Garage for its garage.toml; a
// service `up -d` replaced is not restarted again.
func TestABindMountChangedBesideAnUpIsRestartedAfterIt(t *testing.T) {
	host := newContainerHost(appliedHost(t), "patroni", "garage")
	p, err := apply.Build("home-a", planChanging(t, "srv/paisans/f2a9/infra/patroni.env", "srv/paisans/f2a9/infra/haproxy/haproxy.cfg", "srv/paisans/f2a9/infra/garage/garage.toml"), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	a := infraAction(t, p)
	if !a.Recreate || strings.Join(a.Refresh, ",") != "garage,haproxy" {
		t.Fatalf("infra plans %q refreshing %v", a.Command(apply.Fixture), a.Refresh)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	up := host.indexOf("compose.yaml up -d")
	restart := host.indexOf("compose.yaml restart haproxy")
	if up < 0 || restart < up {
		t.Errorf("haproxy was not restarted after `up -d` (up %d, restart %d): %v", up, restart, host.commands)
	}
	if host.ran("restart garage") || host.ran("restart patroni") {
		t.Errorf("a service `up -d` replaced was restarted again: %v", host.commands)
	}
}

// When `up -d` replaces every container with a changed file, nothing is
// restarted after it.
func TestNothingIsRestartedWhenUpReplacedIt(t *testing.T) {
	host := newContainerHost(appliedHost(t), "haproxy", "patroni")
	p, err := apply.Build("home-a", planChanging(t, "srv/paisans/f2a9/infra/patroni.env", "srv/paisans/f2a9/infra/haproxy/haproxy.cfg"), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if host.ran("compose.yaml restart") {
		t.Errorf("a container `up -d` replaced was restarted: %v", host.commands)
	}
}

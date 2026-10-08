package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// statusNetwork is the uptime app's compose default network in the fixture,
// <project>_default, the name Compose gives it.
const statusNetwork = "paisans-f2a9-status_default"

const (
	pinnedPool   = `[{"Subnet":"10.255.255.0/29","Gateway":"10.255.255.1"}]`
	unpinnedPool = `[{"Subnet":"172.18.0.0/16","Gateway":"172.18.0.1"}]`
)

// external renders the fixture with the monitor site in ingress mode
// external, which pins the uptime app's compose network.
func external(t *testing.T) *render.Plan {
	t.Helper()
	return planWith(t, func(cfg *config.Config) {
		watch := cfg.Sites["watch"]
		watch.Ingress = &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"}
		cfg.Sites["watch"] = watch
	})
}

// appliedFrom runs a first apply of rendered on site, so that a test about a
// later apply starts from a host that has everything and records it.
func appliedFrom(t *testing.T, site string, rendered *render.Plan) *fakeHost {
	t.Helper()
	host := newHost()
	first, err := apply.Build(site, rendered, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}
	host.commands = nil
	return host
}

func statusAction(t *testing.T, p *apply.Plan) apply.Action {
	t.Helper()
	for _, a := range p.Actions {
		if a.Stack == "status" {
			return a
		}
	}
	t.Fatalf("no action on status in %+v", p.Actions)
	return apply.Action{}
}

// Switching to ingress mode external pins the network, and the network the
// host already has keeps the pool Docker gave it. The plan takes the stack
// down first, so that `up` creates the network the compose file declares,
// and building the plan changes nothing on the host.
func TestPinningANetworkTakesTheStackDown(t *testing.T) {
	host := appliedFrom(t, "watch", plan(t))
	host.networks = map[string]string{statusNetwork: unpinnedPool}

	p, err := apply.Build("watch", external(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	action := statusAction(t, p)
	if !action.Down || !action.Recreate {
		t.Fatalf("a newly pinned network is not taken down and recreated: %+v", action)
	}
	if !strings.Contains(action.Reason, "10.255.255.0/29") {
		t.Errorf("the reason does not say which network: %q", action.Reason)
	}
	if host.ran("compose.yaml down") {
		t.Fatalf("building the plan took a stack down: %v", host.commands)
	}

	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	down := host.indexOf("status/compose.yaml down")
	up := host.indexOf("status/compose.yaml up -d")
	if down < 0 || up < 0 || down > up {
		t.Fatalf("want down then up for status, got %v", host.commands)
	}
}

// Leaving ingress mode external drops the pin, and a network still on the
// pinned pool would keep its old gateway, which TRUST_PROXY no longer names.
func TestDroppingAPinTakesTheStackDown(t *testing.T) {
	host := appliedFrom(t, "watch", external(t))
	host.networks = map[string]string{statusNetwork: pinnedPool}

	p, err := apply.Build("watch", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if action := statusAction(t, p); !action.Down {
		t.Fatalf("a network left on the old pin is not taken down: %+v", action)
	}
}

// A network that already matches what the compose file declares, pinned or
// not, plans nothing beyond the ordinary recreate.
func TestAMatchingNetworkIsLeftUp(t *testing.T) {
	for name, tc := range map[string]struct {
		rendered *render.Plan
		pool     string
	}{
		"pinned":   {external(t), pinnedPool},
		"unpinned": {plan(t), unpinnedPool},
	} {
		t.Run(name, func(t *testing.T) {
			host := appliedFrom(t, "watch", tc.rendered)
			host.networks = map[string]string{statusNetwork: tc.pool}
			p, err := apply.Build("watch", tc.rendered, acmeModule(t), host, apply.Recreate("status"))
			if err != nil {
				t.Fatal(err)
			}
			if action := statusAction(t, p); action.Down {
				t.Fatalf("a matching network is taken down: %+v", action)
			}
			if err := apply.Execute(p, host); err != nil {
				t.Fatal(err)
			}
			if host.ran("compose.yaml down") {
				t.Fatalf("a matching network was taken down: %v", host.commands)
			}
		})
	}
}

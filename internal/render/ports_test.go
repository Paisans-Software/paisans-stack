package render_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/render"
)

// Every listener names the paisans.yaml key that makes the site bind it, so
// a refusal over a port the host already uses can tell the operator what to
// change.
func TestEveryListenerNamesTheKeyThatClaimsIt(t *testing.T) {
	cfg := fixture(t)
	want := map[string]map[string]string{
		"home-a": {"WireGuard": "mesh", "etcd client": "etcd.members", "Postgres": "sites.home-a.roles (data)", "Garage S3 API": "storage.garage.sites"},
		"vm":     {"Caddy": "sites.vm.roles (gateway)"},
	}
	for site, owners := range want {
		listeners := render.SiteListeners(cfg, site)
		for _, l := range listeners {
			if l.Key == "" {
				t.Errorf("%s: %s %s names no key", site, l.Owner, l)
			}
			if strings.HasPrefix(l.Owner, "app ") {
				name := strings.Fields(l.Owner)[1]
				if l.Key != "apps."+name {
					t.Errorf("%s: %s has key %q, want apps.%s", site, l.Owner, l.Key, name)
				}
			}
		}
		for owner, key := range owners {
			found := false
			for _, l := range listeners {
				if l.Owner == owner {
					found = true
					if l.Key != key {
						t.Errorf("%s: %s has key %q, want %q", site, owner, l.Key, key)
					}
				}
			}
			if !found {
				t.Errorf("%s: no %s listener", site, owner)
			}
		}
	}
}

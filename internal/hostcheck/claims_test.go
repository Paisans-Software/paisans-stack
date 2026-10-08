package hostcheck_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
)

func fixture(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("testdata/hostcheck.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func claims(t *testing.T, site string) hostcheck.Claims {
	t.Helper()
	c, err := hostcheck.ClaimsFor(fixture(t), site)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func claimed(c hostcheck.Claims, spec, key string) bool {
	for _, l := range c.Listeners {
		if l.String() == spec && l.Key == key {
			return true
		}
	}
	return false
}

// A site claims what its roles run, each with the key that makes it, and
// nothing a role it does not hold would run.
func TestClaimsFollowRoles(t *testing.T) {
	data := claims(t, "home-a")
	for spec, key := range map[string]string{
		"*:51820/udp":        "mesh",
		"10.44.0.1:5432/tcp": "sites.home-a.roles (data)",
		"127.0.0.1:5432/tcp": "sites.home-a.roles (data)",
		"10.44.0.1:3900/tcp": "storage.garage.sites",
		"10.44.0.1:2379/tcp": "etcd.members",
		"10.44.0.1:5000/tcp": "cluster.port",
	} {
		if !claimed(data, spec, key) {
			t.Errorf("home-a does not claim %s for %s: %v", spec, key, data.Listeners)
		}
	}
	for _, l := range data.Listeners {
		if l.Port == 80 || l.Port == 443 {
			t.Errorf("a site without the gateway role claims %s", l)
		}
	}
	gateway := claims(t, "edge")
	for _, spec := range []string{"*:80/tcp", "*:443/tcp"} {
		if !claimed(gateway, spec, "sites.edge.roles (gateway)") {
			t.Errorf("the gateway does not claim %s: %v", spec, gateway.Listeners)
		}
	}
	if data.Site != "home-a" || data.Interface != "wg0" || data.Mesh.String() != "10.44.0.0/24" {
		t.Errorf("site %q, interface %q, mesh %v", data.Site, data.Interface, data.Mesh)
	}
}

func TestAnUndeclaredSiteHasNoClaims(t *testing.T) {
	if _, err := hostcheck.ClaimsFor(fixture(t), "nowhere"); err == nil {
		t.Error("an undeclared site has claims")
	}
}

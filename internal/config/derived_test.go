package config_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// derivedBase declares its sites out of name order, two witnesses and two
// data sites, and neither list, so the derivation's order is what a test
// sees.
const derivedBase = `version: 1
id: f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01
community:
  name: "Fixture"
  domain: example.org
mesh:
  subnet: 10.44.0.0/24
sites:
  vm:
    roles: [witness]
    address: 10.44.0.3
    ssh:
      host: site.example.org
      user: ubuntu
      public_key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
  home-b:
    roles: [data]
    address: 10.44.0.2
    ssh:
      host: site.example.org
      user: ubuntu
      public_key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
  box:
    roles: [witness]
    address: 10.44.0.5
    ssh:
      host: site.example.org
      user: ubuntu
      public_key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
  home-a:
    roles: [data, apps]
    address: 10.44.0.1
    ssh:
      host: site.example.org
      user: ubuntu
      public_key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
  watch:
    roles: [apps]
    address: 10.44.0.4
    ssh:
      host: site.example.org
      user: ubuntu
      public_key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
cluster:
  port: 5000
etcd:
  heartbeat_ms: 200
apps:
  talk:
    kind: mbin
    hostname: talk.example.org
    placement: cluster
`

func same(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestListsLeftOutAreDerivedFromTheRoles(t *testing.T) {
	cfg, err := config.Load(write(t, derivedBase))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"box", "vm", "home-a", "home-b"}; !same(cfg.Etcd.Members, want) {
		t.Errorf("etcd.members derived as %v, want %v: witnesses in name order, then data sites in name order", cfg.Etcd.Members, want)
	}
	if want := []string{"home-a", "home-b"}; !same(cfg.Cluster.Sites, want) {
		t.Errorf("cluster.sites derived as %v, want %v", cfg.Cluster.Sites, want)
	}
	if !cfg.Etcd.MembersDerived || !cfg.Cluster.SitesDerived {
		t.Errorf("derived lists not recorded as derived: members %t, sites %t", cfg.Etcd.MembersDerived, cfg.Cluster.SitesDerived)
	}
}

// With no etcd or cluster block at all, the lists are still derived.
func TestListsAreDerivedWithoutTheirBlocks(t *testing.T) {
	body := strings.Replace(derivedBase, "cluster:\n  port: 5000\netcd:\n  heartbeat_ms: 200\n", "", 1)
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if !same(cfg.Etcd.Members, []string{"box", "vm", "home-a", "home-b"}) || !same(cfg.Cluster.Sites, []string{"home-a", "home-b"}) {
		t.Errorf("derived %v and %v", cfg.Etcd.Members, cfg.Cluster.Sites)
	}
}

// A written list is used exactly as written, in its own order, even where it
// leaves out a site the roles would have put in.
func TestWrittenListsOverrideTheDerivation(t *testing.T) {
	body := strings.Replace(derivedBase, "  port: 5000\n", "  port: 5000\n  sites: [home-b, home-a]\n", 1)
	body = strings.Replace(body, "  heartbeat_ms: 200\n", "  heartbeat_ms: 200\n  members: [home-b]\n", 1)
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if !same(cfg.Etcd.Members, []string{"home-b"}) || !same(cfg.Cluster.Sites, []string{"home-b", "home-a"}) {
		t.Errorf("written lists loaded as %v and %v", cfg.Etcd.Members, cfg.Cluster.Sites)
	}
	if cfg.Etcd.MembersDerived || cfg.Cluster.SitesDerived {
		t.Errorf("written lists recorded as derived: members %t, sites %t", cfg.Etcd.MembersDerived, cfg.Cluster.SitesDerived)
	}
}

// Each list is derived or written on its own.
func TestOneListWrittenTheOtherDerived(t *testing.T) {
	body := strings.Replace(derivedBase, "  heartbeat_ms: 200\n", "  heartbeat_ms: 200\n  members: [home-a]\n", 1)
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Etcd.MembersDerived || !same(cfg.Etcd.Members, []string{"home-a"}) {
		t.Errorf("etcd.members %v, derived %t", cfg.Etcd.Members, cfg.Etcd.MembersDerived)
	}
	if !cfg.Cluster.SitesDerived || !same(cfg.Cluster.Sites, []string{"home-a", "home-b"}) {
		t.Errorf("cluster.sites %v, derived %t", cfg.Cluster.Sites, cfg.Cluster.SitesDerived)
	}
}

// A key written with an empty list, or with no value at all, is written, not
// left out, and an empty list means nothing that works.
func TestAWrittenEmptyListIsRefused(t *testing.T) {
	cases := map[string]struct{ from, to, key string }{
		"members empty":    {"  heartbeat_ms: 200\n", "  heartbeat_ms: 200\n  members: []\n", "etcd.members: empty"},
		"members no value": {"  heartbeat_ms: 200\n", "  heartbeat_ms: 200\n  members:\n", "etcd.members: empty"},
		"sites empty":      {"  port: 5000\n", "  port: 5000\n  sites: []\n", "cluster.sites: empty"},
		"sites no value":   {"  port: 5000\n", "  port: 5000\n  sites:\n", "cluster.sites: empty"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(write(t, strings.Replace(derivedBase, c.from, c.to, 1)))
			if err == nil {
				t.Fatal("a written empty list loaded")
			}
			if !strings.Contains(err.Error(), c.key) || !strings.Contains(err.Error(), "derived from the roles") {
				t.Errorf("the refusal does not name the key and the way out: %v", err)
			}
		})
	}
}

// A message about a list names it as the file has it: a derived list says so,
// so an operator who never wrote the key is not sent looking for it.
func TestListKeysSayWhetherTheyWereDerived(t *testing.T) {
	cfg, err := config.Load(write(t, derivedBase))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EtcdMembersKey() != "etcd.members (derived from the roles)" || cfg.ClusterSitesKey() != "cluster.sites (derived from the roles)" {
		t.Errorf("derived keys named %q and %q", cfg.EtcdMembersKey(), cfg.ClusterSitesKey())
	}
	cfg.Etcd.MembersDerived, cfg.Cluster.SitesDerived = false, false
	if cfg.EtcdMembersKey() != "etcd.members" || cfg.ClusterSitesKey() != "cluster.sites" {
		t.Errorf("written keys named %q and %q", cfg.EtcdMembersKey(), cfg.ClusterSitesKey())
	}
}

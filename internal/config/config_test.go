package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/josephquigley/paisans-stack/internal/config"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `version: 1
community:
  name: "Fixture"
  domain: example.org
mesh:
  subnet: 10.44.0.0/24
sites:
  home-a:
    roles: [data, apps]
    address: 10.44.0.1
    ssh: home-a.local
etcd:
  members: [home-a]
apps:
  talk:
    kind: mbin
    hostname: talk.example.org
    placement: cluster
`

func TestLoadsAMinimalFile(t *testing.T) {
	cfg, err := config.Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Apps["talk"].Placement.Mode != config.PlacementCluster {
		t.Fatalf("placement parsed as %q", cfg.Apps["talk"].Placement.Mode)
	}
	if !cfg.Sites["home-a"].Has(config.RoleData) {
		t.Fatal("the data role was not parsed")
	}
}

func TestPinnedPlacement(t *testing.T) {
	body := strings.Replace(minimal, "placement: cluster", "placement: { pinned: home-a }", 1)
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	placement := cfg.Apps["talk"].Placement
	if placement.Mode != config.PlacementPinned || placement.Site != "home-a" {
		t.Fatalf("pinned placement parsed as %+v", placement)
	}
}

// An unrecognised placement is carried through the decode rather than
// aborting it, so that validate can report it beside every other problem in
// the file.
func TestInvalidPlacementSurvivesTheDecode(t *testing.T) {
	body := strings.Replace(minimal, "placement: cluster", "placement: standby", 1)
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Apps["talk"].Placement.Mode != config.PlacementInvalid {
		t.Fatal("an invalid placement was not recorded as invalid")
	}
}

// Every structural problem in a file is reported at once. Fixing a
// configuration one error per run is miserable.
func TestStructuralProblemsAreReportedTogether(t *testing.T) {
	body := `version: 2
community:
  name: "Fixture"
mesh:
  subnet: 10.44.0.5/24
sites:
  home-a:
    roles: [data, wizard]
    address: not-an-address
apps:
  talk:
    kind: gopher
    hostname: ""
`
	_, err := config.Load(write(t, body))
	if err == nil {
		t.Fatal("a broken file loaded cleanly")
	}
	msg := err.Error()
	for _, want := range []string{
		"version:", "community.domain:", "mesh.subnet:", "sites.home-a.roles:", "sites.home-a.address:",
		"sites.home-a.ssh:", "apps.talk.kind:", "apps.talk.hostname:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not name %s:\n%s", want, msg)
		}
	}
}

// Unknown keys are refused. trusted_proxies is deliberately not in the schema,
// and a typo that silently does nothing is worse than a refusal.
func TestUnknownKeysAreRefused(t *testing.T) {
	body := minimal + "trusted_proxies: 10.44.0.1\n"
	_, err := config.Load(write(t, body))
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if !strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("the error does not name the offending key:\n%v", err)
	}
}

// The shipped example must load. It is the documentation of the format.
func TestExampleLoads(t *testing.T) {
	if _, err := config.Load(filepath.Join("..", "..", "examples", "paisans.example.yaml")); err != nil {
		t.Fatal(err)
	}
}

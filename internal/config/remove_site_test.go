package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

const removeFile = `version: 1
id: f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01   # never changes

community:
  name: Example
  domain: example.org

mesh:
  subnet: 10.44.0.0/24

sites:
  home-a:
    roles: [data, apps]   # the first one
    address: 10.44.0.1
  # The second data site, joined later.
  home-b:
    roles: [data]
    address: 10.44.0.2
    # its own comment

  # The third.
  home-c:
    roles: [data]
    address: 10.44.0.5
  vm: { roles: [gateway, witness], address: 10.44.0.3, endpoint: vm.example.org:51820 }

cluster:
  sites: [home-a, home-b, home-c]   # all of them
etcd:
  members:
    - home-a
    - home-b   # joined second
    - home-c
    - vm
  heartbeat_ms: 200

storage:
  garage:
    sites: [home-a, home-b, home-c]
    replication: 2
    capacities:
      home-a: 100G
      home-b: 50G
`

func TestRemoveSiteKeepsEveryOtherByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(removeFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.RemoveSite(path, "home-b"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := strings.NewReplacer(
		"  # The second data site, joined later.\n  home-b:\n    roles: [data]\n    address: 10.44.0.2\n    # its own comment\n", "",
		"[home-a, home-b, home-c]   # all of them", "[home-a, home-c]   # all of them",
		"    - home-b   # joined second\n", "",
		"sites: [home-a, home-b, home-c]\n    replication", "sites: [home-a, home-c]\n    replication",
		"      home-b: 50G\n", "",
	).Replace(removeFile)
	if string(got) != want {
		t.Errorf("RemoveSite wrote:\n%s\nwant:\n%s", got, want)
	}
}

func TestRemoveSiteLastEntryAndFlowEntry(t *testing.T) {
	for _, site := range []string{"vm", "home-c"} {
		path := filepath.Join(t.TempDir(), "paisans.yaml")
		if err := os.WriteFile(path, []byte(removeFile), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := config.RemoveSite(path, site); err != nil {
			t.Fatalf("%s: %v", site, err)
		}
		got, _ := os.ReadFile(path)
		if strings.Contains(string(got), site+":") || strings.Contains(string(got), "- "+site+"\n") {
			t.Errorf("%s is still in the file:\n%s", site, got)
		}
		if !strings.Contains(string(got), "\ncluster:\n") || !strings.Contains(string(got), "# The second data site, joined later.") {
			t.Errorf("removing %s took more than its own lines:\n%s", site, got)
		}
	}
}

// The edit and the in memory end state agree.
func TestRemoveSiteMatchesWithoutSite(t *testing.T) {
	cfg := loadFixtureConfig(t)
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.RemoveSite(path, "home-b"); err != nil {
		t.Fatal(err)
	}
	edited, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := cfg.WithoutSite("home-b")
	if !reflect.DeepEqual(edited.SiteNames(), want.SiteNames()) ||
		!reflect.DeepEqual(edited.Cluster.Sites, want.Cluster.Sites) ||
		!reflect.DeepEqual(edited.Etcd.Members, want.Etcd.Members) ||
		!reflect.DeepEqual(edited.Storage.Garage.Sites, want.Storage.Garage.Sites) {
		t.Errorf("the file says %v %v %v %v, WithoutSite %v %v %v %v",
			edited.SiteNames(), edited.Cluster.Sites, edited.Etcd.Members, edited.Storage.Garage.Sites,
			want.SiteNames(), want.Cluster.Sites, want.Etcd.Members, want.Storage.Garage.Sites)
	}
	if _, ok := cfg.Sites["home-b"]; !ok || len(cfg.Cluster.Sites) != 2 {
		t.Error("WithoutSite changed the original")
	}
}

func TestRemoveSiteRefusesWhatItCannotEditInPlace(t *testing.T) {
	for name, body := range map[string]string{
		"only item":        strings.Replace(removeFile, "sites: [home-a, home-b, home-c]   # all of them", "sites: [home-b]", 1),
		"flow over lines":  strings.Replace(removeFile, "sites: [home-a, home-b, home-c]   # all of them", "sites: [home-a,\n    home-b, home-c]", 1),
		"undeclared":       strings.Replace(removeFile, "  home-b:\n", "  home-x:\n", 1),
		"flow sites block": "version: 1\nsites: { home-a: {}, home-b: {} }\n",
	} {
		path := filepath.Join(t.TempDir(), "paisans.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := config.RemoveSite(path, "home-b"); err == nil {
			t.Errorf("%s: RemoveSite did not refuse", name)
		}
		if after, _ := os.ReadFile(path); string(after) != body {
			t.Errorf("%s: a refused edit changed the file", name)
		}
	}
}

func loadFixtureConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// derivedFile is removeFile with cluster.sites and etcd.members left out.
var derivedFile = strings.NewReplacer(
	"  sites: [home-a, home-b, home-c]   # all of them\n", "",
	"  members:\n    - home-a\n    - home-b   # joined second\n    - home-c\n    - vm\n", "",
).Replace(removeFile)

// A list the file leaves out is not added, and the site's block is the edit.
func TestRemoveSiteLeavesDerivedListsOut(t *testing.T) {
	if derivedFile == removeFile || strings.Contains(derivedFile, "members:") {
		t.Fatal("the fixture still writes the lists")
	}
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(derivedFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.RemoveSite(path, "home-b"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := strings.NewReplacer(
		"  # The second data site, joined later.\n  home-b:\n    roles: [data]\n    address: 10.44.0.2\n    # its own comment\n", "",
		"sites: [home-a, home-b, home-c]\n    replication", "sites: [home-a, home-c]\n    replication",
		"      home-b: 50G\n", "",
	).Replace(derivedFile)
	if string(got) != want {
		t.Errorf("RemoveSite wrote:\n%s\nwant:\n%s", got, want)
	}
}

// The edit and the in memory end state agree for derived lists too, and the
// end state still knows they are derived.
func TestRemoveSiteMatchesWithoutSiteWhenDerived(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := regexp.MustCompile(`(?m)^  (sites|members): \[[^\]]*\]\n`).ReplaceAllString(string(data), "")
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Etcd.MembersDerived || !cfg.Cluster.SitesDerived {
		t.Fatal("the fixture still writes the lists")
	}
	want := cfg.WithoutSite("home-b")
	if err := config.RemoveSite(path, "home-b"); err != nil {
		t.Fatal(err)
	}
	edited, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(edited.Cluster.Sites, want.Cluster.Sites) || !reflect.DeepEqual(edited.Etcd.Members, want.Etcd.Members) {
		t.Errorf("the file derives %v %v, WithoutSite %v %v", edited.Cluster.Sites, edited.Etcd.Members, want.Cluster.Sites, want.Etcd.Members)
	}
	if !want.Etcd.MembersDerived || !want.Cluster.SitesDerived {
		t.Error("WithoutSite lost the record that the lists are derived")
	}
}

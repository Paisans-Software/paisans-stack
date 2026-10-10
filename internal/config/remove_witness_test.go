package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

const witnessFile = `version: 1
sites:
  home-a:
    roles: [data, apps]
    address: 10.44.0.1
  home-b:
    roles: [data]
    address: 10.44.0.2
  vm:
    # The gateway, and the witness while two data sites run.
    roles:
      - gateway   # public
      - witness
    address: 10.44.0.3
etcd:
  members:   # the voters
    - home-a
    - home-b
    - vm
`

// The site, the witness's membership and its role go in one write, and every
// other line stays as it was.
func TestRemoveSiteAndWitness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(witnessFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.RemoveSiteAndWitness(path, "home-b", "vm"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := strings.NewReplacer(
		"  home-b:\n    roles: [data]\n    address: 10.44.0.2\n", "",
		"      - witness\n", "",
		"    - home-b\n", "",
		"    - vm\n", "",
	).Replace(witnessFile)
	if string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
}

// A witness role written alone on a block list cannot become an empty list in
// place, and the file is left as it was.
func TestRemoveSiteAndWitnessRefusesALoneBlockRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	text := strings.Replace(witnessFile, "      - gateway   # public\n", "", 1)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	err := config.RemoveSiteAndWitness(path, "home-b", "vm")
	if err == nil || !strings.Contains(err.Error(), "names only witness") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != text {
		t.Error("the file changed")
	}
}

// With etcd.members left out, the witness's role coming off is what takes it
// out of etcd, so that and the site's block are the whole edit, and the file
// loads with the one data site as the one voter.
func TestRemoveSiteAndWitnessWithDerivedMembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	text := witnessFile[:strings.Index(witnessFile, "etcd:\n")]
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.RemoveSiteAndWitness(path, "home-b", "vm"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := strings.NewReplacer(
		"  home-b:\n    roles: [data]\n    address: 10.44.0.2\n", "",
		"      - witness\n", "",
	).Replace(text)
	if string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
}

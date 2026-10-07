package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
)

// emptyHost is a machine with nothing on it, which is all a plan for a first
// apply needs to read.
type emptyHost struct{}

func (emptyHost) Describe() string { return "home-a.local" }
func (emptyHost) Run(command string) (string, error) {
	// Every image is present, so the plan carries no disk check; apply's
	// own tests cover that.
	if list, ok := strings.CutPrefix(command, "for r in "); ok {
		list, _, _ = strings.Cut(list, "; do")
		var out strings.Builder
		for _, ref := range strings.Fields(list) {
			out.WriteString("present sha256:0123456789abcdef " + strings.Trim(ref, "'") + "\n")
		}
		return out.String(), nil
	}
	return "down\n", nil
}
func (emptyHost) RunInput(string, string) (string, error) { return "", nil }
func (emptyHost) ReadFile(string) (string, bool, error)   { return "", false, nil }
func (emptyHost) WriteFile(string, string, uint32) error  { return nil }

const freshSite = `version: 1
community:
  name: Example
  domain: example.org
mesh:
  subnet: 10.44.0.0/24
acme:
  provider: desec
sites:
  home-a:
    roles: [data, apps]
    address: 10.44.0.1
    ssh:
      host: home-a.local
      user: ubuntu
      public_key: |
        ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
cluster:
  sites: [home-a]
  port: 5000
  postgres_version: "18"
etcd:
  members: [home-a]
storage:
  garage:
    sites: [home-a]
    replication: 1
apps:
  talk:
    kind: mbin
    hostname: talk.example.org
    placement: cluster
`

// A first apply on a fresh data and apps site shows the mesh first, then the
// infrastructure, then the wait and the database, and only then the app: the
// order Execute runs them in, so an operator reading a dry run reads the real
// sequence.
func TestAFreshSitePlanShowsTheDatabaseBeforeTheApp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(freshSite), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &config.Secrets{Version: 1}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatal(err)
	}
	rendered, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := apply.Build("home-a", rendered, acme.Module(cfg.ACME.Provider), emptyHost{})
	if err != nil {
		t.Fatal(err)
	}
	databases, err := apply.Databases(cfg, secrets, "home-a")
	if err != nil {
		t.Fatal(err)
	}
	plan.WithDatabases(databases)

	printed := captureStdout(t, func() { printPlan(plan) })
	t.Log("\n" + printed)

	order := []string{"mesh ", "recreate  infra", "check     infra:", "wait ", "bootstrap database talk", "recreate  talk", "check     talk:"}
	last := -1
	for _, want := range order {
		i := strings.Index(printed, want)
		if i < 0 {
			t.Fatalf("the plan does not show %q:\n%s", want, printed)
		}
		if i < last {
			t.Errorf("%q is shown out of order:\n%s", want, printed)
		}
		last = i
	}
	if password, _ := secrets.Apps["talk"]["database_password"].(string); strings.Contains(printed, password) {
		t.Error("the plan printed a database password")
	}
}

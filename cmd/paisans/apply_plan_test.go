package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// emptyHost is a machine with nothing on it, which is all a plan for a first
// apply needs to read.
type emptyHost struct{}

func (emptyHost) Describe() string { return "home-a.local" }
func (emptyHost) Run(command string) (string, error) {
	if out, ok := absentNetworks(command); ok {
		return out, nil
	}
	// Every image is present, so the plan carries no disk check; apply's
	// own tests cover that.
	if list, ok := strings.CutPrefix(command, "for r in "); ok {
		list, _, _ = strings.Cut(list, "; do")
		// The volume probe asks the same loop what each image declares:
		// nothing, here.
		answer := "present sha256:0123456789abcdef "
		if strings.Contains(command, ".Config.Volumes") {
			answer = "volumes "
		}
		var out strings.Builder
		for _, ref := range strings.Fields(list) {
			out.WriteString(answer + strings.Trim(ref, "'"))
			if answer == "volumes " {
				out.WriteString(" null")
			}
			out.WriteString("\n")
		}
		return out.String(), nil
	}
	return "down\n", nil
}

// absentNetworks answers apply's probe of the stacks' compose networks for a
// host that has none of them, false for any other command.
func absentNetworks(command string) (string, bool) {
	list, ok := strings.CutPrefix(command, "for n in ")
	if !ok {
		return "", false
	}
	list, _, _ = strings.Cut(list, "; do")
	var out strings.Builder
	for _, name := range strings.Fields(list) {
		out.WriteString("absent " + strings.Trim(name, "'") + "\n")
	}
	return out.String(), true
}

func (emptyHost) RunInput(string, string) (string, error) { return "", nil }
func (emptyHost) ReadFile(string) (string, bool, error)   { return "", false, nil }
func (emptyHost) WriteFile(string, string, uint32) error  { return nil }

const freshSite = `version: 1
id: f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01
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

	rec := &ui.Recorder{Verbose_: true}
	listPlan(rec, plan)
	t.Log("\n" + rec.Lines())

	order := []string{"start mesh", "recreate infra", "wait for infra", "wait for Patroni primary", "bootstrap database talk", "recreate talk", "wait for talk"}
	last := -1
	for _, want := range order {
		i := rec.Index("item", want)
		if i < 0 {
			t.Fatalf("the plan does not show %q:\n%s", want, rec.Lines())
		}
		if i < last {
			t.Errorf("%q is shown out of order:\n%s", want, rec.Lines())
		}
		last = i
	}
	if password, _ := secrets.Apps["talk"]["database_password"].(string); strings.Contains(rec.Lines(), password) {
		t.Error("the plan printed a database password")
	}
}

// freshPlan is the plan for a first apply of freshSite on home-a, reached
// through t and reporting to r, as runApply builds it.
func freshPlan(tb testing.TB, r ui.Reporter, t apply.Transport) *apply.Plan {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(freshSite), 0o600); err != nil {
		tb.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		tb.Fatal(err)
	}
	secrets := &config.Secrets{Version: 1}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		tb.Fatal(err)
	}
	plan, err := planSiteApply(cfg, secrets, "home-a", t, apply.Report(r))
	if err != nil {
		tb.Fatal(err)
	}
	return plan
}

// failingHost is emptyHost, except that a command containing match fails
// with message, as a daemon's refusal comes back over ssh.
type failingHost struct {
	emptyHost
	match, message string
}

func (h failingHost) Run(command string) (string, error) {
	if strings.Contains(command, h.match) {
		return h.message + "\n", errors.New(h.message)
	}
	// No container runs yet, which is what a stack's `ps` says before its
	// first `up`.
	if strings.HasSuffix(command, " ps --all --format json") {
		return "", nil
	}
	return h.emptyHost.Run(command)
}

func (h failingHost) RunInput(command, stdin string) (string, error) {
	if strings.Contains(command, h.match) {
		return h.message + "\n", errors.New(h.message)
	}
	return h.emptyHost.RunInput(command, stdin)
}

// failOn is a host on which the command containing match fails with message.
func failOn(match, message string) apply.Transport {
	return failingHost{match: match, message: message}
}

// executeForTest plans freshSite on home-a through t and executes it,
// reporting both to r.
func executeForTest(tb testing.TB, r ui.Reporter, t apply.Transport) error {
	tb.Helper()
	return apply.Execute(freshPlan(tb, r, t), t)
}

// A step that fails is the last step the operator sees end, and the error
// returned for main to print carries the host's own words.
func TestExecuteFailureShowsFailedStepAndFullError(t *testing.T) {
	rec := &ui.Recorder{}
	err := executeForTest(t, rec, failOn("infra/compose.yaml up -d", "Error response from daemon: port is already allocated"))
	if err == nil || !strings.Contains(err.Error(), "port is already allocated") {
		t.Fatalf("error lost its detail: %v", err)
	}
	last := -1
	for i, e := range rec.Events {
		if e.Kind == "done" || e.Kind == "fail" {
			last = i
		}
	}
	if last < 0 || rec.Events[last].Kind != "fail" || rec.Events[last].Text != "recreate infra" {
		t.Fatalf("last ended step is not the failed one:\n%s", rec.Lines())
	}
}

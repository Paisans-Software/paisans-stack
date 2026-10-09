package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/appadmin"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

const adminPassword = "not-a-real-password-0002"

func fixtureConfig() string {
	return filepath.Join("..", "..", "internal", "render", "testdata", "deployment.yaml")
}

func TestAdminSite(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := adminSite(cfg, "talk", "", "app admin create"); got != "home-a" {
		t.Errorf("cluster default %q, want the first apps site", got)
	}
	if got, _ := adminSite(cfg, "talk", "home-b", "app admin create"); got != "home-b" {
		t.Errorf("override %q", got)
	}
	if _, err := adminSite(cfg, "talk", "vm", "app admin create"); err == nil {
		t.Error("a site the app does not run on was accepted")
	}
	if got, _ := adminSite(cfg, "chat", "", "app admin create"); got != "vm" {
		t.Errorf("pinned %q, want vm", got)
	}
}

// pidFake is a Pocket ID with no users and no groups until they are created,
// reached through curl configs.
type pidFake struct {
	registry    *registryFake
	destination string
	sudo        bool
	stdins      []string
	commands    []string
	mutated     bool
	created     bool
	group       bool
	joined      bool
}

func (f *pidFake) Describe() string           { return f.destination }
func (f *pidFake) Run(string) (string, error) { return "", errors.New("unexpected Run") }
func (f *pidFake) RunInput(command, stdin string) (string, error) {
	f.commands = append(f.commands, command)
	f.stdins = append(f.stdins, stdin)
	switch {
	case strings.Contains(stdin, `request = "GET"`) && strings.Contains(stdin, "/api/user-groups"):
		if f.group {
			return `{"data":[{"id":"g-1","name":"admins","friendlyName":"admins"}],"pagination":{"totalPages":1}}` + "\npaisans-http-status:200", nil
		}
		return `{"data":[],"pagination":{"totalPages":0}}` + "\npaisans-http-status:200", nil
	case strings.Contains(stdin, `request = "POST"`) && strings.Contains(stdin, "/api/user-groups"):
		f.mutated = true
		f.group = true
		return `{"id":"g-1","name":"admins","friendlyName":"admins"}` + "\npaisans-http-status:201", nil
	case strings.Contains(stdin, `request = "PUT"`) && strings.Contains(stdin, "/api/users/u-1/user-groups"):
		f.mutated = true
		f.joined = strings.Contains(stdin, "g-1")
		return `{}` + "\npaisans-http-status:200", nil
	case strings.Contains(stdin, `request = "GET"`) && strings.Contains(stdin, "/api/users?"):
		if f.created {
			groups := ""
			if f.joined {
				groups = `,"userGroups":[{"id":"g-1","name":"admins"}]`
			}
			return `{"data":[{"id":"u-1","username":"founder","isAdmin":true` + groups + `}],"pagination":{"totalPages":1}}` + "\npaisans-http-status:200", nil
		}
		return `{"data":[],"pagination":{"totalPages":0}}` + "\npaisans-http-status:200", nil
	case strings.Contains(stdin, "one-time-access-token"):
		f.mutated = true
		return `{"token":"tok-not-real-0009"}` + "\npaisans-http-status:201", nil
	case strings.Contains(stdin, `request = "POST"`):
		f.mutated = true
		f.created = true
		return `{"id":"u-1","username":"founder","isAdmin":true}` + "\npaisans-http-status:201", nil
	}
	return `{"error":"no route"}` + "\npaisans-http-status:404", nil
}

func withPIDFake(t *testing.T) *pidFake {
	t.Helper()
	fake := &pidFake{}
	fake.registry = withRegistryFake(t)
	saved := adminTransport
	adminTransport = func(tr apply.SSHTransport) appadmin.Transport {
		fake.destination = tr.Describe()
		fake.sudo = tr.Sudo
		return fake
	}
	t.Cleanup(func() { adminTransport = saved })
	return fake
}

func fixtureSecretsPath() string {
	return filepath.Join("..", "..", "internal", "render", "testdata", "secrets.fixture.yaml")
}

func pidArgs(extra ...string) []string {
	return append([]string{"--config", fixtureConfig(), "--secrets", fixtureSecretsPath(), "--app", "auth", "--username", "founder", "--email", "founder@example.org"}, extra...)
}

func fixtureAPIKey(t *testing.T) string {
	t.Helper()
	secrets, err := config.LoadSecrets(fixtureSecretsPath())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := secrets.Apps["auth"]["static_api_key"].(string)
	if key == "" {
		t.Fatal("the fixture has no static API key")
	}
	return key
}

func TestPocketIDAdminDryRunPlansWithoutSudoAndChangesNothing(t *testing.T) {
	fake := withPIDFake(t)
	key := fixtureAPIKey(t)
	rec := withRecorder(t, true)
	var err error
	stdout := captureStdout(t, func() {
		err = runAppAdminCreate(pidArgs(), strings.NewReader(""))
	})
	if err != nil {
		t.Fatal(err)
	}
	printed := rec.Lines() + stdout
	if stdout != "" {
		t.Errorf("a dry run wrote to stdout, which carries only the login link:\n%s", stdout)
	}
	for _, want := range []string{"section: auth on home-a (pocket-id)", "item: create administrator founder", "item: issue login link for founder", "detail: create user founder as an administrator", "result: Nothing changed"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	if fake.mutated {
		t.Error("a dry run mutated")
	}
	if fake.sudo {
		t.Error("curl was run through sudo")
	}
	if fake.registry.claims != 0 || fake.registry.reads != 0 {
		t.Error("a dry run reached the host registry")
	}
	if strings.Contains(printed, key) {
		t.Error("the output carries the API key")
	}
	for i, c := range fake.commands {
		if strings.Contains(c, key) {
			t.Errorf("a command line carries the API key: %s", c)
		}
		if !strings.Contains(fake.stdins[i], "http://10.44.0.1:1411/api/") {
			t.Errorf("the request did not go to the mesh port on home-a")
		}
	}
}

func TestPocketIDAdminExecutePrintsTheLinkOnce(t *testing.T) {
	fake := withPIDFake(t)
	rec := withRecorder(t, true)
	var err error
	stdout := captureStdout(t, func() {
		err = runAppAdminCreate(pidArgs("--execute"), strings.NewReader(""))
	})
	if err != nil {
		t.Fatal(err)
	}
	link := "https://id.example.org/lc/tok-not-real-0009"
	// The link is the only thing on stdout, so a script can read it, and it
	// is nowhere in the report.
	if strings.TrimSpace(stdout) != link {
		t.Errorf("stdout is not the link alone:\n%s", stdout)
	}
	if strings.Contains(rec.Lines(), link) {
		t.Errorf("the link reached the report:\n%s", rec.Lines())
	}
	if !rec.Has("warn", "20m0s") {
		t.Errorf("the expiry is not stated:\n%s", rec.Lines())
	}
	if !rec.Has("result", "founder is an administrator of auth") {
		t.Errorf("no result:\n%s", rec.Lines())
	}
	if fake.registry.claims != 1 || !fake.registry.sudo {
		t.Errorf("--execute claimed the host %d time(s), sudo %v; want once, through sudo", fake.registry.claims, fake.registry.sudo)
	}
}

func TestPocketIDAdminRefusesAPipedPassword(t *testing.T) {
	fake := withPIDFake(t)
	err := runAppAdminCreate(pidArgs(), strings.NewReader(adminPassword+"\n"))
	if err == nil || !strings.Contains(err.Error(), "passkeys") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), adminPassword) {
		t.Error("the refusal carries what was piped")
	}
	if len(fake.commands) != 0 {
		t.Error("a host was reached")
	}
}

func TestAppAdminCreateRefusesAnUnimplementedKind(t *testing.T) {
	fake := withPIDFake(t)
	err := runAppAdminCreate([]string{"--config", fixtureConfig(), "--app", "docs", "--username", "u", "--email", "u@example.org"}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "outline admin creation is not implemented yet. Implemented kinds: pocket-id") {
		t.Errorf("got %v", err)
	}
	if len(fake.commands) != 0 {
		t.Error("a host was reached")
	}
}

// An Mbin app's administrators come only through single sign on, so it is
// refused before a site is resolved, pointing at the command run against
// Pocket ID.
func TestAppAdminCreateRefusesMbinWithTheSSORoute(t *testing.T) {
	fake := withPIDFake(t)
	saved := standbyLook
	looked := false
	standbyLook = func(tr apply.SSHTransport) apply.Transport { looked = true; return tr }
	t.Cleanup(func() { standbyLook = saved })
	err := runAppAdminCreate([]string{"--config", fixtureConfig(), "--app", "talk", "--username", "founder", "--execute"}, strings.NewReader(""))
	want := "talk is mbin, whose administrators come only through single sign on. Run `paisans app admin create --app auth --username founder --email <e>`"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("got %v", err)
	}
	if len(fake.commands) != 0 || looked {
		t.Error("a host was reached")
	}
}

// Default output names what will happen; the requests behind it show only
// with --verbose, and neither carries the API key.
func TestPocketIDAdminDryRunKeepsRequestBodiesForVerbose(t *testing.T) {
	withPIDFake(t)
	key := fixtureAPIKey(t)
	for _, verbose := range []bool{false, true} {
		var b strings.Builder
		reporterOverride = ui.NewPlain(&b, verbose)
		err := runAppAdminCreate(pidArgs(), strings.NewReader(""))
		reporterOverride = nil
		if err != nil {
			t.Fatal(err)
		}
		out := b.String()
		if strings.Contains(out, key) {
			t.Errorf("verbose=%v: the output carries the API key", verbose)
		}
		if got := strings.Contains(out, "POST "); got != verbose {
			t.Errorf("verbose=%v: output has a POST request: %v\n%s", verbose, got, out)
		}
		if !strings.Contains(out, "create administrator founder") {
			t.Errorf("verbose=%v: no short title:\n%s", verbose, out)
		}
	}
}

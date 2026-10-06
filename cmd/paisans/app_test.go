package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/appadmin"
	"github.com/paisans-software/paisans-stack/internal/config"
)

const adminPassword = "not-a-real-password-0002"

// adminFake answers every probe with one fixed line and records each call.
type adminFake struct {
	destination string
	probe       string
	commands    []string
	mutated     bool
}

func (f *adminFake) Describe() string { return f.destination }
func (f *adminFake) Run(command string) (string, error) {
	f.commands = append(f.commands, command)
	return "", errors.New("unexpected Run")
}
func (f *adminFake) RunInput(command, stdin string) (string, error) {
	f.commands = append(f.commands, command)
	if !strings.Contains(stdin, "findOneByUsername") {
		f.mutated = true
	}
	return f.probe, nil
}

func withAdminFake(t *testing.T, probe string) *adminFake {
	t.Helper()
	fake := &adminFake{probe: probe}
	saved := adminTransport
	adminTransport = func(destination string, _ bool) appadmin.Transport {
		fake.destination = destination
		return fake
	}
	t.Cleanup(func() { adminTransport = saved })
	return fake
}

func fixtureConfig() string {
	return filepath.Join("..", "..", "internal", "render", "testdata", "deployment.yaml")
}

func TestAppAdminCreateDryRunPlansAndChangesNothing(t *testing.T) {
	fake := withAdminFake(t, "paisans-admin-probe {\"exists\":false,\"verified\":false,\"admin\":false}\n")
	var err error
	printed := captureStdout(t, func() {
		err = runAppAdminCreate([]string{"--config", fixtureConfig(), "--app", "talk", "--username", "founder", "--email", "founder@example.org"}, strings.NewReader(adminPassword+"\n"))
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"talk on home-a (mbin)", "create user founder", "verify founder", "grant admin founder", "Nothing was changed"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	if fake.mutated {
		t.Error("a dry run ran a mutating script")
	}
	if fake.destination != "home-a.local" {
		t.Errorf("reached %q, want the first apps site's ssh address", fake.destination)
	}
	if strings.Contains(printed, adminPassword) {
		t.Error("the output carries the password")
	}
	for _, c := range fake.commands {
		if strings.Contains(c, adminPassword) {
			t.Errorf("a command line carries the password: %s", c)
		}
	}
}

func TestAppAdminCreateRefusesAnUnimplementedKind(t *testing.T) {
	withAdminFake(t, "")
	err := runAppAdminCreate([]string{"--config", fixtureConfig(), "--app", "docs", "--username", "u", "--email", "u@example.org"}, strings.NewReader(adminPassword))
	if err == nil || !strings.Contains(err.Error(), "outline admin creation is not implemented yet. Implemented kinds: mbin, pocket-id") {
		t.Errorf("got %v", err)
	}
}

func TestReadPassword(t *testing.T) {
	if got, err := readPassword(strings.NewReader("pw\n")); err != nil || got != "pw" {
		t.Errorf("got %q, %v; want one trailing newline stripped", got, err)
	}
	if got, _ := readPassword(strings.NewReader("pw\n\n")); got != "pw\n" {
		t.Errorf("got %q; want only one newline stripped", got)
	}
	if _, err := readPassword(strings.NewReader("\n")); err == nil {
		t.Error("an empty password was accepted")
	}
}

func TestAdminSite(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := adminSite(cfg, "talk", ""); got != "home-a" {
		t.Errorf("cluster default %q, want the first apps site", got)
	}
	if got, _ := adminSite(cfg, "talk", "home-b"); got != "home-b" {
		t.Errorf("override %q", got)
	}
	if _, err := adminSite(cfg, "talk", "vm"); err == nil {
		t.Error("a site the app does not run on was accepted")
	}
	if got, _ := adminSite(cfg, "chat", ""); got != "vm" {
		t.Errorf("pinned %q, want vm", got)
	}
}

// pidFake is a Pocket ID with no users until one is created, reached through
// curl configs.
type pidFake struct {
	destination string
	sudo        bool
	stdins      []string
	commands    []string
	mutated     bool
	created     bool
}

func (f *pidFake) Describe() string           { return f.destination }
func (f *pidFake) Run(string) (string, error) { return "", errors.New("unexpected Run") }
func (f *pidFake) RunInput(command, stdin string) (string, error) {
	f.commands = append(f.commands, command)
	f.stdins = append(f.stdins, stdin)
	switch {
	case strings.Contains(stdin, `request = "GET"`) && strings.Contains(stdin, "/api/users?"):
		if f.created {
			return `{"data":[{"id":"u-1","username":"founder","isAdmin":true}],"pagination":{"totalPages":1}}` + "\npaisans-http-status:200", nil
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
	saved := adminTransport
	adminTransport = func(destination string, sudo bool) appadmin.Transport {
		fake.destination = destination
		fake.sudo = sudo
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
	var err error
	printed := captureStdout(t, func() {
		err = runAppAdminCreate(pidArgs(), strings.NewReader(""))
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"auth on home-a (pocket-id)", "create user founder as an administrator", "issue one-time login link for founder", "Nothing was changed"} {
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
	withPIDFake(t)
	var err error
	printed := captureStdout(t, func() {
		err = runAppAdminCreate(pidArgs("--execute"), strings.NewReader(""))
	})
	if err != nil {
		t.Fatal(err)
	}
	link := "https://id.example.org/lc/tok-not-real-0009"
	if n := strings.Count(printed, link); n != 1 {
		t.Errorf("the link was printed %d times:\n%s", n, printed)
	}
	if !strings.Contains(printed, "20m0s") {
		t.Errorf("the expiry is not stated:\n%s", printed)
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

func TestAdminFlagsBelongToTheirKinds(t *testing.T) {
	withPIDFake(t)
	if err := runAppAdminCreate(pidArgs("--reset-password"), strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "--login-link") {
		t.Errorf("pocket-id --reset-password: %v", err)
	}
	withAdminFake(t, "")
	err := runAppAdminCreate([]string{"--config", fixtureConfig(), "--app", "talk", "--username", "u", "--email", "u@example.org", "--login-link"}, strings.NewReader(adminPassword))
	if err == nil || !strings.Contains(err.Error(), "passwordless") {
		t.Errorf("mbin --login-link: %v", err)
	}
}

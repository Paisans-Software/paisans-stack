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
	if err == nil || !strings.Contains(err.Error(), "outline admin creation is not implemented yet. Implemented kinds: mbin") {
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

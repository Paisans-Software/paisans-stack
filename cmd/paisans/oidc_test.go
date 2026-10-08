package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
)

// idpFake is a Pocket ID with no clients until one is created.
type idpFake struct {
	destination string
	commands    []string
	mutated     bool
	client      *pocketid.OIDCClient
	prefixes    []string
	// launch is the launch URL a create must send. Empty means Mbin's
	// default, the fork's connect route.
	launch string
}

func (f *idpFake) Describe() string { return f.destination }
func (f *idpFake) RunInput(command, stdin string) (string, error) {
	f.commands = append(f.commands, command)
	reply := func(status int, v any) (string, error) {
		raw, _ := json.Marshal(v)
		return fmt.Sprintf("%s\npaisans-http-status:%d", raw, status), nil
	}
	get := strings.Contains(stdin, `request = "GET"`)
	switch {
	case get && strings.Contains(stdin, "/api/oidc/clients?"):
		var data []pocketid.OIDCClient
		if f.client != nil {
			data = append(data, *f.client)
		}
		return reply(200, map[string]any{"data": data, "pagination": map[string]int{"totalPages": 1}})
	case get && strings.Contains(stdin, "/secrets\""):
		var out []pocketid.ClientSecret
		for _, p := range f.prefixes {
			out = append(out, pocketid.ClientSecret{Prefix: p, IsActive: true})
		}
		return reply(200, out)
	case get && strings.Contains(stdin, "/api/oidc/clients/"):
		return reply(200, f.client)
	case strings.Contains(stdin, "/secrets\""):
		f.mutated = true
		i := strings.Index(stdin, `{\"secret\":\"`) + len(`{\"secret\":\"`)
		f.prefixes = append(f.prefixes, stdin[i:i+4])
		return reply(201, map[string]string{})
	case strings.Contains(stdin, "/api/oidc/clients\""):
		f.mutated = true
		launch := f.launch
		if launch == "" {
			launch = "https://talk.example.org/oauth/oidc/connect"
		}
		if !strings.Contains(stdin, `\"launchURL\":\"`+launch+`\"`) {
			return reply(400, map[string]string{"error": "no launch URL"})
		}
		f.client = &pocketid.OIDCClient{ID: "c-1", Name: "talk", CallbackURLs: []string{"https://talk.example.org/oauth/oidc/verify"}, PkceEnabled: true, LaunchURL: &launch}
		return reply(201, f.client)
	}
	return reply(404, map[string]string{"error": "no route"})
}

func withIDPFake(t *testing.T) *idpFake {
	t.Helper()
	fake := &idpFake{}
	saved := oidcTransport
	oidcTransport = func(tr apply.SSHTransport) pocketid.Transport {
		fake.destination = tr.Describe()
		return fake
	}
	t.Cleanup(func() { oidcTransport = saved })
	return fake
}

// tempSecrets copies the fixture secrets somewhere a test may write, with no
// .sops.yaml beside them, so a write is plaintext.
func tempSecrets(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(fixtureSecretsPath())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOIDCClientCreateDryRunWritesNothing(t *testing.T) {
	fake := withIDPFake(t)
	path := tempSecrets(t)
	before, _ := os.ReadFile(path)
	var err error
	printed := captureStdout(t, func() {
		err = runOIDCClientCreate([]string{"--config", fixtureConfig(), "--secrets", path, "--app", "talk"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"talk's client at auth on home-a (pocket-id)", "create client talk: POST /api/oidc/clients", `"pkceEnabled":true`, `"launchURL":"https://talk.example.org/oauth/oidc/connect"`, "create client secret for talk", "Nothing was changed"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	if fake.mutated {
		t.Error("a dry run mutated Pocket ID")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a dry run wrote the secrets file")
	}
	if fake.destination != "ubuntu@home-a.local" {
		t.Errorf("reached %q", fake.destination)
	}
}

func TestOIDCClientCreateExecuteRecordsTheSecretWithoutPrintingIt(t *testing.T) {
	fake := withIDPFake(t)
	path := tempSecrets(t)
	var err error
	printed := captureStdout(t, func() {
		err = runOIDCClientCreate([]string{"--config", fixtureConfig(), "--secrets", path, "--app", "talk", "--execute"})
	})
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	got := secrets.OIDCClients["talk"]
	if got.ClientID != "c-1" || len(got.ClientSecret) < 16 {
		t.Fatalf("recorded id %q and a %d character secret", got.ClientID, len(got.ClientSecret))
	}
	if len(fake.prefixes) != 1 || !strings.HasPrefix(got.ClientSecret, fake.prefixes[0]) {
		t.Error("Pocket ID was not sent the recorded secret")
	}
	if strings.Contains(printed, got.ClientSecret) {
		t.Error("the secret was printed")
	}
	for _, c := range fake.commands {
		if c != pocketid.CurlCommand {
			t.Errorf("ran %q", c)
		}
	}
	if !strings.Contains(printed, "recorded oidc_clients.talk.client_id") {
		t.Errorf("output:\n%s", printed)
	}
}

func TestOIDCClientCreateRefusesAKindItDoesNotKnow(t *testing.T) {
	fake := withIDPFake(t)
	err := runOIDCClientCreate([]string{"--config", fixtureConfig(), "--secrets", tempSecrets(t), "--app", "docs"})
	if err == nil || !strings.Contains(err.Error(), "Implemented kinds: mbin") {
		t.Fatalf("got %v", err)
	}
	if len(fake.commands) != 0 {
		t.Error("a host was reached")
	}
}

// sso_dashboard_link replaces the kind's path, and the host stays the app's.
func TestOIDCClientCreateUsesTheDashboardLinkSetting(t *testing.T) {
	raw, err := os.ReadFile(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	host := "    hostname: talk.example.org\n"
	if !strings.Contains(string(raw), host) {
		t.Fatal("the fixture no longer declares talk's hostname as expected")
	}
	edited := strings.Replace(string(raw), host, host+"    settings:\n      sso_dashboard_link: /magazines\n", 1)
	cfg := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(cfg, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := withIDPFake(t)
	fake.launch = "https://talk.example.org/magazines"
	path := tempSecrets(t)
	printed := captureStdout(t, func() {
		err = runOIDCClientCreate([]string{"--config", cfg, "--secrets", path, "--app", "talk", "--execute"})
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, printed)
	}
	if !strings.Contains(printed, `"launchURL":"https://talk.example.org/magazines"`) {
		t.Errorf("output lacks the setting's launch URL:\n%s", printed)
	}
}

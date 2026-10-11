package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
)

// idpFake is a Pocket ID with the fixture's recorded clients (docs, blog)
// already in place with live secrets, the groups members and admins, and no
// talk client until one is created.
type idpFake struct {
	destination string
	commands    []string
	mutated     bool
	client      *pocketid.OIDCClient
	prefixes    []string
	// launch is the launch URL a create must send. Empty means Mbin's
	// default, the fork's connect route.
	launch string
	groups []pocketid.Group
	others map[string]*pocketid.OIDCClient
	// created is every group name a POST /api/user-groups created, in order.
	created []string
}

var searchParam = regexp.MustCompile(`search=([A-Za-z0-9_-]*)`)

// groupNameParam is the name in a group create's body, as it appears quoted
// inside curl's configuration.
var groupNameParam = regexp.MustCompile(`\\"name\\":\\"([^\\"]+)\\"`)

func newIDPFake() *idpFake {
	// members and newcomers carry the IDs the fixture secrets record under
	// pocket_id_groups, so the signup group step finds nothing to do unless
	// a test changes one side.
	groups := []pocketid.Group{{ID: "fixture-group-members-id", Name: "members"}, {ID: "g-admins", Name: "admins"}, {ID: "fixture-group-newcomers-id", Name: "newcomers"}}
	launch := func(s string) *string { return &s }
	return &idpFake{groups: groups, others: map[string]*pocketid.OIDCClient{
		"docs": {ID: "fixture-not-a-secret-docs-id", Name: "docs", CallbackURLs: []string{"https://docs.example.org/auth/oidc.callback"},
			IsGroupRestricted: true, AllowedUserGroups: groups[:2], LaunchURL: launch("https://docs.example.org")},
		"blog": {ID: "fixture-not-a-secret-blog-id", Name: "blog", CallbackURLs: []string{"https://blog.example.org/oauth/callback/generic"},
			IsGroupRestricted: true, AllowedUserGroups: groups[:2], LaunchURL: launch("https://blog.example.org")},
	}}
}

func (f *idpFake) byID(id string) *pocketid.OIDCClient {
	if f.client != nil && f.client.ID == id {
		return f.client
	}
	for _, c := range f.others {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (f *idpFake) Describe() string { return f.destination }
func (f *idpFake) RunInput(command, stdin string) (string, error) {
	f.commands = append(f.commands, command)
	reply := func(status int, v any) (string, error) {
		raw, _ := json.Marshal(v)
		return fmt.Sprintf("%s\npaisans-http-status:%d", raw, status), nil
	}
	get := strings.Contains(stdin, `request = "GET"`)
	search := ""
	if m := searchParam.FindStringSubmatch(stdin); m != nil {
		search = m[1]
	}
	idOf := func(route string) string {
		i := strings.Index(stdin, route)
		rest := stdin[i+len(route):]
		return rest[:strings.IndexAny(rest, `/"`)]
	}
	switch {
	case get && strings.Contains(stdin, "/api/oidc/clients?"):
		var data []pocketid.OIDCClient
		if f.client != nil && strings.Contains(f.client.Name, search) {
			data = append(data, *f.client)
		}
		for _, c := range f.others {
			if strings.Contains(c.Name, search) {
				data = append(data, *c)
			}
		}
		return reply(200, map[string]any{"data": data, "pagination": map[string]int{"totalPages": 1}})
	case get && strings.Contains(stdin, "/secrets\""):
		id := idOf("/api/oidc/clients/")
		var out []pocketid.ClientSecret
		if f.client != nil && id == f.client.ID {
			for _, p := range f.prefixes {
				out = append(out, pocketid.ClientSecret{Prefix: p, IsActive: true})
			}
		} else if f.byID(id) != nil {
			out = append(out, pocketid.ClientSecret{Prefix: "fixt", IsActive: true})
		}
		return reply(200, out)
	case get && strings.Contains(stdin, "/api/oidc/clients/"):
		return reply(200, f.byID(idOf("/api/oidc/clients/")))
	case get && strings.Contains(stdin, "/api/user-groups"):
		var data []pocketid.Group
		for _, g := range f.groups {
			if strings.Contains(g.Name, search) {
				data = append(data, g)
			}
		}
		return reply(200, map[string]any{"data": data, "pagination": map[string]int{"totalPages": 1}})
	case strings.Contains(stdin, "/secrets\""):
		f.mutated = true
		i := strings.Index(stdin, `{\"secret\":\"`) + len(`{\"secret\":\"`)
		f.prefixes = append(f.prefixes, stdin[i:i+4])
		return reply(201, map[string]string{})
	case strings.Contains(stdin, "/api/user-groups\""):
		f.mutated = true
		m := groupNameParam.FindStringSubmatch(stdin)
		if m == nil {
			return reply(400, map[string]string{"error": "no name"})
		}
		g := pocketid.Group{ID: "g-" + m[1], Name: m[1], FriendlyName: m[1]}
		f.groups = append(f.groups, g)
		f.created = append(f.created, m[1])
		return reply(201, g)
	case strings.Contains(stdin, "/allowed-user-groups\""):
		f.mutated = true
		c := f.byID(idOf("/api/oidc/clients/"))
		c.AllowedUserGroups = nil
		for _, g := range f.groups {
			if strings.Contains(stdin, g.ID) {
				c.AllowedUserGroups = append(c.AllowedUserGroups, g)
			}
		}
		return reply(200, c)
	case strings.Contains(stdin, "/api/oidc/clients\""):
		f.mutated = true
		launch := f.launch
		if launch == "" {
			launch = "https://talk.example.org/oauth/oidc/connect"
		}
		if !strings.Contains(stdin, `\"launchURL\":\"`+launch+`\"`) {
			return reply(400, map[string]string{"error": "no launch URL"})
		}
		f.client = &pocketid.OIDCClient{ID: "c-1", Name: "talk", CallbackURLs: []string{"https://talk.example.org/oauth/oidc/verify"}, PkceEnabled: true, LaunchURL: &launch,
			IsGroupRestricted: strings.Contains(stdin, `\"isGroupRestricted\":true`)}
		return reply(201, f.client)
	}
	return reply(404, map[string]string{"error": "no route"})
}

func withIDPFake(t *testing.T) *idpFake {
	t.Helper()
	fake := newIDPFake()
	withRegistryFake(t)
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
	rec := withRecorder(t, true)
	err := runOIDCClientCreate([]string{"--config", fixtureConfig(), "--secrets", path, "--app", "talk"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"create OIDC client talk", "create client secret for talk"} {
		if !rec.Has("pending", want) {
			t.Errorf("no item %q:\n%s", want, rec.Lines())
		}
	}
	for _, want := range []string{"talk's client at auth on home-a (pocket-id)", "POST /api/oidc/clients", `"pkceEnabled":true`, `"launchURL":"https://talk.example.org/oauth/oidc/connect"`} {
		if !rec.Has("detail", want) {
			t.Errorf("no detail %q:\n%s", want, rec.Lines())
		}
	}
	if !rec.Has("result", "Nothing changed") {
		t.Errorf("no result:\n%s", rec.Lines())
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
	rec := withRecorder(t, true)
	err := runOIDCClientCreate([]string{"--config", fixtureConfig(), "--secrets", path, "--app", "talk", "--execute"})
	if err != nil {
		t.Fatal(err)
	}
	printed := rec.Lines()
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
	if !rec.Has("detail", "recorded oidc_clients.talk.client_id") || !rec.Has("result", "talk's client is in place") {
		t.Errorf("output:\n%s", printed)
	}
}

func TestOIDCClientCreateRefusesAKindItDoesNotKnow(t *testing.T) {
	fake := withIDPFake(t)
	// web is an Element client, which authenticates at the homeserver and
	// holds no client of its own.
	err := runOIDCClientCreate([]string{"--config", fixtureConfig(), "--secrets", tempSecrets(t), "--app", "web"})
	if err == nil || !strings.Contains(err.Error(), "Implemented kinds: mbin, outline, writefreely, uptime") {
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
	rec := withRecorder(t, true)
	printed := captureStdout(t, func() {
		err = runOIDCClientCreate([]string{"--config", cfg, "--secrets", path, "--app", "talk", "--execute"})
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, printed)
	}
	if !rec.Has("detail", `"launchURL":"https://talk.example.org/magazines"`) {
		t.Errorf("the request lacks the setting's launch URL:\n%s", rec.Lines())
	}
}

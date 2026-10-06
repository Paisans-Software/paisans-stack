package oidcclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/pocketid"
)

const testKey = "not-a-real-api-key-0003"

// fakeIDP is Pocket ID in memory: clients with secret prefixes, groups and
// users, reached through the curl configs internal/pocketid writes.
type fakeIDP struct {
	clients   []*pocketid.OIDCClient
	secrets   map[string][]pocketid.ClientSecret
	groups    []pocketid.Group
	users     []*pocketid.User
	commands  []string
	stdins    []string
	mutations []string
	// sent is every secret value Pocket ID was given, in order.
	sent []string
	// onMutation runs before a mutation is served, so a test can see what
	// had already happened at that moment.
	onMutation func(method, path string)
}

func newFake() *fakeIDP {
	return &fakeIDP{secrets: map[string][]pocketid.ClientSecret{}}
}

func (f *fakeIDP) Describe() string { return "home-a.local" }

func (f *fakeIDP) RunInput(command, stdin string) (string, error) {
	f.commands = append(f.commands, command)
	f.stdins = append(f.stdins, stdin)
	method, target, body := readCurlConfig(stdin)
	u, _ := url.Parse(target)
	if method != "GET" {
		f.mutations = append(f.mutations, method+" "+u.Path)
		if f.onMutation != nil {
			f.onMutation(method, u.Path)
		}
	}
	status, out := f.serve(method, u, body)
	return fmt.Sprintf("%s\npaisans-http-status:%d", out, status), nil
}

func page[T any](data []T) string {
	raw, _ := json.Marshal(map[string]any{"data": data, "pagination": map[string]int{"totalPages": 1}})
	return string(raw)
}

func marshal(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func (f *fakeIDP) client(id string) *pocketid.OIDCClient {
	for _, c := range f.clients {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (f *fakeIDP) serve(method string, u *url.URL, body string) (int, string) {
	path := u.Path
	search := u.Query().Get("search")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case method == "GET" && path == "/api/oidc/clients":
		var out []pocketid.OIDCClient
		for _, c := range f.clients {
			if strings.Contains(c.Name, search) {
				out = append(out, *c)
			}
		}
		return 200, page(out)
	case method == "GET" && len(parts) == 4 && parts[2] == "clients":
		return 200, marshal(f.client(parts[3]))
	case method == "GET" && len(parts) == 5 && parts[4] == "secrets":
		return 200, marshal(f.secrets[parts[3]])
	case method == "POST" && path == "/api/oidc/clients":
		var n pocketid.NewOIDCClient
		_ = json.Unmarshal([]byte(body), &n)
		c := &pocketid.OIDCClient{ID: fmt.Sprintf("c-%d", len(f.clients)+1), Name: n.Name, CallbackURLs: n.CallbackURLs, PkceEnabled: n.PkceEnabled, IsGroupRestricted: n.IsGroupRestricted, IsPublic: n.IsPublic}
		f.clients = append(f.clients, c)
		return 201, marshal(c)
	case method == "POST" && len(parts) == 5 && parts[4] == "secrets":
		var in struct{ Secret string }
		_ = json.Unmarshal([]byte(body), &in)
		f.sent = append(f.sent, in.Secret)
		f.secrets[parts[3]] = append(f.secrets[parts[3]], pocketid.ClientSecret{ID: "s", Prefix: in.Secret[:4], IsActive: true})
		return 201, marshal(map[string]string{"secret": in.Secret})
	case method == "PUT" && len(parts) == 5 && parts[4] == "allowed-user-groups":
		var in struct {
			UserGroupIds []string `json:"userGroupIds"`
		}
		_ = json.Unmarshal([]byte(body), &in)
		c := f.client(parts[3])
		c.AllowedUserGroups = nil
		for _, id := range in.UserGroupIds {
			for _, g := range f.groups {
				if g.ID == id {
					c.AllowedUserGroups = append(c.AllowedUserGroups, g)
				}
			}
		}
		return 200, marshal(c)
	case method == "GET" && path == "/api/user-groups":
		var out []pocketid.Group
		for _, g := range f.groups {
			if strings.Contains(g.Name, search) {
				out = append(out, g)
			}
		}
		return 200, page(out)
	case method == "POST" && path == "/api/user-groups":
		var g pocketid.Group
		_ = json.Unmarshal([]byte(body), &g)
		g.ID = fmt.Sprintf("g-%d", len(f.groups)+1)
		f.groups = append(f.groups, g)
		return 201, marshal(g)
	case method == "GET" && path == "/api/users":
		var out []pocketid.User
		for _, u := range f.users {
			if strings.Contains(u.Username, search) {
				out = append(out, *u)
			}
		}
		return 200, page(out)
	case method == "PUT" && len(parts) == 4 && parts[3] == "user-groups":
		var in struct {
			UserGroupIds []string `json:"userGroupIds"`
		}
		_ = json.Unmarshal([]byte(body), &in)
		for _, u := range f.users {
			if u.ID == parts[2] {
				u.UserGroups = nil
				for _, id := range in.UserGroupIds {
					for _, g := range f.groups {
						if g.ID == id {
							u.UserGroups = append(u.UserGroups, g)
						}
					}
				}
			}
		}
		return 200, `{}`
	}
	return 404, `{"error":"no route ` + method + " " + path + `"}`
}

func readCurlConfig(cfg string) (method, target, body string) {
	for _, line := range strings.Split(cfg, "\n") {
		option, quoted, ok := strings.Cut(line, " = ")
		if !ok {
			continue
		}
		value := strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(strings.Trim(quoted, `"`))
		switch option {
		case "url":
			target = value
		case "request":
			method = value
		case "data-raw":
			body = value
		}
	}
	return method, target, body
}

// memRecorder is the secrets file, in memory.
type memRecorder struct {
	rec  Recorded
	fail bool
	// calls counts writes.
	calls int
}

func (m *memRecorder) Record(id, secret string) error {
	m.calls++
	if m.fail {
		return errors.New("disk full")
	}
	m.rec = Recorded{ClientID: id, ClientSecret: secret}
	return nil
}

func api(f *fakeIDP) *pocketid.Client {
	return &pocketid.Client{Transport: f, BaseURL: "http://10.44.0.1:1411", APIKey: testKey}
}

func talk() Desired {
	return Desired{App: "talk", CallbackURL: "https://talk.example.org/oauth/oidc/verify", PKCE: true, AdminGroup: "admins", AdminUser: "founder"}
}

func secretSource(values ...string) func() (string, error) {
	return func() (string, error) {
		v := values[0]
		values = values[1:]
		return v, nil
	}
}

func plan(t *testing.T, f *fakeIDP, d Desired, rec Recorded) *Plan {
	t.Helper()
	s, err := Probe(api(f), d)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(d, rec, s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func kinds(p *Plan) []StepKind {
	var out []StepKind
	for _, s := range p.Steps {
		out = append(out, s.Kind)
	}
	return out
}

func withFounder(f *fakeIDP) *fakeIDP {
	f.users = append(f.users, &pocketid.User{ID: "u-1", Username: "founder", IsAdmin: true})
	return f
}

// From nothing, the plan creates the group, the client, the secret and the
// membership, shows the client body exactly, and probing changed nothing.
func TestAFreshPlanShowsEverythingAndChangesNothing(t *testing.T) {
	f := withFounder(newFake())
	p := plan(t, f, talk(), Recorded{})
	want := []StepKind{CreateGroup, CreateClient, AddSecret, AddUserToGroup}
	if fmt.Sprint(kinds(p)) != fmt.Sprint(want) {
		t.Fatalf("planned %v, want %v", kinds(p), want)
	}
	if !strings.Contains(p.Steps[1].Line, `{"name":"talk","callbackURLs":["https://talk.example.org/oauth/oidc/verify"],"isPublic":false,"pkceEnabled":true,"isGroupRestricted":false}`) {
		t.Errorf("the client line does not show the body: %s", p.Steps[1].Line)
	}
	if len(f.mutations) != 0 {
		t.Errorf("a probe mutated: %v", f.mutations)
	}
}

// The secret is recorded before Pocket ID is sent it, never appears in a
// command line, and a fresh plan afterwards is empty.
func TestExecuteRecordsFirstAndConverges(t *testing.T) {
	f := withFounder(newFake())
	rec := &memRecorder{}
	f.onMutation = func(method, path string) {
		if strings.HasSuffix(path, "/secrets") && rec.rec.ClientSecret == "" {
			t.Error("Pocket ID was sent a secret before it was recorded")
		}
	}
	secret := "generated-client-secret-not-real-01"
	p := plan(t, f, talk(), Recorded{})
	if err := Execute(p, api(f), rec, secretSource(secret)); err != nil {
		t.Fatal(err)
	}
	if rec.rec != (Recorded{ClientID: "c-1", ClientSecret: secret}) {
		t.Errorf("recorded %+v", rec.rec)
	}
	if len(f.sent) != 1 || f.sent[0] != secret {
		t.Errorf("Pocket ID holds %v", f.sent)
	}
	for _, c := range f.commands {
		if c != pocketid.CurlCommand {
			t.Errorf("ran %q", c)
		}
	}
	for _, s := range p.Steps {
		if strings.Contains(s.Line, secret) {
			t.Error("a plan line carries the secret")
		}
	}
	if !f.users[0].InGroup("g-1") {
		t.Error("founder is not in admins")
	}

	again := plan(t, f, talk(), rec.rec)
	if len(again.Steps) != 0 {
		t.Errorf("a second run plans %v", kinds(again))
	}
}

func TestRotateAddsASecretAndKeepsTheOldOne(t *testing.T) {
	f := withFounder(newFake())
	rec := &memRecorder{}
	if err := Execute(plan(t, f, talk(), Recorded{}), api(f), rec, secretSource("first-secret-not-real-0001")); err != nil {
		t.Fatal(err)
	}
	d := talk()
	d.RotateSecret = true
	p := plan(t, f, d, rec.rec)
	if fmt.Sprint(kinds(p)) != fmt.Sprint([]StepKind{AddSecret}) || !strings.Contains(p.Steps[0].Line, "stay valid") {
		t.Fatalf("rotation planned %v: %v", kinds(p), p.Steps)
	}
	if err := Execute(p, api(f), rec, secretSource("second-secret-not-real-0002")); err != nil {
		t.Fatal(err)
	}
	if rec.rec.ClientSecret != "second-secret-not-real-0002" || len(f.secrets["c-1"]) != 2 {
		t.Errorf("after rotation: recorded %q, Pocket ID holds %d", rec.rec.ClientSecret, len(f.secrets["c-1"]))
	}
}

// A run that recorded its secret and stopped before sending it resumes by
// sending the recorded one, not by minting another.
func TestAnInterruptedRunSendsTheRecordedSecret(t *testing.T) {
	f := withFounder(newFake())
	rec := &memRecorder{}
	if err := Execute(plan(t, f, talk(), Recorded{}), api(f), rec, secretSource("first-secret-not-real-0001")); err != nil {
		t.Fatal(err)
	}
	f.secrets["c-1"] = nil // as if the POST never landed
	p := plan(t, f, talk(), rec.rec)
	if fmt.Sprint(kinds(p)) != fmt.Sprint([]StepKind{SendRecordedSecret}) {
		t.Fatalf("planned %v", kinds(p))
	}
	if err := Execute(p, api(f), rec, func() (string, error) { return "", errors.New("must not generate") }); err != nil {
		t.Fatal(err)
	}
	if f.sent[len(f.sent)-1] != "first-secret-not-real-0001" {
		t.Error("the recorded secret was not the one sent")
	}
}

// If the secret cannot be recorded, Pocket ID is sent nothing.
func TestARecordFailureSendsNothing(t *testing.T) {
	f := withFounder(newFake())
	err := Execute(plan(t, f, talk(), Recorded{}), api(f), &memRecorder{fail: true}, secretSource("never-sent-secret-not-real-01"))
	if err == nil || !strings.Contains(err.Error(), "sent nothing") {
		t.Fatalf("got %v", err)
	}
	if len(f.sent) != 0 {
		t.Error("a secret reached Pocket ID that is recorded nowhere")
	}
	if strings.Contains(err.Error(), "never-sent-secret-not-real-01") {
		t.Error("the error carries the secret")
	}
}

func TestAnExistingClientThatDiffersIsRefused(t *testing.T) {
	f := withFounder(newFake())
	f.clients = []*pocketid.OIDCClient{{ID: "c-9", Name: "talk", CallbackURLs: []string{"https://talk.example.org/oauth/callback"}}}
	s, err := Probe(api(f), talk())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(talk(), Recorded{}, s)
	if err == nil || !strings.Contains(err.Error(), "do not include https://talk.example.org/oauth/oidc/verify") || !strings.Contains(err.Error(), "PKCE is off") {
		t.Fatalf("got %v", err)
	}
}

// A member group restricts the client to the member and admin groups.
func TestAMemberGroupRestrictsTheClient(t *testing.T) {
	f := withFounder(newFake())
	d := talk()
	d.MemberGroup = "members"
	p := plan(t, f, d, Recorded{})
	want := []StepKind{CreateGroup, CreateGroup, CreateClient, AllowGroups, AddSecret, AddUserToGroup}
	if fmt.Sprint(kinds(p)) != fmt.Sprint(want) {
		t.Fatalf("planned %v", kinds(p))
	}
	rec := &memRecorder{}
	if err := Execute(p, api(f), rec, secretSource("member-secret-not-real-0001")); err != nil {
		t.Fatal(err)
	}
	c := f.clients[0]
	if !c.IsGroupRestricted || len(c.AllowedUserGroups) != 2 {
		t.Errorf("client %+v", c)
	}
}

func TestAdminUserNeedsAGroupAndAnAccount(t *testing.T) {
	d := talk()
	d.AdminGroup = ""
	if _, err := Build(d, Recorded{}, State{}); err == nil || !strings.Contains(err.Error(), "admin group") {
		t.Errorf("no group: %v", err)
	}
	if _, err := Build(talk(), Recorded{}, State{Groups: map[string]*pocketid.Group{}}); err == nil || !strings.Contains(err.Error(), "app admin create") {
		t.Errorf("no user: %v", err)
	}
}

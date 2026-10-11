package appadmin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
)

const pidKey = "not-a-real-api-key-0002"

// fakePocketID is a Pocket ID held in memory, reached through curl configs.
type fakePocketID struct {
	users     []pocketid.User
	groups    []pocketid.Group
	commands  []string
	mutations []string
	// bodies is every mutation's body, in order.
	bodies []string
	tokens int
	// echo makes every mutation fail with a body that repeats its input, the
	// worst case for a leak.
	echo bool
}

func (f *fakePocketID) Describe() string           { return "home-a.local" }
func (f *fakePocketID) Run(string) (string, error) { return "", errors.New("unused") }

func (f *fakePocketID) RunInput(command, stdin string) (string, error) {
	f.commands = append(f.commands, command)
	method, target, body := readCurlConfig(stdin)
	u, _ := url.Parse(target)
	status, out := f.serve(method, u, body, stdin)
	return fmt.Sprintf("%s\npaisans-http-status:%d", out, status), nil
}

func (f *fakePocketID) serve(method string, u *url.URL, body, stdin string) (int, string) {
	if method != "GET" {
		f.mutations = append(f.mutations, method+" "+u.Path)
		f.bodies = append(f.bodies, body)
		if f.echo {
			return 500, `{"error":"echo ` + strings.ReplaceAll(stdin, `"`, `'`) + `"}`
		}
	}
	switch {
	case method == "GET" && u.Path == "/api/users":
		var data []pocketid.User
		for _, user := range f.users {
			if strings.Contains(user.Username, u.Query().Get("search")) {
				data = append(data, user)
			}
		}
		raw, _ := json.Marshal(map[string]any{"data": data, "pagination": map[string]int{"totalPages": 1}})
		return 200, string(raw)
	case method == "POST" && u.Path == "/api/users":
		var n pocketid.NewUser
		_ = json.Unmarshal([]byte(body), &n)
		created := pocketid.User{ID: fmt.Sprintf("u-%d", len(f.users)+1), Username: n.Username, Email: n.Email, EmailVerified: n.EmailVerified, FirstName: n.FirstName, DisplayName: n.DisplayName, IsAdmin: n.IsAdmin}
		f.users = append(f.users, created)
		raw, _ := json.Marshal(created)
		return 201, string(raw)
	case method == "GET" && u.Path == "/api/user-groups":
		raw, _ := json.Marshal(map[string]any{"data": f.groups, "pagination": map[string]int{"totalPages": 1}})
		return 200, string(raw)
	case method == "POST" && u.Path == "/api/user-groups":
		var g pocketid.Group
		_ = json.Unmarshal([]byte(body), &g)
		g.ID = fmt.Sprintf("g-%d", len(f.groups)+1)
		f.groups = append(f.groups, g)
		raw, _ := json.Marshal(g)
		return 201, string(raw)
	case method == "PUT" && strings.HasSuffix(u.Path, "/user-groups"):
		id := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/api/users/"), "/user-groups")
		var sent struct {
			UserGroupIDs []string `json:"userGroupIds"`
		}
		_ = json.Unmarshal([]byte(body), &sent)
		for i := range f.users {
			if f.users[i].ID == id {
				f.users[i].UserGroups = nil
				for _, gid := range sent.UserGroupIDs {
					for _, g := range f.groups {
						if g.ID == gid {
							f.users[i].UserGroups = append(f.users[i].UserGroups, g)
						}
					}
				}
				return 200, `{}`
			}
		}
		return 404, `{"error":"user not found"}`
	case method == "PUT" && strings.HasPrefix(u.Path, "/api/users/"):
		id := strings.TrimPrefix(u.Path, "/api/users/")
		for i := range f.users {
			if f.users[i].ID == id {
				var sent map[string]any
				_ = json.Unmarshal([]byte(body), &sent)
				f.users[i].IsAdmin, _ = sent["isAdmin"].(bool)
				f.users[i].EmailVerified, _ = sent["emailVerified"].(bool)
				f.users[i].FirstName, _ = sent["firstName"].(string)
				return 200, `{}`
			}
		}
		return 404, `{"error":"user not found"}`
	case method == "POST" && strings.HasSuffix(u.Path, "/one-time-access-token"):
		f.tokens++
		return 201, fmt.Sprintf(`{"token":"tok-not-real-%04d"}`, f.tokens)
	}
	return 404, `{"error":"no route"}`
}

// readCurlConfig pulls the request back out of the config pocketid writes.
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

func pidRequest() Request {
	return Request{App: "auth", Username: "founder", Email: "founder@example.org", FirstName: "Fern", LastName: "Founder",
		APIBase: "http://10.44.0.1:1411", APIKey: pidKey, PublicURL: "https://id.example.org"}
}

// A dry run reads and plans. It creates nothing and issues no link, and the
// plan shows the exact body a create would send.
func TestPocketIDPlanShowsTheBodyAndChangesNothing(t *testing.T) {
	f := &fakePocketID{}
	plan, err := Build(config.KindPocketID, f, pidRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Actions, []Action{ActionCreate, ActionLoginLink}) {
		t.Fatalf("planned %v", plan.Actions)
	}
	lines := strings.Join(plan.Lines(), "\n")
	for _, want := range []string{
		`POST /api/users {"username":"founder","email":"founder@example.org","emailVerified":true,"firstName":"Fern","lastName":"Founder","displayName":"Fern Founder","isAdmin":true}`,
		"valid 20m0s",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("the plan lacks %q:\n%s", want, lines)
		}
	}
	if len(f.mutations) != 0 || f.tokens != 0 {
		t.Errorf("a dry run mutated: %v", f.mutations)
	}
}

func TestPocketIDExecuteCreatesAndReturnsTheLinkOnce(t *testing.T) {
	f := &fakePocketID{}
	plan, err := Build(config.KindPocketID, f, pidRequest())
	if err != nil {
		t.Fatal(err)
	}
	out, err := Execute(plan, f)
	if err != nil {
		t.Fatal(err)
	}
	if out.LoginLink != "https://id.example.org/lc/tok-not-real-0001" {
		t.Errorf("link %q", out.LoginLink)
	}
	if len(f.users) != 1 || !f.users[0].IsAdmin || !f.users[0].EmailVerified {
		t.Errorf("users %+v", f.users)
	}
	for _, c := range f.commands {
		if c != pocketid.CurlCommand {
			t.Errorf("ran %q", c)
		}
	}
	for _, line := range plan.Lines() {
		if strings.Contains(line, "tok-not-real") || strings.Contains(line, pidKey) {
			t.Errorf("a plan line carries a credential: %s", line)
		}
	}
}

// Re-running against an existing administrator plans nothing; asking for a
// link plans only the link; a non-admin is granted admin with its name kept.
func TestPocketIDIsIdempotent(t *testing.T) {
	email := "founder@example.org"
	f := &fakePocketID{users: []pocketid.User{{ID: "u-1", Username: "founder", Email: &email, EmailVerified: true, FirstName: "Fern", IsAdmin: true}}}
	plan, err := Build(config.KindPocketID, f, pidRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 || plan.Lines()[0] != "founder" {
		t.Errorf("an existing admin planned %v", plan.Actions)
	}

	req := pidRequest()
	req.LoginLink = true
	plan, err = Build(config.KindPocketID, f, req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Actions, []Action{ActionLoginLink}) {
		t.Errorf("--login-link planned %v", plan.Actions)
	}

	f.users[0].IsAdmin = false
	plan, err = Build(config.KindPocketID, f, pidRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Actions, []Action{ActionGrantAdmin}) {
		t.Fatalf("a non-admin planned %v", plan.Actions)
	}
	out, err := Execute(plan, f)
	if err != nil {
		t.Fatal(err)
	}
	if out.LoginLink != "" {
		t.Error("a link was issued that nobody asked for")
	}
	if !f.users[0].IsAdmin || f.users[0].FirstName != "Fern" {
		t.Errorf("after grant: %+v", f.users[0])
	}
}

func TestPocketIDErrorsCarryNoKey(t *testing.T) {
	f := &fakePocketID{echo: true}
	plan, err := Build(config.KindPocketID, f, pidRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Execute(plan, f)
	if err == nil {
		t.Fatal("a failed create was reported as success")
	}
	if strings.Contains(err.Error(), pidKey) {
		t.Fatalf("the error carries the API key: %v", err)
	}
}

func TestPocketIDRefusesADisabledAccount(t *testing.T) {
	f := &fakePocketID{users: []pocketid.User{{ID: "u-1", Username: "founder", Disabled: true}}}
	if _, err := Build(config.KindPocketID, f, pidRequest()); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("got %v", err)
	}
}

// An existing administrator whose email is not verified plans only the
// verify step, which sends the user back unchanged but for emailVerified; a
// second run plans nothing.
func TestPocketIDVerifiesAnExistingAdminsEmail(t *testing.T) {
	email := "founder@example.org"
	last := "Founder"
	f := &fakePocketID{users: []pocketid.User{{ID: "u-1", Username: "founder", Email: &email, FirstName: "Fern", LastName: &last, DisplayName: "Fern Founder", IsAdmin: true}}}
	plan, err := Build(config.KindPocketID, f, pidRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Actions, []Action{ActionVerify}) {
		t.Fatalf("an unverified admin planned %v", plan.Actions)
	}
	if got := plan.Lines()[0]; !strings.HasPrefix(got, "mark email verified for founder: PUT /api/users/<id>") {
		t.Errorf("line %q", got)
	}
	if len(f.mutations) != 0 {
		t.Fatalf("a dry run mutated: %v", f.mutations)
	}
	if _, err := Execute(plan, f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.mutations, []string{"PUT /api/users/u-1"}) {
		t.Fatalf("mutations %v", f.mutations)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"username": "founder", "email": email, "emailVerified": true, "firstName": "Fern", "lastName": "Founder",
		"displayName": "Fern Founder", "isAdmin": true, "locale": nil, "disabled": false}
	if !reflect.DeepEqual(sent, want) {
		t.Errorf("sent %v, want %v", sent, want)
	}
	for _, c := range f.commands {
		if c != pocketid.CurlCommand || strings.Contains(c, pidKey) {
			t.Errorf("ran %q", c)
		}
	}
	plan, err = Build(config.KindPocketID, f, pidRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 {
		t.Errorf("a second run planned %v", plan.Actions)
	}
}

// Verify and grant on one account: the second whole-user update must not undo
// the first.
func TestPocketIDVerifyAndGrantBothLand(t *testing.T) {
	email := "founder@example.org"
	f := &fakePocketID{users: []pocketid.User{{ID: "u-1", Username: "founder", Email: &email, FirstName: "Fern"}}}
	plan, err := Build(config.KindPocketID, f, pidRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Actions, []Action{ActionVerify, ActionGrantAdmin}) {
		t.Fatalf("planned %v", plan.Actions)
	}
	if _, err := Execute(plan, f); err != nil {
		t.Fatal(err)
	}
	if !f.users[0].EmailVerified || !f.users[0].IsAdmin {
		t.Errorf("after: %+v", f.users[0])
	}
}

// An account with no email has nothing to verify.
func TestPocketIDAnAccountWithoutEmailNeedsNoVerify(t *testing.T) {
	f := &fakePocketID{users: []pocketid.User{{ID: "u-1", Username: "founder", IsAdmin: true}}}
	plan, err := Build(config.KindPocketID, f, pidRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 {
		t.Errorf("planned %v", plan.Actions)
	}
}

// Creating an account needs an email: the address is set only at creation,
// and an administrator without one cannot be matched to an app account. The
// refusal comes at planning, so a dry run catches it and nothing is sent. An
// existing account is not asked for one.
func TestPocketIDCreateNeedsAnEmail(t *testing.T) {
	f := &fakePocketID{}
	req := pidRequest()
	req.Email = ""
	if _, err := Build(config.KindPocketID, f, req); err == nil || !strings.Contains(err.Error(), "--email") {
		t.Fatalf("a create without an email was planned: %v", err)
	}
	if len(f.users) != 0 {
		t.Error("a user was created")
	}

	f = &fakePocketID{users: []pocketid.User{{ID: "u-1", Username: "founder", FirstName: "Fern"}}}
	if _, err := Build(config.KindPocketID, f, req); err != nil {
		t.Errorf("an existing account was asked for an email: %v", err)
	}
}

// The administrator is put in every app's admin group: a group that does not
// exist yet is created, the user's other groups are kept, and a second run
// plans nothing.
func TestPocketIDPutsTheAdminInEveryAdminGroup(t *testing.T) {
	email := "founder@example.org"
	f := &fakePocketID{
		groups: []pocketid.Group{{ID: "g-1", Name: "admins"}, {ID: "g-2", Name: "friends"}},
		users:  []pocketid.User{{ID: "u-1", Username: "founder", Email: &email, EmailVerified: true, FirstName: "Fern", IsAdmin: true, UserGroups: []pocketid.Group{{ID: "g-2", Name: "friends"}}}},
	}
	req := pidRequest()
	req.AdminGroups = []string{"admins", "editors"}
	plan, err := Build(config.KindPocketID, f, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0] != ActionJoinGroups {
		t.Fatalf("planned %v, want only the group step", plan.Actions)
	}
	if !strings.Contains(plan.Lines()[0], "admins, editors") {
		t.Errorf("the line does not name the groups: %s", plan.Lines()[0])
	}
	if len(f.mutations) != 0 {
		t.Errorf("planning mutated: %v", f.mutations)
	}
	if _, err := Execute(plan, f); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range f.users[0].UserGroups {
		names = append(names, g.Name)
	}
	sort.Strings(names)
	if fmt.Sprint(names) != "[admins editors friends]" {
		t.Errorf("founder is in %v", names)
	}
	again, err := Build(config.KindPocketID, f, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Actions) != 0 {
		t.Errorf("a second run plans %v", again.Actions)
	}
}

// A new administrator is created, put in the admin groups, then given the
// link, in that order.
func TestPocketIDNewAdminJoinsTheGroupsBeforeTheLink(t *testing.T) {
	f := &fakePocketID{}
	req := pidRequest()
	req.AdminGroups = []string{"admins"}
	plan, err := Build(config.KindPocketID, f, req)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{ActionCreate, ActionJoinGroups, ActionLoginLink}
	if fmt.Sprint(plan.Actions) != fmt.Sprint(want) {
		t.Fatalf("planned %v, want %v", plan.Actions, want)
	}
	if _, err := Execute(plan, f); err != nil {
		t.Fatal(err)
	}
	if len(f.groups) != 1 || len(f.users[0].UserGroups) != 1 || f.users[0].UserGroups[0].Name != "admins" {
		t.Errorf("groups %v, user in %v", f.groups, f.users[0].UserGroups)
	}
}

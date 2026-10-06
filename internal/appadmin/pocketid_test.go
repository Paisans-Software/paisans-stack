package appadmin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
)

const pidKey = "not-a-real-api-key-0002"

// fakePocketID is a Pocket ID held in memory, reached through curl configs.
type fakePocketID struct {
	users     []pocketid.User
	commands  []string
	mutations []string
	tokens    int
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
		created := pocketid.User{ID: fmt.Sprintf("u-%d", len(f.users)+1), Username: n.Username, Email: n.Email, FirstName: n.FirstName, DisplayName: n.DisplayName, IsAdmin: n.IsAdmin}
		f.users = append(f.users, created)
		raw, _ := json.Marshal(created)
		return 201, string(raw)
	case method == "PUT" && strings.HasPrefix(u.Path, "/api/users/"):
		id := strings.TrimPrefix(u.Path, "/api/users/")
		for i := range f.users {
			if f.users[i].ID == id {
				var sent map[string]any
				_ = json.Unmarshal([]byte(body), &sent)
				f.users[i].IsAdmin, _ = sent["isAdmin"].(bool)
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

func TestPocketIDIsPasswordless(t *testing.T) {
	if !Passwordless(config.KindPocketID) || Passwordless(config.KindMbin) {
		t.Error("only pocket-id is passwordless")
	}
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
		`POST /api/users {"username":"founder","email":"founder@example.org","firstName":"Fern","lastName":"Founder","displayName":"Fern Founder","isAdmin":true}`,
		"valid 15m0s",
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
	if len(f.users) != 1 || !f.users[0].IsAdmin {
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
	f := &fakePocketID{users: []pocketid.User{{ID: "u-1", Username: "founder", Email: &email, FirstName: "Fern", IsAdmin: true}}}
	plan, err := Build(config.KindPocketID, f, pidRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 || plan.Lines()[0] != "present founder" {
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

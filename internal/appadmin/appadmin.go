// Package appadmin creates an application's first administrator on a running
// stack.
//
// An app with registrations closed has no way to make its first account from
// a browser, so the toolkit makes it, through the application's own
// administration interface. Planning is shared by every kind: probe the user,
// decide the missing steps, and change nothing unless told to. How to probe,
// which steps a state implies and how to act are per kind, behind Creator.
// The one kind implemented is Pocket ID; an app that signs its users in
// through Pocket ID gets its administrators from there, not from here.
//
// Pocket ID's users sign in with passkeys, so there is no password to take.
// Its first administrator is made usable by a one-time login link instead,
// which is a credential: it is returned to the caller, which prints it once,
// and it never enters a plan line, an error or a log. The API key that reaches
// Pocket ID is handled the same way.
package appadmin

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// Transport is the subset of apply.Transport this package uses. It is
// declared here rather than imported so the package depends only on the shape
// it needs; apply.SSHTransport satisfies it.
type Transport interface {
	Run(command string) (string, error)
	RunInput(command, stdin string) (string, error)
	Describe() string
}

// Request is what the operator asked for.
type Request struct {
	// App is the stack's name, which is also its directory under the deployment root.
	App      string
	Username string
	Email    string

	// FirstName and LastName are used only by a kind that stores them, and
	// only when the account is created.
	FirstName string
	LastName  string
	// AdminGroups is every group an app signing in through this identity
	// provider reads as its admin group, sorted and without duplicates. The
	// user is put in each, so one command makes an administrator of every
	// such app as well as of the provider.
	AdminGroups []string

	// LoginLink asks for a fresh one-time login link for
	// an account that already exists. A created account always gets one,
	// since it cannot be signed in to without it.
	LoginLink bool

	// APIBase, APIKey and PublicURL are for a kind administered through its
	// own HTTP API rather than through commands in its container. APIBase is
	// where the API answers from the site's host, APIKey is never printed,
	// and PublicURL is what a login link is built on.
	APIBase   string
	APIKey    string
	PublicURL string
}

// State is what a probe found about one username.
type State struct {
	Exists   bool
	Verified bool
	Admin    bool
	// MissingGroups is the part of Request.AdminGroups the user is not in,
	// including any group that does not exist yet. All of them for a user
	// that does not exist.
	MissingGroups []string
}

// Action is one step towards an existing, verified administrator.
type Action string

const (
	ActionCreate     Action = "create user"
	ActionVerify     Action = "verify"
	ActionGrantAdmin Action = "grant admin"
	ActionJoinGroups Action = "add to admin groups"
	ActionLoginLink  Action = "issue one-time login link for"
)

// Outcome is what an execute produced that the operator has to be given.
// LoginLink is a credential: the caller prints it once to the terminal and
// keeps it nowhere else.
type Outcome struct {
	LoginLink string
	ExpiresIn time.Duration
}

// Creator is what a kind implements to support admin creation. Probe must
// change nothing. Steps decides what is missing and must be a pure function
// of the probe, which is what makes re-running safe: an existing admin plans
// nothing, and an existing account is never re-created. Execute performs the
// actions in order and stops at the first failure.
type Creator interface {
	Probe(t Transport, req Request) (State, error)
	Steps(state State, req Request) []Action
	Execute(t Transport, req Request, actions []Action) (Outcome, error)
}

// describer is a Creator that shows more of an action than its name, because
// the operator approves exactly what is printed.
type describer interface {
	Describe(Action, Request) string
}

var creators = map[config.Kind]Creator{
	config.KindPocketID: pocketID{},
}

// For returns the Creator for a kind, or an error naming the kinds that have
// one.
func For(kind config.Kind) (Creator, error) {
	if c, ok := creators[kind]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("%s admin creation is not implemented yet. Implemented kinds: %s", kind, strings.Join(Implemented(), ", "))
}

// Implemented lists the kinds with a Creator, sorted.
func Implemented() []string {
	out := make([]string, 0, len(creators))
	for k := range creators {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

// Plan is a probe and the steps it implies.
type Plan struct {
	Kind    config.Kind
	State   State
	Actions []Action
	creator Creator
	req     Request
}

// Build probes and plans. It changes nothing.
func Build(kind config.Kind, t Transport, req Request) (*Plan, error) {
	if req.Username == "" {
		return nil, errors.New("a username is required")
	}
	c, err := For(kind)
	if err != nil {
		return nil, err
	}
	state, err := c.Probe(t, req)
	if err != nil {
		return nil, redact(err, req.APIKey)
	}
	actions := c.Steps(state, req)
	if req.Email == "" && slices.Contains(actions, ActionCreate) {
		return nil, fmt.Errorf("%s does not exist yet, and creating it needs --email. An administrator without an address cannot be matched to an app account it signs in to, and the address is set only when the account is created", req.Username)
	}
	return &Plan{Kind: kind, State: state, Actions: actions, creator: c, req: req}, nil
}

// Lines is the plan as printed: one line per action, or the user's name alone
// when there is nothing to do, for a line marked already there. It names the
// user and never a credential.
func (p *Plan) Lines() []string {
	if len(p.Actions) == 0 {
		return []string{p.req.Username}
	}
	out := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		if d, ok := p.creator.(describer); ok {
			out = append(out, d.Describe(a, p.req))
			continue
		}
		out = append(out, string(a)+" "+p.req.Username)
	}
	return out
}

// Titles is one short phrase per action, in the order of Lines, for the
// default output: the operator needs to see what will happen, and the request
// behind each action is Lines' business, shown only on request. Like Lines it
// names the user and the groups, never a credential.
func (p *Plan) Titles() []string {
	out := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		switch a {
		case ActionCreate:
			// The user is created as the app's administrator, which is the
			// point of the command, so the title says so.
			out = append(out, "create administrator "+p.req.Username)
		case ActionVerify:
			out = append(out, "verify "+p.req.Username)
		case ActionGrantAdmin:
			out = append(out, "make "+p.req.Username+" an administrator")
		case ActionJoinGroups:
			out = append(out, "add "+p.req.Username+" to admin groups "+strings.Join(p.req.AdminGroups, ", "))
		case ActionLoginLink:
			out = append(out, "issue login link for "+p.req.Username)
		default:
			out = append(out, string(a)+" "+p.req.Username)
		}
	}
	return out
}

// Execute runs the plan, then probes again and refuses to report success
// unless the user now exists, is verified and is an admin. A command that
// exits zero without doing its job is exactly what that second probe exists
// to catch.
func Execute(p *Plan, t Transport) (Outcome, error) {
	if len(p.Actions) == 0 {
		return Outcome{}, nil
	}
	outcome, err := p.creator.Execute(t, p.req, p.Actions)
	if err != nil {
		return Outcome{}, redact(err, p.req.APIKey, outcome.LoginLink)
	}
	after, err := p.creator.Probe(t, p.req)
	if err != nil {
		return Outcome{}, redact(err, p.req.APIKey, outcome.LoginLink)
	}
	if !after.Exists || !after.Verified || !after.Admin {
		return Outcome{}, fmt.Errorf("%s: the commands succeeded but %s is now exists=%t verified=%t admin=%t", t.Describe(), p.req.Username, after.Exists, after.Verified, after.Admin)
	}
	if len(after.MissingGroups) > 0 {
		return Outcome{}, fmt.Errorf("%s: the calls succeeded but %s is still not in %s", t.Describe(), p.req.Username, strings.Join(after.MissingGroups, ", "))
	}
	return outcome, nil
}

// redact removes every given secret from an error's text, and each one's
// base64 form as well, so a remote error that echoes its input cannot carry
// the API key or a login link out in either shape.
func redact(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, s, "[redacted]")
		msg = strings.ReplaceAll(msg, base64.StdEncoding.EncodeToString([]byte(s)), "[redacted]")
	}
	return errors.New(msg)
}

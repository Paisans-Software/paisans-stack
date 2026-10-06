// Package appadmin creates an application's first administrator on a running
// stack.
//
// An app with registrations closed has no way to make its first account from
// a browser, so the toolkit makes it, through the application's own
// administration commands run inside its container. Planning is shared by
// every kind: probe the user, decide the missing steps, and change nothing
// unless told to. Only how to probe and how to act is per kind, behind
// Creator.
//
// The password is the one input here that must never be seen. It travels only
// on stdin, never in a command line (which `ps` shows to every user on the
// host), and it is scrubbed from any output or error that comes back.
package appadmin

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

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
	// App is the stack's name, which is also its directory under /srv.
	App      string
	Username string
	Email    string
	// Password is used only by a create or a reset. It never appears in a
	// command line, a plan line, or an error.
	Password string
	// ResetPassword replaces an existing user's password. Without it an
	// existing account's password is never touched, so re-running the command
	// cannot lock out an admin who has since changed theirs.
	ResetPassword bool
}

// State is what a probe found about one username.
type State struct {
	Exists   bool
	Verified bool
	Admin    bool
}

// Action is one step towards an existing, verified administrator.
type Action string

const (
	ActionCreate        Action = "create user"
	ActionVerify        Action = "verify"
	ActionGrantAdmin    Action = "grant admin"
	ActionResetPassword Action = "reset password"
)

// Creator is what a kind implements to support admin creation. Probe must
// change nothing. Execute performs the actions in order and stops at the
// first failure.
type Creator interface {
	Probe(t Transport, req Request) (State, error)
	Execute(t Transport, req Request, actions []Action) error
}

var creators = map[config.Kind]Creator{
	config.KindMbin: mbin{},
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

// Steps decides what is missing. It is a pure function of the probe, which is
// what makes re-running safe: an existing admin plans nothing, and an
// existing account is never re-created and never has its password changed
// unless that was asked for.
func Steps(state State, req Request) []Action {
	if !state.Exists {
		return []Action{ActionCreate, ActionVerify, ActionGrantAdmin}
	}
	var out []Action
	if !state.Verified {
		out = append(out, ActionVerify)
	}
	if !state.Admin {
		out = append(out, ActionGrantAdmin)
	}
	if req.ResetPassword {
		out = append(out, ActionResetPassword)
	}
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
		return nil, redact(err, req.Password)
	}
	return &Plan{Kind: kind, State: state, Actions: Steps(state, req), creator: c, req: req}, nil
}

// Lines is the plan as printed: one line per action, or `present` when there
// is nothing to do. It names the user and never the password.
func (p *Plan) Lines() []string {
	if len(p.Actions) == 0 {
		return []string{"present " + p.req.Username}
	}
	out := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		out = append(out, string(a)+" "+p.req.Username)
	}
	return out
}

// Execute runs the plan, then probes again and refuses to report success
// unless the user now exists, is verified and is an admin. A command that
// exits zero without doing its job is exactly what that second probe exists
// to catch.
func Execute(p *Plan, t Transport) error {
	if len(p.Actions) == 0 {
		return nil
	}
	if p.req.Password == "" {
		for _, a := range p.Actions {
			if a == ActionCreate || a == ActionResetPassword {
				return fmt.Errorf("%s %s needs a password, and none was given", a, p.req.Username)
			}
		}
	}
	if err := p.creator.Execute(t, p.req, p.Actions); err != nil {
		return redact(err, p.req.Password)
	}
	after, err := p.creator.Probe(t, p.req)
	if err != nil {
		return redact(err, p.req.Password)
	}
	if !after.Exists || !after.Verified || !after.Admin {
		return fmt.Errorf("%s: the commands succeeded but %s is now exists=%t verified=%t admin=%t", t.Describe(), p.req.Username, after.Exists, after.Verified, after.Admin)
	}
	return nil
}

// redact removes every given secret from an error's text, and each one's
// base64 form as well. A Creator that encodes its stdin passes its encoded
// payload too, so a remote error that echoes its input cannot carry the
// password out in any shape this package produced.
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

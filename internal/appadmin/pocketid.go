package appadmin

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/pocketid"
)

// pocketID creates an administrator through Pocket ID's REST API, with the
// static API key, from curl on the site's host (see internal/pocketid).
//
// Pocket ID users sign in with passkeys, so there is no password to set. A
// new administrator is useless until they have registered one, and the way
// Pocket ID itself offers for that is a one-time access token: an admin
// creates one for a user (POST /api/users/:id/one-time-access-token,
// onetimeaccess/module.go:68), and opening {APP_URL}/lc/{token} signs that
// user in once, where they add a passkey. The binary's own
// `one-time-access-token` subcommand does the same
// (cmds/one_time_access_token.go:19-82) but takes the username on argv and
// always issues a one hour token, so the API is used instead, with a shorter
// one.
//
// Rejected: Pocket ID's first-admin signup page (POST /api/signup/setup,
// usersignup/module.go:87). It is open to whoever reaches it first while no
// user exists, which on a public hostname is a race with the internet, and it
// is a browser step.
//
// The account is made with its email marked verified, and an existing
// account whose email is not verified is marked so. Pocket ID otherwise sends
// email_verified false, and the paisans Mbin fork refuses an OIDC sign in
// whose email matches an existing local account unless the provider marked it
// verified: a guard against taking over an account by claiming its address
// at the provider. The operator making an administrator vouches for the
// address they typed, which is the verification the guard asks for.
//
// Rejected: turning that guard off in the fork. It is correct for every member
// who signs up at Pocket ID by themselves, and the founder's case is fixed
// where the vouching happens.
type pocketID struct{}

// loginTTL is how long a login link stays usable. It is 20 minutes, not
// Pocket ID's own 15 minute default, because the length of the code depends
// on it: a TTL of 15 minutes or less gets a 6 character code
// (onetimeaccess/service.go:271-274 at v2.14.0), and the login page accepts a
// 6 character code only when unauthenticated email login is enabled
// (frontend login/alternative/code/+page.svelte:24-29); otherwise it waits
// for 12 and its submit button stays disabled. On the first real host, which
// has no SMTP, the 15 minute link was unusable. Over 15 minutes gets the 12
// character code, which the page accepts either way.
const loginTTL = 20 * time.Minute

func (pocketID) Passwordless() {}

func (pocketID) client(t Transport, req Request) *pocketid.Client {
	return &pocketid.Client{Transport: t, BaseURL: req.APIBase, APIKey: req.APIKey}
}

func (p pocketID) Probe(t Transport, req Request) (State, error) {
	u, err := p.client(t, req).FindUser(req.Username)
	if err != nil {
		return State{}, fmt.Errorf("looking up %s in %s: %w", req.Username, req.App, err)
	}
	if u == nil {
		return State{}, nil
	}
	if u.Disabled {
		return State{}, fmt.Errorf("%s exists in %s and is disabled. Re-enabling an account is not something this command decides; do it deliberately, then re-run", req.Username, req.App)
	}
	// An account without an email has nothing to verify, and no address an
	// app could match against one of its own.
	return State{Exists: true, Verified: u.Email == nil || u.EmailVerified, Admin: u.IsAdmin}, nil
}

// Steps for a passkey account: a new one is created as an administrator and
// given a link, since it cannot sign in without one. An existing one is only
// made an administrator if it is not, has its email marked verified if it is
// not, and is given a link only if asked.
func (pocketID) Steps(state State, req Request) []Action {
	if !state.Exists {
		return []Action{ActionCreate, ActionLoginLink}
	}
	var out []Action
	if !state.Verified {
		out = append(out, ActionVerify)
	}
	if !state.Admin {
		out = append(out, ActionGrantAdmin)
	}
	if req.LoginLink {
		out = append(out, ActionLoginLink)
	}
	return out
}

// newUser is exactly what a create sends, and what the plan prints.
func (pocketID) newUser(req Request) pocketid.NewUser {
	first := req.FirstName
	if first == "" {
		first = req.Username
	}
	u := pocketid.NewUser{
		Username:    req.Username,
		FirstName:   first,
		LastName:    req.LastName,
		DisplayName: strings.TrimSpace(first + " " + req.LastName),
		IsAdmin:     true,
	}
	if req.Email != "" {
		email := req.Email
		u.Email = &email
		u.EmailVerified = true
	}
	return u
}

func (p pocketID) Describe(a Action, req Request) string {
	switch a {
	case ActionCreate:
		body, _ := json.Marshal(p.newUser(req))
		return fmt.Sprintf("create user %s as an administrator: POST /api/users %s", req.Username, body)
	case ActionVerify:
		return fmt.Sprintf("mark email verified for %s: PUT /api/users/<id> with emailVerified true and every other field sent back as it is now", req.Username)
	case ActionGrantAdmin:
		return fmt.Sprintf("grant admin %s: PUT /api/users/<id> with isAdmin true and every other field sent back as it is now", req.Username)
	case ActionLoginLink:
		return fmt.Sprintf("issue one-time login link for %s: valid %s and for one sign in, printed once and only with --execute", req.Username, loginTTL)
	}
	return string(a) + " " + req.Username
}

func (p pocketID) Execute(t Transport, req Request, actions []Action) (Outcome, error) {
	c := p.client(t, req)
	var user *pocketid.User
	load := func() error {
		if user != nil {
			return nil
		}
		found, err := c.FindUser(req.Username)
		if err != nil {
			return err
		}
		if found == nil {
			return fmt.Errorf("%s does not exist in %s", req.Username, req.App)
		}
		user = found
		return nil
	}
	var out Outcome
	for _, a := range actions {
		switch a {
		case ActionCreate:
			created, err := c.CreateUser(p.newUser(req))
			if err != nil {
				return Outcome{}, fmt.Errorf("creating %s: %w", req.Username, err)
			}
			user = &created
		// Each update sends the whole user, so the copy held here is kept
		// as Pocket ID now has it, or the second update would undo the first.
		case ActionVerify:
			if err := load(); err != nil {
				return Outcome{}, err
			}
			if err := c.VerifyEmail(*user); err != nil {
				return Outcome{}, fmt.Errorf("marking %s's email verified: %w", req.Username, err)
			}
			user.EmailVerified = true
		case ActionGrantAdmin:
			if err := load(); err != nil {
				return Outcome{}, err
			}
			if err := c.SetAdmin(*user); err != nil {
				return Outcome{}, fmt.Errorf("making %s an administrator: %w", req.Username, err)
			}
			user.IsAdmin = true
		case ActionLoginLink:
			if err := load(); err != nil {
				return Outcome{}, err
			}
			token, err := c.LoginToken(user.ID, loginTTL)
			if err != nil {
				return Outcome{}, fmt.Errorf("issuing a login link for %s: %w", req.Username, err)
			}
			out.LoginLink = pocketid.LoginLink(req.PublicURL, token)
			out.ExpiresIn = loginTTL
		default:
			return Outcome{}, fmt.Errorf("pocket-id has no step for %q", a)
		}
	}
	return out, nil
}

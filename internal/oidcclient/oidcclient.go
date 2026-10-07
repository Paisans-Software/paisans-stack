// Package oidcclient creates an app's client at the deployment's Pocket ID,
// with the groups the app reads, and records the client's credentials in the
// secrets file.
//
// Every step is a Pocket ID client, group or user mutation, and those need a
// human's approval each time. So the shape is the one `storage init` and
// `app admin create` use: probe read only, plan one printed line per
// mutation, showing what it sends, and change nothing until the operator
// re-runs with --execute. The printed plan is what is approved.
//
// The client secret is never printed and never returned by Pocket ID to be
// captured. It is generated on the workstation, written into the secrets file
// first, and only then sent to Pocket ID as a caller supplied value
// (dto/oidc_dto.go:77-83 at tag v2.14.0). The order matters: an interruption
// after the write leaves a recorded secret Pocket ID does not hold, which the
// next run sees by its prefix and sends; the other order would leave a live
// secret at Pocket ID that exists nowhere else.
package oidcclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/pocketid"
)

// Desired is the client an app needs, from its kind and its configuration.
type Desired struct {
	// App is the app's name, which is also the client's name at Pocket ID and
	// its key under oidc_clients in the secrets file.
	App         string
	CallbackURL string
	// LaunchURL is what the app's tile on Pocket ID's dashboard opens, and a
	// client without one is not listed there. Empty plans nothing for it.
	LaunchURL string
	// ToolkitLaunchURLs are the launch URLs this toolkit has set by default
	// (kinds.ToolkitLaunchURLs). An existing client holding one of them, or
	// none, is moved to LaunchURL. Any other value is the operator's and is
	// never overwritten.
	ToolkitLaunchURLs []string
	// LaunchURLChosen is whether the operator set LaunchURL through the app's
	// sso_dashboard_link, rather than taking the kind's default. A custom
	// value that differs from a chosen one is warned about, since the
	// operator evidently wants the tile to go somewhere it does not.
	LaunchURLChosen bool
	PKCE            bool
	// AdminGroup is the group whose members the app makes administrators, and
	// MemberGroup the group a member must be in to sign in at all. Each is
	// empty when the app's configuration names none. A member group also
	// restricts the client at Pocket ID to that group and the admin group, so
	// a refused member is stopped before the app sees them.
	AdminGroup  string
	MemberGroup string
	// AdminUser is a username to add to AdminGroup, or empty.
	AdminUser string
	// RotateSecret adds a new secret even when the recorded one is live.
	RotateSecret bool
}

// Recorded is what the secrets file holds for this client now.
type Recorded struct {
	ClientID     string
	ClientSecret string
}

// Recorder writes the client's credentials into the secrets file. It is
// called before Pocket ID is sent a secret.
type Recorder interface {
	Record(clientID, clientSecret string) error
}

// State is what a probe found. It changes nothing to find it.
type State struct {
	Client    *pocketid.OIDCClient
	Secrets   []pocketid.ClientSecret
	Groups    map[string]*pocketid.Group
	AdminUser *pocketid.User
}

// Probe reads everything the plan depends on.
func Probe(c *pocketid.Client, d Desired) (State, error) {
	var s State
	var err error
	if s.Client, err = c.FindOIDCClient(d.App); err != nil {
		return s, fmt.Errorf("looking up client %s: %w", d.App, err)
	}
	if s.Client != nil {
		if s.Secrets, err = c.ClientSecrets(s.Client.ID); err != nil {
			return s, fmt.Errorf("listing client %s's secrets: %w", d.App, err)
		}
	}
	s.Groups = map[string]*pocketid.Group{}
	for _, name := range d.groups() {
		if s.Groups[name], err = c.FindGroup(name); err != nil {
			return s, fmt.Errorf("looking up group %s: %w", name, err)
		}
	}
	if d.AdminUser != "" {
		if s.AdminUser, err = c.FindUser(d.AdminUser); err != nil {
			return s, fmt.Errorf("looking up user %s: %w", d.AdminUser, err)
		}
	}
	return s, nil
}

// groups is the group names the app reads, without duplicates, member first.
func (d Desired) groups() []string {
	var out []string
	for _, g := range []string{d.MemberGroup, d.AdminGroup} {
		if g != "" && (len(out) == 0 || out[0] != g) {
			out = append(out, g)
		}
	}
	return out
}

// restricted reports whether the client should admit only some groups.
func (d Desired) restricted() bool { return d.MemberGroup != "" }

// StepKind is one kind of mutation.
type StepKind int

const (
	CreateGroup StepKind = iota
	CreateClient
	AllowGroups
	// AddSecret generates a secret, records it, then sends it.
	AddSecret
	// SendRecordedSecret sends the secret already recorded, which Pocket ID
	// does not hold because an earlier run stopped between the two.
	SendRecordedSecret
	// RecordClientID writes only the client ID, when the recorded secret is
	// live but the ID beside it is missing.
	RecordClientID
	AddUserToGroup
	// SetLaunchURL gives an existing client the launch URL it lacks, or moves
	// it off a value the toolkit set by default.
	SetLaunchURL
)

// Step is one mutation and the line that shows it.
type Step struct {
	Kind  StepKind
	Group string
	Line  string
}

// Plan is a probe, what is already right, and the steps that remain.
type Plan struct {
	Desired  Desired
	Recorded Recorded
	State    State
	Present  []string
	Steps    []Step
	// Warnings are things the operator should know and the plan does not
	// change. They do not stop the run.
	Warnings []string
}

// Build decides the steps. It is a pure function of the probe and the
// recorded credentials, which is what makes re-running safe: everything
// already right plans nothing.
//
// It refuses rather than changes an existing client that differs in a way the
// app cannot work with. Rewriting a client means a full update of every
// field (dto/oidc_dto.go:41-60), and a client somebody shaped by hand is not
// this command's to reshape.
//
// The launch URL is the one exception, and only when the toolkit put the
// current value there: empty, which is a client made before this command set
// one, or one of the toolkit's own earlier defaults (Desired.ToolkitLaunchURLs).
// Neither is a choice anybody made. A launch URL set to anything else may be
// deliberate and is left alone, with a warning when the operator has chosen a
// different one in the configuration.
func Build(d Desired, rec Recorded, s State) (*Plan, error) {
	p := &Plan{Desired: d, Recorded: rec, State: s}
	if d.AdminUser != "" && d.AdminGroup == "" {
		return nil, fmt.Errorf("--admin-user %s names nobody to add them to: apps.%s.config sets no admin group for the app to read", d.AdminUser, d.App)
	}
	if d.AdminUser != "" && s.AdminUser == nil {
		return nil, fmt.Errorf("user %s does not exist at Pocket ID. Create it first with `paisans app admin create` against the Pocket ID app, then re-run", d.AdminUser)
	}

	client := s.Client
	if client != nil {
		if err := checkExisting(d, client); err != nil {
			return nil, err
		}
		p.Present = append(p.Present, fmt.Sprintf("present client %s (id %s)", d.App, client.ID))
	}

	for _, g := range d.groups() {
		if s.Groups[g] == nil {
			body, _ := json.Marshal(map[string]string{"name": g, "friendlyName": g})
			p.Steps = append(p.Steps, Step{Kind: CreateGroup, Group: g, Line: fmt.Sprintf("create group %s: POST /api/user-groups %s", g, body)})
		} else {
			p.Present = append(p.Present, "present group "+g)
		}
	}

	if client == nil {
		body, _ := json.Marshal(d.newClient())
		p.Steps = append(p.Steps, Step{Kind: CreateClient, Line: fmt.Sprintf("create client %s: POST /api/oidc/clients %s", d.App, body)})
	} else if d.LaunchURL != "" {
		current := ""
		if client.LaunchURL != nil {
			current = *client.LaunchURL
		}
		switch {
		case current == d.LaunchURL:
			p.Present = append(p.Present, fmt.Sprintf("present client %s launch URL %s", d.App, d.LaunchURL))
		case current == "" || d.setByToolkit(current):
			p.Steps = append(p.Steps, Step{Kind: SetLaunchURL, Line: fmt.Sprintf("set launch URL for client %s: PUT /api/oidc/clients/%s with launchURL %q and every other field sent back as it is now", d.App, client.ID, d.LaunchURL)})
		default:
			p.Present = append(p.Present, fmt.Sprintf("present client %s launch URL %s, not %s; left as it is, since it may have been set deliberately", d.App, current, d.LaunchURL))
			if d.LaunchURLChosen {
				p.Warnings = append(p.Warnings, fmt.Sprintf("apps.%s.settings.sso_dashboard_link gives launch URL %s, but client %s has %s, which this toolkit did not set and will not overwrite. To use the setting, clear the client's launch URL in Pocket ID's admin UI and re-run; to keep the client's, remove the setting or make it match", d.App, d.LaunchURL, d.App, current))
			}
		}
	}

	if d.restricted() {
		var missing []string
		for _, g := range d.groups() {
			if client == nil || !allows(client, g) {
				missing = append(missing, g)
			}
		}
		if len(missing) > 0 {
			p.Steps = append(p.Steps, Step{Kind: AllowGroups, Line: fmt.Sprintf("allow groups %s on client %s: PUT /api/oidc/clients/<id>/allowed-user-groups with the groups it allows now, plus these", strings.Join(missing, ", "), d.App)})
		} else {
			p.Present = append(p.Present, fmt.Sprintf("present client %s allows %s", d.App, strings.Join(d.groups(), ", ")))
		}
	}

	key := "oidc_clients." + d.App
	switch {
	case client == nil || d.RotateSecret || rec.ClientSecret == "" || (rec.ClientID != "" && rec.ClientID != client.ID):
		if client != nil && len(s.Secrets) >= pocketid.MaxClientSecrets {
			return nil, fmt.Errorf("client %s already holds %d secrets, Pocket ID's limit (model/oidc.go:41). Delete an unused one before adding another", d.App, len(s.Secrets))
		}
		line := fmt.Sprintf("create client secret for %s: generated on this workstation, written to %s.client_id and %s.client_secret, then sent to POST /api/oidc/clients/<id>/secrets. Never printed", d.App, key, key)
		if client != nil && len(s.Secrets) > 0 {
			line += fmt.Sprintf(". The %d secret(s) it holds now stay valid until deleted", len(s.Secrets))
		}
		p.Steps = append(p.Steps, Step{Kind: AddSecret, Line: line})
	case !pocketid.HasActiveSecret(s.Secrets, rec.ClientSecret):
		if len(s.Secrets) >= pocketid.MaxClientSecrets {
			return nil, fmt.Errorf("client %s already holds %d secrets, Pocket ID's limit (model/oidc.go:41). Delete an unused one before adding another", d.App, len(s.Secrets))
		}
		p.Steps = append(p.Steps, Step{Kind: SendRecordedSecret, Line: fmt.Sprintf("send the secret recorded at %s.client_secret, which client %s does not hold, to POST /api/oidc/clients/%s/secrets. Never printed", key, d.App, client.ID)})
	case rec.ClientID == "":
		p.Steps = append(p.Steps, Step{Kind: RecordClientID, Line: fmt.Sprintf("record %s.client_id %s, beside a secret that is already live", key, client.ID)})
	default:
		p.Present = append(p.Present, fmt.Sprintf("present %s.client_secret, which matches an active secret of client %s by its first %d characters", key, d.App, pocketid.SecretPrefixLength))
	}

	if d.AdminUser != "" {
		group := s.Groups[d.AdminGroup]
		if group == nil || !s.AdminUser.InGroup(group.ID) {
			p.Steps = append(p.Steps, Step{Kind: AddUserToGroup, Group: d.AdminGroup, Line: fmt.Sprintf("add %s to group %s: PUT /api/users/%s/user-groups with the groups %s is in now, plus %s", d.AdminUser, d.AdminGroup, s.AdminUser.ID, d.AdminUser, d.AdminGroup)})
		} else {
			p.Present = append(p.Present, fmt.Sprintf("present %s in group %s", d.AdminUser, d.AdminGroup))
		}
	}
	return p, nil
}

// setByToolkit reports whether a launch URL is one this toolkit sets by
// default, and so is not an operator's choice.
func (d Desired) setByToolkit(launchURL string) bool {
	for _, u := range d.ToolkitLaunchURLs {
		if u == launchURL {
			return true
		}
	}
	return false
}

// newClient is exactly what a create sends, and what the plan prints. A
// confidential client: Mbin holds a secret, and a public client cannot have
// one (service/oidc_service.go:352-354).
func (d Desired) newClient() pocketid.NewOIDCClient {
	return pocketid.NewOIDCClient{
		Name:              d.App,
		CallbackURLs:      []string{d.CallbackURL},
		PkceEnabled:       d.PKCE,
		IsGroupRestricted: d.restricted(),
		LaunchURL:         d.LaunchURL,
	}
}

func checkExisting(d Desired, c *pocketid.OIDCClient) error {
	var wrong []string
	found := false
	for _, u := range c.CallbackURLs {
		if u == d.CallbackURL {
			found = true
		}
	}
	if !found {
		wrong = append(wrong, fmt.Sprintf("its callback URLs %v do not include %s", c.CallbackURLs, d.CallbackURL))
	}
	if d.PKCE && !c.PkceEnabled {
		wrong = append(wrong, "PKCE is off, and the app always sends a code challenge")
	}
	if c.IsPublic {
		wrong = append(wrong, "it is a public client, which cannot hold the secret the app signs in with")
	}
	if d.restricted() && !c.IsGroupRestricted {
		wrong = append(wrong, fmt.Sprintf("it is not restricted to groups, and the app's configuration names a member group, %s", d.MemberGroup))
	}
	if len(wrong) == 0 {
		return nil
	}
	return fmt.Errorf("client %s exists at Pocket ID (id %s) and differs from what the app needs: %s. This command creates clients and does not reshape one that exists; fix it there, or delete it and re-run", d.App, c.ID, strings.Join(wrong, "; "))
}

func allows(c *pocketid.OIDCClient, group string) bool {
	for _, g := range c.AllowedUserGroups {
		if g.Name == group {
			return true
		}
	}
	return false
}

// Execute runs the plan's steps in order and stops at the first failure.
// newSecret makes a fresh client secret. After the steps it probes again and
// refuses to report success unless a fresh plan would do nothing, because a
// call that answers 2xx without doing its job is the failure nobody notices.
func Execute(p *Plan, c *pocketid.Client, rec Recorder, newSecret func() (string, error)) error {
	d := p.Desired
	groups := map[string]*pocketid.Group{}
	for name, g := range p.State.Groups {
		groups[name] = g
	}
	client := p.State.Client
	recorded := p.Recorded
	var secretsToHide []string

	for _, step := range p.Steps {
		var err error
		switch step.Kind {
		case CreateGroup:
			var g pocketid.Group
			if g, err = c.CreateGroup(step.Group); err == nil {
				groups[step.Group] = &g
			}
		case CreateClient:
			var created pocketid.OIDCClient
			if created, err = c.CreateOIDCClient(d.newClient()); err == nil {
				client = &created
			}
		case SetLaunchURL:
			err = c.SetLaunchURL(client.ID, d.LaunchURL)
		case AllowGroups:
			ids := map[string]bool{}
			for _, g := range client.AllowedUserGroups {
				ids[g.ID] = true
			}
			for _, name := range d.groups() {
				if groups[name] == nil {
					err = fmt.Errorf("group %s was not created", name)
					break
				}
				ids[groups[name].ID] = true
			}
			if err == nil {
				err = c.SetAllowedGroups(client.ID, sortedKeys(ids))
			}
		case AddSecret:
			var secret string
			if secret, err = newSecret(); err != nil {
				break
			}
			secretsToHide = append(secretsToHide, secret)
			if err = rec.Record(client.ID, secret); err != nil {
				err = fmt.Errorf("recording oidc_clients.%s: %w. Pocket ID was sent nothing", d.App, err)
				break
			}
			recorded = Recorded{ClientID: client.ID, ClientSecret: secret}
			if err = c.AddClientSecret(client.ID, secret); err != nil {
				err = fmt.Errorf("%w. oidc_clients.%s holds the new secret and Pocket ID does not: re-run to send it, and do not apply %s until it has", err, d.App, d.App)
			}
		case SendRecordedSecret:
			secretsToHide = append(secretsToHide, recorded.ClientSecret)
			if recorded.ClientID != client.ID {
				if err = rec.Record(client.ID, recorded.ClientSecret); err != nil {
					break
				}
			}
			err = c.AddClientSecret(client.ID, recorded.ClientSecret)
		case RecordClientID:
			secretsToHide = append(secretsToHide, recorded.ClientSecret)
			err = rec.Record(client.ID, recorded.ClientSecret)
			recorded.ClientID = client.ID
		case AddUserToGroup:
			group := groups[step.Group]
			if group == nil {
				err = fmt.Errorf("group %s was not created", step.Group)
				break
			}
			ids := map[string]bool{group.ID: true}
			for _, g := range p.State.AdminUser.UserGroups {
				ids[g.ID] = true
			}
			err = c.SetUserGroups(p.State.AdminUser.ID, sortedKeys(ids))
		default:
			err = fmt.Errorf("no step %d", step.Kind)
		}
		if err != nil {
			return pocketid.Redact(fmt.Errorf("%s: %w", firstWords(step.Line), err), secretsToHide...)
		}
	}

	// A rotation is done once its secret is sent; asking the fresh plan for
	// another would make the check below fail on its own success.
	d.RotateSecret = false
	after, err := Probe(c, d)
	if err != nil {
		return pocketid.Redact(err, secretsToHide...)
	}
	again, err := Build(d, recorded, after)
	if err != nil {
		return pocketid.Redact(fmt.Errorf("after the changes: %w", err), secretsToHide...)
	}
	if len(again.Steps) > 0 {
		var left []string
		for _, s := range again.Steps {
			left = append(left, firstWords(s.Line))
		}
		return errors.New("the calls succeeded but a fresh probe still plans: " + strings.Join(left, "; "))
	}
	return nil
}

// firstWords is a step line up to its colon, for an error that should name
// the step without repeating its payload.
func firstWords(line string) string {
	if i := strings.Index(line, ":"); i > 0 {
		return line[:i]
	}
	return line
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Deterministic, so a request is the same for the same state.
	sort.Strings(out)
	return out
}

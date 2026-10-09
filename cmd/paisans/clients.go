package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/oidcclient"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// The path from an app's declaration to its client at Pocket ID, shared by
// `oidc client create` and apply's identity step so that both plan, send and
// record exactly the same thing.

// pocketIDApp names the deployment's pocket-id app, empty when it declares
// none.
func pocketIDApp(cfg *config.Config) string {
	idp := ""
	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Kind == config.KindPocketID {
			idp = name
		}
	}
	return idp
}

// clientDesired is the client an app needs at Pocket ID, from its kind and
// its declaration, and whether this toolkit knows its kind's client.
func clientDesired(name string, app config.App, rotate bool) (oidcclient.Desired, bool) {
	spec, ok := kinds.OIDCClient(app.Kind, app.Hostname)
	if !ok {
		return oidcclient.Desired{}, false
	}
	adminGroup, memberGroup := spec.Groups(app)
	d := oidcclient.Desired{
		App:               name,
		CallbackURL:       spec.CallbackURL,
		LaunchURL:         spec.LaunchURL,
		ToolkitLaunchURLs: kinds.ToolkitLaunchURLs(app.Kind, app.Hostname),
		PKCE:              spec.PKCE,
		AdminGroup:        adminGroup,
		MemberGroup:       memberGroup,
		RotateSecret:      rotate,
	}
	// validate.Check has refused a malformed one already.
	if link, ok := app.Settings[kinds.DashboardLinkSetting].(string); ok {
		d.LaunchURL = kinds.LaunchURL(app.Kind, app.Hostname, link)
		d.LaunchURLChosen = true
	}
	return d, true
}

// recordedClient is what the secrets file holds for an app's client.
func recordedClient(secrets *config.Secrets, app string) oidcclient.Recorded {
	return oidcclient.Recorded{
		ClientID:     secrets.OIDCClients[app].ClientID,
		ClientSecret: secrets.OIDCClients[app].ClientSecret,
	}
}

// clientAPI is Pocket ID's API as the host on site reaches it, over the
// site's ssh section or destination verbatim, and without sudo: curl needs
// no root.
func clientAPI(cfg *config.Config, site, destination, key string) *pocketid.Client {
	return &pocketid.Client{Transport: oidcTransport(siteTransport(site, cfg.Sites[site], destination, false)), BaseURL: pocketIDBase(cfg, site), APIKey: key}
}

// probeError is a client plan that failed because Pocket ID could not be
// asked. apply treats it as Pocket ID being unreachable, which holds back
// only the apps that have no client yet, rather than as a refusal.
type probeError struct{ err error }

func (e *probeError) Error() string { return e.err.Error() }
func (e *probeError) Unwrap() error { return e.err }

// planClient probes Pocket ID for one app's client and plans it.
func planClient(api *pocketid.Client, d oidcclient.Desired, rec oidcclient.Recorded) (*oidcclient.Plan, error) {
	state, err := probeClient(api, d)
	if err != nil {
		return nil, err
	}
	return buildClient(d, rec, state)
}

// probeClient reads what Pocket ID holds for one app's client. It only
// reads, and a failure is a *probeError.
func probeClient(api *pocketid.Client, d oidcclient.Desired) (oidcclient.State, error) {
	state, err := oidcclient.Probe(api, d)
	if err != nil {
		return state, &probeError{err}
	}
	return state, nil
}

// buildClient plans one app's client from a probe. A refusal has the
// recorded secret redacted.
func buildClient(d oidcclient.Desired, rec oidcclient.Recorded, state oidcclient.State) (*oidcclient.Plan, error) {
	plan, err := oidcclient.Build(d, rec, state)
	if err != nil {
		return nil, pocketid.Redact(err, rec.ClientSecret)
	}
	return plan, nil
}

// listClientPlan shows one app's client in a dry run: each mutation as an
// item, with what it sends as its detail, and what is already present as
// details, since only what would change is a line by default. The plan's
// warnings are the caller's to show, once whether or not it lists the plan.
func listClientPlan(r ui.Reporter, app, idp, where string, plan *oidcclient.Plan) {
	r.Detail("%s's client at %s on %s (pocket-id)", app, idp, where)
	for _, line := range plan.Present {
		r.Detail("%s", line)
	}
	for _, step := range plan.Steps {
		r.Item(step.Title)
		r.Detail("%s", step.Detail)
	}
}

// clientStep is apply's identity step for one site. Every app the site runs
// whose kind has a client shape (kinds.OIDCClient) gets its client ensured
// at the deployment's Pocket ID before it renders, with the same plan and
// the same recorder as `oidc client create`. Declaring the app in
// paisans.yaml is the approval for its client (founder decision,
// 2026-10-08), so apply does not ask again. It never rotates a secret;
// `oidc client create --rotate-secret` does.
type clientStep struct {
	// report is where the step's plan and progress go.
	report ui.Reporter
	// executing is whether the command runs with --execute. The plan is
	// listed on a dry run, and on --execute only with --verbose, since the
	// steps that carry it out say the same.
	executing bool
	// warned is every plan warning already shown in this run, so the plan
	// made before Pocket ID is called and the one carried out do not both
	// show it. It survives reset, which starts each ensure.
	warned      map[string]bool
	cfg         *config.Config
	site        string
	destination string
	secretsPath string
	secrets     *config.Secrets
	recipients  []string
	idp         string
	// apps are the apps on this site whose kind has a client shape, sorted.
	apps []string
	// held is every app held back from this apply, with why.
	held map[string]string
	// refused is every held app whose client was refused rather than merely
	// unreachable. These fail the apply once the rest of the site is done.
	refused map[string]error
	// steps counts the mutations the last ensure planned.
	steps int
	// heldOverwrites are --overwrite paths the last pass did not write,
	// because their stack was held back.
	heldOverwrites []string
	// signupGroups are the Pocket ID app's signup_default_groups, when this
	// site runs it, which apply resolves to IDs (see ensureGroups). Empty
	// otherwise.
	signupGroups []string
	// groupsErr is why the last ensure could not put a signup group in
	// place. It fails the apply once the rest of the site is done.
	groupsErr error
}

// newClientStep is the identity step for site, or nil when the deployment
// declares no pocket-id app, or the site runs no app with a client shape
// among those --only names and no Pocket ID with signup_default_groups.
// Without a Pocket ID there is no client to make, and an app is applied as it
// always was, without sign in. It refuses, before
// Pocket ID is asked anything, a secrets file a new client could not be
// written back into: encrypted, with no recipient beside it. The secret is
// written before Pocket ID is sent it, so a run that cannot write must not
// start.
func newClientStep(r ui.Reporter, cfg *config.Config, site, destination, secretsPath string, secrets *config.Secrets, only []string) (*clientStep, error) {
	sites := render.AppSites(cfg)
	c := &clientStep{report: r, cfg: cfg, site: site, destination: destination, secretsPath: secretsPath, secrets: secrets, idp: pocketIDApp(cfg)}
	if c.idp == "" {
		return nil, nil
	}
	for _, name := range cfg.AppNames() {
		if !slices.Contains(sites[name], site) || (len(only) > 0 && !slices.Contains(only, name)) {
			continue
		}
		if _, ok := kinds.OIDCClient(cfg.Apps[name].Kind, cfg.Apps[name].Hostname); ok {
			c.apps = append(c.apps, name)
		}
		if name == c.idp {
			c.signupGroups = kinds.PocketIDSignupGroups(cfg.Apps[name].Settings)
		}
	}
	if len(c.apps) == 0 && len(c.signupGroups) == 0 {
		return nil, nil
	}
	recipients, err := config.Recipients(filepath.Dir(secretsPath))
	if err != nil {
		return nil, err
	}
	c.recipients = recipients
	var missing []string
	for _, app := range c.apps {
		if r := c.secrets.OIDCClients[app]; r.ClientID == "" && r.ClientSecret == "" {
			missing = append(missing, app)
		}
	}
	if secrets.Encrypted && len(recipients) == 0 && len(missing) > 0 {
		return nil, fmt.Errorf("apply: %s is encrypted, but no %s beside it names a recipient, so the client %s needs could not be written back encrypted. Nothing was changed", secretsPath, config.SOPSConfigName, strings.Join(missing, ", "))
	}
	var unrecorded []string
	for _, g := range c.signupGroups {
		if secrets.PocketIDGroups[g] == "" {
			unrecorded = append(unrecorded, g)
		}
	}
	if secrets.Encrypted && len(recipients) == 0 && len(unrecorded) > 0 {
		return nil, fmt.Errorf("apply: %s is encrypted, but no %s beside it names a recipient, so the ID of signup group %s could not be written back encrypted. Nothing was changed", secretsPath, config.SOPSConfigName, strings.Join(unrecorded, ", "))
	}
	c.reset()
	return c, nil
}

// reporter is the step's report, or ui.Discard for a step made without one.
func (c *clientStep) reporter() ui.Reporter {
	if c.report == nil {
		return ui.Discard
	}
	return c.report
}

// warn shows a plan warning, once per run.
func (c *clientStep) warn(w string) {
	if c.warned == nil {
		c.warned = map[string]bool{}
	}
	if c.warned[w] {
		return
	}
	c.warned[w] = true
	c.reporter().Warn(w, "")
}

func (c *clientStep) reset() {
	c.held, c.refused, c.steps, c.groupsErr = map[string]string{}, map[string]error{}, 0, nil
}

// recorded reports whether the secrets file holds both halves of app's
// client, which is what rendering it needs.
func (c *clientStep) recorded(app string) bool {
	r := c.secrets.OIDCClients[app]
	return r.ClientID != "" && r.ClientSecret != ""
}

// pocketIDHere reports whether this site runs the Pocket ID the clients are
// made at, in which case it starts before the apps that sign in through it.
func (c *clientStep) pocketIDHere() bool {
	return c.idp != "" && slices.Contains(render.AppSites(c.cfg)[c.idp], c.site)
}

// holdForPocketID is every app stack on this site except Pocket ID's: what
// the first pass on a site running Pocket ID holds back, so that only the
// infrastructure and Pocket ID move before the clients exist.
func (c *clientStep) holdForPocketID() []string {
	sites := render.AppSites(c.cfg)
	var out []string
	for _, name := range c.cfg.AppNames() {
		if name != c.idp && slices.Contains(sites[name], c.site) {
			out = append(out, name)
		}
	}
	return out
}

// heldApps is every app held back, sorted.
func (c *clientStep) heldApps() []string {
	out := make([]string, 0, len(c.held))
	for app := range c.held {
		out = append(out, app)
	}
	sort.Strings(out)
	return out
}

// ensure plans every app's client and, with execute, puts it in place and
// records it, one step per mutation. The read only call lists each plan as
// `oidc client create` does, unless the command executes without --verbose;
// the executing call lists nothing, since its steps say the same. Without
// execute, Pocket ID is only read. It returns an error only for a refusal of
// the whole step, made before Pocket ID is sent anything; a refused app is
// held back and reported by result.
//
// waiting is for a dry run on a site whose Pocket ID this apply starts
// first: a Pocket ID that does not answer yet is then no reason to hold an
// app back, since the apply will start it before the clients are made.
func (c *clientStep) ensure(execute, waiting bool) error {
	c.reset()
	r := c.reporter()
	if len(c.apps) > 0 {
		r.Detail("OIDC clients for %s, before they start", strings.Join(c.apps, ", "))
	}
	key, _ := c.secrets.Apps[c.idp]["static_api_key"].(string)
	if key == "" {
		sites := render.AppSites(c.cfg)[c.idp]
		why := fmt.Sprintf("secrets apps.%s.static_api_key is missing, so Pocket ID's API cannot be called. Run `paisans init` to generate it, apply %s (where %s runs) so Pocket ID has it, then apply this site again", c.idp, strings.Join(sites, " and "), c.idp)
		c.cannotAsk(c.apps, why, false)
		c.groupsCannotAsk(c.signupGroups, why, false)
		return nil
	}
	where, err := c.pocketIDWhere()
	if err != nil {
		c.cannotAsk(c.apps, unreachable(err), waiting)
		c.groupsCannotAsk(c.signupGroups, unreachable(err), waiting)
		return nil
	}
	destination := ""
	if where == c.site {
		destination = c.destination
	}
	api := clientAPI(c.cfg, where, destination, key)

	type planned struct {
		app  string
		plan *oidcclient.Plan
	}
	var plans []planned
	// creating is every group a client plan creates, which a dry run's
	// group lines then name as made by that step rather than planning a
	// second create.
	creating := map[string]bool{}
	// down is why Pocket ID stopped answering part way, so the groups are
	// not asked about either.
	down := ""
	for i, app := range c.apps {
		desired, _ := clientDesired(app, c.cfg.Apps[app], false)
		recorded := recordedClient(c.secrets, app)
		state, err := probeClient(api, desired)
		if err != nil {
			down = unreachable(fmt.Errorf("Pocket ID on %s could not be asked: %w", where, err))
			c.cannotAsk(c.apps[i:], down, waiting)
			break
		}
		if err := keepsRecorded(app, recorded, state); err != nil {
			c.refuse(app, err)
			continue
		}
		plan, err := buildClient(desired, recorded, state)
		if err != nil {
			c.refuse(app, err)
			continue
		}
		if !execute && (!c.executing || r.Verbose()) {
			listClientPlan(r, app, c.idp, where, plan)
		}
		for _, w := range plan.Warnings {
			c.warn(w)
		}
		c.steps += len(plan.Steps)
		for _, step := range plan.Steps {
			if step.Kind == oidcclient.CreateGroup {
				creating[step.Group] = true
			}
		}
		plans = append(plans, planned{app, plan})
	}
	if !execute {
		if down != "" {
			c.groupsCannotAsk(c.signupGroups, down, waiting)
		} else {
			c.ensureGroups(api, where, false, creating, waiting)
		}
		return nil
	}

	// Every plan is made before any is carried out, so a step that would
	// write the secrets file is refused before Pocket ID is sent anything,
	// rather than after another app's client was created.
	if c.secrets.Encrypted && len(c.recipients) == 0 {
		var recording []string
		for _, p := range plans {
			for _, step := range p.plan.Steps {
				if step.Kind == oidcclient.AddSecret || step.Kind == oidcclient.RecordClientID {
					recording = append(recording, p.app)
					break
				}
			}
		}
		if len(recording) > 0 {
			return fmt.Errorf("apply: %s is encrypted, but no %s beside it names a recipient, so the client %s needs could not be written back encrypted. Pocket ID was sent nothing", c.secretsPath, config.SOPSConfigName, strings.Join(recording, ", "))
		}
	}

	for _, p := range plans {
		if len(p.plan.Steps) == 0 {
			continue
		}
		rec := &secretsRecorder{app: p.app, path: c.secretsPath, secrets: c.secrets, recipients: c.recipients}
		p.plan.Report = r
		if err := oidcclient.Execute(p.plan, api, rec, secretsgen.ClientSecret); err != nil {
			c.refuse(p.app, err)
			continue
		}
		if rec.wrote && len(c.recipients) == 0 {
			warnUnencrypted(r, c.secretsPath)
		}
	}
	// After the clients, so that a group a client step just created is
	// found rather than created twice.
	if down != "" {
		c.groupsCannotAsk(c.signupGroups, down, false)
	} else {
		c.ensureGroups(api, where, true, nil, false)
	}
	return nil
}

// keepsRecorded refuses an app whose recorded credentials Pocket ID does not
// hold as that app's client. apply creates a client only when nothing is
// recorded; replacing recorded credentials is the operator's call, made with
// `oidc client create`, which re-records them. Session owner decision,
// 2026-10-08.
func keepsRecorded(app string, rec oidcclient.Recorded, state oidcclient.State) error {
	var why string
	switch {
	case rec.ClientID == "" && rec.ClientSecret == "":
		return nil
	case rec.ClientID == "" || rec.ClientSecret == "":
		why = fmt.Sprintf("oidc_clients.%s is only partly recorded", app)
	case state.Client == nil:
		why = fmt.Sprintf("oidc_clients.%s.client_id is %s, and Pocket ID has no client named %s", app, rec.ClientID, app)
	case state.Client.ID != rec.ClientID:
		why = fmt.Sprintf("oidc_clients.%s.client_id is %s, and Pocket ID's client %s has ID %s", app, rec.ClientID, app, state.Client.ID)
	default:
		return nil
	}
	return fmt.Errorf("%s. apply does not replace recorded credentials: run `paisans oidc client create --app %s` to create or re-record the client, then apply again", why, app)
}

// pocketIDWhere is the site to call Pocket ID on, as `oidc client create`
// finds it without --site: its own site when pinned, otherwise the site
// whose instance is active.
func (c *clientStep) pocketIDWhere() (string, error) {
	if !slices.Contains(apply.StandbyApps(c.cfg), c.idp) {
		return adminSite(c.cfg, c.idp, "", "apply")
	}
	site, list, err := activeInstance(c.cfg, c.idp, c.site, c.destination)
	if err != nil {
		var states []string
		for _, in := range list {
			states = append(states, fmt.Sprintf("%s %s", in.Site, in.State))
		}
		return "", fmt.Errorf("%w (%s)", err, strings.Join(states, ", "))
	}
	return site, nil
}

// unreachable is why an app is held back when Pocket ID did not answer.
func unreachable(err error) string {
	return err.Error() + ". A re-run once Pocket ID answers creates the client and starts the app"
}

// cannotAsk is what happens to apps when Pocket ID cannot be asked about
// them. An app with a recorded client goes ahead on it: the client was
// right when it was made, and an outage elsewhere is no reason to hold back
// an app that can already sign people in. An app with none is held back,
// since it cannot render credentials that do not exist, and a re-run once
// Pocket ID answers finishes it.
func (c *clientStep) cannotAsk(apps []string, why string, waiting bool) {
	if waiting {
		// A dry run's plan item. --execute starts Pocket ID first and then
		// reports the clients as steps, so it is not listed there.
		r := c.reporter()
		if c.executing {
			c.steps++
			return
		}
		r.Item("ensure OIDC clients after " + c.idp + " starts")
		r.Detail("client for %s once pocket-id %s on %s has started and answers, before %s starts. It does not answer yet: %s",
			strings.Join(apps, ", "), c.idp, c.site, strings.Join(apps, ", "), why)
		c.steps++
		return
	}
	for _, app := range apps {
		// The reason is long and the same for every app held back by one
		// outage, so the line says what became of the app and the reason
		// follows under --verbose. The error that ends the run, when there
		// is one, carries it in full.
		if c.recorded(app) {
			c.reporter().Warn(app+": Pocket ID not asked, recorded client used", why)
			continue
		}
		c.held[app] = why
		c.reporter().Warn(app+" held back: no client yet and Pocket ID not asked", why)
	}
}

// refuse holds an app back because its client was refused or could not be
// put in place. The rest of the site still applies.
func (c *clientStep) refuse(app string, err error) {
	c.refused[app] = err
	c.held[app] = err.Error()
	c.reporter().Refuse(app+" refused; the rest of the site is applied", err.Error())
}

// checkOneActive is how apply waits for Pocket ID. Tests replace it.
var checkOneActive = apply.CheckOneActive

// waitForPocketID waits until the deployment's Pocket ID has exactly one
// active instance, its /healthz answering on a site's mesh address, as the
// check after an apply does. The first pass on a site running Pocket ID has
// just started it, and a client probe made before it answers would hold
// every app back for nothing.
func (c *clientStep) waitForPocketID() error {
	transports := map[string]apply.Transport{}
	for _, name := range render.AppSites(c.cfg)[c.idp] {
		destination := ""
		if name == c.site {
			destination = c.destination
		}
		transports[name] = standbyLook(siteTransport(name, c.cfg.Sites[name], destination, false))
	}
	// What the check saw is the detail of waiting for Pocket ID. A failure
	// goes to the caller as an error, which is shown in full.
	var seen bytes.Buffer
	err := checkOneActive(c.cfg, c.idp, transports, &seen)
	if seen.Len() > 0 {
		c.reporter().Trace("pocket-id "+c.idp, seen.String())
	}
	return err
}

// result is the step's verdict once the rest of the site is applied. An app
// held back only because Pocket ID could not be asked does not fail the
// apply, because re-running finishes it. A refused one does, because its
// client needs a person to fix it.
func (c *clientStep) result() error {
	if c == nil {
		return nil
	}
	if len(c.held) > len(c.refused) || len(c.heldOverwrites) > 0 {
		r := c.reporter()
		var apps, why []string
		for _, app := range c.heldApps() {
			if _, refused := c.refused[app]; !refused {
				apps = append(apps, app)
				why = append(why, app+": "+c.held[app])
			}
		}
		if len(apps) > 0 {
			r.Warn("held back from this apply: "+strings.Join(apps, ", "), strings.Join(why, "\n"))
		}
		for _, path := range c.heldOverwrites {
			r.Warn("--overwrite not applied, its stack was held back: "+path, "Name it again on the apply that starts it.")
		}
	}
	if len(c.refused) == 0 {
		if c.groupsErr != nil {
			return fmt.Errorf("apply: the rest of %s was applied, but %v", c.site, c.groupsErr)
		}
		return nil
	}
	var names, reasons []string
	for _, app := range c.heldApps() {
		if err, ok := c.refused[app]; ok {
			names = append(names, app)
			reasons = append(reasons, app+": "+err.Error())
		}
	}
	if c.groupsErr != nil {
		reasons = append(reasons, c.groupsErr.Error())
	}
	return fmt.Errorf("apply: the rest of %s was applied, but %s was refused, and stays held back until its client at Pocket ID is fixed:\n  %s", c.site, strings.Join(names, ", "), strings.Join(reasons, "\n  "))
}

// planWithClients plans the site for apply, with its identity step when c is
// not nil. The whole site is planned first and, when the apply will execute,
// refused there if Execute would refuse it, so a file edited on the host
// stops the apply before Pocket ID is asked anything or any pass writes, as
// it stops a single pass apply before its first write. Then the clients are
// planned, read only, and the site is planned again without the apps the
// step holds back.
func planWithClients(c *clientStep, plan func(hold []string) (*apply.Plan, error), execute bool) (*apply.Plan, error) {
	full, err := plan(nil)
	if err != nil {
		return nil, err
	}
	if execute {
		if err := apply.Refusal(full); err != nil {
			return nil, err
		}
	}
	if c == nil {
		return full, nil
	}
	c.executing = execute
	if err := c.ensure(false, c.pocketIDHere()); err != nil {
		return nil, err
	}
	held := c.heldApps()
	if len(held) == 0 {
		return full, nil
	}
	return plan(held)
}

// sitePass is one plan and execute of the site. plan holds the named app
// stacks back with apply.Except.
type sitePass struct {
	// plan is told every earlier pass of this apply, for apply.After.
	plan    func(hold []string, done []*apply.Plan) (*apply.Plan, error)
	execute func(*apply.Plan) error
}

// executeWithClients is apply --execute on a site with an identity step.
//
// On a site that runs Pocket ID, a first pass applies everything but the
// other app stacks, Pocket ID is waited on until it answers, and only then
// are the clients ensured. Elsewhere they are ensured first. Either way the
// site is then planned again, so each app renders with the credentials just
// recorded, holding back what the step held back, and executed. It returns
// every pass's plan, in order.
func executeWithClients(c *clientStep, pass sitePass) ([]*apply.Plan, error) {
	var plans []*apply.Plan
	if c.pocketIDHere() {
		first, err := pass.plan(c.holdForPocketID(), nil)
		if err != nil {
			return nil, err
		}
		// A step around the pass, whose own steps report within it: it
		// ends once Pocket ID and what it runs on are up, with how long
		// that took.
		s := c.reporter().Step("start pocket-id")
		s.Detail("starting %s's Pocket ID, and what it runs on, before the apps that sign in through it", c.site)
		if err := pass.execute(first); err != nil {
			s.Fail(err)
			return nil, err
		}
		s.Done("")
		plans = append(plans, first)
		if err := c.waitForPocketID(); err != nil {
			c.reset()
			c.reporter().Detail("OIDC clients for %s, before they start", strings.Join(c.apps, ", "))
			c.cannotAsk(c.apps, unreachable(err), false)
		} else if err := c.ensure(true, false); err != nil {
			return plans, err
		}
	} else if err := c.ensure(true, false); err != nil {
		return plans, err
	}
	final, err := pass.plan(c.heldApps(), plans)
	if err != nil {
		return plans, err
	}
	c.heldOverwrites = final.HeldOverwrites
	if err := pass.execute(final); err != nil {
		return plans, err
	}
	return append(plans, final), nil
}

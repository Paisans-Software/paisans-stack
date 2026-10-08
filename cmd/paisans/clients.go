package main

import (
	"fmt"
	"os"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/oidcclient"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
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
	return &pocketid.Client{Transport: oidcTransport(siteTransport(cfg.Sites[site], destination, false)), BaseURL: pocketIDBase(cfg, site), APIKey: key}
}

// probeError is a client plan that failed because Pocket ID could not be
// asked. apply treats it as Pocket ID being unreachable, which holds back
// only the apps that have no client yet, rather than as a refusal.
type probeError struct{ err error }

func (e *probeError) Error() string { return e.err.Error() }
func (e *probeError) Unwrap() error { return e.err }

// planClient probes Pocket ID for one app's client and plans it. The probe
// only reads. A refusal from the plan has the recorded secret redacted.
func planClient(api *pocketid.Client, d oidcclient.Desired, rec oidcclient.Recorded) (*oidcclient.Plan, error) {
	state, err := oidcclient.Probe(api, d)
	if err != nil {
		return nil, &probeError{err}
	}
	plan, err := oidcclient.Build(d, rec, state)
	if err != nil {
		return nil, pocketid.Redact(err, rec.ClientSecret)
	}
	return plan, nil
}

// printClientPlan shows one app's client: what is present, and each
// mutation with what it sends. Warnings go to stderr.
func printClientPlan(app, idp, where string, plan *oidcclient.Plan) {
	fmt.Fprintf(os.Stdout, "%s's client at %s on %s (pocket-id)\n", app, idp, where)
	for _, line := range plan.Present {
		fmt.Fprintf(os.Stdout, "  %s\n", line)
	}
	for _, step := range plan.Steps {
		fmt.Fprintf(os.Stdout, "  %s\n", step.Line)
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(os.Stderr, "paisans: warning: %s\n", w)
	}
}

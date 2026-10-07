package render

import (
	"fmt"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// clusteredDatabaseHost is what a clustered application instance connects to:
// the HAProxy on its own site, reached at that site's mesh address. It is the
// local HAProxy, from the first install, with a backend list that initially
// has one entry. Adding a site grows that list and no application
// configuration changes, ever, because the address an instance is given is
// its own site's, and a site's mesh address does not change when another site
// joins.
//
// It is not 127.0.0.1, which is what this used to be. Every application runs
// in a bridge networked compose container, and inside one of those the
// loopback address is the container itself: a DSN naming 127.0.0.1 reaches
// nothing, and the app fails at its first query. Two other ways round that
// were rejected. Running the apps with host networking would put every
// sidecar on the host's interfaces and make two kinds that listen on the same
// port collide. Pointing at the Docker bridge gateway would need HAProxy to
// bind an address that differs per compose network and does not exist until
// Docker creates it. The mesh address exists from the moment wg0 is up, which
// is before anything else starts, and it is already declared in the
// configuration.
//
// site is the name of the site the instance runs on. The gateway rebuilds an
// app without one (plannedFor), because routing never needs a database, and
// gets an empty host.
func (p *planner) clusteredDatabaseHost(site string) string {
	return p.meshAddress(site)
}

// appDatabasePassword returns an app's own database credential. There is no
// fallback to a shared password, and that is the point.
//
// One cluster holds every app's database, so a credential shared between apps
// is a credential that reads every other app's data. Handing them the admin
// password is worse still: it can create and drop roles, and it is the
// password an operator uses. An app compromise should cost that app's data and
// stop there.
//
// A pinned app runs its own Postgres, so its credential is not shared with
// anything by construction, but it still gets its own rather than borrowing
// the cluster's.
func (p *planner) appDatabasePassword(planned plannedApp) (string, error) {
	if secret := p.appSecret(planned.Name, "database_password"); secret != "" {
		return secret, nil
	}
	return "", fmt.Errorf(
		"secrets apps.%s.database_password: required, and there is no fallback. Every app connects with its own role and its own password, so that one app's credential does not read another app's database. Generate one and record it",
		planned.Name)
}

// requiredAppSecrets lists, per kind, the app secrets without which the
// application will not start, and why. Most app secrets render as an empty
// value when unset, because the kinds differ in what they need and an absent
// optional one is not an error. These are not optional, and an empty one is a
// container that exits on a host after the apply has already moved, so render
// refuses first and names the command that fixes it.
var requiredAppSecrets = map[config.Kind][]struct{ Key, Why string }{
	config.KindUptime: {
		// src/config.js at 1.1.0-oidc.1: ADMIN_PASS falls back to "admin" and
		// SESSION_SECRET to a value invented per boot.
		{"admin_password", "the monitor falls back to the password `admin` when ADMIN_PASS is unset"},
		{"session_secret", "without it every restart signs every admin out"},
	},
	config.KindPocketID: {
		// env_config.go:167-169 at tag v2.14.0.
		{"encryption_key", "Pocket ID refuses to start without an ENCRYPTION_KEY of at least 16 bytes"},
		// Not needed to start, but without it the toolkit cannot create an
		// administrator or a client, and Pocket ID deletes the synthetic
		// user an earlier key created (apikey/service.go:31-36).
		{"static_api_key", "it is the only credential `paisans app admin create` and `paisans oidc client create` can reach Pocket ID's API with"},
	},
}

// requireAppSecrets refuses an app whose kind needs a secret that is unset.
func (p *planner) requireAppSecrets(planned plannedApp) error {
	for _, need := range requiredAppSecrets[planned.Kind] {
		if p.appSecret(planned.Name, need.Key) == "" {
			return fmt.Errorf("secrets apps.%s.%s: required, and there is no fallback: %s. Run `paisans init` to generate it", planned.Name, need.Key, need.Why)
		}
	}
	return nil
}

func (p *planner) appSecret(app, key string) string {
	entries, ok := p.secrets.Apps[app]
	if !ok {
		return ""
	}
	value, _ := entries[key].(string)
	return value
}

func (p *planner) clusterPort() int {
	if p.cfg.Cluster.Port == 0 {
		return 5000
	}
	return p.cfg.Cluster.Port
}

// garageEndpointHost is the address applications use to reach object storage:
// the first site in storage.garage.sites, over the mesh, on every site.
//
// The list is a preference order, and the first site is the one with the best
// upload (founder decision, docs/specs/2026-10-07-multisite-garage.md). A
// Garage node that receives a write sends the other copies itself, so an app
// on a home site writing to its own node would push every copy out over that
// home's upload, the slow direction of a residential line. Writing through
// the first site sends one copy out of the home. Rejected: each site's own
// node first, which keeps uploads working while the first site is down at the
// cost of that fan-out on every write.
//
// It used to be the first site in sorted order, which made the choice an
// accident of naming.
//
// A deployment with no Garage site gets an empty host rather than a loopback
// address. The fallback used to be 127.0.0.1, which from inside an app's
// bridge networked container is the container itself, so it named an
// endpoint that could never answer.
func garageEndpointHost(p *planner) string {
	if addrs := garageAddresses(p); len(addrs) > 0 {
		return addrs[0]
	}
	return ""
}

// garageAddresses is every Garage site's mesh address, in
// storage.garage.sites order. The gateway's media routes list them all in
// this order, so the first serves and the rest stand by.
func garageAddresses(p *planner) []string {
	var out []string
	for _, name := range p.cfg.Storage.Garage.Sites {
		if site, ok := p.sites[name]; ok {
			out = append(out, site.Address)
		}
	}
	return out
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// meshAddress is a site's address on the mesh, empty for a site the
// configuration does not declare. A published port binds it.
func (p *planner) meshAddress(site string) string {
	if s, ok := p.sites[site]; ok {
		return s.Address
	}
	return ""
}

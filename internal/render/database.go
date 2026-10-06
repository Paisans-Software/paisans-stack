package render

import (
	"fmt"
	"sort"
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

// garageEndpointHost is the address applications use to reach object storage.
// It is the first Garage site in sorted order, over the mesh.
//
// A deployment with no Garage site gets an empty host rather than a loopback
// address. The fallback used to be 127.0.0.1, which from inside an app's
// bridge networked container is the container itself, so it named an
// endpoint that could never answer.
func garageEndpointHost(p *planner) string {
	sites := append([]string(nil), p.cfg.Storage.Garage.Sites...)
	sort.Strings(sites)
	for _, name := range sites {
		if site, ok := p.sites[name]; ok {
			return site.Address
		}
	}
	return ""
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

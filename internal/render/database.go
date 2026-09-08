package render

import (
	"fmt"
	"sort"
)

// databaseHost is what every clustered application connects to. It is the
// local HAProxy, from the first install, with a backend list that initially
// has one entry. Adding a site grows that list and no application
// configuration changes, ever.
const databaseHost = "127.0.0.1"

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
func garageEndpointHost(p *planner) string {
	sites := append([]string(nil), p.cfg.Storage.Garage.Sites...)
	sort.Strings(sites)
	for _, name := range sites {
		if site, ok := p.sites[name]; ok {
			return site.Address
		}
	}
	return databaseHost
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

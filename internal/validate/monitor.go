package validate

import (
	"fmt"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// monitorRoles checks where the uptime monitor may run. README.md: "The
// monitor has a site of its own".
//
// A monitor exists to report the failures of everything else, so it must not
// share a failure with what it reports. On the gateway it would go dark with
// the edge it is meant to watch; on the witness, etcd's tiebreaker and the
// thing that reports etcd losing quorum would fail together, and the witness
// is usually the gateway machine anyway. Both are refused, with no override.
// Beside data or apps it is allowed with a warning: a small deployment may
// have no other machine, and the warning says which failures go unreported.
//
// The monitor serves its own hostname from its own public address, so that
// address must be declared. Founder decision.
func (c *checker) monitorRoles() {
	for _, name := range c.cfg.SiteNames() {
		site := c.cfg.Sites[name]
		if !site.Has(config.RoleMonitor) {
			continue
		}
		key := fmt.Sprintf("sites.%s.roles", name)
		if site.Has(config.RoleGateway) {
			c.refuse("monitor-on-gateway", key,
				"holds both monitor and gateway. The monitor's job is to report the gateway's failures, and on the same machine it goes dark with the gateway, at exactly the moment it is needed. Put the monitor on another site.")
		}
		if site.Has(config.RoleWitness) {
			c.refuse("monitor-on-witness", key,
				"holds both monitor and witness. etcd's tiebreaker and the thing that reports etcd losing quorum must not fail together, and the witness is usually the gateway machine as well. Put the monitor on another site.")
		}
		if site.Has(config.RoleData) || site.Has(config.RoleApps) {
			c.warn("monitor-shares-a-site", key,
				"holds monitor beside data or apps. A small deployment may have no other machine, but the monitor cannot report its own host failing, so losing %s takes the database or the apps there down with nothing left to say so. A separate small machine reports it.", name)
		}
		if !c.hostsUptime(name) {
			c.refuse("monitor-without-uptime", key,
				"holds the monitor role, but no app of kind uptime is pinned to %s, so the site would run nothing the role exists for. Pin the uptime app here, or drop the role.", name)
		}
		if site.PublicAddress == "" {
			c.refuse("monitor-without-public-address", fmt.Sprintf("sites.%s.public_address", name),
				"is not set, and %s holds the monitor role. The monitor serves its own hostname from its own address rather than through the gateway, so the address the internet reaches it on has to be declared: its hostname's DNS record points there. Declare it, for example public_address: 203.0.113.20.", name)
		}
	}
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindUptime || app.Placement.Mode != config.PlacementPinned {
			continue
		}
		site, ok := c.cfg.Sites[app.Placement.Site]
		if !ok || site.Has(config.RoleMonitor) {
			continue // undeclared-site reports a missing one
		}
		c.refuse("uptime-needs-a-monitor-site", fmt.Sprintf("apps.%s.placement", name),
			"pins the monitor to %s, which does not hold the monitor role. The monitor runs on a site of its own role, never the gateway or the witness, and serves its own hostname rather than through the gateway. Give a site roles: [monitor] and pin it there.", app.Placement.Site)
	}
}

// hostsUptime reports whether an uptime app is pinned to a site.
func (c *checker) hostsUptime(site string) bool {
	for _, name := range c.cfg.PinnedTo(site) {
		if c.cfg.Apps[name].Kind == config.KindUptime {
			return true
		}
	}
	return false
}

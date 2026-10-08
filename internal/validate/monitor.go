package validate

import (
	"fmt"
	"net"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
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

// ingress checks a site's ingress block. README.md: "The monitor has a site
// of its own".
//
// Only a monitor may carry one: a gateway always runs the toolkit's Caddy,
// and every other site is reached through the gateway, so the block would be
// ignored anywhere else. listen is where Docker publishes the app for the
// operator's own web server, so it belongs to mode external alone and that
// mode cannot work without it. Docker publishes a port in front of ufw, so a
// listen address the internet could reach would expose the app around the
// proxy whatever the firewall says: only loopback, a private LAN address
// (RFC 1918, or carrier grade NAT space as Tailscale uses) or the site's own
// mesh address are accepted, and all but loopback with a warning.
// One listen publishes one app, so an external monitor hosts the monitor and
// nothing else: a second app pinned there would be routed by nothing.
func (c *checker) ingress() {
	for _, name := range c.cfg.SiteNames() {
		site := c.cfg.Sites[name]
		if site.Ingress == nil {
			continue
		}
		key := fmt.Sprintf("sites.%s.ingress", name)
		if !site.Has(config.RoleMonitor) {
			c.refuse("ingress-outside-monitor", key,
				"is declared on %s, which does not hold the monitor role. A gateway always runs the toolkit's Caddy, and every other site is reached through the gateway, so the block would be ignored. Remove it, or give the site the monitor role.", name)
			continue
		}
		mode, listen := site.IngressMode(), site.Ingress.Listen
		switch {
		case mode == config.IngressPaisans && listen != "":
			c.refuse("ingress-listen-mode", key+".listen",
				"is set, but the ingress mode is paisans, where the toolkit's own Caddy serves the app and nothing is published for another web server. Remove listen, or set mode: external if your own web server is to proxy to it.")
			continue
		case mode == config.IngressExternal && listen == "":
			c.refuse("ingress-listen-mode", key+".listen",
				"is required with mode: external. It is where the app is published for your web server to proxy to, for example 127.0.0.1:8480.")
			continue
		case mode != config.IngressExternal:
			continue
		}
		if _, network, err := net.ParseCIDR(render.IngressNetwork); err == nil {
			if _, mesh, err := net.ParseCIDR(c.cfg.Mesh.Subnet); err == nil && (mesh.Contains(network.IP) || network.Contains(mesh.IP)) {
				c.refuse("ingress-network-overlaps-mesh", key,
					"puts the app on the compose network %s, which this toolkit pins so that the address your web server's connections arrive from is known, and mesh.subnet %s overlaps it. The mesh would be routed into a Docker bridge. Declare a mesh outside %s.", render.IngressNetwork, c.cfg.Mesh.Subnet, render.IngressNetwork)
			}
		}
		if pinned := c.cfg.PinnedTo(name); len(pinned) > 1 {
			c.refuse("ingress-external-serves-one-app", key+".listen",
				"publishes one app, but %s are pinned to %s. With mode: external nothing but your own web server is in front of the site, and it is handed the monitor alone, so the others would be routed by nothing. Pin them elsewhere, or use mode: paisans.", strings.Join(pinned, ", "), name)
		}
		host, _, ok := site.Ingress.ListenHostPort()
		if !ok {
			continue // the loader has already reported this
		}
		ip := net.ParseIP(host)
		switch {
		case ip.IsLoopback():
		case host == site.Address || (!c.cfg.Mesh.Contains(host) && (ip.IsPrivate() || cgnat.Contains(ip))):
			c.warn("ingress-listen-bypasses-firewall", key+".listen",
				"is %s, which is not loopback. Docker publishes a port with its own iptables rules, in front of ufw, so the firewall does not protect it: anything that can reach %s can reach the app around your web server. Prefer 127.0.0.1 when the web server runs on this machine.", listen, host)
		default:
			c.refuse("ingress-listen-public", key+".listen",
				"is %s. The app would be published where the proxy is not in front of it, and Docker's rules bypass ufw. Use loopback (127.0.0.1), a private LAN address (RFC 1918, or 100.64.0.0/10 as Tailscale uses), or this site's own mesh address, %s.", listen, site.Address)
		}
	}
}

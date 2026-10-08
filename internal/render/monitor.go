package render

import (
	"fmt"
	"net"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// ServedBy is the monitor site that serves an app, when the app is pinned to
// a site holding the monitor role. Such an app is left out of the gateway's
// routing and served from the monitor itself, by the toolkit's Caddy there or
// by the operator's own web server: a monitor reached only through the
// gateway goes dark at exactly the moment it is needed. Every other app is
// the gateway's, and ServedBy returns false for it.
func ServedBy(cfg *config.Config, app string) (string, bool) {
	a, ok := cfg.Apps[app]
	if !ok || a.Placement.Mode != config.PlacementPinned {
		return "", false
	}
	site, ok := cfg.Sites[a.Placement.Site]
	if !ok || !site.Has(config.RoleMonitor) {
		return "", false
	}
	return a.Placement.Site, true
}

// ExternalListen is where an app is published for the operator's own web
// server: the listen address of the monitor in ingress mode external that
// the app is pinned to. False for every other app.
func ExternalListen(cfg *config.Config, app string) (string, int, bool) {
	site, ok := ServedBy(cfg, app)
	if !ok {
		return "", 0, false
	}
	s := cfg.Sites[site]
	if s.IngressMode() != config.IngressExternal || s.Ingress == nil {
		return "", 0, false
	}
	return s.Ingress.ListenHostPort()
}

// privateBlocks are the RFC 1918 ranges, for naming the network a LAN listen
// address sits in.
var privateBlocks = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

// trustProxy is what an app trusts to tell it the client's address, in
// Express's `trust proxy` syntax, which the uptime fork reads from
// TRUST_PROXY (src/server.js at 1.1.0-oidc.3): a comma separated list of
// addresses, networks and the names loopback, linklocal and uniquelocal. It
// is where the proxy in front of the app connects from, because the fork's
// login rate limiter is keyed on the client address, and with the proxy
// untrusted every visitor is one client and one attacker locks every admin
// out.
//
// The gateway, and a monitor's own Caddy, reach the app on its site's mesh
// address, so the mesh subnet. A web server reaching a loopback listen goes
// through Docker's publish, which hands the connection to the container from
// the compose network's gateway (docker-proxy, or with the userland proxy off
// a masqueraded hairpin): a private address, never 127.0.0.1, so loopback is
// joined by uniquelocal. A LAN listen is reached from the LAN, so the private
// block holding it; the site's own mesh address, from the mesh.
func trustProxy(cfg *config.Config, app string) string {
	host, _, ok := ExternalListen(cfg, app)
	if !ok {
		return cfg.Mesh.Subnet
	}
	ip := net.ParseIP(host)
	switch {
	case ip.IsLoopback():
		return "loopback,uniquelocal"
	case cfg.Mesh.Contains(host):
		return cfg.Mesh.Subnet
	}
	for _, block := range privateBlocks {
		if _, network, err := net.ParseCIDR(block); err == nil && network.Contains(ip) {
			return block
		}
	}
	// validate refuses any other listen (ingress-listen-public), so this is
	// only reached by a caller that rendered without validating.
	return cfg.Mesh.Subnet
}

// ingressListen is the extra publish an app gets on an external monitor, as
// compose's host:port, empty for every other app.
func ingressListen(cfg *config.Config, app string) string {
	host, port, ok := ExternalListen(cfg, app)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s:%d", host, port)
}

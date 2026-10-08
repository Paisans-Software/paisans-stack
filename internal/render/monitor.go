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

// privateBlocks are the RFC 1918 ranges and carrier grade NAT space, for
// naming the network a LAN listen address sits in.
var privateBlocks = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"}

// IngressNetwork is the compose network an app on a monitor in ingress mode
// external runs on, pinned so that the address the operator's web server
// reaches it from is known, and IngressGateway that network's gateway, which
// is that address for a loopback listen. Docker's default address pools are
// 172.17.0.0/16 to 172.31.0.0/16 and 192.168.0.0/16, so a network it creates
// for anything else never lands here; validate refuses a mesh over it
// (ingress-network-overlaps-mesh), and the host check refuses a host where a
// foreign network or route already holds it.
const (
	IngressNetwork = "10.255.255.0/29"
	IngressGateway = "10.255.255.1"
)

// Network is a subnet a site's rendered stacks create, with the paisans.yaml
// key behind it, as Listener is for a port.
type Network struct {
	Owner  string
	Key    string
	Subnet string
}

// SiteNetworks is every subnet the rendered stacks on one site pin. Only an
// app on a monitor in ingress mode external pins one; every other compose
// network takes what Docker's pools give it.
func SiteNetworks(cfg *config.Config, site string) []Network {
	var out []Network
	for _, name := range cfg.PinnedTo(site) {
		if _, _, ok := ExternalListen(cfg, name); ok {
			out = append(out, Network{
				Owner:  fmt.Sprintf("app %s (%s)", name, cfg.Apps[name].Kind),
				Key:    fmt.Sprintf("sites.%s.ingress", site),
				Subnet: IngressNetwork,
			})
		}
	}
	return out
}

// trustProxy is what an app on a monitor site trusts to tell it the
// client's address, in Express's `trust proxy` syntax, which the uptime fork
// reads from TRUST_PROXY (src/server.js at 1.1.0-oidc.3): a comma separated
// list of addresses and networks. It is exactly where the proxy in front of
// the app connects from, because the fork's login rate limiter is keyed on
// the client address: with the proxy untrusted every visitor is one client,
// and with anything wider trusted, whatever else can reach the app can
// claim any client address.
//
// The monitor's own Caddy runs on the host and dials the app on the site's
// mesh address, so the connection comes from that address. A web server
// reaching a loopback listen goes through Docker's publish, which hands the
// connection to the container from the compose network's gateway
// (docker-proxy, or with the userland proxy off a masqueraded hairpin), and
// that network is pinned (IngressNetwork). A LAN or mesh listen is reached
// by the web server's own address, which Docker's forwarding keeps; the
// toolkit knows only the network it lies in.
func trustProxy(cfg *config.Config, app string) string {
	if site, ok := ServedBy(cfg, app); ok && cfg.Sites[site].IngressMode() == config.IngressPaisans {
		return cfg.Sites[site].Address + "/32"
	}
	host, _, ok := ExternalListen(cfg, app)
	if !ok {
		return cfg.Mesh.Subnet
	}
	ip := net.ParseIP(host)
	switch {
	case ip.IsLoopback():
		return IngressGateway + "/32"
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

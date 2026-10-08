package hostcheck

import (
	"fmt"
	"net"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// Claims is what the toolkit will take on one site's host.
type Claims struct {
	Site string
	// Listeners is render.SiteListeners, the one list of what the rendered
	// stacks bind, each with the paisans.yaml key behind it. A listener
	// added there is claimed here without a change to this package.
	Listeners []render.Listener
	// Interface is this deployment's mesh interface, psns-<token>, and Mesh
	// the subnet routed through it. ListenPort is the WireGuard port the
	// site's endpoint declares, 0 when it has none.
	Interface  string
	ListenPort int
	Mesh       *net.IPNet
	// Deployment is whose host this is checked for: a Docker object is the
	// toolkit's only when it carries this deployment's label, so another
	// deployment's containers on the same host are as foreign as anyone's.
	Deployment deployment.Deployment
	// SSHPort is the site's ssh.port, which any advice about enabling a
	// firewall has to allow first.
	SSHPort int
	// Networks are the subnets the site's stacks pin, each with the key
	// behind it (render.SiteNetworks): a foreign Docker network or route
	// over one is a conflict, as one over the mesh is.
	Networks []ClaimedNetwork
}

// ClaimedNetwork is one pinned subnet a site claims.
type ClaimedNetwork struct {
	Net *net.IPNet
	Key string
	// Project is the compose project the network belongs to. Any other
	// project holding the subnet, the toolkit's own included, is a
	// conflict.
	Project string
}

// ClaimsFor is what the toolkit will take on one site's host, from the
// configuration alone: no host is reached.
func ClaimsFor(cfg *config.Config, site string) (Claims, error) {
	if _, ok := cfg.Sites[site]; !ok {
		return Claims{}, fmt.Errorf("host check: no site %q is declared. Declared sites are %s", site, strings.Join(cfg.SiteNames(), ", "))
	}
	_, mesh, err := net.ParseCIDR(cfg.Mesh.Subnet)
	if err != nil {
		return Claims{}, fmt.Errorf("host check: mesh.subnet %q is not a network", cfg.Mesh.Subnet)
	}
	claims := Claims{
		Site:       site,
		Deployment: cfg.Deployment(),
		Listeners:  render.SiteListeners(cfg, site),
		Interface:  cfg.Deployment().Interface(),
		ListenPort: cfg.Sites[site].ListenPort(),
		Mesh:       mesh,
		SSHPort:    cfg.Sites[site].SSH.PortOrDefault(),
	}
	for _, n := range render.SiteNetworks(cfg, site) {
		_, subnet, err := net.ParseCIDR(n.Subnet)
		if err != nil {
			return Claims{}, fmt.Errorf("host check: %s pins %q, which is not a network", n.Key, n.Subnet)
		}
		claims.Networks = append(claims.Networks, ClaimedNetwork{Net: subnet, Key: n.Key, Project: n.Project})
	}
	return claims, nil
}

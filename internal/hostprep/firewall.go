package hostprep

import (
	"fmt"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// Rule is one inbound allowance. Either Port and Proto are set, or Interface
// is, never both: "everything arriving on wg0" is a rule about where traffic
// comes from, not which port it is for.
type Rule struct {
	Port      int
	Proto     string
	Interface string
	// Why is printed beside the rule in the plan.
	Why string
}

func (r Rule) String() string {
	if r.Interface != "" {
		return "all inbound on " + r.Interface
	}
	return fmt.Sprintf("%d/%s", r.Port, r.Proto)
}

// meshInterface is the interface wg0.conf creates. Every host network service
// (etcd, Patroni, Garage, HAProxy) listens on the site's mesh address, so the
// mesh is let in whole rather than port by port: a new mesh service would
// otherwise be one more rule to remember on every site.
const meshInterface = "wg0"

// wireguardPort matches ListenPort in the rendered wg0.conf.
const wireguardPort = 51820

// Rules derives a site's inbound rules from its roles. Nothing is listed per
// site: a site that gains the gateway role gains 80 and 443 on its next
// prepare, and one that loses it keeps them until removed by hand, since this
// package plans only what is missing and never deletes a rule.
func Rules(site config.Site) []Rule {
	rules := []Rule{
		// First, and on every site: the firewall is enabled after it, and an
		// enabled firewall without it would lock the operator out of the
		// connection running this command.
		{Port: 22, Proto: "tcp", Why: "ssh, the bootstrap route"},
		{Port: wireguardPort, Proto: "udp", Why: "WireGuard, the mesh"},
		{Interface: meshInterface, Why: "the mesh: etcd, Patroni, Garage, HAProxy"},
	}
	if site.Has(config.RoleGateway) {
		rules = append(rules,
			Rule{Port: 80, Proto: "tcp", Why: "the gateway, HTTP"},
			Rule{Port: 443, Proto: "tcp", Why: "the gateway, HTTPS"},
		)
	}
	return rules
}

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
	// To narrows an interface rule to one destination address and Port, for
	// traffic that should reach one service and nothing else.
	To string
	// Why is printed beside the rule in the plan.
	Why string
}

func (r Rule) String() string {
	if r.Interface != "" && r.To != "" {
		return fmt.Sprintf("%d/%s on %s to %s", r.Port, r.Proto, r.Interface, r.To)
	}
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

// containerBridges matches every Docker bridge a compose project creates
// (br-<id>), in ufw's interface wildcard syntax.
const containerBridges = "br-+"

// garageS3Port is Garage's S3 API, as rendered in garage.toml.
const garageS3Port = 3900

// ContainerRules lets an app's containers reach the host network services they
// connect to on this site's mesh address. A container's packets to a host
// address arrive on its compose project's bridge, not on wg0, so the default
// deny drops them: on the first real host Mbin waited forever for its database
// with "[UFW BLOCK] IN=br-... DST=<mesh> DPT=5000" in the kernel log.
//
// Matched by interface rather than by Docker's address pools, which change
// with every network created; and narrowed to the mesh address and the two
// ports apps use, so a container gains its database proxy and object storage
// and not etcd, Patroni's API or Garage's admin port. Rejected: pinning
// Docker's address pool in daemon.json and allowing that subnet, which needs a
// daemon restart and does not cover networks that already exist.
func ContainerRules(cfg *config.Config, name string) []Rule {
	site, ok := cfg.Sites[name]
	if !ok || !site.Has(config.RoleApps) {
		return nil
	}
	port := cfg.Cluster.Port
	if port == 0 {
		port = 5000
	}
	rules := []Rule{{Interface: containerBridges, To: site.Address, Port: port, Proto: "tcp", Why: "apps' containers to the local database proxy"}}
	for _, garage := range cfg.Storage.Garage.Sites {
		if garage == name {
			rules = append(rules, Rule{Interface: containerBridges, To: site.Address, Port: garageS3Port, Proto: "tcp", Why: "apps' containers to object storage"})
		}
	}
	return rules
}

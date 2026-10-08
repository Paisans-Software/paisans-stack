package hostprep

import (
	"fmt"
	"sort"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// Rule is one inbound allowance. Either Port and Proto are set, or Interface
// is, never both: "everything arriving on psns-f2a9" is a rule about where traffic
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
	// SSH marks the allow for the site's ssh.port. It is the one rule never
	// removed, and a deny on it is refused rather than warned about; see
	// planRules.
	SSH bool
}

// sshWhy is the SSH rule's reason, and so part of its ownership comment on the
// host. It must not change: it is how planRules recognises an SSH allow host
// prepare added for an earlier ssh.port, and keeps it.
const sshWhy = "ssh, the bootstrap route"

func (r Rule) String() string {
	if r.Interface != "" && r.To != "" {
		return fmt.Sprintf("%d/%s on %s to %s", r.Port, r.Proto, r.Interface, r.To)
	}
	if r.Interface != "" {
		return "all inbound on " + r.Interface
	}
	return fmt.Sprintf("%d/%s", r.Port, r.Proto)
}

// Rules derives a site's inbound rules from its roles and its endpoint. Nothing is listed per
// site: a site that gains the gateway role gains 80 and 443 on its next
// prepare, and one that loses it has them removed by the same prepare, since
// the profile removes a rule it added once nothing derives it any more. SSH is
// the exception and is never removed; see the profile's Firewall. Its port is
// the site's ssh.port, 22 unless declared.
//
// Why is written into the host's firewall as part of the rule's ownership
// comment, so it must not contain a single quote: ufw refuses one in a comment.
//
// The mesh interface is d's own, psns-<token>, and every host network service
// (etcd, Patroni, Garage, HAProxy) listens on the site's mesh address, so the
// mesh is let in whole rather than port by port: a new mesh service would
// otherwise be one more rule to remember on every site. WireGuard's own port
// is the site's endpoint port, the ListenPort its file is rendered with; a
// site with no endpoint is never dialled and opens none, since the replies
// to the packets it sends out are let in as part of that exchange.
func Rules(d deployment.Deployment, site config.Site) []Rule {
	rules := []Rule{
		// First, and on every site: the firewall is enabled after it, and an
		// enabled firewall without it would lock the operator out of the
		// connection running this command.
		{Port: site.SSH.PortOrDefault(), Proto: "tcp", Why: sshWhy, SSH: true},
	}
	if port := site.ListenPort(); port != 0 {
		rules = append(rules, Rule{Port: port, Proto: "udp", Why: "WireGuard, the mesh"})
	}
	rules = append(rules, Rule{Interface: d.Interface(), Why: "the mesh: etcd, Patroni, Garage, HAProxy"})
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
// address arrive on its compose project's bridge, not on the mesh interface, so the default
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
	if !ok {
		return nil
	}
	var rules []Rule
	if site.Has(config.RoleApps) {
		port := cfg.Cluster.Port
		if port == 0 {
			port = 5000
		}
		rules = append(rules, Rule{Interface: containerBridges, To: site.Address, Port: port, Proto: "tcp", Why: "app containers to the local database proxy"})
		for _, garage := range cfg.Storage.Garage.Sites {
			if garage == name {
				rules = append(rules, Rule{Interface: containerBridges, To: site.Address, Port: garageS3Port, Proto: "tcp", Why: "app containers to object storage"})
			}
		}
	}
	return append(rules, monitorRules(cfg, name, site)...)
}

// monitorRules lets an uptime monitor's direct checks reach the apps that run
// on its own site. To another site they leave over the mesh, which is let in whole;
// to its own site they arrive on a compose bridge, where the default deny drops
// them, and on a single site that would fail every direct check.
func monitorRules(cfg *config.Config, name string, site config.Site) []Rule {
	hosts := false
	for _, app := range cfg.PinnedTo(name) {
		if cfg.Apps[app].Kind == config.KindUptime {
			hosts = true
		}
	}
	if !hosts {
		return nil
	}
	ports := map[int]bool{}
	for app, sites := range render.AppSites(cfg) {
		kind := cfg.Apps[app].Kind
		if kind == config.KindUptime {
			continue // a monitor does not check itself
		}
		for _, s := range sites {
			if s == name {
				ports[render.AppPort(kind)] = true
			}
		}
	}
	sorted := make([]int, 0, len(ports))
	for port := range ports {
		sorted = append(sorted, port)
	}
	sort.Ints(sorted)
	var rules []Rule
	for _, port := range sorted {
		rules = append(rules, Rule{Interface: containerBridges, To: site.Address, Port: port, Proto: "tcp", Why: "the uptime monitor to apps on this site"})
	}
	return rules
}

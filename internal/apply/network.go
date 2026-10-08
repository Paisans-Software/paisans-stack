package apply

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/render"
	"gopkg.in/yaml.v3"
)

// A stack's compose default network is created once, by the first `up`, with
// the address pool its compose file declared then. When that declaration
// changes on an existing deployment, the network on the host has to change
// with it, and apply does not leave that to `up`: Compose (2.31 and later)
// recreates a network whose recorded config hash differs from the file's, but
// it leaves alone a network carrying no recorded hash, which is one created by
// an older Compose or by hand. The case that matters is ingress mode
// external, where the uptime app's TRUST_PROXY names the pinned network's
// gateway (render.IngressGateway). A network left on its old pool makes the
// web server's connections arrive from an address TRUST_PROXY does not name,
// and the login rate limiter then counts every client as the same one.
//
// So Build asks the host how each recreated stack's default network is
// addressed, and where that differs from the compose file, the stack is taken
// down before its `up`. `down` removes the network along with the
// containers, and `up` then creates it as declared. The question is asked of
// the network itself rather than of Compose's labels, so the answer holds
// whichever Compose created it.

// pool is one address pool of a network, as `docker network inspect` prints
// IPAM.Config and as a compose file declares ipam.config.
type pool struct {
	Subnet  string `json:"Subnet" yaml:"subnet"`
	Gateway string `json:"Gateway" yaml:"gateway"`
}

// composePools reads the pools a rendered compose file pins its default
// network to, empty when it pins none. The file is ours, rendered a moment
// ago, so a parse failure is a bug rather than a host problem.
func composePools(content string) ([]pool, error) {
	var doc struct {
		Networks map[string]struct {
			IPAM struct {
				Config []pool `yaml:"config"`
			} `yaml:"ipam"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, err
	}
	return doc.Networks["default"].IPAM.Config, nil
}

// networkProbe asks the host for each network's address pools in one round
// trip. Like imageProbe it prints a line for every network rather than exiting
// non zero, so that "absent" cannot be confused with ssh failing.
func networkProbe(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = shellQuote(name)
	}
	return fmt.Sprintf(`for n in %s; do if c=$(docker network inspect --format '{{json .IPAM.Config}}' "$n" 2>/dev/null); then echo "present $n $c"; else echo "absent $n"; fi; done`,
		strings.Join(quoted, " "))
}

// probeNetworks returns each named network's pools, and whether it exists.
// Every name must be answered for, as in probeImages.
func probeNetworks(names []string, t Transport) (map[string][]pool, map[string]bool, error) {
	pools, present := map[string][]pool{}, map[string]bool{}
	if len(names) == 0 {
		return pools, present, nil
	}
	text, err := t.Run(networkProbe(names))
	if err != nil {
		return nil, nil, fmt.Errorf("asking how the host's compose networks are addressed: %w", err)
	}
	answered := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), " ", 3)
		switch {
		case len(fields) == 3 && fields[0] == "present":
			var config []pool
			if err := json.Unmarshal([]byte(fields[2]), &config); err != nil {
				return nil, nil, fmt.Errorf("asking how network %s is addressed: unreadable answer %q", fields[1], fields[2])
			}
			pools[fields[1]], present[fields[1]], answered[fields[1]] = config, true, true
		case len(fields) == 2 && fields[0] == "absent":
			answered[fields[1]] = true
		}
	}
	for _, name := range names {
		if !answered[name] {
			return nil, nil, fmt.Errorf("asking how the host's compose networks are addressed: no answer for %s in:\n%s", name, text)
		}
	}
	return pools, present, nil
}

// staleNetwork reports why a network on the host does not match the pools its
// compose file declares, empty when it does. A declared pin must be what the
// network has, gateway included. A file that pins nothing leaves the pool to
// Docker, which never allocates render.IngressNetwork, since that subnet is
// outside its default pools; a network still on it was pinned by an earlier
// render and keeps that render's gateway.
func staleNetwork(declared, live []pool) string {
	if len(declared) == 0 {
		for _, p := range live {
			if p.Subnet == render.IngressNetwork {
				return fmt.Sprintf("its compose network is still pinned to %s, which the compose file no longer declares", p.Subnet)
			}
		}
		return ""
	}
	key := func(ps []pool) string {
		var parts []string
		for _, p := range ps {
			parts = append(parts, p.Subnet+" "+p.Gateway)
		}
		sort.Strings(parts)
		return strings.Join(parts, ", ")
	}
	if key(declared) == key(live) {
		return ""
	}
	var want []string
	for _, p := range declared {
		want = append(want, p.Subnet)
	}
	return fmt.Sprintf("its compose network is not on %s, which the compose file pins", strings.Join(want, ", "))
}

// probeStaleNetworks marks each recreated stack whose default network on the
// host differs from its compose file with Down. Only a recreate is asked
// about: a change to the compose file is always one, and a restart starts the
// containers it has on the network they are already on. Building the plan
// only reads the host, so a dry run shows the down without taking it.
func (p *Plan) probeStaleNetworks(t Transport) error {
	declared := map[string][]pool{}
	for _, c := range p.renderedChanges {
		if c.Stack == "" || !isStackCompose(c) {
			continue
		}
		pools, err := composePools(c.content)
		if err != nil {
			return fmt.Errorf("reading the network %s declares: %w", c.Path, err)
		}
		declared[c.Stack] = pools
	}
	var names []string
	for _, a := range p.Actions {
		if a.Recreate {
			names = append(names, p.Deployment.Project(a.Stack)+"_default")
		}
	}
	live, present, err := probeNetworks(names, t)
	if err != nil {
		return err
	}
	for i, a := range p.Actions {
		name := p.Deployment.Project(a.Stack) + "_default"
		if !a.Recreate || !present[name] {
			continue
		}
		if why := staleNetwork(declared[a.Stack], live[name]); why != "" {
			p.Actions[i].Down = true
			p.Actions[i].Reason += "; " + why + ", so the stack is taken down first, which removes the network, and `up` creates it as declared"
		}
	}
	return nil
}

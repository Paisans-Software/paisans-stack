package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/mesh"
	"github.com/paisans-software/paisans-stack/internal/registry"
)

// initHost is how `paisans init` reaches a site to read its registry and its
// networks. A test replaces it.
var initHost = func(site config.Site, sudo bool) registry.Runner {
	return siteTransport(site, "", sudo)
}

// meshRandom is where init's subnet rolls come from. A test replaces it.
var meshRandom io.Reader = rand.Reader

// checkMeshLive refuses when anything on the host h reaches, outside this
// deployment's own interface, holds a network overlapping the mesh subnet:
// a route, an address, a Docker network or one of Docker's address pools.
// apply and host prepare run it before they claim the host, dry run or not,
// so a clash stops them with nothing changed. See internal/mesh.
func checkMeshLive(cfg *config.Config, site string, h mesh.Host) error {
	subnet, err := mesh.ParsePrefix(cfg.Mesh.Subnet)
	if err != nil {
		return fmt.Errorf("mesh.subnet %q is not a network: %w", cfg.Mesh.Subnet, err)
	}
	probed, err := mesh.Probe(h)
	if err != nil {
		return err
	}
	taken, err := probed.Taken(cfg.Deployment().Interface(), site)
	if err != nil {
		return fmt.Errorf("%s: %w", h.Describe(), err)
	}
	clash := mesh.Clashes(subnet, taken)
	if len(clash) == 0 {
		return nil
	}
	what := make([]string, len(clash))
	for i, c := range clash {
		what[i] = c.What
	}
	return fmt.Errorf("%s: the mesh subnet %s overlaps %s, so nothing was changed. Whichever route the kernel prefers would take the other network's traffic. A deployed mesh subnet never changes, so the other network has to move; if this deployment has never been applied anywhere, `paisans init` picks a subnet clear of every host", h.Describe(), cfg.Mesh.Subnet, strings.Join(what, ", "))
}

// settleMesh is init's subnet decision. It reads every site's registry
// first. Once any of them records this deployment, the subnet is deployed
// and settleMesh never changes it. Otherwise every site must answer, since an
// unchecked host is exactly where a collision would go unseen; it gathers
// every other deployment's mesh from each registry and everything each host
// holds (see checkMeshLive), keeps the declared subnet if it overlaps none of
// it, and otherwise rolls a random /24 that overlaps none, moving each
// site's address into it with its host number kept. It writes paisans.yaml
// only when it rolled, and says why. It reports whether it wrote.
func settleMesh(cfg *config.Config, path string, hosts map[string]registry.Runner, random io.Reader, w io.Writer) (bool, error) {
	sites := cfg.SiteNames()
	registries := map[string]registry.Registry{}
	var unreachable []string
	var claimedOn []string
	for _, name := range sites {
		r, err := registry.Read(hosts[name])
		if err != nil {
			unreachable = append(unreachable, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		registries[name] = r
		if _, ok := r.Deployments[cfg.ID]; ok {
			claimedOn = append(claimedOn, name)
		}
	}

	if len(claimedOn) > 0 {
		recorded := registries[claimedOn[0]].Deployments[cfg.ID].Subnet
		if cfg.Mesh.Subnet == "" {
			hint := ""
			if recorded != "" {
				hint = fmt.Sprintf(" %s records it as %s.", claimedOn[0], recorded)
			}
			return false, fmt.Errorf("%s: mesh.subnet is missing, and this deployment is already on %s, so its subnet is deployed and is not chosen again.%s Restore it in %s", path, strings.Join(claimedOn, ", "), hint, path)
		}
		fmt.Fprintf(w, "%s: mesh.subnet %s is deployed (this deployment is on %s), so it is kept as it is.\n", path, cfg.Mesh.Subnet, strings.Join(claimedOn, ", "))
		return false, nil
	}
	if len(unreachable) > 0 {
		return false, fmt.Errorf("init reaches every site before it settles mesh.subnet, and could not read:\n  %s\nNothing was written. Every host has to be checked, since a subnet chosen without one is a subnet that may collide on it", strings.Join(unreachable, "\n  "))
	}

	iface := cfg.Deployment().Interface()
	var taken []mesh.Taken
	for _, name := range sites {
		taken = append(taken, registry.Meshes(registries[name], cfg.ID, name)...)
		probed, err := mesh.Probe(hosts[name])
		if err != nil {
			return false, fmt.Errorf("%v. Nothing was written", err)
		}
		live, err := probed.Taken(iface, name)
		if err != nil {
			return false, fmt.Errorf("%s: %v. Nothing was written", name, err)
		}
		taken = append(taken, live...)
	}

	choice, err := mesh.Choose(cfg.Mesh.Subnet, taken, random)
	if err != nil {
		return false, fmt.Errorf("%s: %w. Nothing was written", path, err)
	}
	subnet := choice.Subnet.String()
	addresses := map[string]string{}
	if choice.Rolled {
		current := map[string]string{}
		for _, name := range sites {
			current[name] = cfg.Sites[name].Address
		}
		if addresses, err = mesh.Readdress(cfg.Mesh.Subnet, choice.Subnet, current); err != nil {
			return false, fmt.Errorf("%s: rolled %s, but %v. Nothing was written", path, subnet, err)
		}
	}

	// The registries are already in hand, so the rest of a claim's decision
	// is made here too: a clash on the interface or listen port would refuse
	// the first apply, and is better found before anything is written.
	next := *cfg
	next.Mesh.Subnet = subnet
	next.Sites = map[string]config.Site{}
	for name, s := range cfg.Sites {
		if a, ok := addresses[name]; ok {
			s.Address = a
		}
		next.Sites[name] = s
	}
	for _, name := range sites {
		id, e := registry.For(&next, name, time.Time{})
		if c := registry.Conflicts(registries[name], id, e); len(c) > 0 {
			return false, fmt.Errorf("%s: %v, and nothing was written. %s", name, c[0], conflictAdvice(c[0]))
		}
	}

	if !choice.Rolled {
		fmt.Fprintf(w, "%s: mesh.subnet %s overlaps nothing on %s; kept.\n", path, subnet, strings.Join(sites, ", "))
		return false, nil
	}
	if err := config.SetMesh(path, subnet, addresses); err != nil {
		return false, err
	}
	fmt.Fprintf(w, "%s: rolled mesh.subnet %s (%s).\n", path, subnet, strings.Join(choice.Why, "; "))
	names := make([]string, 0, len(addresses))
	for name := range addresses {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  sites.%s.address %s -> %s\n", name, cfg.Sites[name].Address, addresses[name])
	}
	fmt.Fprintf(w, "It is fixed from the first apply on: every app trusts it for forwarded client addresses and every service binds an address in it.\n")
	return true, nil
}

// conflictAdvice is what an operator does about a registry conflict init
// found before anything was deployed.
func conflictAdvice(c registry.Conflict) string {
	for _, clash := range c.Clashes {
		if strings.HasPrefix(clash, "WireGuard listen port") {
			return "Give that site's endpoint another port in paisans.yaml, and forward it where the host is behind NAT"
		}
	}
	return "Use another host for that site"
}

// initHosts reaches every declared site for init.
func initHosts(cfg *config.Config, sudo bool) map[string]registry.Runner {
	hosts := map[string]registry.Runner{}
	for _, name := range cfg.SiteNames() {
		hosts[name] = initHost(cfg.Sites[name], sudo)
	}
	return hosts
}

// initOut is where init's subnet report goes. A test replaces it.
var initOut io.Writer = os.Stdout

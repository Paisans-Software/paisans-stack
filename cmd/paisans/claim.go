package main

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
)

// claimHosts is what every command that writes to a host does before
// anything else there: with execute, it claims each named site's host for
// this deployment in the host registry, and a refusal stops the command with
// nothing on any host changed. Without execute it only reads each registry
// and refuses the same way, so a dry run shows a conflict the real run would
// meet. Sites are claimed in name order, and a claim already made stays: it
// is a record that this deployment is on that host, which it is about to be.
// See internal/registry.
func claimHosts(cfg *config.Config, execute bool, transports map[string]registry.Runner) error {
	sites := make([]string, 0, len(transports))
	for name := range transports {
		sites = append(sites, name)
	}
	sort.Strings(sites)
	now := time.Now()
	for _, site := range sites {
		t := transports[site]
		if !execute {
			if err := registry.Check(t, cfg, site); err != nil {
				return err
			}
			continue
		}
		if err := registry.Claim(t, cfg, site, now); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "paisans: %s: claimed for deployment %s in %s\n", site, cfg.ID, registry.Path)
	}
	return nil
}

// registryHost is how a claim reaches a site's host when the command has no
// transport of its own to lend it. A test replaces it.
var registryHost = func(site config.Site, destination string, sudo bool) registry.Runner {
	return siteTransport(site, destination, sudo)
}

// claimSites is claimHosts over the named sites, each reached through its
// own ssh section, with sudo unless the operator turned it off, since the
// registry is root's.
func claimSites(cfg *config.Config, execute, sudo bool, sites ...string) error {
	transports := map[string]registry.Runner{}
	for _, name := range sites {
		transports[name] = registryHost(cfg.Sites[name], "", sudo)
	}
	return claimHosts(cfg, execute, transports)
}

// union is the sorted set of every name in lists.
func union(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, name := range list {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

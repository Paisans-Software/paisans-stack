package main

import (
	"sort"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// claimHosts is what every command that writes to a host does before
// anything else there: with execute, it claims each named site's host for
// this deployment in the host registry, and a refusal stops the command with
// nothing on any host changed. Without execute it only reads each registry
// and refuses the same way, so a dry run shows a conflict the real run would
// meet. The read is a step all the same, so a spinner shows while ssh
// answers. Sites are
// claimed in name order, and a claim already made stays: it is a record that
// this deployment is on that host, which it is about to be. See
// internal/registry.
func claimHosts(r ui.Reporter, cfg *config.Config, execute bool, transports map[string]registry.Runner) error {
	sites := make([]string, 0, len(transports))
	for name := range transports {
		sites = append(sites, name)
	}
	sort.Strings(sites)
	now := time.Now()
	for _, site := range sites {
		t := transports[site]
		// One site's claim is "claim site" under that site's section; a
		// command claiming several names each, since they share one.
		name := "site"
		if len(sites) > 1 {
			name = site
		}
		title := "claim " + name
		if !execute {
			if err := ui.Run(r, "check "+name+"'s claim", func() error { return registry.Check(t, cfg, site) }); err != nil {
				return err
			}
			continue
		}
		s := r.Step(title)
		s.Detail("claimed for deployment %s in %s", cfg.ID, registry.Path)
		if err := registry.Claim(t, cfg, site, now); err != nil {
			s.Fail(err)
			return err
		}
		s.Done("")
	}
	return nil
}

// registryHost is how a claim reaches a site's host when the command has no
// transport of its own to lend it. A test replaces it.
var registryHost = func(name string, site config.Site, destination string, sudo bool) registry.Runner {
	return siteTransport(name, site, destination, sudo)
}

// claimSites is claimHosts over the named sites, each reached through its
// own ssh section, with sudo unless the operator turned it off, since the
// registry is root's.
func claimSites(r ui.Reporter, cfg *config.Config, execute, sudo bool, sites ...string) error {
	transports := map[string]registry.Runner{}
	for _, name := range sites {
		transports[name] = registryHost(name, cfg.Sites[name], "", sudo)
	}
	return claimHosts(r, cfg, execute, transports)
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

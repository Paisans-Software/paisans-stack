// Package ownership reports what is on a host that apply leaves alone: this
// deployment's own leftovers, and foreign things that rely on its Caddy.
//
// A thing is this deployment's only when that is provable: a container with
// the deployment label carrying this id and a compose project named
// paisans-<token>-<stack>, or a file listed in this deployment's manifest.
// Everything else is foreign. Neither kind is changed here or by apply: a
// leftover is reported so the operator knows it is there, and a foreign thing
// relying on the gateway's Caddy is reported so whoever moves or stops the
// gateway knows what else goes with it.
//
// Classify is a pure function of the configuration, an inventory from
// internal/hostcheck and the manifest's entries, so every judgement is tested
// without a host.
package ownership

import (
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// Report is one site's leftovers and the foreign users of its Caddy.
type Report struct {
	Site string
	// Stacks are this deployment's compose projects on the host that the
	// configuration no longer renders for the site, sorted by name.
	Stacks []Stack
	// Files are the manifest's entries marked left over, sorted by path.
	Files []render.ManifestFile
	// HostSites are the *.caddy files in render.HostSitesDir, which the
	// gateway's Caddy serves, on a gateway site only.
	HostSites []string
	// Neighbours are containers without this deployment's label attached
	// to a network the gateway's Caddy is attached to.
	Neighbours []Neighbour
}

// Stack is one left over compose project.
type Stack struct {
	// Name is the stack, Project its compose project.
	Name, Project string
	// Containers are its containers by name, sorted, and Running how many
	// of them run.
	Containers []string
	Running    int
}

// Neighbour is a foreign container on one of Caddy's networks.
type Neighbour struct {
	Container, Network string
}

// Leftover reports whether anything of this deployment's is left over.
func (r Report) Leftover() bool { return len(r.Stacks) > 0 || len(r.Files) > 0 }

// Foreign reports whether anything foreign relies on this site's Caddy.
func (r Report) Foreign() bool { return len(r.HostSites) > 0 || len(r.Neighbours) > 0 }

// sharedNamespaces are the networks that attach nothing to anything: "host"
// is the host's own namespace, which every host networked container shares
// whether or not it talks to Caddy, and "none" has no interface at all.
var sharedNamespaces = map[string]bool{"host": true, "none": true}

// Classify builds site's report from inv, the inventory of its host, and
// manifest, the entries this deployment's manifest holds for it.
func Classify(cfg *config.Config, site string, inv *hostcheck.Inventory, manifest []render.ManifestFile) (Report, error) {
	out := Report{Site: site}
	declared, ok := cfg.Sites[site]
	if !ok {
		return out, fmt.Errorf("no site %q is declared", site)
	}
	stacks, err := render.SiteStacks(cfg, site)
	if err != nil {
		return out, err
	}
	rendered := map[string]bool{}
	for _, s := range stacks {
		rendered[s] = true
	}
	d := cfg.Deployment()
	prefix := d.Prefix() + "-"

	left := map[string]*Stack{}
	var caddy *hostcheck.Container
	for i, c := range inv.Containers {
		if c.Deployment != d.ID {
			continue
		}
		name, ok := strings.CutPrefix(c.Project, prefix)
		if !ok || name == "" {
			continue
		}
		if c.Project == d.Project("infra") && c.Service == "caddy" {
			caddy = &inv.Containers[i]
		}
		if rendered[name] {
			continue
		}
		s := left[name]
		if s == nil {
			s = &Stack{Name: name, Project: c.Project}
			left[name] = s
		}
		s.Containers = append(s.Containers, c.Name)
		if c.PID > 0 {
			s.Running++
		}
	}
	for _, s := range left {
		sort.Strings(s.Containers)
		out.Stacks = append(out.Stacks, *s)
	}
	sort.Slice(out.Stacks, func(i, j int) bool { return out.Stacks[i].Name < out.Stacks[j].Name })

	for _, f := range manifest {
		if f.Leftover {
			out.Files = append(out.Files, f)
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })

	if declared.Has(config.RoleGateway) {
		out.HostSites = append(out.HostSites, inv.HostSites...)
		sort.Strings(out.HostSites)
	}

	if caddy != nil {
		for _, network := range caddy.Networks {
			if sharedNamespaces[network] {
				continue
			}
			for _, c := range inv.Containers {
				if c.Deployment == d.ID || !contains(c.Networks, network) {
					continue
				}
				out.Neighbours = append(out.Neighbours, Neighbour{Container: c.Name, Network: network})
			}
		}
		sort.Slice(out.Neighbours, func(i, j int) bool {
			if out.Neighbours[i].Network != out.Neighbours[j].Network {
				return out.Neighbours[i].Network < out.Neighbours[j].Network
			}
			return out.Neighbours[i].Container < out.Neighbours[j].Container
		})
	}
	return out, nil
}

// LeftoverLines describes each leftover in one line, for a plan or a report.
func (r Report) LeftoverLines() []string {
	var out []string
	for _, s := range r.Stacks {
		state := "stopped"
		if s.Running > 0 {
			state = fmt.Sprintf("%d of %d running", s.Running, len(s.Containers))
		}
		out = append(out, fmt.Sprintf("stack %s (compose project %s, %s): %s", s.Name, s.Project, state, strings.Join(s.Containers, ", ")))
	}
	for _, f := range r.Files {
		line := "file /" + f.Path
		if f.LeftoverSince != "" {
			line += ", left over since " + f.LeftoverSince
		}
		out = append(out, line)
	}
	return out
}

// ForeignLines describes each foreign user of the site's Caddy in one line.
func (r Report) ForeignLines() []string {
	var out []string
	for _, f := range r.HostSites {
		out = append(out, "site block "+f+", served by this deployment's Caddy")
	}
	for _, n := range r.Neighbours {
		out = append(out, fmt.Sprintf("container %s, on network %s with this deployment's Caddy", n.Container, n.Network))
	}
	return out
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

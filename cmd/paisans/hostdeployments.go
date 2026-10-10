package main

import (
	"flag"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// reachDestination is how a command with no configuration reaches the host
// --ssh names: the way the operator's own ssh reaches it, with their agent,
// keys and ssh config, since there are no declared keys to offer. A port
// other than 22 goes in an ssh:// URL, the one destination form that
// carries it. Tests replace it.
var reachDestination = func(dest config.Destination, sudo bool) apply.Transport {
	to := dest.User + "@" + dest.Host
	if dest.Port != 0 && dest.Port != config.DefaultSSHPort {
		to = "ssh://" + dest.User + "@" + net.JoinHostPort(dest.Host, strconv.Itoa(dest.Port))
	}
	return siteTransport("", config.Site{}, to, sudo)
}

// deploymentsProbe lists, read only, each directory under the deployments'
// base, each site file in render.HostSitesDir and, where Docker is
// installed, the deployment label, compose project and compose service of
// every container carrying the label, tab separated. It fails,
// saying what, rather than answer partly: a directory it may not read (the
// registry's, which a read without sudo would take for an empty registry,
// or the deployments' base), and docker ps failing. Its last line is "end",
// so an answer cut short is told from a host holding nothing.
var deploymentsProbe = fmt.Sprintf(`set -e; for d in %[1]s %[2]s %[4]s; do if [ -e "$d" ] && ! { [ -r "$d" ] && [ -x "$d" ]; }; then echo "cannot read $d"; exit 3; fi; done; if [ -d %[2]s ]; then for d in %[2]s/*; do if [ -d "$d" ]; then echo "root ${d##*/}"; fi; done; fi; if [ -d %[4]s ]; then for f in %[4]s/*.caddy; do if [ -f "$f" ]; then echo "site ${f##*/}"; fi; done; fi; if command -v docker >/dev/null 2>&1; then if ! out=$(docker ps -a --filter label=%[3]s --format '{{printf "container\t%%s\t%%s\t%%s" (.Label "%[3]s") (.Label "com.docker.compose.project") (.Label "com.docker.compose.service")}}' 2>&1); then echo "docker ps failed: $out"; exit 4; fi; if [ -n "$out" ]; then printf '%%s\n' "$out"; fi; fi; echo end`, registry.Dir, deployment.Base, deployment.Label, render.HostSitesDir)

// leftoverProbe is what deploymentsProbe found: the directories under the
// base and the site files in render.HostSitesDir, by name, and each labelled
// container's deployment, project and service.
type leftoverProbe struct {
	roots, sites []string
	containers   [][3]string
}

func parseDeploymentsProbe(out string) (leftoverProbe, error) {
	var p leftoverProbe
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 || lines[len(lines)-1] != "end" {
		return p, fmt.Errorf("its answer was cut short: %s", strings.TrimSpace(out))
	}
	for _, line := range lines[:len(lines)-1] {
		switch {
		case strings.HasPrefix(line, "root "):
			p.roots = append(p.roots, strings.TrimPrefix(line, "root "))
		case strings.HasPrefix(line, "site "):
			p.sites = append(p.sites, strings.TrimPrefix(line, "site "))
		case strings.HasPrefix(line, "container\t"):
			fields := strings.Split(strings.TrimPrefix(line, "container\t"), "\t")
			if len(fields) != 3 {
				return p, fmt.Errorf("an unreadable line: %q", line)
			}
			p.containers = append(p.containers, [3]string{fields[0], fields[1], fields[2]})
		default:
			return p, fmt.Errorf("an unreadable line: %q", line)
		}
	}
	return p, nil
}

// runHostDeployments is `paisans host deployments`: what the registry of the
// host --ssh names holds, and what of a deployment is on it with no registry
// entry. It needs no configuration, and changes nothing. See
// docs/specs/2026-10-10-remove-without-config.md.
func runHostDeployments(args []string) error {
	fs := flag.NewFlagSet("host deployments", flag.ContinueOnError)
	reporter := commonFlags(fs)
	sshFlag := fs.String("ssh", "", "the host to read, user@host[:port]")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since the registry is root's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("host deployments takes no argument: paisans host deployments --ssh user@host[:port]. Got extra argument(s): %s", strings.Join(fs.Args(), " "))
	}
	if *sshFlag == "" {
		return fmt.Errorf("host deployments: name the host with --ssh user@host[:port], Eg: --ssh admin@203.0.113.9")
	}
	dest, err := config.ParseDestination(*sshFlag)
	if err != nil {
		return fmt.Errorf("host deployments: --ssh: %w", err)
	}
	r := reporter()
	t := reachDestination(dest, *sudo)
	// The probe first, since it fails on a registry directory it may not
	// read, which reading the registry would take for an empty one.
	out, err := t.Run(deploymentsProbe)
	if err != nil {
		return fmt.Errorf("host deployments: %s: listing %s and the labelled containers failed, so nothing is listed: %w: %s", dest, deployment.Base, err, strings.TrimSpace(out))
	}
	found, err := parseDeploymentsProbe(out)
	if err != nil {
		return fmt.Errorf("host deployments: %s: listing %s and the labelled containers: %w. Nothing is listed", dest, deployment.Base, err)
	}
	reg, err := registry.Read(t)
	if err != nil {
		return fmt.Errorf("host deployments: %w. Nothing is listed", err)
	}

	r.Section(fmt.Sprintf("deployments on %s", dest))
	ids := make([]string, 0, len(reg.Deployments))
	for id := range reg.Deployments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	tokens, roots := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		e := reg.Deployments[id]
		tokens[e.Token], roots[e.Root] = true, true
		roles := e.Roles
		if roles == "" {
			roles = "none"
		}
		line := fmt.Sprintf("%s: token %s, %s, site %s, roles %s, root %s", id, e.Token, e.Domain, e.Site, roles, e.Root)
		switch {
		case e.Kept == registry.KeptCaddy && len(found.sites) > 0:
			line += fmt.Sprintf("; caddy kept: serves %s sites (%s)", render.HostSitesDir, strings.Join(found.sites, ", "))
		case e.Kept == registry.KeptCaddy:
			line += fmt.Sprintf("; caddy kept, but %s holds no site now: paisans site remove --force --ssh %s --id %s removes it", render.HostSitesDir, *sshFlag, e.Token)
		}
		r.Item(line)
	}
	if len(ids) == 0 {
		r.Item("none in " + registry.Path)
	}

	var left []string
	seen := map[string]bool{}
	for _, name := range found.roots {
		dir := deployment.Base + "/" + name
		if !tokens[name] && !roots[dir] {
			left = append(left, dir+": no registry entry names this deployment directory")
		}
	}
	for _, c := range found.containers {
		id, project, service := c[0], c[1], c[2]
		if _, ok := reg.Deployments[id]; ok {
			continue
		}
		// A Caddy kept for the owner's sites whose entry was removed by
		// hand still serves them, so it is named as that.
		if service == "caddy" && len(found.sites) > 0 && deployment.ValidID(id) && project == (deployment.Deployment{ID: id}).Project("infra") {
			if !seen["orphaned "+id] {
				seen["orphaned "+id] = true
				left = append(left, fmt.Sprintf("%s orphaned Caddy: serves %s sites (%s); its registry entry was removed, so remove it by hand once those sites have moved", id, render.HostSitesDir, strings.Join(found.sites, ", ")))
			}
			continue
		}
		what := "containers"
		if project != "" {
			what = "compose project " + project
		}
		if !seen[what+" "+id] {
			seen[what+" "+id] = true
			left = append(left, fmt.Sprintf("%s: no registry entry for deployment %q, which its containers' label names", what, id))
		}
	}
	if len(left) > 0 {
		r.Section("not in the registry")
		sort.Strings(left)
		for _, l := range left {
			r.Item(l)
		}
	}

	switch {
	case len(ids) > 0:
		r.Result("%d deployment(s) in the registry. paisans site remove --force --ssh %s --id <id or token> cleans one off this host.", len(ids), *sshFlag)
	case len(left) > 0:
		r.Result("No deployment in the registry; what is listed above is removed by hand.")
	default:
		r.Result("No deployment in the registry, and nothing else of paisans on %s.", dest)
	}
	return nil
}

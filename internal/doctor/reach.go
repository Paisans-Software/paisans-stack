package doctor

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// ReachCommand is the trivial command that proves a site answers: ssh
// connected, the login worked, and, through a sudo transport, sudo did too.
const ReachCommand = "true"

// SiteReach is one site's answer to ReachCommand.
type SiteReach struct {
	Site string
	// Destination is the transport's description, Eg: ubuntu@home-a.local.
	Destination string
	// Err is what failed, empty when the site answered.
	Err string
	// Sudo is true when the site answered ssh but sudo could not be used,
	// which leaves it unreadable: Docker and /srv are root's.
	Sudo bool
}

// Answered reports whether the rest of the checks can ask this site.
func (r SiteReach) Answered() bool { return r.Err == "" }

// Reach is one line per site. A site that did not answer is a Fail, never a
// crash: the rest of the report is made from the sites that did, and this
// says what the deployment is missing while it is gone, so an operator can
// tell an inconvenience from an outage at a glance.
func Reach(cfg *config.Config, list []SiteReach) []Finding {
	var out []Finding
	for _, r := range list {
		switch {
		case r.Answered():
			out = append(out, Finding{Section: SectionReach, Level: OK, Line: fmt.Sprintf("%s: %s answers", r.Site, r.Destination)})
		case r.Sudo:
			out = append(out, Finding{Section: SectionReach, Level: Fail,
				Line: fmt.Sprintf("%s: %s answers, but sudo cannot be used, so nothing on it could be read (%s)", r.Site, r.Destination, firstLine(r.Err)),
				More: []string{"Every check reads Docker and /srv, which are root's. Fix sudo for the login user, or run doctor with --sudo=false where that user is in the docker group."}})
		default:
			more := []string{roleLine(cfg, r.Site)}
			if lost := losses(cfg, r.Site); lost != "" {
				more = append(more, "While it is down the deployment is without "+lost+".")
			}
			more = append(more, "Start the host or fix the route to it. Every other check ran with the sites that answered.")
			out = append(out, Finding{Section: SectionReach, Level: Fail,
				Line: fmt.Sprintf("%s: ssh to %s did not answer (%s)", r.Site, r.Destination, sshReason(firstLine(r.Err))),
				More: more})
		}
	}
	return out
}

func roleLine(cfg *config.Config, site string) string {
	var roles []string
	for _, role := range cfg.Sites[site].Roles {
		roles = append(roles, string(role))
	}
	if len(roles) == 0 {
		return "roles: none (it holds only pinned apps)"
	}
	return "roles: " + strings.Join(roles, ", ")
}

// losses is what a site's absence takes away, from its roles and its place
// in the configuration rather than from its roles alone: a data site that is
// not in etcd.members holds no etcd vote, and a gateway that is not the only
// one is not every hostname.
func losses(cfg *config.Config, site string) string {
	declared := cfg.Sites[site]
	var lost []string
	if declared.Has(config.RoleGateway) {
		switch {
		case len(cfg.GatewaySites()) == 1 && len(cfg.MonitorSites()) > 0:
			lost = append(lost, "its gateway, which every public hostname but the monitor's goes through")
		case len(cfg.GatewaySites()) == 1:
			lost = append(lost, "its gateway, which every public hostname goes through")
		default:
			lost = append(lost, "its gateway, and every public hostname whose DNS points only at it")
		}
	}
	if declared.Has(config.RoleMonitor) {
		lost = append(lost, "the uptime monitor, so nothing reports the other sites' failures while it is gone")
	}
	if contains(cfg.Etcd.Members, site) {
		lost = append(lost, fmt.Sprintf("one of %d etcd votes (quorum needs %d)", len(cfg.Etcd.Members), len(cfg.Etcd.Members)/2+1))
	}
	if contains(cfg.Cluster.Sites, site) {
		lost = append(lost, "a Patroni member")
	}
	if contains(cfg.Storage.Garage.Sites, site) {
		lost = append(lost, "a Garage node")
	}
	if declared.Has(config.RoleApps) {
		var clustered []string
		for _, name := range cfg.AppNames() {
			if cfg.Apps[name].Placement.Mode == config.PlacementCluster {
				clustered = append(clustered, name)
			}
		}
		if len(clustered) > 0 {
			lost = append(lost, "its copy of "+strings.Join(clustered, ", ")+" (the other apps sites still serve them)")
		}
	}
	if pinned := cfg.PinnedTo(site); len(pinned) > 0 {
		lost = append(lost, strings.Join(pinned, ", ")+", pinned here and running nowhere else")
	}
	return strings.Join(lost, "; ")
}

var (
	sshConnect = regexp.MustCompile(`^ssh: connect to host \S+ port \d+: (.+)$`)
	sshResolve = regexp.MustCompile(`^ssh: Could not resolve hostname \S+: (.+)$`)
)

// sshReason is ssh's error without the host it names, which the line it goes
// into names already.
func sshReason(err string) string {
	if m := sshConnect.FindStringSubmatch(err); m != nil {
		return m[1]
	}
	if m := sshResolve.FindStringSubmatch(err); m != nil {
		return "the name does not resolve: " + m[1]
	}
	return err
}

package doctor

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/mesh"
)

// MeshProbe is what one site said about the mesh: its interface's link, and
// the routes, addresses and Docker networks and pools the overlap test reads.
type MeshProbe struct {
	Site     string
	Link     string
	LinkErr  string
	Probed   mesh.Probed
	ProbeErr string
}

// Mesh checks each site's mesh: the deployment's interface is up, and
// nothing else on the host holds a network overlapping the mesh subnet. An
// overlap is a FAIL naming both sides, because whichever route the kernel
// prefers takes the other side's traffic, and the advice is to move the
// other network: the mesh subnet is fixed once deployed.
func Mesh(cfg *config.Config, probes []MeshProbe) []Finding {
	d := cfg.Deployment()
	iface := d.Interface()
	subnet, subnetErr := mesh.ParsePrefix(cfg.Mesh.Subnet)
	var out []Finding
	for _, p := range probes {
		if p.LinkErr != "" {
			out = append(out, Finding{Section: SectionMesh, Level: Warn, Line: fmt.Sprintf("%s: could not read %s", p.Site, iface), More: []string{firstLine(p.LinkErr)}})
		} else {
			exists, up, err := mesh.LinkUp(p.Link)
			switch {
			case err != nil:
				out = append(out, Finding{Section: SectionMesh, Level: Warn, Line: fmt.Sprintf("%s: %v", p.Site, err)})
			case !exists || !up:
				state := "does not exist"
				if exists {
					state = "is down"
				}
				out = append(out, Finding{Section: SectionMesh, Level: Fail, Line: fmt.Sprintf("%s: %s %s", p.Site, iface, state),
					More: []string{
						"Every service on the site binds its mesh address, which exists only while the interface is up, so nothing on this site can reach another one.",
						"recover:",
						fmt.Sprintf("  1. `systemctl status %s` and `journalctl -u %s` on %s say why it did not come up.", d.WireGuardUnit(), d.WireGuardUnit(), p.Site),
						fmt.Sprintf("  2. `paisans apply --site %s --execute` starts it and enables it at boot.", p.Site),
					}})
			default:
				out = append(out, Finding{Section: SectionMesh, Level: OK, Line: fmt.Sprintf("%s: %s is up", p.Site, iface)})
			}
		}

		if p.ProbeErr != "" {
			out = append(out, Finding{Section: SectionMesh, Level: Warn, Line: fmt.Sprintf("%s: could not read the host's networks, so mesh overlaps are unknown", p.Site), More: []string{firstLine(p.ProbeErr)}})
			continue
		}
		if subnetErr != nil {
			continue // validate refuses this before doctor runs
		}
		taken, err := p.Probed.Taken(iface, "")
		if err != nil {
			out = append(out, Finding{Section: SectionMesh, Level: Warn, Line: fmt.Sprintf("%s: %v", p.Site, err)})
			continue
		}
		clash := mesh.Clashes(subnet, taken)
		if len(clash) == 0 {
			out = append(out, Finding{Section: SectionMesh, Level: OK, Line: fmt.Sprintf("%s: nothing on the host overlaps the mesh subnet %s", p.Site, cfg.Mesh.Subnet)})
			continue
		}
		what := make([]string, len(clash))
		for i, c := range clash {
			what[i] = c.What
		}
		out = append(out, Finding{Section: SectionMesh, Level: Fail,
			Line: fmt.Sprintf("%s: the mesh subnet %s overlaps %s", p.Site, cfg.Mesh.Subnet, strings.Join(what, ", ")),
			More: []string{
				"Whichever route the kernel prefers takes the other network's traffic, so mesh packets can leave by the wrong interface, or the other network's arrive on the mesh.",
				"The mesh subnet is fixed once deployed: every app trusts it for forwarded client addresses and every service binds an address in it. The other network has to move.",
				"recover:",
				"  1. Move it to a range clear of " + cfg.Mesh.Subnet + ": a LAN or VPN in its own configuration; a Docker network by recreating it with another subnet; Docker's pools with default-address-pools in " + mesh.DaemonConfig + " and a restart of Docker.",
				fmt.Sprintf("  2. `paisans doctor --site %s` again.", p.Site),
			}})
	}
	return out
}

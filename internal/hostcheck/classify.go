package hostcheck

import (
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/render"
)

// Class is what a host is to the toolkit.
type Class int

const (
	// Clean is a host with nothing foreign on it: the toolkit's alone, or
	// about to be.
	Clean Class = iota
	// Shared is a host where something foreign runs and holds nothing the
	// site claims.
	Shared
	// Conflicted is a host where something foreign holds a claim.
	Conflicted
)

func (c Class) String() string {
	switch c {
	case Shared:
		return "shared"
	case Conflicted:
		return "conflict"
	}
	return "clean"
}

// Conflict is one claim something foreign holds.
type Conflict struct {
	// Resource is what is claimed, Eg: "*:80/tcp (Caddy)" or "interface wg0".
	Resource string
	// Key is the paisans.yaml key that claims it.
	Key string
	// Holder is what holds it: a container and its compose project, or a
	// process and its PID.
	Holder string
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s: claimed by %s, held by %s", c.Resource, c.Key, c.Holder)
}

// Report is the host check's finding on one site's host.
type Report struct {
	Site, Host string
	Class      Class
	Conflicts  []Conflict
	// Foreign is everything found that is neither the toolkit's nor the base
	// system's, one line each. It is what makes a host shared.
	Foreign []string
	// Notes are things found that are neither foreign nor a conflict but
	// worth an operator's eye, one line each.
	Notes     []string
	Inventory *Inventory
	// SSHPort is the site's ssh.port, for the firewall advice.
	SSHPort int
}

// Shared reports whether the host is shared, so that a command touches only
// what is the toolkit's.
func (r *Report) Shared() bool { return r.Class == Shared }

// projectPrefix starts every compose project the toolkit renders: each
// compose template is named paisans-<stack>.
const projectPrefix = "paisans-"

func ours(project string) bool { return strings.HasPrefix(project, projectPrefix) }

// baseProcesses are the base system's own listeners: what a stock server
// runs and the toolkit expects beside it. ss prints a process's name as the
// kernel keeps it, cut to 15 characters, so systemd-resolved and
// systemd-networkd (whose DHCP client listens on 68/udp) are listed both
// ways.
var baseProcesses = map[string]bool{
	"sshd":             true,
	"systemd-resolve":  true,
	"systemd-resolved": true,
	"systemd-network":  true,
	"systemd-networkd": true,
	"chronyd":          true,
	"tailscaled":       true,
}

// defaultNetworks are the networks every Docker install has.
var defaultNetworks = map[string]bool{"bridge": true, "host": true, "none": true}

// holder is who a socket belongs to.
type holder struct {
	ours, base bool
	desc       string
}

func containerHolder(c Container) holder {
	desc := "container " + c.Name
	if c.Project != "" {
		desc += " (compose project " + c.Project + ")"
	} else {
		desc += " (no compose project)"
	}
	return holder{ours: ours(c.Project), desc: desc}
}

type owners struct {
	inv  *Inventory
	byID map[string]Container
}

// socket decides who holds a listener. A process in a container's cgroup is
// that container's, which covers a host network container's own processes.
// A docker-proxy runs in Docker's cgroup rather than the container's, so it
// is matched to the container publishing the same address and port. The
// kernel's own WireGuard socket has no process, and is the toolkit's when
// the toolkit wrote the wg0 it serves.
func (o owners) socket(s Socket) holder {
	if s.PID > 0 {
		if c, ok := o.byID[o.inv.Cgroups[s.PID]]; ok {
			return containerHolder(c)
		}
	}
	if s.Process == "docker-proxy" {
		for _, c := range o.inv.Containers {
			for _, b := range c.Bindings {
				if b.Proto == s.Proto && b.Port == s.Port && b.Address == s.Address {
					return containerHolder(c)
				}
			}
		}
	}
	if s.Process == "" && s.Proto == "udp" && s.Port == render.WireGuardPort && o.inv.ManifestWireGuard && contains(o.inv.Links, meshInterface) {
		return holder{ours: true, desc: "WireGuard (" + meshInterface + ")"}
	}
	desc := "a kernel socket (no process)"
	if s.Process != "" {
		desc = fmt.Sprintf("process %s (pid %d)", s.Process, s.PID)
	}
	return holder{base: loopback(s.Address) || baseProcesses[s.Process], desc: desc}
}

// Classify decides who owns what the inventory found and compares it with
// the claims.
//
// A listener conflicts when the ports and protocols match and either side
// binds every address or both bind the same one, which is what stops a bind
// (render.Listener.Overlaps). That holds for the base system too: a
// loopback Postgres does not make a host shared, but the toolkit's own
// loopback bind on its port would fail.
func Classify(claims Claims, inv *Inventory) *Report {
	r := &Report{Site: claims.Site, Host: inv.Host, Inventory: inv, SSHPort: claims.SSHPort}
	o := owners{inv: inv, byID: map[string]Container{}}
	for _, c := range inv.Containers {
		o.byID[c.ID] = c
	}
	seen := map[Conflict]bool{}
	conflict := func(c Conflict) {
		if !seen[c] {
			seen[c] = true
			r.Conflicts = append(r.Conflicts, c)
		}
	}

	for _, claim := range claims.Listeners {
		resource := fmt.Sprintf("%s (%s)", claim, claim.Owner)
		for _, s := range inv.Sockets {
			if h := o.socket(s); !h.ours && claim.Overlaps(s.Listener()) {
				conflict(Conflict{Resource: resource, Key: claim.Key, Holder: h.desc})
			}
		}
		for _, c := range inv.Containers {
			if ours(c.Project) {
				continue
			}
			for _, b := range c.Bindings {
				if claim.Overlaps(b) {
					conflict(Conflict{Resource: resource, Key: claim.Key, Holder: containerHolder(c).desc})
				}
			}
		}
	}

	if contains(inv.Links, claims.Interface) && !inv.ManifestWireGuard {
		conflict(Conflict{
			Resource: "interface " + claims.Interface,
			Key:      "mesh",
			Holder:   "an interface the toolkit did not write (" + manifestPath + " records no " + wireguardFile + ")",
		})
	}

	// A Docker network's own bridge route is that network, judged by its
	// subnet below rather than a second time as a route.
	bridges := map[string]bool{"docker0": true}
	for _, n := range inv.Networks {
		if len(n.ID) >= 12 {
			bridges["br-"+n.ID[:12]] = true
		}
		if ours(n.Project) {
			continue
		}
		for _, subnet := range n.Subnets {
			if overlapsMesh(claims.Mesh, subnet) {
				conflict(Conflict{Resource: "subnet " + subnet, Key: "mesh.subnet", Holder: networkDesc(n)})
			}
		}
	}
	// A route equal to the mesh or inside it captures mesh traffic: the
	// kernel picks the longest matching prefix, and it is at least as long
	// as wg0's. A broader one (a provider's 10.0.0.0/8 private network) is
	// shorter than wg0's route, so the mesh still wins, and it is noted.
	for _, route := range inv.Routes {
		if route.Dst == "default" || route.Dev == claims.Interface || bridges[route.Dev] {
			continue
		}
		n, ok := network(route.Dst)
		if !ok || !(n.Contains(claims.Mesh.IP) || claims.Mesh.Contains(n.IP)) {
			continue
		}
		resource := "route " + route.Dst + " dev " + route.Dev
		if within(n, claims.Mesh) {
			conflict(Conflict{Resource: resource, Key: "mesh.subnet", Holder: "the host's routing table"})
		} else {
			r.Notes = append(r.Notes, fmt.Sprintf("%s contains the mesh subnet %s; wg0's route is more specific and takes the mesh's traffic", resource, claims.Mesh))
		}
	}

	r.Foreign = foreign(o)
	switch {
	case len(r.Conflicts) > 0:
		r.Class = Conflicted
	case len(r.Foreign) > 0:
		r.Class = Shared
	}
	return r
}

// foreign lists what is neither the toolkit's nor the base system's. An
// anonymous volume with no compose label is neither: it may be a replaced
// paisans container's leftover or somebody else's, and on its own it says
// nothing about who else lives here.
func foreign(o owners) []string {
	var out []string
	for _, c := range o.inv.Containers {
		if !ours(c.Project) {
			out = append(out, containerHolder(c).desc)
		}
	}
	for _, s := range o.inv.Sockets {
		if h := o.socket(s); !h.ours && !h.base {
			out = append(out, fmt.Sprintf("%s held by %s", s.Listener(), h.desc))
		}
	}
	for _, n := range o.inv.Networks {
		if !ours(n.Project) && !(defaultNetworks[n.Name] && n.Project == "") {
			out = append(out, networkDesc(n))
		}
	}
	for _, v := range o.inv.Volumes {
		switch {
		case ours(v.Project), v.Project == "" && v.Anonymous:
		case v.Project != "":
			out = append(out, fmt.Sprintf("volume %s (compose project %s)", v.Name, v.Project))
		default:
			out = append(out, fmt.Sprintf("volume %s (named, no compose project)", v.Name))
		}
	}
	return out
}

func networkDesc(n Network) string {
	if n.Project == "" {
		return "docker network " + n.Name
	}
	return fmt.Sprintf("docker network %s (compose project %s)", n.Name, n.Project)
}

// overlapsMesh reports whether a network, as CIDR or as a bare address,
// overlaps the mesh subnet at all.
func overlapsMesh(mesh *net.IPNet, cidr string) bool {
	n, ok := network(cidr)
	return ok && (n.Contains(mesh.IP) || mesh.Contains(n.IP))
}

// within reports whether n equals the mesh or lies inside it.
func within(n, mesh *net.IPNet) bool {
	nOnes, _ := n.Mask.Size()
	meshOnes, _ := mesh.Mask.Size()
	return mesh.Contains(n.IP) && nOnes >= meshOnes
}

// network reads a CIDR, or a bare address as a host route.
func network(cidr string) (*net.IPNet, bool) {
	if !strings.Contains(cidr, "/") {
		if ip := net.ParseIP(cidr); ip != nil && ip.To4() == nil {
			cidr += "/128"
		} else {
			cidr += "/32"
		}
	}
	_, n, err := net.ParseCIDR(cidr)
	return n, err == nil
}

func loopback(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil && ip.IsLoopback()
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// Refusal is the reason a command must not go on, or nil. A conflict is
// always refused, with no override: the operator moves what holds the claim
// or changes the configuration. A shared host is refused when its firewall
// is not already up and denying (or rejecting) by default, because on a
// shared host the toolkit never sets the default policy or enables ufw
// itself.
func (r *Report) Refusal() error {
	switch r.Class {
	case Conflicted:
		return fmt.Errorf("host check: %s holds %d thing(s) %s claims, each listed above as CONFLICT, so nothing was changed. Move what holds each one, or change the paisans.yaml key it names, and run again", r.Host, len(r.Conflicts), r.Site)
	case Shared:
		if why := firewallGap(r.Inventory.Firewall); why != "" {
			port := r.SSHPort
			if port == 0 {
				port = 22
			}
			return fmt.Errorf("host check: %s runs services this deployment does not own, and on a shared host the toolkit never sets ufw's default policy or enables it, so both must already be in place, and %s. In this order: allow SSH so the session you are in survives (ufw allow %d/tcp), allow what those services need, deny incoming by default (ufw default deny incoming), enable ufw (ufw enable), and run again", r.Host, why, port)
		}
	}
	return nil
}

// firewallGap says what is missing from a shared host's firewall, "" for
// nothing.
func firewallGap(f Firewall) string {
	switch {
	case f.Firewalld:
		return "firewalld is active: two firewalls on one host each decide what gets in, and the toolkit's rules are ufw's"
	case !f.UFW:
		return "ufw is not installed"
	case !f.Active:
		return "ufw is inactive"
	case f.Incoming != "deny" && f.Incoming != "reject":
		return fmt.Sprintf("ufw's default for incoming traffic is %s, not deny or reject", f.Incoming)
	}
	return ""
}

// Print writes the report: the class, Docker and the firewall as found,
// then one line per foreign thing and per conflict.
func (r *Report) Print(w io.Writer) {
	line := func(label, text string) { fmt.Fprintf(w, "  %-9s %s\n", label, text) }
	fmt.Fprintf(w, "host check: %s (%s) is %s\n", r.Site, r.Host, r.Class)
	inv := r.Inventory
	docker := "absent"
	if inv.Docker.Present {
		docker = inv.Docker.Version
	}
	if len(inv.Docker.Packages) > 0 {
		docker += " (" + strings.Join(inv.Docker.Packages, ", ") + ")"
	}
	line("docker", docker)
	line("firewall", firewallSummary(inv.Firewall))
	for _, f := range r.Foreign {
		line("foreign", f)
	}
	for _, n := range r.Notes {
		line("note", n)
	}
	for _, c := range r.Conflicts {
		line("CONFLICT", c.String())
	}
	if r.Class == Shared {
		line("shared", "ufw's default policy and enabled state are left alone, superseded images are kept, and prune removes only volumes a paisans-* compose project labelled")
	}
}

func firewallSummary(f Firewall) string {
	var s string
	switch {
	case !f.UFW:
		s = "ufw absent"
	case !f.Active:
		s = "ufw inactive"
	default:
		s = "ufw active, incoming " + f.Incoming
	}
	if f.Firewalld {
		s += "; firewalld active"
	}
	return s
}

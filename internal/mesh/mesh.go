// Package mesh keeps a deployment's mesh subnet clear of every other network
// on the hosts it runs on.
//
// The mesh subnet is routed through the deployment's WireGuard interface on
// every site. Anything else on a host that routes or allocates an
// overlapping range competes for the same addresses: another deployment's
// mesh, a LAN, another VPN, a Docker network, or a range Docker will hand to
// its next network. Whichever route the kernel prefers wins, and the other
// side's traffic silently goes to the wrong place. The subnet also cannot
// move once a site is deployed, because every app trusts it for forwarded
// client addresses and every service binds an address in it.
//
// So the subnet is checked against each host as it is now (Probe, Clashes)
// whenever a command is about to write there, and `paisans init` chooses one
// that clears every host before anything is deployed (Choose). Everything
// that decides is a pure function of what a host printed, tested here
// without one.
package mesh

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
)

// Taken is a network something on a host already holds, and what holds it,
// in words for a refusal: Eg: "route 10.44.0.0/16 dev eth1 on home-a".
type Taken struct {
	Prefix netip.Prefix
	What   string
}

func (t Taken) String() string { return t.What }

// ParsePrefix reads an IPv4 network or address, an address meaning a /32,
// and returns it masked to its network.
func ParsePrefix(s string) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return p.Masked(), nil
}

// Overlap reports whether two networks share any address: one contains the
// other, either way round, or they are the same. Two networks of different
// families never overlap.
func Overlap(a, b netip.Prefix) bool {
	a, b = a.Masked(), b.Masked()
	if a.Addr().Is4() != b.Addr().Is4() {
		return false
	}
	return a.Contains(b.Addr()) || b.Contains(a.Addr())
}

// Clashes is every taken network that overlaps subnet, in the order given.
func Clashes(subnet netip.Prefix, taken []Taken) []Taken {
	var out []Taken
	for _, t := range taken {
		if Overlap(subnet, t.Prefix) {
			out = append(out, t)
		}
	}
	return out
}

// Commands a probe runs. Each prints JSON on stdout and nothing else.
const (
	// RouteCommand lists the main routing table.
	RouteCommand = "ip -j route"
	// AddrCommand lists every address on every interface.
	AddrCommand = "ip -j addr"
	// NetworksCommand inspects every Docker network, or prints an empty list
	// when Docker is not installed yet: a host before `host prepare` has no
	// networks, and its pools are still Docker's defaults.
	NetworksCommand = `command -v docker >/dev/null 2>&1 || { echo '[]'; exit 0; }; ids=$(docker network ls -q); if [ -z "$ids" ]; then echo '[]'; else docker network inspect $ids; fi`
	// DaemonConfig is Docker's daemon configuration, where
	// default-address-pools is set when it is.
	DaemonConfig = "/etc/docker/daemon.json"
)

// LinkCommand prints iface's link as JSON, or "absent" when there is none.
func LinkCommand(iface string) string {
	return "ip -j link show dev " + iface + " 2>/dev/null || echo absent"
}

// ParseRoutes reads `ip -j route` and returns every route's destination as a
// taken network, except the default route and routes through own, the
// deployment's own interface, which are the mesh itself.
func ParseRoutes(out, own string) ([]Taken, error) {
	var routes []struct {
		Dst string `json:"dst"`
		Dev string `json:"dev"`
	}
	if err := decode(out, &routes); err != nil {
		return nil, fmt.Errorf("unreadable `%s` output: %w", RouteCommand, err)
	}
	var taken []Taken
	for _, r := range routes {
		if r.Dst == "default" || r.Dev == own {
			continue
		}
		p, err := ParsePrefix(r.Dst)
		if err != nil || !p.Addr().Is4() {
			continue
		}
		taken = append(taken, Taken{p, fmt.Sprintf("route %s dev %s", r.Dst, r.Dev)})
	}
	return taken, nil
}

// ParseAddrs reads `ip -j addr` and returns each IPv4 address's network,
// except on own.
func ParseAddrs(out, own string) ([]Taken, error) {
	var links []struct {
		IfName   string `json:"ifname"`
		AddrInfo []struct {
			Family    string `json:"family"`
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := decode(out, &links); err != nil {
		return nil, fmt.Errorf("unreadable `%s` output: %w", AddrCommand, err)
	}
	var taken []Taken
	for _, l := range links {
		if l.IfName == own {
			continue
		}
		for _, a := range l.AddrInfo {
			if a.Family != "inet" {
				continue
			}
			addr, err := netip.ParseAddr(a.Local)
			if err != nil {
				continue
			}
			p, err := addr.Prefix(a.PrefixLen)
			if err != nil {
				continue
			}
			taken = append(taken, Taken{p, fmt.Sprintf("address %s/%d on %s", a.Local, a.PrefixLen, l.IfName)})
		}
	}
	return taken, nil
}

// ParseNetworks reads `docker network inspect` and returns every IPv4 subnet
// its IPAM configuration holds.
func ParseNetworks(out string) ([]Taken, error) {
	var networks []struct {
		Name string `json:"Name"`
		IPAM struct {
			Config []struct {
				Subnet string `json:"Subnet"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if err := decode(out, &networks); err != nil {
		return nil, fmt.Errorf("unreadable `docker network inspect` output: %w", err)
	}
	var taken []Taken
	for _, n := range networks {
		for _, c := range n.IPAM.Config {
			p, err := ParsePrefix(c.Subnet)
			if err != nil || !p.Addr().Is4() {
				continue
			}
			taken = append(taken, Taken{p, fmt.Sprintf("Docker network %s (%s)", n.Name, c.Subnet)})
		}
	}
	return taken, nil
}

// DefaultPools are the ranges Docker allocates a new local network from when
// daemon.json sets no default-address-pools: moby v27.5.1,
// libnetwork/ipamutils/utils.go, localScopeDefaultNetworks, used by
// daemon/daemon.go unless the daemon's DefaultAddressPools are set. A range
// in a pool is taken even before a network holds it, since the next
// `docker network create` (any compose project's first `up`) may.
var DefaultPools = []string{
	"172.17.0.0/16", "172.18.0.0/16", "172.19.0.0/16",
	"172.20.0.0/14", "172.24.0.0/14", "172.28.0.0/14",
	"192.168.0.0/16",
}

// Pools reads Docker's daemon.json and returns the address pools it will
// allocate from: default-address-pools when the file sets it, and
// DefaultPools otherwise.
func Pools(daemonJSON string, found bool) ([]Taken, error) {
	var bases []string
	configured := false
	if found && strings.TrimSpace(daemonJSON) != "" {
		var daemon struct {
			Pools []struct {
				Base string `json:"base"`
			} `json:"default-address-pools"`
		}
		if err := json.Unmarshal([]byte(daemonJSON), &daemon); err != nil {
			return nil, fmt.Errorf("unreadable %s: %w", DaemonConfig, err)
		}
		for _, p := range daemon.Pools {
			bases = append(bases, p.Base)
		}
		configured = len(bases) > 0
	}
	what := "Docker's default address pool %s"
	if !configured {
		bases = DefaultPools
	} else {
		what = "Docker's address pool %s (" + DaemonConfig + ")"
	}
	var taken []Taken
	for _, b := range bases {
		p, err := ParsePrefix(b)
		if err != nil {
			return nil, fmt.Errorf("%s: pool %q is not a network", DaemonConfig, b)
		}
		if p.Addr().Is4() {
			taken = append(taken, Taken{p, fmt.Sprintf(what, b)})
		}
	}
	return taken, nil
}

func decode(out string, v any) error {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	return json.Unmarshal([]byte(out), v)
}

// Host is what a probe needs of a transport.
type Host interface {
	Run(command string) (string, error)
	ReadFile(path string) (content string, found bool, err error)
	Describe() string
}

// Probed is what one host said, raw, so that a caller holding it (doctor)
// judges it as a pure function.
type Probed struct {
	Routes, Addrs, Networks string
	Daemon                  string
	DaemonFound             bool
}

// Probe asks a host for its routes, addresses, Docker networks and Docker's
// pools. It reads and never writes.
func Probe(h Host) (Probed, error) {
	var p Probed
	var err error
	for _, step := range []struct {
		name, command string
		into          *string
	}{
		{RouteCommand, RouteCommand, &p.Routes},
		{AddrCommand, AddrCommand, &p.Addrs},
		{"docker network inspect", NetworksCommand, &p.Networks},
	} {
		out, runErr := h.Run(step.command)
		if runErr != nil {
			return p, fmt.Errorf("%s: `%s` failed: %v: %s", h.Describe(), step.name, runErr, strings.TrimSpace(out))
		}
		*step.into = out
	}
	if p.Daemon, p.DaemonFound, err = h.ReadFile(DaemonConfig); err != nil {
		return p, fmt.Errorf("%s: reading %s: %w", h.Describe(), DaemonConfig, err)
	}
	return p, nil
}

// Taken is everything the probe found holding a network, outside own, the
// deployment's own interface. on names the host, for the message.
func (p Probed) Taken(own, on string) ([]Taken, error) {
	var all []Taken
	routes, err := ParseRoutes(p.Routes, own)
	if err != nil {
		return nil, err
	}
	addrs, err := ParseAddrs(p.Addrs, own)
	if err != nil {
		return nil, err
	}
	networks, err := ParseNetworks(p.Networks)
	if err != nil {
		return nil, err
	}
	pools, err := Pools(p.Daemon, p.DaemonFound)
	if err != nil {
		return nil, err
	}
	for _, list := range [][]Taken{routes, addrs, networks, pools} {
		for _, t := range list {
			if on != "" {
				t.What += " on " + on
			}
			all = append(all, t)
		}
	}
	return all, nil
}

// LinkUp reads LinkCommand's output: whether the interface exists and is
// administratively up.
func LinkUp(out string) (exists, up bool, err error) {
	out = strings.TrimSpace(out)
	if out == "absent" || out == "" {
		return false, false, nil
	}
	var links []struct {
		Flags []string `json:"flags"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil || len(links) == 0 {
		return false, false, fmt.Errorf("unreadable `ip -j link` output: %q", out)
	}
	for _, f := range links[0].Flags {
		if f == "UP" {
			return true, true, nil
		}
	}
	return true, false, nil
}

// MaxRolls is how many random subnets Choose tries before it gives up.
const MaxRolls = 32

// Choice is what Choose decided and why.
type Choice struct {
	Subnet netip.Prefix
	// Rolled is false when the declared subnet was kept.
	Rolled bool
	// Why is each reason a subnet was passed over, in order: the declared one
	// first, then each roll that collided.
	Why []string
}

// Choose keeps current when it is set and overlaps nothing taken, and
// otherwise rolls a random 10.<a>.<b>.0/24 from random until one overlaps
// nothing, up to MaxRolls. A /24 holds 254 sites, far more than any
// deployment has, and the rest of 10.0.0.0/8 is 65536 of them to choose
// from, so a collision is rare and a second roll almost always clears it.
// Failing after MaxRolls names every network that blocked a roll.
func Choose(current string, taken []Taken, random io.Reader) (Choice, error) {
	var c Choice
	if current == "" {
		c.Why = append(c.Why, "no mesh.subnet was declared")
	} else {
		p, err := ParsePrefix(current)
		if err != nil {
			return c, fmt.Errorf("mesh.subnet %q is not a network: %w", current, err)
		}
		hits := Clashes(p, taken)
		if len(hits) == 0 {
			c.Subnet = p
			return c, nil
		}
		c.Why = append(c.Why, fmt.Sprintf("%s overlapped %s", current, describe(hits)))
	}
	blockers := map[string]bool{}
	for i := 0; i < MaxRolls; i++ {
		var b [2]byte
		if _, err := io.ReadFull(random, b[:]); err != nil {
			return c, fmt.Errorf("reading random bytes for a subnet: %w", err)
		}
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, b[0], b[1], 0}), 24)
		hits := Clashes(p, taken)
		if len(hits) == 0 {
			c.Subnet, c.Rolled = p, true
			return c, nil
		}
		c.Why = append(c.Why, fmt.Sprintf("%s overlapped %s", p, describe(hits)))
		for _, h := range hits {
			blockers[h.What] = true
		}
	}
	names := make([]string, 0, len(blockers))
	for name := range blockers {
		names = append(names, name)
	}
	sort.Strings(names)
	return c, fmt.Errorf("no free /24 in 10.0.0.0/8 after %d random tries; each overlapped one of: %s. Free some of that space on the hosts, or declare a mesh.subnet that is clear of it", MaxRolls, strings.Join(names, "; "))
}

func describe(hits []Taken) string {
	parts := make([]string, len(hits))
	for i, h := range hits {
		parts[i] = h.What
	}
	return strings.Join(parts, ", ")
}

// Readdress moves each address into subnet, keeping its host number: the
// part of the address under old's mask when old is a network the address
// lies in, and otherwise the part under subnet's own mask. A host number
// that does not fit subnet, or lands on its network or broadcast address, or
// two sites landing on one address, is refused.
func Readdress(old string, subnet netip.Prefix, addresses map[string]string) (map[string]string, error) {
	oldNet, oldErr := ParsePrefix(old)
	bits := 32 - subnet.Bits()
	size := uint32(1) << bits
	base := v4(subnet.Masked().Addr())
	out := map[string]string{}
	used := map[string]string{}
	names := make([]string, 0, len(addresses))
	for name := range addresses {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a, err := netip.ParseAddr(addresses[name])
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("sites.%s.address %q is not an IPv4 address", name, addresses[name])
		}
		host := v4(a) & (size - 1)
		if old != "" && oldErr == nil && oldNet.Contains(a) {
			host = v4(a) - v4(oldNet.Addr())
			if host >= size {
				return nil, fmt.Errorf("sites.%s.address %s is host %d of %s, which does not fit in %s", name, a, host, old, subnet)
			}
		}
		if host == 0 || host == size-1 {
			return nil, fmt.Errorf("sites.%s.address %s would be %s's network or broadcast address", name, a, subnet)
		}
		next := from4(base | host).String()
		if other, ok := used[next]; ok {
			return nil, fmt.Errorf("sites.%s and sites.%s would both be %s", other, name, next)
		}
		used[next] = name
		out[name] = next
	}
	return out, nil
}

func v4(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func from4(n uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

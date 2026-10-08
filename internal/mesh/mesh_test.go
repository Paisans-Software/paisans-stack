package mesh_test

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/mesh"
)

func prefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := mesh.ParsePrefix(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"10.44.0.0/24", "10.44.0.0/24", true},  // equal
		{"10.44.0.0/16", "10.44.7.0/24", true},  // a contains b
		{"10.44.7.0/24", "10.44.0.0/16", true},  // b contains a
		{"10.44.0.0/24", "10.44.0.9/32", true},  // a host route inside
		{"10.44.0.0/24", "10.44.1.0/24", false}, // neighbours
		{"10.44.0.0/24", "172.17.0.0/16", false},
		{"10.0.0.0/8", "10.212.37.0/24", true},
		{"10.44.0.5/24", "10.44.0.0/24", true}, // an unmasked network is its network
	} {
		a, b := prefix(t, tc.a), prefix(t, tc.b)
		if got := mesh.Overlap(a, b); got != tc.want {
			t.Errorf("Overlap(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := mesh.Overlap(b, a); got != tc.want {
			t.Errorf("Overlap(%s, %s) = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
	if mesh.Overlap(prefix(t, "10.44.0.0/24"), netip.MustParsePrefix("::/0")) {
		t.Error("an IPv6 network overlapped an IPv4 one")
	}
}

// Output of `ip -j route` on a host with its own mesh up, a LAN, a Docker
// bridge and a host route.
const routes = `[{"dst":"default","gateway":"192.0.2.1","dev":"eth0","protocol":"dhcp","flags":[]},
{"dst":"10.44.0.0/24","dev":"psns-f2a9","scope":"link","flags":[]},
{"dst":"172.17.0.0/16","dev":"docker0","protocol":"kernel","scope":"link","prefsrc":"172.17.0.1","flags":["linkdown"]},
{"dst":"192.0.2.0/24","dev":"eth0","protocol":"kernel","scope":"link","prefsrc":"192.0.2.10","flags":[]},
{"dst":"10.9.0.7","dev":"tun0","scope":"link","flags":[]}]`

func TestParseRoutesSkipsTheDefaultAndOwnInterface(t *testing.T) {
	taken, err := mesh.ParseRoutes(routes, "psns-f2a9")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range taken {
		got = append(got, tk.Prefix.String()+" "+tk.What)
	}
	want := []string{
		"172.17.0.0/16 route 172.17.0.0/16 dev docker0",
		"192.0.2.0/24 route 192.0.2.0/24 dev eth0",
		"10.9.0.7/32 route 10.9.0.7 dev tun0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("ParseRoutes =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Another deployment's interface is not this one's own.
	taken, _ = mesh.ParseRoutes(routes, "psns-0c1d")
	if len(mesh.Clashes(prefix(t, "10.44.0.0/24"), taken)) != 1 {
		t.Error("a route through another deployment's interface was not taken")
	}
	if _, err := mesh.ParseRoutes("not json", ""); err == nil {
		t.Error("garbage was read as routes")
	}
}

const addrs = `[{"ifindex":1,"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8},{"family":"inet6","local":"::1","prefixlen":128}]},
{"ifindex":2,"ifname":"eth0","addr_info":[{"family":"inet","local":"192.0.2.10","prefixlen":24}]},
{"ifindex":5,"ifname":"psns-f2a9","addr_info":[{"family":"inet","local":"10.44.0.1","prefixlen":24}]},
{"ifindex":6,"ifname":"wg9","addr_info":[{"family":"inet","local":"10.44.0.200","prefixlen":32}]}]`

func TestParseAddrs(t *testing.T) {
	taken, err := mesh.ParseAddrs(addrs, "psns-f2a9")
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 3 {
		t.Fatalf("ParseAddrs = %v, want lo, eth0 and wg9", taken)
	}
	hits := mesh.Clashes(prefix(t, "10.44.0.0/24"), taken)
	if len(hits) != 1 || hits[0].What != "address 10.44.0.200/32 on wg9" {
		t.Errorf("clashes = %v, want wg9's address", hits)
	}
	if taken[1].Prefix.String() != "192.0.2.0/24" {
		t.Errorf("eth0's network = %s, want 192.0.2.0/24", taken[1].Prefix)
	}
}

const networks = `[{"Name":"bridge","IPAM":{"Driver":"default","Config":[{"Subnet":"172.17.0.0/16","Gateway":"172.17.0.1"}]}},
{"Name":"host","IPAM":{"Driver":"default","Config":[]}},
{"Name":"paisans-0c1d-talk_default","IPAM":{"Driver":"default","Config":[{"Subnet":"10.44.0.0/20","Gateway":"10.44.0.1"},{"Subnet":"fd00::/64"}]}}]`

func TestParseNetworks(t *testing.T) {
	taken, err := mesh.ParseNetworks(networks)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 2 {
		t.Fatalf("ParseNetworks = %v, want two IPv4 subnets", taken)
	}
	hits := mesh.Clashes(prefix(t, "10.44.0.0/24"), taken)
	if len(hits) != 1 || hits[0].What != "Docker network paisans-0c1d-talk_default (10.44.0.0/20)" {
		t.Errorf("clashes = %v", hits)
	}
	if taken, err := mesh.ParseNetworks("[]\n"); err != nil || len(taken) != 0 {
		t.Errorf("no networks = %v, %v", taken, err)
	}
}

func TestPools(t *testing.T) {
	defaults, err := mesh.Pools("", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaults) != len(mesh.DefaultPools) || !strings.Contains(defaults[0].What, "default address pool 172.17.0.0/16") {
		t.Fatalf("defaults = %v", defaults)
	}
	for subnet, want := range map[string]bool{"192.168.40.0/24": true, "172.21.0.0/24": true, "10.44.0.0/24": false} {
		if got := len(mesh.Clashes(prefix(t, subnet), defaults)) > 0; got != want {
			t.Errorf("%s against Docker's defaults: clash %v, want %v", subnet, got, want)
		}
	}

	// A daemon.json without pools leaves the defaults.
	if same, err := mesh.Pools(`{"log-driver":"journald"}`, true); err != nil || len(same) != len(mesh.DefaultPools) {
		t.Errorf("a daemon.json without pools = %v, %v", same, err)
	}
	set, err := mesh.Pools(`{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || set[0].Prefix.String() != "10.200.0.0/16" || !strings.Contains(set[0].What, "daemon.json") {
		t.Fatalf("configured pools = %v", set)
	}
	if len(mesh.Clashes(prefix(t, "172.17.0.0/24"), set)) != 0 {
		t.Error("configured pools still included the defaults")
	}
	if _, err := mesh.Pools(`{`, true); err == nil {
		t.Error("an unreadable daemon.json was accepted")
	}
}

type host struct {
	outputs map[string]string
	daemon  string
}

func (h host) Run(c string) (string, error) { return h.outputs[c], nil }
func (h host) ReadFile(string) (string, bool, error) {
	return h.daemon, h.daemon != "", nil
}
func (h host) Describe() string { return "fake" }

func TestProbeGathersEverySource(t *testing.T) {
	h := host{outputs: map[string]string{
		mesh.RouteCommand:    routes,
		mesh.AddrCommand:     addrs,
		mesh.NetworksCommand: networks,
	}}
	probed, err := mesh.Probe(h)
	if err != nil {
		t.Fatal(err)
	}
	taken, err := probed.Taken("psns-f2a9", "home-a")
	if err != nil {
		t.Fatal(err)
	}
	hits := mesh.Clashes(prefix(t, "10.44.0.0/24"), taken)
	var whats []string
	for _, h := range hits {
		whats = append(whats, h.What)
	}
	want := "address 10.44.0.200/32 on wg9 on home-a; Docker network paisans-0c1d-talk_default (10.44.0.0/20) on home-a"
	if strings.Join(whats, "; ") != want {
		t.Errorf("clashes = %q, want %q", strings.Join(whats, "; "), want)
	}
}

func TestLinkUp(t *testing.T) {
	for out, want := range map[string][2]bool{
		"absent\n": {false, false},
		`[{"ifname":"psns-f2a9","flags":["POINTOPOINT","NOARP","UP","LOWER_UP"],"operstate":"UNKNOWN"}]`: {true, true},
		`[{"ifname":"psns-f2a9","flags":["POINTOPOINT","NOARP"],"operstate":"DOWN"}]`:                    {true, false},
	} {
		exists, up, err := mesh.LinkUp(out)
		if err != nil || exists != want[0] || up != want[1] {
			t.Errorf("LinkUp(%q) = %v %v %v", out, exists, up, err)
		}
	}
}

func TestChooseKeepsAClearSubnet(t *testing.T) {
	c, err := mesh.Choose("10.44.0.0/24", []mesh.Taken{{prefix(t, "172.17.0.0/16"), "docker0"}}, bytes.NewReader(nil))
	if err != nil || c.Rolled || c.Subnet.String() != "10.44.0.0/24" {
		t.Fatalf("Choose = %+v, %v", c, err)
	}
}

func TestChooseRollsAgainOnACollision(t *testing.T) {
	taken := []mesh.Taken{
		{prefix(t, "10.44.0.0/24"), "deployment x's mesh on home-a"},
		{prefix(t, "10.1.0.0/16"), "route 10.1.0.0/16 dev eth1 on vm"},
	}
	// The first roll is 10.1.2.0/24, inside the route; the second is clear.
	random := bytes.NewReader([]byte{1, 2, 212, 37})
	c, err := mesh.Choose("10.44.0.0/24", taken, random)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Rolled || c.Subnet.String() != "10.212.37.0/24" {
		t.Fatalf("Choose = %+v, want 10.212.37.0/24 rolled", c)
	}
	want := []string{
		"10.44.0.0/24 overlapped deployment x's mesh on home-a",
		"10.1.2.0/24 overlapped route 10.1.0.0/16 dev eth1 on vm",
	}
	if strings.Join(c.Why, "\n") != strings.Join(want, "\n") {
		t.Errorf("Why = %q, want %q", c.Why, want)
	}

	// No subnet declared: the first roll is taken, and the reason says so.
	c, err = mesh.Choose("", taken, bytes.NewReader([]byte{212, 37}))
	if err != nil || c.Subnet.String() != "10.212.37.0/24" || c.Why[0] != "no mesh.subnet was declared" {
		t.Errorf("Choose with no subnet = %+v, %v", c, err)
	}
}

func TestChooseGivesUpNamingWhatBlockedIt(t *testing.T) {
	taken := []mesh.Taken{
		{prefix(t, "10.0.0.0/9"), "route 10.0.0.0/9 dev tun0 on home-a"},
		{prefix(t, "10.128.0.0/9"), "Docker's address pool 10.128.0.0/9 on vm"},
	}
	random := bytes.NewReader(bytes.Repeat([]byte{0x7f, 1, 0x80, 1}, mesh.MaxRolls))
	_, err := mesh.Choose("", taken, random)
	if err == nil {
		t.Fatal("Choose found a subnet in a fully taken 10.0.0.0/8")
	}
	for _, want := range []string{"after 32 random tries", "route 10.0.0.0/9 dev tun0 on home-a", "Docker's address pool 10.128.0.0/9 on vm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not contain %q", err, want)
		}
	}
}

func TestReaddressKeepsHostNumbers(t *testing.T) {
	to := prefix(t, "10.212.37.0/24")
	got, err := mesh.Readdress("10.44.0.0/24", to, map[string]string{"home-a": "10.44.0.1", "vm": "10.44.0.3"})
	if err != nil || got["home-a"] != "10.212.37.1" || got["vm"] != "10.212.37.3" {
		t.Fatalf("Readdress = %v, %v", got, err)
	}
	// With no subnet declared, the host number is the last octet.
	got, err = mesh.Readdress("", to, map[string]string{"home-a": "10.44.0.17"})
	if err != nil || got["home-a"] != "10.212.37.17" {
		t.Fatalf("Readdress with no old subnet = %v, %v", got, err)
	}
	// Host 257 of a /16 does not fit a /24.
	if _, err := mesh.Readdress("10.44.0.0/16", to, map[string]string{"home-a": "10.44.1.1"}); err == nil {
		t.Error("a host number too big for the new subnet was truncated")
	}
	if _, err := mesh.Readdress("", to, map[string]string{"home-a": "10.44.0.255"}); err == nil {
		t.Error("a broadcast address was accepted")
	}
}

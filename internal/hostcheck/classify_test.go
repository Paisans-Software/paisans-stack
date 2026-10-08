package hostcheck_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/hostcheck"
)

func check(t *testing.T, site string, h *fakeHost) *hostcheck.Report {
	t.Helper()
	r, err := hostcheck.Run(fixture(t), site, h)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func printed(r *hostcheck.Report) string {
	var b bytes.Buffer
	r.Print(&b)
	return b.String()
}

func wantClass(t *testing.T, r *hostcheck.Report, class hostcheck.Class) {
	t.Helper()
	if r.Class != class {
		t.Fatalf("class %s, want %s:\n%s", r.Class, class, printed(r))
	}
}

func wantLine(t *testing.T, r *hostcheck.Report, line string) {
	t.Helper()
	if !strings.Contains(printed(r), line) {
		t.Errorf("the report has no line %q:\n%s", line, printed(r))
	}
}

// An Ubuntu server with Docker installed and nothing else is clean: sshd,
// the stub resolver, chrony and the DHCP client are the base system, and
// Docker's own networks are Docker's.
func TestAnEmptyDockerInstallIsClean(t *testing.T) {
	r := check(t, "home-a", cleanHost())
	wantClass(t, r, hostcheck.Clean)
	if len(r.Foreign) != 0 || len(r.Conflicts) != 0 {
		t.Errorf("foreign %q, conflicts %v", r.Foreign, r.Conflicts)
	}
	if err := r.Refusal(); err != nil {
		t.Errorf("a clean host is refused: %v", err)
	}
	if r.Shared() {
		t.Error("a clean host reads as shared")
	}
	wantLine(t, r, "host check: home-a (ubuntu@host.example.org) is clean")
}

// A host already running this deployment is clean: the host network
// Postgres is ours through its cgroup, the published app port through
// docker-proxy and the container's binding, and the process-less WireGuard
// socket through the wg0.conf the manifest records.
func TestOurOwnDeploymentIsClean(t *testing.T) {
	var appPort int
	for _, l := range claims(t, "home-a").Listeners {
		if l.Key == "apps.blog" {
			appPort = l.Port
		}
	}
	h := cleanHost()
	h.files["/srv/.paisans-manifest.json"] = `{"version":1,"files":[{"path":"etc/wireguard/wg0.conf","sha256":"x","mode":"0600"}]}`
	h.answers["ip -o link"] += "4: wg0: <POINTOPOINT,NOARP,UP,LOWER_UP> mtu 1420 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000\\    link/none\n"
	h.answers["ip -j route"] = `[{"dst":"default","dev":"eth0"},{"dst":"10.44.0.0/24","dev":"wg0"},{"dst":"172.18.0.0/16","dev":"br-` + id("5")[:12] + `"}]`
	h.answers["docker inspect"] = fmt.Sprintf(`{"id":%q,"name":"/paisans-infra-patroni-1","pid":1500,"labels":{"com.docker.compose.project":"paisans-infra"},"ports":{}}
{"id":%q,"name":"/paisans-blog-app-1","pid":1700,"labels":{"com.docker.compose.project":"paisans-blog"},"ports":{"8080/tcp":[{"HostIp":"10.44.0.1","HostPort":"%d"}]}}
`, infraID, talkID, appPort)
	h.answers["docker network inspect"] = defaultNetworks + fmt.Sprintf(`{"id":%q,"name":"paisans-blog_default","labels":{"com.docker.compose.project":"paisans-blog"},"ipam":[{"Subnet":"172.18.0.0/16"}]}
`, id("5"))
	h.answers["docker volume inspect"] = `{"name":"` + id("9") + `","labels":{"com.docker.volume.anonymous":""}}
`
	h.answers["ss -Hltnup"] = baseSockets + fmt.Sprintf(`tcp LISTEN 0 4096 10.44.0.1:5432 0.0.0.0:* users:(("postgres",pid=1510,fd=5))
tcp LISTEN 0 4096 127.0.0.1:5432 0.0.0.0:* users:(("postgres",pid=1510,fd=6))
udp UNCONN 0 0 0.0.0.0:51820 0.0.0.0:*
tcp LISTEN 0 4096 10.44.0.1:%d 0.0.0.0:* users:(("docker-proxy",pid=1600,fd=4))
`, appPort)
	h.answers["/proc/"] = "1510 0::/system.slice/docker-" + infraID + ".scope \n1600 0::/system.slice/docker.service \n"
	r := check(t, "home-a", h)
	wantClass(t, r, hostcheck.Clean)
}

// Somebody's Caddy on 80 and 443 beside a site that claims neither makes the
// host shared, and with ufw up and denying by default that is allowed.
func TestAForeignCaddyBesideASiteThatDoesNotClaimItIsShared(t *testing.T) {
	r := check(t, "home-a", caddyHost())
	wantClass(t, r, hostcheck.Shared)
	if !r.Shared() {
		t.Error("Shared() is false")
	}
	for _, line := range []string{
		"foreign   container web-caddy-1 (compose project web)",
		"foreign   container shop-app-1 (compose project shop)",
		"foreign   *:80/tcp held by container web-caddy-1 (compose project web)",
		"foreign   203.0.113.10:8081/tcp held by container shop-app-1 (compose project shop)",
		"foreign   docker network shop_default (compose project shop)",
		"foreign   volume shop_data (compose project shop)",
		"shared    ",
	} {
		wantLine(t, r, line)
	}
	if err := r.Refusal(); err != nil {
		t.Errorf("a shared host with its firewall up is refused: %v", err)
	}
}

// The same Caddy against a gateway is a conflict on both ports, each line
// naming the claim, the key and the holder, and nothing proceeds.
func TestAForeignCaddyConflictsWithAGateway(t *testing.T) {
	r := check(t, "edge", caddyHost())
	wantClass(t, r, hostcheck.Conflicted)
	wantLine(t, r, "CONFLICT  *:80/tcp (Caddy): claimed by sites.edge.roles (gateway), held by container web-caddy-1 (compose project web)")
	wantLine(t, r, "CONFLICT  *:443/tcp (Caddy): claimed by sites.edge.roles (gateway), held by container web-caddy-1 (compose project web)")
	if len(r.Conflicts) != 2 {
		t.Errorf("conflicts %v, want 80 and 443 once each", r.Conflicts)
	}
	err := r.Refusal()
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("refusal %v", err)
	}
}

// A Postgres somebody runs on the host's 5432 conflicts with a data site,
// on the mesh address and on loopback, which the toolkit's binds both need.
func TestAForeignPostgresConflictsWithADataSite(t *testing.T) {
	h := cleanHost()
	h.answers["ss -Hltnup"] = baseSockets + `tcp LISTEN 0 244 0.0.0.0:5432 0.0.0.0:* users:(("postgres",pid=1200,fd=5))
`
	r := check(t, "home-a", h)
	wantClass(t, r, hostcheck.Conflicted)
	wantLine(t, r, "CONFLICT  10.44.0.1:5432/tcp (Postgres): claimed by sites.home-a.roles (data), held by process postgres (pid 1200)")
	wantLine(t, r, "CONFLICT  127.0.0.1:5432/tcp (Postgres): claimed by sites.home-a.roles (data), held by process postgres (pid 1200)")
}

// A loopback listener does not make a host shared, but one on an address
// and port a site binds still stops that bind.
func TestALoopbackListenerOnAClaimIsStillAConflict(t *testing.T) {
	h := cleanHost()
	h.answers["ss -Hltnup"] = baseSockets + `tcp LISTEN 0 244 127.0.0.1:5432 0.0.0.0:* users:(("postgres",pid=1200,fd=5))
`
	r := check(t, "home-a", h)
	wantClass(t, r, hostcheck.Conflicted)
	if len(r.Conflicts) != 1 || !strings.HasPrefix(r.Conflicts[0].Resource, "127.0.0.1:5432/tcp") {
		t.Errorf("conflicts %v, want the loopback bind alone", r.Conflicts)
	}
	if len(r.Foreign) != 0 {
		t.Errorf("a loopback listener was counted as foreign: %q", r.Foreign)
	}
}

// A port a foreign container publishes conflicts even with no listener:
// Docker without its userland proxy has none, and a stopped container binds
// again when it starts.
func TestAForeignPublishedPortConflictsWithoutAListener(t *testing.T) {
	h := cleanHost()
	h.answers["docker inspect"] = fmt.Sprintf(`{"id":%q,"name":"/proxy-nginx-1","pid":0,"labels":{"com.docker.compose.project":"proxy"},"ports":{"443/tcp":[{"HostIp":"","HostPort":"443"}]}}
`, shopID)
	r := check(t, "edge", h)
	wantClass(t, r, hostcheck.Conflicted)
	wantLine(t, r, "CONFLICT  *:443/tcp (Caddy): claimed by sites.edge.roles (gateway), held by container proxy-nginx-1 (compose project proxy)")
}

// A wg0 the toolkit's manifest does not record is somebody else's VPN, and
// so is the udp socket on 51820 that comes with it.
func TestAWireGuardTheToolkitDidNotWriteConflicts(t *testing.T) {
	h := cleanHost()
	h.answers["ip -o link"] += "4: wg0: <POINTOPOINT,NOARP,UP,LOWER_UP> mtu 1420\\    link/none\n"
	h.answers["ss -Hltnup"] = baseSockets + "udp UNCONN 0 0 0.0.0.0:51820 0.0.0.0:*\n"
	r := check(t, "home-a", h)
	wantClass(t, r, hostcheck.Conflicted)
	wantLine(t, r, "CONFLICT  interface wg0: claimed by mesh, held by an interface the toolkit did not write")
	wantLine(t, r, "CONFLICT  *:51820/udp (WireGuard): claimed by mesh, held by a kernel socket (no process)")
}

// A foreign Docker network or a route over the mesh subnet would capture
// mesh traffic, or have its own captured.
func TestSomethingOverlappingTheMeshConflicts(t *testing.T) {
	h := cleanHost()
	h.answers["docker network inspect"] = defaultNetworks + fmt.Sprintf(`{"id":%q,"name":"lab_default","labels":{"com.docker.compose.project":"lab"},"ipam":[{"Subnet":"10.44.0.0/16"}]}
`, id("6"))
	h.answers["ip -j route"] = `[{"dst":"default","dev":"eth0"},{"dst":"10.44.0.0/16","dev":"br-` + id("6")[:12] + `"},{"dst":"10.44.0.128/25","dev":"tun0"}]`
	r := check(t, "home-a", h)
	wantClass(t, r, hostcheck.Conflicted)
	wantLine(t, r, "CONFLICT  subnet 10.44.0.0/16: claimed by mesh.subnet, held by docker network lab_default (compose project lab)")
	wantLine(t, r, "CONFLICT  route 10.44.0.128/25 dev tun0: claimed by mesh.subnet, held by the host's routing table")
	if len(r.Conflicts) != 2 {
		t.Errorf("conflicts %v: the network's own bridge route is the network, reported once", r.Conflicts)
	}
}

// On a shared host the toolkit never sets the firewall's default or enables
// it, so the host must already have both, and firewalld must not be active.
func TestASharedHostNeedsTheFirewallUp(t *testing.T) {
	for _, tc := range []struct {
		name, ufw, firewalld, want string
	}{
		{"ufw inactive", ufwInactive, "inactive\n", "ufw is inactive"},
		{"default allow", "Status: active\nDefault: allow (incoming), allow (outgoing), disabled (routed)\n", "inactive\n", "ufw's default for incoming traffic is allow"},
		{"no ufw", "ufw absent\n", "inactive\n", "ufw is not installed"},
		{"firewalld", ufwActive, "active\n", "firewalld is active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := caddyHost()
			h.answers["ufw status verbose"] = tc.ufw
			h.answers["is-active firewalld"] = tc.firewalld
			r := check(t, "home-a", h)
			wantClass(t, r, hostcheck.Shared)
			err := r.Refusal()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %v, want one saying %q", err, tc.want)
			}
		})
	}
}

// A clean host's firewall is the toolkit's to set, as it always was.
func TestACleanHostNeedsNoFirewallYet(t *testing.T) {
	h := cleanHost()
	h.answers["ufw status verbose"] = "ufw absent\n"
	if err := check(t, "home-a", h).Refusal(); err != nil {
		t.Errorf("a clean host without ufw is refused: %v", err)
	}
}

// A route broader than the mesh, such as a provider's private network
// (10.0.0.0/8 via its gateway), is less specific than the route wg0 adds,
// so the mesh still wins and nothing is captured. It is noted, not refused.
// A route equal to the mesh or inside it would capture mesh traffic, and
// conflicts.
func TestABroaderRouteIsANoteNotAConflict(t *testing.T) {
	h := cleanHost()
	h.answers["ip -j route"] = `[{"dst":"default","dev":"eth0"},{"dst":"10.0.0.0/8","gateway":"10.0.0.1","dev":"enp7s0"}]`
	r := check(t, "home-a", h)
	wantClass(t, r, hostcheck.Clean)
	wantLine(t, r, "note      route 10.0.0.0/8 dev enp7s0 contains the mesh subnet 10.44.0.0/24")

	h.answers["ip -j route"] = `[{"dst":"10.44.0.0/24","dev":"tun0"}]`
	wantClass(t, check(t, "home-a", h), hostcheck.Conflicted)
}

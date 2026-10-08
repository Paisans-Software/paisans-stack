package hostcheck_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/hostcheck"
)

func inspect(t *testing.T, h *fakeHost) *hostcheck.Inventory {
	t.Helper()
	inv, err := hostcheck.Inspect(h)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// Every fact the spec lists is read, from one probe each.
func TestInspectReadsEveryFact(t *testing.T) {
	h := caddyHost()
	inv := inspect(t, h)
	if inv.Host != "ubuntu@host.example.org" {
		t.Errorf("host %q", inv.Host)
	}
	if !inv.Docker.Present || inv.Docker.Version != "27.3.1" || strings.Join(inv.Docker.Packages, ",") != "docker-ce" {
		t.Errorf("docker %+v", inv.Docker)
	}
	if len(inv.Containers) != 2 {
		t.Fatalf("containers %+v", inv.Containers)
	}
	if c := inv.Containers[0]; c.ID != caddyID || c.Name != "web-caddy-1" || c.Project != "web" || c.PID != 812 || len(c.Bindings) != 0 {
		t.Errorf("caddy %+v", c)
	}
	if b := inv.Containers[1].Bindings; len(b) != 1 || b[0].String() != "203.0.113.10:8081/tcp" || b[0].Owner != "shop-app-1" {
		t.Errorf("shop's bindings %+v", b)
	}
	if !reflect.DeepEqual(inv.Volumes, []hostcheck.Volume{{Name: "shop_data", Project: "shop"}}) {
		t.Errorf("volumes %+v", inv.Volumes)
	}
	if len(inv.Networks) != 4 || inv.Networks[3].Name != "shop_default" || inv.Networks[3].Project != "shop" || !reflect.DeepEqual(inv.Networks[3].Subnets, []string{"192.0.2.0/24"}) {
		t.Errorf("networks %+v", inv.Networks)
	}
	if inv.Cgroups[812] != caddyID || inv.Cgroups[1001] != "" {
		t.Errorf("cgroups %v", inv.Cgroups)
	}
	if !reflect.DeepEqual(inv.Links, []string{"lo", "eth0", "docker0"}) {
		t.Errorf("links %v", inv.Links)
	}
	if len(inv.Routes) != 3 || inv.Routes[2] != (hostcheck.Route{Dst: "172.17.0.0/16", Dev: "docker0"}) {
		t.Errorf("routes %+v", inv.Routes)
	}
	if inv.Firewall != (hostcheck.Firewall{UFW: true, Active: true, Incoming: "deny"}) {
		t.Errorf("firewall %+v", inv.Firewall)
	}
	if inv.Manifest || inv.ManifestWireGuard {
		t.Errorf("a host with no manifest read as having one")
	}
	found := false
	for _, s := range inv.Sockets {
		if s == (hostcheck.Socket{Proto: "tcp", Address: "", Port: 80, Process: "caddy", PID: 812}) {
			found = true
		}
	}
	if !found {
		t.Errorf("caddy's *:80 was not read: %+v", inv.Sockets)
	}
	for _, c := range h.ran {
		for _, change := range []string{" rm ", "ufw default", "ufw --force", "ufw enable", "systemctl start", "systemctl stop"} {
			if strings.Contains(c, change) {
				t.Errorf("the inventory changed something: %s", c)
			}
		}
	}
}

// ss prints every form of "any address" and an interface scope; each is
// read as render.Listener has it, with "" for any address.
func TestSocketsAreReadAsListeners(t *testing.T) {
	h := cleanHost()
	h.answers["ss -Hltnup"] = `tcp LISTEN 0 4096 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=701,fd=3))
tcp LISTEN 0 4096 [::]:5432 [::]:* users:(("postgres",pid=1200,fd=5))
tcp LISTEN 0 4096 *:80 *:* users:(("caddy",pid=812,fd=7))
udp UNCONN 0 0 127.0.0.53%lo:53 0.0.0.0:* users:(("systemd-resolve",pid=402,fd=14))
udp UNCONN 0 0 0.0.0.0:51820 0.0.0.0:*
tcp LISTEN 0 4096 [::1]:2379 [::]:* users:(("etcd",pid=600,fd=7))
`
	var got []string
	for _, s := range inspect(t, h).Sockets {
		got = append(got, s.Listener().String()+" "+s.Process)
	}
	want := []string{"*:22/tcp sshd", "*:5432/tcp postgres", "*:80/tcp caddy", "127.0.0.53:53/udp systemd-resolve", "*:51820/udp ", "::1:2379/tcp etcd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// The manifest is how a wg0 the toolkit wrote is told from one it did not.
func TestTheManifestSaysWhetherWireGuardIsOurs(t *testing.T) {
	h := cleanHost()
	h.files["/srv/.paisans-manifest.json"] = `{"version":1,"files":[{"path":"etc/wireguard/wg0.conf","sha256":"x","mode":"0600"}]}`
	inv := inspect(t, h)
	if !inv.Manifest || !inv.ManifestWireGuard {
		t.Errorf("manifest %v, wireguard %v", inv.Manifest, inv.ManifestWireGuard)
	}
	h.files["/srv/.paisans-manifest.json"] = `{"version":1,"files":[{"path":"srv/infra/compose.yaml","sha256":"x","mode":"0644"}]}`
	if inv := inspect(t, h); !inv.Manifest || inv.ManifestWireGuard {
		t.Errorf("manifest %v, wireguard %v", inv.Manifest, inv.ManifestWireGuard)
	}
}

// Where Docker came from is reported: Docker's own package, Ubuntu's, or
// the snap.
func TestDockersPackageIsRead(t *testing.T) {
	h := cleanHost()
	h.answers["dpkg-query"] = "docker.io install ok installed\nName    Version  Rev   Tracking       Publisher   Notes\ndocker  27.2.0   2963  latest/stable  canonical✓  -\n"
	if got := strings.Join(inspect(t, h).Docker.Packages, ","); got != "docker.io,snap" {
		t.Errorf("packages %q", got)
	}
}

// A host without Docker has no containers to look for, and is not asked.
func TestNoDockerSkipsTheContainerProbes(t *testing.T) {
	h := cleanHost()
	h.answers["docker version"] = "absent\n"
	if inv := inspect(t, h); inv.Docker.Present {
		t.Error("docker read as present")
	}
	for _, c := range h.ran {
		if strings.Contains(c, "xargs") {
			t.Errorf("probed a Docker that is not there: %s", c)
		}
	}
}

// Docker installed and not answering is "could not look", which is not
// "looked and found nothing".
func TestADockerThatDoesNotAnswerIsAnError(t *testing.T) {
	h := cleanHost()
	h.fail = map[string]error{"docker version": errors.New("exit status 1")}
	if _, err := hostcheck.Inspect(h); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("got %v", err)
	}
}

// Any probe that fails stops the inventory with what it was reading.
func TestAFailedProbeIsAnError(t *testing.T) {
	for _, probe := range []string{"ss -Hltnup", "ip -j route", "ufw status verbose", "docker inspect"} {
		h := cleanHost()
		h.fail = map[string]error{probe: errors.New("exit status 1")}
		if _, err := hostcheck.Inspect(h); err == nil {
			t.Errorf("%s failed and the inventory went on", probe)
		}
	}
}

// ufw absent, and firewalld active, are both read.
func TestTheFirewallIsRead(t *testing.T) {
	h := cleanHost()
	h.answers["ufw status verbose"] = "ufw absent\n"
	h.answers["is-active firewalld"] = "active\n"
	if f := inspect(t, h).Firewall; f != (hostcheck.Firewall{Firewalld: true}) {
		t.Errorf("firewall %+v", f)
	}
}

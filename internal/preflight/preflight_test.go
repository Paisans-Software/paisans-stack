package preflight

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostprep"
	"github.com/paisans-software/paisans-stack/internal/registry"
)

// rule answers any command containing match. The first matching rule wins,
// so a test overrides a default by putting its rule first.
type rule struct {
	match string
	out   string
	err   error
}

// fakeHost is a machine that answers from rules and records what it ran.
type fakeHost struct {
	name  string
	rules []rule
	files map[string]string
	ran   []string
}

func (h *fakeHost) Describe() string { return "ubuntu@" + h.name }

func (h *fakeHost) Run(command string) (string, error) {
	h.ran = append(h.ran, command)
	for _, r := range h.rules {
		if strings.Contains(command, r.match) {
			return r.out, r.err
		}
	}
	return "fake: no rule for " + command, errors.New("exit status 127")
}

func (h *fakeHost) RunInput(command, _ string) (string, error) { return h.Run(command) }

func (h *fakeHost) ReadFile(path string) (string, bool, error) {
	content, ok := h.files[path]
	return content, ok, nil
}

func (h *fakeHost) WriteFile(string, string, uint32) error {
	return errors.New("preflight must not write")
}

// override puts rules ahead of the defaults.
func (h *fakeHost) override(rules ...rule) { h.rules = append(rules, h.rules...) }

const ubuntu = "ID=ubuntu\nVERSION_ID=\"24.04\"\n"

const cluster = `{"members":[{"name":"home-a","role":"leader","state":"running"}]}`

// healthy returns a host whose every probe passes.
func healthy(name string) *fakeHost {
	return &fakeHost{
		name:  name,
		files: map[string]string{"/etc/os-release": ubuntu},
		rules: []rule{
			{match: "sudo -n true"},
			{match: "timedatectl", out: "yes\n"},
			{match: "ldd --version", out: "ldd (Ubuntu GLIBC 2.39-0ubuntu8.3) 2.39\nCopyright\n"},
			{match: "wireguard"},
			{match: "/dev/watchdog"},
			{match: "ss -Hltnu", out: "tcp LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*\nudp UNCONN 0 0 127.0.0.53%lo:53 0.0.0.0:*\n"},
			{match: "ip -j route", out: `[{"dst":"default","dev":"eth0"},{"dst":"203.0.113.0/24","dev":"eth0"},{"dst":"172.17.0.0/16","dev":"docker0"}]`},
			{match: "/cluster", out: cluster},
			{match: "pg_database_size", out: fmt.Sprintf("%d\n", int64(1)<<30)},
			{match: "df -B1", out: "Avail\n" + fmt.Sprint(int64(40)<<30) + "\n"},
			{match: "/dev/tcp", out: "11800\n12000\n12500\n"},
			{match: "docker info", out: "/var/lib/docker\n"},
			{match: "findmnt", out: "ext4 /dev/sda1\n"},
			// The host check's inventory: Docker and nothing run on it.
			{match: "id -u", out: "0\n"},
			{match: "docker version", out: "27.3.1\n"},
			{match: "dpkg-query", out: "docker-ce install ok installed\n"},
			{match: "docker inspect", out: ""},
			{match: "docker volume inspect", out: ""},
			{match: "docker network inspect", out: ""},
			{match: "ip -o link", out: "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536\n2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500\n"},
			{match: "ufw status verbose", out: "Status: active\nDefault: deny (incoming), allow (outgoing), disabled (routed)\n"},
			{match: "is-active firewalld", out: "inactive\n"},
			{match: "/proc/", out: ""},
		},
	}
}

func fixture(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("testdata/site-add.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func hosts() map[string]*fakeHost {
	return map[string]*fakeHost{"home-a": healthy("home-a"), "home-b": healthy("home-b"), "vm": healthy("vm")}
}

func transportsOf(h map[string]*fakeHost) map[string]apply.Transport {
	transports := map[string]apply.Transport{}
	for name, host := range h {
		transports[name] = host
	}
	return transports
}

func run(t *testing.T, cfg *config.Config, h map[string]*fakeHost) Report {
	t.Helper()
	report, err := Run(cfg, "home-b", transportsOf(h))
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// prepared stubs host prepare's plan, which hostprep's own tests cover.
func prepared(t *testing.T, steps ...string) {
	old := buildHostPrep
	buildHostPrep = func(site string, _ *config.Config, _ hostprep.Transport, _ ...hostprep.Option) (*hostprep.Plan, error) {
		plan := &hostprep.Plan{Site: site, Profile: "ubuntu 24.04"}
		for _, s := range steps {
			plan.Steps = append(plan.Steps, hostprep.Step{Describe: s})
		}
		return plan, nil
	}
	t.Cleanup(func() { buildHostPrep = old })
}

func find(r Report, site, name string) []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Site == site && c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

func refusedDetail(t *testing.T, r Report, site, name string) string {
	t.Helper()
	for _, c := range find(r, site, name) {
		if c.Refused {
			return c.Detail
		}
	}
	t.Fatalf("%s %s was not refused:\n%s", site, name, printed(r))
	return ""
}

func printed(r Report) string {
	var b strings.Builder
	r.Print(&b)
	return b.String()
}

func TestAHealthyJoinPassesEveryCheck(t *testing.T) {
	prepared(t)
	h := hosts()
	r := run(t, fixture(t), h)
	if r.Refused() {
		t.Fatalf("refused:\n%s", printed(r))
	}
	for _, c := range r.Checks {
		if c.Warned {
			t.Errorf("unexpected warning: %+v", c)
		}
	}
	for _, name := range []string{"ssh", "clock"} {
		for _, site := range []string{"home-a", "home-b", "vm"} {
			if len(find(r, site, name)) != 1 {
				t.Errorf("%s not checked on %s", name, site)
			}
		}
	}
	for _, name := range []string{"host", "prepared", "platform", "wireguard", "watchdog", "ports", "routes", "disk", "storage"} {
		if len(find(r, "home-b", name)) == 0 {
			t.Errorf("%s not checked on the new site", name)
		}
	}
	if got := len(find(r, "home-b", "rtt")); got != 2 {
		t.Errorf("rtt measured to %d sites, want home-a and vm", got)
	}
	// The new site is data only, so it runs no HAProxy and 5000 is not asked
	// for; every data and etcd port is.
	ports := find(r, "home-b", "ports")[0].Detail
	for _, p := range []string{"51820/udp", "2379/tcp", "2380/tcp", "5432/tcp", "8008/tcp", "8009/tcp"} {
		if !strings.Contains(ports, p) {
			t.Errorf("port %s not checked: %s", p, ports)
		}
	}
	if strings.Contains(ports, "5000") {
		t.Errorf("cluster port checked on a site without HAProxy: %s", ports)
	}
	// The RTT goes to the public address on the site's own ssh port.
	var probed bool
	for _, c := range h["home-b"].ran {
		if strings.Contains(c, "/dev/tcp") && strings.Contains(c, "'203.0.113.30' 2222") {
			probed = true
		}
	}
	if !probed {
		t.Errorf("vm was not probed at 203.0.113.30:2222: %q", h["home-b"].ran)
	}
	// The leader's size is read on the leader.
	var sized bool
	for _, c := range h["home-a"].ran {
		sized = sized || strings.Contains(c, "pg_database_size")
	}
	if !sized {
		t.Errorf("the leader's database size was not read on home-a")
	}
}

func TestPreflightRefuses(t *testing.T) {
	cases := []struct {
		name  string
		site  string
		check string
		want  string
		setup func(h map[string]*fakeHost)
		steps []string
	}{
		{name: "ssh unreachable", site: "vm", check: "ssh", want: "does not answer",
			setup: func(h map[string]*fakeHost) {
				h["vm"].override(rule{match: "sudo -n true", out: "ssh: connect to host port 2222: Connection refused", err: errors.New("exit status 255")})
			}},
		{name: "sudo wants a password", site: "home-b", check: "ssh", want: "asks for a password",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "sudo -n true", out: "sudo: a password is required\n", err: errors.New("exit status 1")})
			}},
		{name: "sudo cannot be asked", site: "home-b", check: "ssh", want: "no terminal to ask on",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "sudo -n true", err: fmt.Errorf("home-b: sudo asks for a password, and there is no terminal to ask on: %w", apply.ErrSudo)})
			}},
		{name: "clock", site: "vm", check: "clock", want: "not synchronised",
			setup: func(h map[string]*fakeHost) { h["vm"].override(rule{match: "timedatectl", out: "no\n"}) }},
		{name: "host prepare owes steps", site: "home-b", check: "prepared", want: "install Docker", steps: []string{"install Docker"}},
		{name: "glibc differs", site: "home-b", check: "platform", want: "glibc 2.41",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "ldd --version", out: "ldd (GNU libc) 2.41\n"})
			}},
		{name: "os differs", site: "home-b", check: "platform", want: "ubuntu 22.04",
			setup: func(h map[string]*fakeHost) { h["home-a"].files["/etc/os-release"] = "ID=ubuntu\nVERSION_ID=22.04\n" }},
		{name: "no wireguard", site: "home-b", check: "wireguard", want: "no WireGuard",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "wireguard", out: "modprobe: FATAL: Module wireguard not found", err: errors.New("exit status 1")})
			}},
		{name: "no watchdog", site: "home-b", check: "watchdog", want: "no /dev/watchdog",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "/dev/watchdog", err: errors.New("exit status 1")})
			}},
		{name: "port taken", site: "home-b", check: "ports", want: "5432/tcp (Postgres)",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "ss -Hltnu", out: "tcp LISTEN 0 244 [::]:5432 [::]:*\n"})
			}},
		{name: "route collides", site: "home-b", check: "routes", want: "10.44.0.0/16 dev br-lan",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "ip -j route", out: `[{"dst":"10.44.0.0/16","dev":"br-lan"}]`})
			}},
		{name: "disk short", site: "home-b", check: "disk", want: "needs 3.0 GiB",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "df -B1", out: "Avail\n1073741824\n"})
			}},
		{name: "postgres on nfs", site: "home-b", check: "storage", want: "/srv/paisans/f2a9 on nfs4 (nas:/export/srv) is network attached",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "d='/srv/paisans/f2a9'", out: "nfs4 nas:/export/srv\n"})
			}},
		{name: "docker root on cifs", site: "home-b", check: "storage", want: "/mnt/share/docker on cifs",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(
					rule{match: "docker info", out: "/mnt/share/docker\n"},
					rule{match: "d='/mnt/share/docker'", out: "cifs //nas/share\n"})
			}},
		{name: "no docker", site: "home-b", check: "storage", want: "Docker's root directory",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "docker info", out: "Cannot connect to the Docker daemon", err: errors.New("exit status 1")})
			}},
		{name: "no findmnt answer", site: "home-b", check: "storage", want: "could not read the filesystem under /var/lib/docker",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "findmnt", out: "findmnt: not found", err: errors.New("exit status 127")})
			}},
		{name: "no leader", site: "home-b", check: "disk", want: "no running leader",
			setup: func(h map[string]*fakeHost) {
				h["home-a"].override(rule{match: "/cluster", out: `{"members":[{"name":"home-a","role":"replica","state":"starting"}]}`})
			}},
		{name: "unreachable peer", site: "home-b", check: "rtt", want: "no TCP connection",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "203.0.113.10", out: "bash: connect: Connection timed out", err: errors.New("exit status 1")})
			}},
		{name: "election timeout under five round trips", site: "home-b", check: "rtt", want: "at least 1250",
			setup: func(h map[string]*fakeHost) {
				h["home-b"].override(rule{match: "203.0.113.10", out: "250000\n250000\n250000\n"})
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prepared(t, tc.steps...)
			h := hosts()
			if tc.setup != nil {
				tc.setup(h)
			}
			r := run(t, fixture(t), h)
			if !r.Refused() {
				t.Fatalf("not refused:\n%s", printed(r))
			}
			if got := refusedDetail(t, r, tc.site, tc.check); !strings.Contains(got, tc.want) {
				t.Errorf("detail %q does not mention %q", got, tc.want)
			}
		})
	}
}

// A filesystem in neither list is warned about, not refused: it is not known
// to be wrong. The probe walks up from a path that does not exist yet.
func TestUnknownFilesystemWarns(t *testing.T) {
	prepared(t)
	h := hosts()
	h["home-b"].override(rule{match: "findmnt", out: "bcachefs /dev/nvme0n1p2\n"})
	r := run(t, fixture(t), h)
	if r.Refused() {
		t.Fatalf("refused:\n%s", printed(r))
	}
	checks := find(r, "home-b", "storage")
	if len(checks) != 1 || !checks[0].Warned || !strings.Contains(checks[0].Detail, "bcachefs") {
		t.Fatalf("got %+v, want one warning naming bcachefs", checks)
	}
	var walked bool
	for _, c := range h["home-b"].ran {
		walked = walked || (strings.Contains(c, "d='/srv/paisans/f2a9'") && strings.Contains(c, "dirname") && strings.Contains(c, "findmnt -no FSTYPE,SOURCE --target"))
	}
	if !walked {
		t.Errorf("the deployment root was not probed by walking up to a directory that exists: %q", h["home-b"].ran)
	}
}

func TestRTTWarnsWithoutRefusing(t *testing.T) {
	cases := map[string]string{
		// 150 ms: heartbeat 100 is under one round trip, election 1000 is
		// over five (750) and under ten (1500).
		"150 ms": "150000\n150000\n150000\n",
		// 120 ms: the heartbeat is under it; the election timeout is under
		// ten round trips.
		"120 ms": "120000\n120000\n120000\n",
	}
	for name, samples := range cases {
		t.Run(name, func(t *testing.T) {
			prepared(t)
			h := hosts()
			h["home-b"].override(rule{match: "203.0.113.10", out: samples})
			r := run(t, fixture(t), h)
			if r.Refused() {
				t.Fatalf("refused:\n%s", printed(r))
			}
			var warned bool
			for _, c := range find(r, "home-b", "rtt") {
				warned = warned || c.Warned
			}
			if !warned {
				t.Fatalf("no rtt warning:\n%s", printed(r))
			}
		})
	}
}

func TestHeartbeatUnderOneRoundTripWarns(t *testing.T) {
	prepared(t)
	cfg := fixture(t)
	cfg.Etcd.ElectionTimeoutMS = 5000
	h := hosts()
	h["home-b"].override(rule{match: "203.0.113.10", out: "130000\n130000\n130000\n"})
	r := run(t, cfg, h)
	if r.Refused() {
		t.Fatalf("refused:\n%s", printed(r))
	}
	var detail string
	for _, c := range find(r, "home-b", "rtt") {
		if c.Warned {
			detail = c.Detail
		}
	}
	if !strings.Contains(detail, "heartbeat is under one round trip") {
		t.Fatalf("want a heartbeat warning, got %q", detail)
	}
}

// A route through wg0 is the mesh itself, which a re-run after stage 2 will
// find on the new host.
func TestTheMeshsOwnRouteIsNotACollision(t *testing.T) {
	prepared(t)
	h := hosts()
	h["home-b"].override(rule{match: "ip -j route", out: `[{"dst":"10.44.0.0/24","dev":"wg0"}]`})
	if r := run(t, fixture(t), h); r.Refused() {
		t.Fatalf("refused:\n%s", printed(r))
	}
}

func TestWatchdogOffNeedsNoDevice(t *testing.T) {
	prepared(t)
	cfg := fixture(t)
	site := cfg.Sites["home-b"]
	site.Watchdog = config.WatchdogOff
	cfg.Sites["home-b"] = site
	h := hosts()
	h["home-b"].override(rule{match: "/dev/watchdog", err: errors.New("exit status 1")})
	if r := run(t, cfg, h); r.Refused() {
		t.Fatalf("refused:\n%s", printed(r))
	}
}

// An unreachable site is refused once, at ssh, and nothing else is tried on
// it; the rest of the report still arrives.
func TestAnUnreachableNewSiteIsCheckedNoFurther(t *testing.T) {
	prepared(t)
	h := hosts()
	h["home-b"].override(rule{match: "sudo -n true", out: "ssh: Could not resolve hostname", err: errors.New("exit status 255")})
	r := run(t, fixture(t), h)
	if len(h["home-b"].ran) != 1 {
		t.Errorf("ran more than the ssh check on an unreachable host: %q", h["home-b"].ran)
	}
	if len(find(r, "home-a", "clock")) != 1 {
		t.Errorf("the other sites were not still checked:\n%s", printed(r))
	}
}

func TestMissingTransportIsRefused(t *testing.T) {
	prepared(t)
	h := hosts()
	delete(h, "vm")
	r := run(t, fixture(t), h)
	if !strings.Contains(refusedDetail(t, r, "vm", "ssh"), "no way to reach") {
		t.Fatal("a site with no transport was not refused")
	}
}

func TestUndeclaredSiteIsAnError(t *testing.T) {
	if _, err := Run(fixture(t), "home-c", nil); err == nil {
		t.Fatal("no error for an undeclared site")
	}
}

func TestMedianAndGlibc(t *testing.T) {
	ms, samples, err := medianMS("12500\n11800\n12000\n")
	if err != nil || ms != 12.0 || strings.Join(samples, ",") != "11.8,12.0,12.5" {
		t.Fatalf("median = %v %v %v", ms, samples, err)
	}
	if got := glibcRelease("2.39"); got != "2.39" {
		t.Errorf("glibcRelease(2.39) = %q", got)
	}
	if got := glibcRelease("2.35.1"); got != "2.35" {
		t.Errorf("glibcRelease(2.35.1) = %q", got)
	}
}

// A gateway being added is checked for 80 and 443, which come from the host
// check's claims like every other port. Something foreign on one is also a
// host check conflict, naming what holds it.
func TestAGatewaysWebPortsAreChecked(t *testing.T) {
	prepared(t)
	h := hosts()
	h["vm"].override(rule{match: "ss -Hltnu", out: "tcp LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:((\"nginx\",pid=900,fd=6))\n"})
	r, err := Run(fixture(t), "vm", transportsOf(h))
	if err != nil {
		t.Fatal(err)
	}
	if got := refusedDetail(t, r, "vm", "ports"); !strings.Contains(got, "80/tcp (Caddy)") {
		t.Errorf("ports detail %q", got)
	}
	if got := refusedDetail(t, r, "vm", "host"); !strings.Contains(got, "process nginx (pid 900)") {
		t.Errorf("host detail %q", got)
	}
}

// A shared host is prepared without the firewall's defaults, so preflight
// asks host prepare for that plan rather than one it would never run.
func TestASharedHostIsPreparedAsShared(t *testing.T) {
	var shared bool
	old := buildHostPrep
	buildHostPrep = func(site string, _ *config.Config, _ hostprep.Transport, opts ...hostprep.Option) (*hostprep.Plan, error) {
		shared = len(opts) > 0
		return &hostprep.Plan{Site: site, Profile: "ubuntu 24.04"}, nil
	}
	t.Cleanup(func() { buildHostPrep = old })
	h := hosts()
	h["home-b"].override(rule{match: "docker inspect", out: `{"id":"` + strings.Repeat("d", 64) + `","name":"/shop-app-1","pid":0,"labels":{"com.docker.compose.project":"shop"},"ports":{}}` + "\n"})
	r := run(t, fixture(t), h)
	if !shared {
		t.Error("host prepare was planned as for a dedicated host")
	}
	if c := find(r, "home-b", "host"); len(c) != 1 || c[0].Refused || !strings.Contains(c[0].Detail, "shared") {
		t.Errorf("host check %+v", c)
	}
}

// A host another deployment with the same token has claimed is refused, by
// reading its registry; preflight never claims.
func TestARegistryConflictIsRefused(t *testing.T) {
	prepared(t)
	h := hosts()
	if h["home-b"].files == nil {
		h["home-b"].files = map[string]string{}
	}
	h["home-b"].files[registry.Path] = `{"version":1,"deployments":{
"f2a91111-2222-4333-8444-555566667777":{"token":"f2a9","root":"/srv/paisans/f2a9","domain":"example.net","site":"vm","claimed_at":"2026-10-01T00:00:00Z"}
}}
`
	r := run(t, fixture(t), h)
	got := refusedDetail(t, r, "home-b", "registry")
	if !strings.Contains(got, "f2a91111-2222-4333-8444-555566667777") || !strings.Contains(got, "example.net") {
		t.Errorf("the refusal does not name the other deployment: %q", got)
	}
	for _, c := range h["home-b"].ran {
		if strings.Contains(c, "flock") {
			t.Errorf("preflight claimed the host: %s", c)
		}
	}
}

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/doctor"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/mesh"
	"github.com/paisans-software/paisans-stack/internal/patroni"
)

// doctorHost answers doctor's read commands from a table, records every
// command it is sent, and refuses every write.
type doctorHost struct {
	name    string
	down    bool
	answers map[string]string
	sent    *[]string
	writes  *[]string
}

func (h doctorHost) Describe() string { return "ubuntu@" + h.name }
func (h doctorHost) Run(command string) (string, error) {
	*h.sent = append(*h.sent, h.name+": "+command)
	if h.down {
		return "ssh: connect to host " + h.name + " port 22: Operation timed out\n",
			fmt.Errorf("%w after 3 attempts: exit status 255", apply.ErrUnreachable)
	}
	for prefix, out := range h.answers {
		if strings.HasPrefix(command, prefix) {
			return out, nil
		}
	}
	return "", fmt.Errorf("unexpected command %q", command)
}
func (h doctorHost) RunInput(command, _ string) (string, error) {
	*h.writes = append(*h.writes, "RunInput "+command)
	return "", errors.New("doctor sends no stdin")
}
func (h doctorHost) ReadFile(path string) (string, bool, error) {
	*h.sent = append(*h.sent, h.name+": read "+path)
	return "", false, nil
}
func (h doctorHost) WriteFile(path string, _ string, _ uint32) error {
	*h.writes = append(*h.writes, "WriteFile "+path)
	return errors.New("doctor writes nothing")
}

// readOnly is every command doctor may send, exactly or by prefix. Anything
// else, and above all a patronictl failover, fails the test.
func readOnly(cfg *config.Config) (exact []string, prefixes []string) {
	exact = []string{
		doctor.ReachCommand,
		doctor.ClockCommand,
		doctor.EtcdHealthCommand(cfg),
		doctor.EtcdVersionCommand,
		doctor.SyncCommand(cfg.Deployment()),
		doctor.PatroniLogCommand(cfg.Deployment()),
		doctor.ContainersCommand(cfg.Deployment()),
		mesh.LinkCommand(cfg.Deployment().Interface()),
		mesh.RouteCommand,
		mesh.AddrCommand,
		mesh.NetworksCommand,
		"read " + mesh.DaemonConfig,
		patroni.ClusterCommand(cfg.Deployment(), "10.44.0.1:8008"),
		patroni.ClusterCommand(cfg.Deployment(), "10.44.0.2:8008"),
	}
	prefixes = []string{
		"docker inspect --format '{\"State\":{{json .State}},\"RestartCount\":{{.RestartCount}}}' ",
		"docker logs --tail 20 ",
		"if [ ! -f '/srv/paisans/f2a9/auth/compose.yaml' ]; then echo absent; ",
		hostcheck.CgroupPrefix,
	}
	exact = append(exact, hostcheck.ReadCommands()...)
	exact = append(exact, "read "+cfg.Deployment().Manifest())
	return exact, prefixes
}

func withDoctorHosts(t *testing.T, hosts map[string]doctorHost) {
	t.Helper()
	saved, savedNow := doctorTransport, doctorNow
	doctorTransport = func(tr apply.SSHTransport) apply.Transport {
		h, ok := hosts[tr.Host]
		if !ok {
			t.Fatalf("doctor reached an unexpected host %q", tr.Host)
		}
		return h
	}
	doctorNow = func() time.Time { return time.Unix(1_760_000_000, 0) }
	t.Cleanup(func() { doctorTransport, doctorNow = saved, savedNow })
}

// The stuck case, end to end: home-b led and is switched off, home-a is a
// replica /sync no longer names. doctor says so, puts starting home-b first
// and the forced failover second, exits non zero, and every command any host
// received is a read.
func TestDoctorDiagnosesTheStuckReplicaAndOnlyReads(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	var sent, writes []string
	inspect := "docker inspect "
	homeA := map[string]string{
		doctor.ReachCommand:                                        "",
		doctor.ClockCommand:                                        "1760000000.100000000\n",
		doctor.EtcdHealthCommand(cfg):                              `[{"endpoint":"http://10.44.0.1:2379","health":true,"took":"4ms"},{"endpoint":"http://10.44.0.2:2379","health":false,"took":"5s","error":"context deadline exceeded"},{"endpoint":"http://10.44.0.3:2379","health":true,"took":"4ms"}]`,
		doctor.EtcdVersionCommand:                                  `{"etcdserver":"3.5.16","etcdcluster":"3.5.0"}`,
		doctor.SyncCommand(cfg.Deployment()):                       `{"leader":"home-b","quorum":0,"sync_standby":null}` + "\n",
		patroni.ClusterCommand(cfg.Deployment(), "10.44.0.1:8008"): `{"members":[{"name":"home-a","role":"replica","state":"running","timeline":2,"lag":1048576}],"scope":"paisans"}`,
		doctor.PatroniLogCommand(cfg.Deployment()):                 "patroni-1  | INFO: following a different leader because i am not the healthiest node\n",
		doctor.ContainersCommand(cfg.Deployment()):                 `{"Names":"paisans-f2a9-infra-patroni-1","State":"running","Status":"Up 5 minutes","Labels":"community.paisans.deployment=f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}` + "\n" + `{"Names":"paisans-f2a9-talk-app-1","State":"restarting","Status":"Restarting (1) 3 seconds ago","Labels":"community.paisans.deployment=f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01,com.docker.compose.project=paisans-f2a9-talk"}`,
		inspect:        `{"State":{"Status":"restarting","Restarting":true,"ExitCode":1,"Error":""},"RestartCount":9}`,
		"docker logs ": "booting\nSQLSTATE[08006] [7] connection to server at \"127.0.0.1\", port 5000 failed: Connection refused\n",
		"if [ ! -f '/srv/paisans/f2a9/auth/compose.yaml' ]": "standby\n",
		mesh.LinkCommand("psns-f2a9"):                       `[{"ifname":"psns-f2a9","flags":["POINTOPOINT","NOARP","UP","LOWER_UP"]}]`,
		mesh.RouteCommand:                                   `[{"dst":"10.44.0.0/24","dev":"psns-f2a9"}]`,
		mesh.AddrCommand:                                    `[{"ifname":"psns-f2a9","addr_info":[{"family":"inet","local":"10.44.0.1","prefixlen":24}]}]`,
		mesh.NetworksCommand:                                "[]",
	}
	vm := map[string]string{
		mesh.LinkCommand("psns-f2a9"):              `[{"ifname":"psns-f2a9","flags":["POINTOPOINT","NOARP","UP","LOWER_UP"]}]`,
		mesh.RouteCommand:                          `[{"dst":"10.44.0.0/24","dev":"psns-f2a9"},{"dst":"10.44.0.0/16","dev":"wg9"}]`,
		mesh.AddrCommand:                           "[]",
		mesh.NetworksCommand:                       "[]",
		doctor.ReachCommand:                        "",
		doctor.ClockCommand:                        "1760000000.000000000\n",
		doctor.ContainersCommand(cfg.Deployment()): `{"Names":"paisans-f2a9-infra-etcd-1","State":"running","Status":"Up 2 days","Labels":"community.paisans.deployment=f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}`,
	}
	watch := map[string]string{
		doctor.ReachCommand:                        "",
		doctor.ClockCommand:                        "1760000000.000000000\n",
		doctor.ContainersCommand(cfg.Deployment()): `{"Names":"paisans-f2a9-status-app-1","State":"running","Status":"Up 2 days","Labels":"community.paisans.deployment=f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}`,
	}
	withDoctorHosts(t, map[string]doctorHost{
		"home-a.local":      {name: "home-a.local", answers: homeA, sent: &sent, writes: &writes},
		"home-b.local":      {name: "home-b.local", down: true, sent: &sent, writes: &writes},
		"vm.example.org":    {name: "vm.example.org", answers: vm, sent: &sent, writes: &writes},
		"watch.example.org": {name: "watch.example.org", answers: watch, sent: &sent, writes: &writes},
	})

	rec := withRecorder(t, true)
	runErr := runDoctor([]string{"--config", fixtureConfig(), "--sudo=false"})
	out := rec.Lines()
	if runErr == nil || !strings.Contains(runErr.Error(), "marked FAIL") {
		t.Fatalf("doctor should fail on a cluster with no primary, got %v\n%s", runErr, out)
	}

	for _, want := range []string{
		"detail: 4 site(s), 3 reached",
		"done: check mesh home-a psns-f2a9 is up",
		"done: check mesh home-a nothing on the host overlaps the mesh subnet 10.44.0.0/24",
		"refuse: check mesh vm: the mesh subnet 10.44.0.0/24 overlaps route 10.44.0.0/16 dev wg9",
		"refuse: check reach home-b: ssh to ubuntu@home-b.local did not answer (ssh: connect to host home-b.local port 22: Operation timed out)",
		"warn: check etcd quorum: 2 of 3 members healthy (needs 2), asked from home-a",
		"refuse: check patroni: no primary: home-a is a replica and will not promote while home-b, which led, is gone",
		"refuse: check containers home-a: paisans-f2a9-talk-app-1 restarting (exit 1, restarted 9 time(s))",
		"It cannot reach its database",
		"refuse: check pocket-id auth: no active instance, so sign in is down",
		"home-b   unreachable did not answer ssh (see reach)",
		"There is no database primary (see patroni)",
		"doctor changes nothing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	start := strings.Index(out, "1. Start home-b (ssh ubuntu@home-b.local)")
	force := strings.Index(out, "2. Only if home-b is lost for good")
	command := strings.Index(out, "patronictl -c /home/postgres/postgres.yml failover --candidate home-a --force")
	warning := strings.Index(out, "WARNING: every commit home-b made")
	if start < 0 || !(start < force && force < command && command < warning) {
		t.Errorf("want start home-b, the forced failover, its command, then its warning, in that order:\n%s", out)
	}

	exact, prefixes := readOnly(cfg)
	for _, s := range sent {
		command := s[strings.Index(s, ": ")+2:]
		allowed := false
		for _, e := range exact {
			allowed = allowed || command == e
		}
		for _, p := range prefixes {
			allowed = allowed || strings.HasPrefix(command, p)
		}
		if !allowed || strings.Contains(command, "patronictl") {
			t.Errorf("doctor sent a command outside its read allow list: %s", s)
		}
	}
	if len(writes) > 0 {
		t.Errorf("doctor wrote: %v", writes)
	}
	// home-b was asked once, and then left alone.
	var toB []string
	for _, s := range sent {
		if strings.HasPrefix(s, "home-b.local: ") {
			toB = append(toB, s)
		}
	}
	if len(toB) != 1 {
		t.Errorf("an unreachable site is asked once, got %v", toB)
	}
}

func TestDoctorRefusesAnUndeclaredSite(t *testing.T) {
	err := runDoctor([]string{"--config", fixtureConfig(), "--sudo=false", "--site", "nowhere"})
	if err == nil || !strings.Contains(err.Error(), `declares no site "nowhere"`) {
		t.Fatalf("err = %v", err)
	}
}

// --site narrows every check to the named sites, and the ones left out are
// never contacted.
func TestDoctorSiteNarrowsTheHostsReached(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	var sent, writes []string
	withDoctorHosts(t, map[string]doctorHost{
		"vm.example.org": {name: "vm.example.org", sent: &sent, writes: &writes, answers: map[string]string{
			doctor.ReachCommand:                        "",
			doctor.ClockCommand:                        "1760000000.000000000\n",
			doctor.ContainersCommand(cfg.Deployment()): `{"Names":"paisans-f2a9-infra-etcd-1","State":"running","Status":"Up","Labels":"community.paisans.deployment=f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}`,
			doctor.EtcdVersionCommand:                  `{"etcdserver":"3.5.16","etcdcluster":"3.5.0"}`,
			"docker compose -f /srv/paisans/f2a9/infra/compose.yaml exec -T etcd etcdctl": `[{"endpoint":"http://10.44.0.1:2379","health":true},{"endpoint":"http://10.44.0.2:2379","health":true},{"endpoint":"http://10.44.0.3:2379","health":true}]`,
		}},
	})
	rec := withRecorder(t, true)
	runErr := runDoctor([]string{"--config", fixtureConfig(), "--sudo=false", "--site", "vm"})
	out := rec.Lines()
	for _, s := range sent {
		if !strings.HasPrefix(s, "vm.example.org: ") {
			t.Errorf("reached a site outside --site: %s", s)
		}
	}
	if !strings.Contains(out, "detail: 1 site(s), 1 reached") || !strings.Contains(out, "no database site reached") || !strings.Contains(out, "not asked (outside --site)") {
		t.Errorf("output:\n%s", out)
	}
	if runErr == nil {
		t.Error("a deployment whose database could not be read is not a pass")
	}
}

// The leftovers check: on the gateway, a stack of this deployment the
// configuration no longer renders is a warning, a site block in the host's
// own directory is info, a foreign container is not reported, and every
// command it sent is a read.
func TestDoctorReportsLeftoversAndForeignCaddyUsers(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	id := cfg.Deployment().ID
	probes := hostcheck.ReadCommands()
	answers := map[string]string{
		doctor.ReachCommand:                        "",
		doctor.ClockCommand:                        "1760000000.000000000\n",
		doctor.ContainersCommand(cfg.Deployment()): `{"Names":"paisans-f2a9-infra-etcd-1","State":"running","Status":"Up","Labels":"community.paisans.deployment=` + id + `"}`,
		doctor.EtcdVersionCommand:                  `{"etcdserver":"3.5.16","etcdcluster":"3.5.0"}`,
		"docker compose -f /srv/paisans/f2a9/infra/compose.yaml exec -T etcd etcdctl": `[{"endpoint":"http://10.44.0.1:2379","health":true},{"endpoint":"http://10.44.0.2:2379","health":true},{"endpoint":"http://10.44.0.3:2379","health":true}]`,
		mesh.LinkCommand("psns-f2a9"): `[{"ifname":"psns-f2a9","flags":["POINTOPOINT","NOARP","UP","LOWER_UP"]}]`,
		mesh.RouteCommand:             `[{"dst":"10.44.0.0/24","dev":"psns-f2a9"}]`,
		mesh.AddrCommand:              "[]",
		mesh.NetworksCommand:          "[]",
		probes[0]:                     "0\n",
		probes[1]:                     "27.3.1\n",
		probes[2]:                     "docker-ce install ok installed\n",
		probes[3]: `{"id":"a","name":"/paisans-f2a9-infra-caddy-1","pid":10,"labels":{"community.paisans.deployment":"` + id + `","com.docker.compose.project":"paisans-f2a9-infra","com.docker.compose.service":"caddy"},"networks":{"web":{}},"ports":{}}
{"id":"b","name":"/paisans-f2a9-old-app-1","pid":11,"labels":{"community.paisans.deployment":"` + id + `","com.docker.compose.project":"paisans-f2a9-old","com.docker.compose.service":"app"},"networks":{"paisans-f2a9-old_default":{}},"ports":{}}
{"id":"c","name":"/blog-ghost-1","pid":12,"labels":{"com.docker.compose.project":"blog"},"networks":{"web":{}},"ports":{}}
`,
		probes[4]:  "",
		probes[5]:  "",
		probes[6]:  "",
		probes[7]:  "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536\n",
		probes[8]:  "[]",
		probes[9]:  "ufw absent\n",
		probes[10]: "inactive\n",
		probes[11]: "/srv/caddy.d/blog.caddy\n",
	}
	var sent, writes []string
	withDoctorHosts(t, map[string]doctorHost{
		"vm.example.org": {name: "vm.example.org", sent: &sent, writes: &writes, answers: answers},
	})
	rec := withRecorder(t, true)
	_ = runDoctor([]string{"--config", fixtureConfig(), "--site", "vm"})
	out := rec.Lines()
	for _, want := range []string{
		"leftovers",
		"warn: check leftovers vm: 1 thing(s) of this deployment left over",
		"stack old (compose project paisans-f2a9-old, 1 of 1 running): paisans-f2a9-old-app-1",
		"apply leaves them in place.",
		"`paisans app remove old` takes old's off every site, once a whole apply has run on each; it keeps its data unless given --delete-data.",
		"done: check leftovers vm 1 foreign thing(s) rely on this deployment's Caddy",
		"site block /srv/caddy.d/blog.caddy, served by this deployment's Caddy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "blog-ghost-1") {
		t.Errorf("a foreign container is reported:\n%s", out)
	}
	exact, prefixes := readOnly(cfg)
	for _, s := range sent {
		command := s[strings.Index(s, ": ")+2:]
		allowed := false
		for _, e := range exact {
			allowed = allowed || command == e
		}
		for _, p := range prefixes {
			allowed = allowed || strings.HasPrefix(command, p)
		}
		if !allowed {
			t.Errorf("doctor sent a command outside its read allow list: %s", s)
		}
	}
	if len(writes) > 0 {
		t.Errorf("doctor wrote: %v", writes)
	}
}

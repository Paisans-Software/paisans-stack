package hostprep_test

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostprep"
)

// fakeHost answers each probe by a substring that only that probe's script
// contains, and keeps files in a map. Nothing here runs a shell: what is under
// test is what the planner does with what a host says, which is where every
// decision in this package is made.
type fakeHost struct {
	files     map[string]string
	responses map[string]string // keyed by a substring of the command
	failures  map[string]error
	ran       []string
	written   []string
}

func (h *fakeHost) Describe() string { return "fake" }

func (h *fakeHost) Run(command string) (string, error) {
	h.ran = append(h.ran, command)
	for key, err := range h.failures {
		if strings.Contains(command, key) {
			return "", err
		}
	}
	for key, out := range h.responses {
		if strings.Contains(command, key) {
			return out, nil
		}
	}
	return "", nil
}

func (h *fakeHost) ReadFile(path string) (string, bool, error) {
	c, ok := h.files[path]
	return c, ok, nil
}

func (h *fakeHost) WriteFile(path, content string, mode uint32) error {
	h.written = append(h.written, path)
	h.files[path] = content
	return nil
}

// Probe keys: each is a string only one probe script contains.
const (
	probePackages = "docker compose version"
	probeWatchdog = "RuntimeWatchdogUSec"
	probeDocker   = "is-enabled docker"
	probeUnit     = "is-enabled paisans-f2a9-watchdog.service"
	probeFirewall = "ufw show added"
	probePasswd   = "getent passwd"
)

// The fixture's sites all log in as ubuntu with alice's key. These are
// obviously fake ed25519 keys: 32 bytes of zeros, ones, twos and threes.
const (
	keyAlice = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org"
	keyBob   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB bob@example.org"
	keyCarol = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgIC carol@example.org"
	keyCloud = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMD cloud-init"

	passwdUbuntu   = "ubuntu:x:1000:1000:Ubuntu:/home/ubuntu:/bin/bash\n"
	authorizedKeys = "/home/ubuntu/.ssh/authorized_keys"
	ownedKeys      = "/etc/paisans/authorized_keys.ubuntu.paisans-f2a9.owned"
)

// keysPrepared is the user's key state after a prepare: alice's key in
// authorized_keys and in the sidecar.
func keysPrepared(h *fakeHost) *fakeHost {
	h.responses[probePasswd] = passwdUbuntu
	h.files[authorizedKeys] = keyAlice + "\n"
	h.files[ownedKeys] = "# header\n" + fingerprint(keyAlice) + " alice@example.org\n"
	return h
}

func fingerprint(line string) string {
	k, _, err := config.ParseKeyLine(line)
	if err != nil {
		panic(err)
	}
	return k.Fingerprint
}

const ubuntu2404 = `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04.1 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
UBUNTU_CODENAME=noble
`

// freshHost is an Ubuntu 24.04 install with nothing added: no Docker, no
// WireGuard tools, no ufw, no watchdog device. Its login user's keys are
// already as prepared, so that tests of everything else see no key steps;
// the key tests set them up themselves.
func freshHost() *fakeHost {
	return keysPrepared(&fakeHost{
		files: map[string]string{"/etc/os-release": ubuntu2404},
		responses: map[string]string{
			probePackages: "compose absent\npkg docker-ce absent\npkg wireguard-tools absent\npkg ufw absent\nkeyring absent\narch amd64\n",
			probeWatchdog: "device absent\nsystemd 0\ndaemon absent\n",
			probeDocker:   "enabled \nactive \n",
			probeUnit:     "enabled \nactive \n",
			probeFirewall: "ufw absent\n",
		},
	})
}

const dockerSources = `Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable
Architectures: amd64
Signed-By: /etc/apt/keyrings/docker.asc
`

// preparedHost is what a fresh host looks like after prepare ran on it with a
// hardware watchdog present, for a site with the given rules.
// dockerAfterWG0 is the drop-in as host prepare writes it, so a prepared host
// carries exactly that file.
const dockerAfterWG0 = `# Written by paisans host prepare. Do not edit: it is rewritten if it differs.
#
# Docker starts after the mesh interface, so a container publishing a port on
# the site's mesh address can bind it at boot. Started first, Docker meets an
# address that does not exist yet ("cannot assign requested address"), and it
# does not retry a container that failed while setting up its network.
[Unit]
Wants=wg-quick@wg0.service
After=wg-quick@wg0.service
`

const dockerDropIn = "/etc/systemd/system/docker.service.d/paisans-f2a9-after-wg0.conf"

func preparedHost(gateway bool) *fakeHost {
	firewall := "ufw present\nstatus active\n" + owned("allow 22/tcp", "allow 51820/udp", "allow in on wg0")
	if !gateway {
		firewall += owned("allow in on br-+ to 10.44.0.1 port 5000 proto tcp", "allow in on br-+ to 10.44.0.1 port 3900 proto tcp")
	}
	if gateway {
		firewall += owned("allow 80/tcp", "allow 443/tcp")
	}
	return keysPrepared(&fakeHost{
		files: map[string]string{
			"/etc/os-release":                        ubuntu2404,
			"/etc/default/ufw":                       "IPV6=yes\nDEFAULT_INPUT_POLICY=\"DROP\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\n",
			"/etc/apt/sources.list.d/docker.sources": dockerSources,
			dockerDropIn:                             dockerAfterWG0,
		},
		responses: map[string]string{
			probePackages: "compose present\npkg docker-ce install ok installed\npkg wireguard-tools install ok installed\npkg ufw install ok installed\nkeyring present\narch amd64\n",
			probeWatchdog: "device present\nidentity watchdog0 iTCO_wdt\nsystemd 0\ndaemon absent\n",
			probeDocker:   "enabled enabled\nactive active\n",
			probeUnit:     "enabled \nactive \n",
			probeFirewall: firewall,
		},
	})
}

// owned is how `ufw show added` prints rules host prepare added, as probe
// lines. The comment's text after the tag is not compared, so any will do.
func owned(specs ...string) string {
	var out string
	for _, spec := range specs {
		out += "rule " + spec + " comment 'paisans-f2a9: test'\n"
	}
	return out
}

func fixture(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatalf("loading the fixture configuration: %v", err)
	}
	return cfg
}

// withoutMonitor is the fixture without its uptime app, for a test about the
// firewall's handling of rules on vm that nothing about the monitor should
// bear on.
func withoutMonitor(t *testing.T) *config.Config {
	t.Helper()
	cfg := fixture(t)
	delete(cfg.Apps, "status")
	return cfg
}

func withWatchdog(t *testing.T, site string, mode config.WatchdogMode) *config.Config {
	t.Helper()
	cfg := fixture(t)
	s := cfg.Sites[site]
	s.Watchdog = mode
	cfg.Sites[site] = s
	return cfg
}

func describes(plan *hostprep.Plan) string {
	var out []string
	for _, s := range plan.Steps {
		out = append(out, s.Describe)
	}
	return strings.Join(out, "\n")
}

func printed(plan *hostprep.Plan) string {
	var buf bytes.Buffer
	plan.Print(&buf)
	return buf.String()
}

// The wording is the founder's. It names what the host is, then lists what it
// could have been, one per line, so the answer to "what do I install" is on
// the screen.
func TestAnUnsupportedHostIsNamedWithWhatIsSupported(t *testing.T) {
	host := freshHost()
	host.files["/etc/os-release"] = "ID=debian\nVERSION_ID=\"12\"\n"
	_, err := hostprep.Build("home-a", fixture(t), host)
	want := "debian 12 is not supported. Use one of the following:\nubuntu 24.04"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want exactly:\n%s", err, want)
	}
	if len(host.ran) != 0 {
		t.Errorf("nothing should run on a host no profile matches, ran %d command(s)", len(host.ran))
	}
}

// Another Ubuntu release is not this one. Docker's apt source names the
// codename, and a profile matched on ID alone would write noble's packages
// onto a host that is not noble.
func TestAnotherUbuntuReleaseIsNotSupported(t *testing.T) {
	host := freshHost()
	host.files["/etc/os-release"] = strings.ReplaceAll(ubuntu2404, "24.04", "22.04")
	_, err := hostprep.Build("home-a", fixture(t), host)
	if err == nil || !strings.HasPrefix(err.Error(), "ubuntu 22.04 is not supported. Use one of the following:\n") {
		t.Fatalf("got %v", err)
	}
}

// A blank data site needs everything: Docker's repository and packages, the
// tools, Docker running, softdog (auto, with no device), and the firewall.
func TestAFreshDataSitePlansEveryStep(t *testing.T) {
	host := freshHost()
	plan, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	got := printed(plan)
	t.Logf("\n%s", got)
	for _, want := range []string{
		"docker: add Docker's apt signing key at /etc/apt/keyrings/docker.asc",
		"docker: add Docker's apt repository for noble/amd64 at /etc/apt/sources.list.d/docker.sources",
		"packages: install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin wireguard-tools ufw",
		"service: enable and start docker",
		"watchdog: load softdog now",
		"watchdog: load softdog at every boot (paisans-f2a9-watchdog.service)",
		"firewall: allow 22/tcp",
		"firewall: allow 51820/udp",
		"firewall: allow all inbound on wg0",
		"firewall: deny incoming by default",
		"firewall: allow outgoing by default",
		"firewall: enable ufw",
		"WARNING   the watchdog will be softdog",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(got, "80/tcp") || strings.Contains(got, "443/tcp") {
		t.Errorf("home-a holds no gateway role and was given the gateway's ports:\n%s", got)
	}

	// The source names the host's own codename and architecture, and is
	// written over stdin rather than echoed through a command line.
	for _, s := range plan.Steps {
		if s.File != nil && s.File.Path == "/etc/apt/sources.list.d/docker.sources" && s.File.Content != dockerSources {
			t.Errorf("the apt source is:\n%s", s.File.Content)
		}
	}
	// Every apt call is non-interactive.
	for _, s := range plan.Steps {
		if strings.Contains(s.Command, "apt-get") && !strings.Contains(s.Command, "DEBIAN_FRONTEND=noninteractive") {
			t.Errorf("an apt step can prompt: %s", s.Describe)
		}
	}
}

// The order is the safety property. The SSH allow lands before ufw is enabled,
// and the firewall comes after the package step that installs ufw.
func TestSSHIsAllowedBeforeTheFirewallIsEnabled(t *testing.T) {
	plan, err := hostprep.Build("home-a", fixture(t), freshHost())
	if err != nil {
		t.Fatal(err)
	}
	index := func(cmd string) int {
		for i, s := range plan.Steps {
			if strings.Contains(s.Command, cmd) {
				return i
			}
		}
		return -1
	}
	ssh, deny, enable, install := index("ufw allow 22/tcp"), index("ufw default deny incoming"), index("ufw --force enable"), index("apt-get -o DPkg::Lock::Timeout=300 -o Dpkg::Options")
	if ssh < 0 || deny < 0 || enable < 0 || install < 0 {
		t.Fatalf("missing a step:\n%s", describes(plan))
	}
	if !(install < ssh && ssh < deny && deny < enable) {
		t.Errorf("want install < allow ssh < default deny < enable, got %d %d %d %d", install, ssh, deny, enable)
	}
}

// A host prepare already prepared plans nothing, on a data site and on a
// gateway, and says what it found.
func TestAPreparedHostPlansNothing(t *testing.T) {
	for _, tc := range []struct {
		site    string
		gateway bool
	}{{"home-a", false}, {"vm", true}} {
		t.Run(tc.site, func(t *testing.T) {
			plan, err := hostprep.Build(tc.site, fixture(t), preparedHost(tc.gateway))
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Steps) != 0 || len(plan.Warnings) != 0 {
				t.Fatalf("a prepared host planned:\n%s", printed(plan))
			}
			if len(plan.Present) == 0 {
				t.Fatal("a plan with nothing to do should still say what it found")
			}
		})
	}
}

// Running Execute writes the files and runs the commands it planned, in order.
func TestExecuteRunsEveryStepInOrder(t *testing.T) {
	host := freshHost()
	plan, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	host.ran = nil
	if err := hostprep.Execute(plan, host); err != nil {
		t.Fatal(err)
	}
	if len(host.written) != 3 {
		t.Errorf("want the apt source, the docker drop-in and the unit written, got %v", host.written)
	}
	if last := host.ran[len(host.ran)-1]; last != "ufw --force enable" {
		t.Errorf("enabling the firewall is the last thing run, got %q", last)
	}
}

// Execute stops at the first failure and names the step.
func TestExecuteStopsAtTheFirstFailure(t *testing.T) {
	host := freshHost()
	plan, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	host.failures = map[string]error{"modprobe softdog": errors.New("exit status 1")}
	host.ran = nil
	err = hostprep.Execute(plan, host)
	if err == nil || !strings.HasPrefix(err.Error(), "watchdog: load softdog now") {
		t.Fatalf("got %v", err)
	}
	for _, c := range host.ran {
		if strings.HasPrefix(c, "ufw") {
			t.Fatalf("ran %q after a failed step", c)
		}
	}
}

func TestFirewallRulesFollowRoles(t *testing.T) {
	cfg := fixture(t)
	var gateway, data []string
	for _, r := range hostprep.Rules(cfg.Sites["vm"]) {
		gateway = append(gateway, r.String())
	}
	for _, r := range hostprep.Rules(cfg.Sites["home-a"]) {
		data = append(data, r.String())
	}
	if got, want := strings.Join(gateway, ","), "22/tcp,51820/udp,all inbound on wg0,80/tcp,443/tcp"; got != want {
		t.Errorf("gateway: got %s, want %s", got, want)
	}
	if got, want := strings.Join(data, ","), "22/tcp,51820/udp,all inbound on wg0"; got != want {
		t.Errorf("data: got %s, want %s", got, want)
	}
}

// A monitor serving its own apps opens 80 and 443 like a gateway; behind the
// operator's own web server it opens nothing for the web, because that server
// already holds both.
func TestAPaisansMonitorOpensEightyAndFourFortyThree(t *testing.T) {
	cfg := fixture(t)
	var got []string
	for _, r := range hostprep.Rules(cfg.Sites["watch"]) {
		got = append(got, r.String())
	}
	if want := "22/tcp,51820/udp,all inbound on wg0,80/tcp,443/tcp"; strings.Join(got, ",") != want {
		t.Errorf("paisans: got %s, want %s", strings.Join(got, ","), want)
	}
	watch := cfg.Sites["watch"]
	watch.Ingress = &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"}
	got = nil
	for _, r := range hostprep.Rules(watch) {
		got = append(got, r.String())
	}
	if want := "22/tcp,51820/udp,all inbound on wg0"; strings.Join(got, ",") != want {
		t.Errorf("external: got %s, want %s", strings.Join(got, ","), want)
	}
}

// A fresh gateway gets 80 and 443 and no watchdog: it holds no data role.
func TestAFreshGatewaySiteDiffersFromADataSite(t *testing.T) {
	host := freshHost()
	plan, err := hostprep.Build("vm", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	got := printed(plan)
	for _, want := range []string{"firewall: allow 80/tcp", "firewall: allow 443/tcp", "watchdog: not needed"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "softdog") {
		t.Errorf("a site with no data role was given a watchdog:\n%s", got)
	}
	for _, c := range host.ran {
		if strings.Contains(c, probeWatchdog) {
			t.Error("the watchdog was probed on a site with no data role")
		}
	}
}

// Only the missing rule is planned on a firewall that is already up.
func TestOnlyMissingFirewallRulesArePlanned(t *testing.T) {
	host := preparedHost(false)
	host.responses[probeFirewall] = "ufw present\nstatus active\n" + owned("allow 22/tcp", "allow in on wg0", "allow in on br-+ to 10.44.0.1 port 5000 proto tcp", "allow in on br-+ to 10.44.0.1 port 3900 proto tcp")
	plan, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if got := describes(plan); got != "firewall: allow 51820/udp (WireGuard, the mesh)" {
		t.Errorf("got:\n%s", got)
	}
}

func TestWatchdogModes(t *testing.T) {
	hardware := "device present\nidentity watchdog0 sp5100_tco\nsystemd 0\ndaemon absent\n"
	softdog := "device present\nidentity watchdog0 Software Watchdog\nsystemd 0\ndaemon absent\n"
	none := "device absent\nsystemd 0\ndaemon absent\n"

	cases := []struct {
		name    string
		mode    config.WatchdogMode
		probe   string
		refuse  string // substring of the error, when refused
		steps   []string
		present string
		warn    bool
	}{
		{name: "auto uses hardware", mode: config.WatchdogAuto, probe: hardware, present: "watchdog0 is sp5100_tco"},
		{name: "auto falls back to softdog", mode: config.WatchdogAuto, probe: none, steps: []string{"watchdog: load softdog now", "watchdog: load softdog at every boot"}, warn: true},
		{name: "auto keeps a loaded softdog and persists it", mode: config.WatchdogAuto, probe: softdog, steps: []string{"watchdog: load softdog at every boot"}, present: "Software Watchdog", warn: true},
		{name: "required accepts hardware", mode: config.WatchdogRequired, probe: hardware, present: "sp5100_tco"},
		{name: "required refuses softdog", mode: config.WatchdogRequired, probe: softdog, refuse: "has only softdog"},
		{name: "required refuses none", mode: config.WatchdogRequired, probe: none, refuse: "has no watchdog device"},
		{name: "softdog loads without a warning", mode: config.WatchdogSoftdog, probe: none, steps: []string{"watchdog: load softdog now", "watchdog: load softdog at every boot"}},
		{name: "off touches nothing", mode: config.WatchdogOff, probe: none, present: "watchdog: off"},
		{name: "systemd holds the device", mode: config.WatchdogAuto, probe: "device present\nidentity watchdog0 iTCO_wdt\nsystemd 1min\ndaemon absent\n", refuse: "RuntimeWatchdogUSec is 1min"},
		{name: "a daemon holds the device", mode: config.WatchdogAuto, probe: "device present\nidentity watchdog0 iTCO_wdt\nsystemd 0\ndaemon running\n", refuse: "a watchdog daemon is running"},
		{name: "contention refuses required too", mode: config.WatchdogRequired, probe: "device present\nidentity watchdog0 iTCO_wdt\nsystemd 30s\ndaemon absent\n", refuse: "RuntimeWatchdogUSec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := preparedHost(false)
			host.responses[probeWatchdog] = tc.probe
			plan, err := hostprep.Build("home-a", withWatchdog(t, "home-a", tc.mode), host)
			if tc.refuse != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refuse) {
					t.Fatalf("want a refusal containing %q, got %v", tc.refuse, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := describes(plan)
			for _, want := range tc.steps {
				if !strings.Contains(got, want) {
					t.Errorf("missing step %q in:\n%s", want, got)
				}
			}
			if len(tc.steps) == 0 && got != "" {
				t.Errorf("want no steps, got:\n%s", got)
			}
			if tc.present != "" && !strings.Contains(strings.Join(plan.Present, "\n"), tc.present) {
				t.Errorf("want %q reported present, got:\n%s", tc.present, strings.Join(plan.Present, "\n"))
			}
			if (len(plan.Warnings) > 0) != tc.warn {
				t.Errorf("warnings: %v, want warning %v", plan.Warnings, tc.warn)
			}
			if tc.mode == config.WatchdogOff {
				for _, c := range host.ran {
					if strings.Contains(c, probeWatchdog) {
						t.Error("off probed the watchdog")
					}
				}
			}
		})
	}
}

// softdog persisted by an earlier prepare plans nothing on the next one, and
// still warns under auto: the host is prepared, and still on softdog.
func TestASoftdogHostReRunsClean(t *testing.T) {
	host := freshHost()
	plan, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := hostprep.Execute(plan, host); err != nil {
		t.Fatal(err)
	}
	after := preparedHost(false)
	after.files["/etc/systemd/system/paisans-f2a9-watchdog.service"] = host.files["/etc/systemd/system/paisans-f2a9-watchdog.service"]
	after.responses[probeWatchdog] = "device present\nidentity watchdog0 Software Watchdog\nsystemd 0\ndaemon absent\n"
	after.responses[probeUnit] = "enabled enabled\nactive active\n"
	again, err := hostprep.Build("home-a", fixture(t), after)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Steps) != 0 {
		t.Fatalf("a softdog host prepared once planned again:\n%s", printed(again))
	}
	if len(again.Warnings) != 1 {
		t.Errorf("auto on softdog should still warn, got %v", again.Warnings)
	}
}

// Ubuntu's own docker.io is refused rather than removed: removing it would take
// down whatever containers it is running.
func TestDistroDockerIsRefusedNotRemoved(t *testing.T) {
	host := freshHost()
	host.responses[probePackages] = "compose absent\npkg docker.io install ok installed\npkg runc install ok installed\nkeyring absent\narch amd64\n"
	_, err := hostprep.Build("home-a", fixture(t), host)
	if err == nil || !strings.Contains(err.Error(), "docker.io, runc") {
		t.Fatalf("got %v", err)
	}
}

// A package removed with its configuration kept reads "deinstall ok
// config-files", which is not installed.
func TestARemovedPackageIsNotInstalled(t *testing.T) {
	host := preparedHost(false)
	host.responses[probePackages] = "compose present\npkg wireguard-tools deinstall ok config-files\npkg ufw install ok installed\n"
	plan, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if got := describes(plan); got != "packages: install wireguard-tools" {
		t.Errorf("got:\n%s", got)
	}
}

// An apps site lets its containers reach the database proxy and, where Garage
// runs, object storage on its own mesh address, over the compose bridges and
// nothing wider. A gateway only site gets neither, and so does the fixture's
// monitor site, where the monitor is the only app and does not check itself.
func TestContainerRulesReachOnlyWhatAppsUse(t *testing.T) {
	cfg := fixture(t)
	var got []string
	for _, r := range hostprep.ContainerRules(cfg, "home-a") {
		got = append(got, r.String())
	}
	if want := "5000/tcp on br-+ to 10.44.0.1,3900/tcp on br-+ to 10.44.0.1"; strings.Join(got, ",") != want {
		t.Errorf("home-a: got %s, want %s", strings.Join(got, ","), want)
	}
	for _, site := range []string{"vm", "watch"} {
		if rules := hostprep.ContainerRules(cfg, site); len(rules) != 0 {
			t.Errorf("%s: got %v, want none", site, rules)
		}
	}
}

func commands(plan *hostprep.Plan) []string {
	var out []string
	for _, s := range plan.Steps {
		out = append(out, s.Command)
	}
	return out
}

func indexOf(cmds []string, cmd string) int {
	for i, c := range cmds {
		if c == cmd {
			return i
		}
	}
	return -1
}

// Every rule a fresh host is given carries the comment that marks it as host
// prepare's, in the syntax ufw 0.36.2 accepts and prints back.
func TestAFreshHostAddsCommentedRules(t *testing.T) {
	plan, err := hostprep.Build("home-a", fixture(t), freshHost())
	if err != nil {
		t.Fatal(err)
	}
	cmds := commands(plan)
	for _, want := range []string{
		"ufw allow 22/tcp comment 'paisans-f2a9: ssh, the bootstrap route'",
		"ufw allow 51820/udp comment 'paisans-f2a9: WireGuard, the mesh'",
		"ufw allow in on wg0 comment 'paisans-f2a9: the mesh: etcd, Patroni, Garage, HAProxy'",
		"ufw allow in on br-+ to 10.44.0.1 port 5000 proto tcp comment 'paisans-f2a9: app containers to the local database proxy'",
	} {
		if indexOf(cmds, want) < 0 {
			t.Errorf("missing %q in:\n%s", want, strings.Join(cmds, "\n"))
		}
	}
	for _, c := range cmds {
		if strings.HasPrefix(c, "ufw allow") && !strings.Contains(c, " comment 'paisans-f2a9: ") {
			t.Errorf("an allow without the ownership comment: %s", c)
		}
	}
}

// A site that loses the gateway role loses 80 and 443 on its next prepare,
// after the firewall's default policy and enabling, and nothing else goes.
func TestAStaleOwnedRuleIsRemovedLast(t *testing.T) {
	cfg := fixture(t)
	vm := cfg.Sites["vm"]
	vm.Roles = []config.Role{config.RoleWitness}
	cfg.Sites["vm"] = vm
	host := preparedHost(true)
	host.files["/etc/default/ufw"] = "DEFAULT_INPUT_POLICY=\"ACCEPT\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\n"
	plan, err := hostprep.Build("vm", cfg, host)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", printed(plan))
	cmds := commands(plan)
	want := []string{
		"ufw default deny incoming",
		"ufw delete allow 80/tcp comment 'paisans-f2a9: test'",
		"ufw delete allow 443/tcp comment 'paisans-f2a9: test'",
	}
	if strings.Join(cmds, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(cmds, "\n"), strings.Join(want, "\n"))
	}
	for _, s := range plan.Steps[1:] {
		if s.Label != "remove" {
			t.Errorf("a removal is labelled %q: %s", s.Label, s.Describe)
		}
	}
	if !strings.Contains(printed(plan), "  remove    firewall: delete `ufw allow 80/tcp comment 'paisans-f2a9: test'`") {
		t.Errorf("the plan does not show the removal as remove:\n%s", printed(plan))
	}
}

// A rule is host prepare's only under this deployment's tag. One another
// deployment on the host added, paisans-<its token>:, and one carrying the
// bare paisans: tag are foreign: neither is removed when no longer derived,
// and neither satisfies or is rewritten for a derived rule.
func TestAnotherDeploymentsRuleIsForeign(t *testing.T) {
	cfg := withoutMonitor(t)
	vm := cfg.Sites["vm"]
	vm.Roles = []config.Role{config.RoleWitness}
	cfg.Sites["vm"] = vm
	host := preparedHost(false)
	host.responses[probeFirewall] = "ufw present\nstatus active\n" +
		owned("allow 22/tcp", "allow 51820/udp", "allow in on wg0") +
		"rule allow 80/tcp comment 'paisans-0c1d: the gateway'\n" +
		"rule allow 443/tcp comment 'paisans: the gateway'\n"
	plan, err := hostprep.Build("vm", cfg, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("another deployment's rule was planned for:\n%s", printed(plan))
	}
}

// Rules host prepare did not add are never removed or changed. One that bears
// on a derived rule is listed; one that does not is not mentioned at all.
func TestAForeignRuleIsNeverTouched(t *testing.T) {
	cfg := withoutMonitor(t)
	vm := cfg.Sites["vm"]
	vm.Roles = []config.Role{config.RoleWitness}
	cfg.Sites["vm"] = vm
	host := preparedHost(false)
	host.responses[probeFirewall] = "ufw present\nstatus active\n" +
		owned("allow 22/tcp", "allow 51820/udp", "allow in on wg0") +
		"rule allow 80/tcp\n" + // uncommented, and no longer derived
		"rule allow 8080/tcp comment 'grafana'\n" + // commented, someone else's
		"rule allow from 192.0.2.7 to any port 22 proto tcp comment 'office'\n" + // bears on SSH
		"rule allow in on wg0 to any port 9100 proto tcp\n" // bears on the mesh
	plan, err := hostprep.Build("vm", cfg, host)
	if err != nil {
		t.Fatal(err)
	}
	out := printed(plan)
	t.Logf("\n%s", out)
	if len(plan.Steps) != 0 {
		t.Fatalf("a foreign rule was planned for:\n%s", out)
	}
	for _, want := range []string{
		"present (not paisans) firewall: `ufw allow from 192.0.2.7 to any port 22 proto tcp comment 'office'` (bears on 22/tcp)",
		"present (not paisans) firewall: `ufw allow in on wg0 to any port 9100 proto tcp` (bears on all inbound on wg0)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, unmentioned := range []string{"80/tcp", "8080"} {
		if strings.Contains(out, unmentioned) {
			t.Errorf("%s bears on no derived rule and was mentioned:\n%s", unmentioned, out)
		}
	}
}

// A rule someone else commented, matching a derived rule exactly, satisfies
// it. Adding ours would make ufw rewrite theirs in place.
func TestAForeignCommentedMatchIsLeftAlone(t *testing.T) {
	host := preparedHost(true)
	host.responses[probeFirewall] = "ufw present\nstatus active\n" +
		owned("allow 22/tcp", "allow 51820/udp", "allow in on wg0", "allow 443/tcp") +
		"rule allow 80/tcp comment 'acme http-01'\n"
	plan, err := hostprep.Build("vm", withoutMonitor(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("planned over a foreign rule:\n%s", printed(plan))
	}
	if !strings.Contains(printed(plan), "present (not paisans) firewall: 80/tcp allowed by `ufw allow 80/tcp comment 'acme http-01'`") {
		t.Errorf("got:\n%s", printed(plan))
	}
}

// A different action on the same traffic is someone else's decision. It is
// warned about and left alone, and a deny on SSH is refused outright, since
// enabling the firewall over it would lock the operator out.
func TestAForeignActionIsWarnedOrRefused(t *testing.T) {
	host := preparedHost(true)
	host.responses[probeFirewall] = "ufw present\nstatus active\n" +
		owned("allow 51820/udp", "allow in on wg0", "allow 80/tcp") +
		"rule limit 22/tcp\nrule deny 443/tcp\n"
	plan, err := hostprep.Build("vm", withoutMonitor(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 || len(plan.Warnings) != 2 {
		t.Fatalf("got:\n%s", printed(plan))
	}

	host.responses[probeFirewall] = "ufw present\nstatus inactive\nrule deny 22/tcp\n"
	if _, err := hostprep.Build("vm", withoutMonitor(t), host); err == nil || !strings.Contains(err.Error(), "blocks SSH") {
		t.Fatalf("a deny on SSH was not refused: %v", err)
	}
}

// A host prepared before rules carried a comment has every rule, uncommented.
// Each is reported present and adopted with one command, which ufw applies as
// an in place rewrite. No delete follows: ufw would let a delete without a
// comment remove the commented rule just adopted.
func TestALegacyRuleIsAdoptedWithoutAGap(t *testing.T) {
	host := preparedHost(false)
	host.responses[probeFirewall] = "ufw present\nstatus active\nrule allow 22/tcp\nrule allow 51820/udp\nrule allow in on wg0\nrule allow in on br-+ to 10.44.0.1 port 5000 proto tcp\nrule allow in on br-+ to 10.44.0.1 port 3900 proto tcp\n"
	plan, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	out := printed(plan)
	t.Logf("\n%s", out)
	if len(plan.Steps) != 5 {
		t.Fatalf("want five adoptions, got:\n%s", out)
	}
	for _, s := range plan.Steps {
		if s.Label != "adopt" || !strings.HasPrefix(s.Command, "ufw allow ") || !strings.Contains(s.Command, " comment 'paisans-f2a9: ") {
			t.Errorf("not an adoption: %s %q", s.Label, s.Command)
		}
	}
	if !strings.Contains(out, "  adopt     firewall: mark `ufw allow 22/tcp` as host prepare's") ||
		!strings.Contains(out, "present   firewall: 22/tcp allowed, by a rule without the paisans comment") {
		t.Errorf("got:\n%s", out)
	}

	// Once adopted, the host plans nothing.
	host.responses[probeFirewall] = "ufw present\nstatus active\n"
	for _, c := range commands(plan) {
		host.responses[probeFirewall] += "rule " + strings.TrimPrefix(c, "ufw ") + "\n"
	}
	again, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Steps) != 0 {
		t.Errorf("an adopted host planned again:\n%s", printed(again))
	}
}

// SSH is never removed, even when host prepare added it and nothing derives it
// any more: with incoming denied, that removal drops the next connection.
func TestSSHIsNeverRemoved(t *testing.T) {
	host := preparedHost(false)
	host.responses[probeFirewall] = "ufw present\nstatus active\n" +
		owned("allow 22/tcp", "allow 51820/udp", "allow in on wg0", "allow in on br-+ to 10.44.0.1 port 5000 proto tcp", "allow in on br-+ to 10.44.0.1 port 3900 proto tcp") +
		owned("allow 22", "allow from 192.0.2.0/24 to any port 22 proto tcp")
	plan, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range commands(plan) {
		if strings.HasPrefix(c, "ufw delete") {
			t.Errorf("planned %q", c)
		}
	}
	if !strings.Contains(printed(plan), "host prepare never removes an SSH allow") {
		t.Errorf("got:\n%s", printed(plan))
	}
}

func withSSHPort(t *testing.T, site string, port int) *config.Config {
	t.Helper()
	cfg := fixture(t)
	s := cfg.Sites[site]
	s.SSH.Port = port
	cfg.Sites[site] = s
	return cfg
}

// The SSH allow is for the declared port, and it is still the first allow.
func TestTheSSHRuleFollowsTheDeclaredPort(t *testing.T) {
	plan, err := hostprep.Build("home-a", withSSHPort(t, "home-a", 2222), freshHost())
	if err != nil {
		t.Fatal(err)
	}
	out := printed(plan)
	if !strings.Contains(out, "firewall: allow 2222/tcp (ssh, the bootstrap route)") {
		t.Fatalf("no allow for the declared port:\n%s", out)
	}
	if strings.Contains(out, "allow 22/tcp") {
		t.Errorf("port 22 allowed although ssh.port is 2222:\n%s", out)
	}
	cmds := commands(plan)
	for _, c := range cmds {
		if strings.HasPrefix(c, "ufw allow") {
			if !strings.HasPrefix(c, "ufw allow 2222/tcp") {
				t.Errorf("the first allow is %q, want SSH's", c)
			}
			break
		}
	}
}

// Moving ssh.port adds the new allow and keeps the old one, saying so: host
// prepare cannot know sshd already listens on the new port.
func TestMovingTheSSHPortKeepsTheOldAllow(t *testing.T) {
	host := preparedHost(false)
	host.responses[probeFirewall] = "ufw present\nstatus active\n" +
		"rule allow 22/tcp comment 'paisans-f2a9: ssh, the bootstrap route'\n" +
		owned("allow 51820/udp", "allow in on wg0", "allow in on br-+ to 10.44.0.1 port 5000 proto tcp", "allow in on br-+ to 10.44.0.1 port 3900 proto tcp")
	plan, err := hostprep.Build("home-a", withSSHPort(t, "home-a", 2222), host)
	if err != nil {
		t.Fatal(err)
	}
	out := printed(plan)
	t.Logf("\n%s", out)
	if got := commands(plan); len(got) != 1 || got[0] != "ufw allow 2222/tcp comment 'paisans-f2a9: ssh, the bootstrap route'" {
		t.Fatalf("want only the new allow, got %q", got)
	}
	if !strings.Contains(out, "present   firewall: `ufw allow 22/tcp comment 'paisans-f2a9: ssh, the bootstrap route'` kept; it is the SSH allow for an earlier ssh.port, and host prepare never removes an SSH allow. Delete it yourself once SSH on 2222 works") {
		t.Errorf("the old allow is not noted:\n%s", out)
	}
}

// A deny on the declared SSH port is refused, whatever the port.
func TestADenyOnTheDeclaredSSHPortIsRefused(t *testing.T) {
	host := preparedHost(false)
	host.responses[probeFirewall] = "ufw present\nstatus inactive\nrule deny 2222/tcp\n"
	if _, err := hostprep.Build("home-a", withSSHPort(t, "home-a", 2222), host); err == nil || !strings.Contains(err.Error(), "blocks SSH") {
		t.Fatalf("a deny on the SSH port was not refused: %v", err)
	}
}

func withKeys(t *testing.T, site string, keys ...string) *config.Config {
	t.Helper()
	cfg := fixture(t)
	s := cfg.Sites[site]
	s.SSH.PublicKey = strings.Join(keys, "\n")
	cfg.Sites[site] = s
	return cfg
}

func stepsLabelled(plan *hostprep.Plan, label string) []hostprep.Step {
	var out []hostprep.Step
	for _, s := range plan.Steps {
		if s.Label == label {
			out = append(out, s)
		}
	}
	return out
}

// A user with no authorized_keys gets the directory and file, owned by them,
// and then every listed key, recorded in the sidecar.
func TestAUserWithoutAuthorizedKeysGetsThem(t *testing.T) {
	host := preparedHost(false)
	delete(host.files, authorizedKeys)
	delete(host.files, ownedKeys)
	plan, err := hostprep.Build("home-a", withKeys(t, "home-a", keyAlice, keyBob), host)
	if err != nil {
		t.Fatal(err)
	}
	out := printed(plan)
	t.Logf("\n%s", out)
	if len(plan.Steps) != 3 {
		t.Fatalf("want create, add, add; got:\n%s", out)
	}
	create := plan.Steps[0].Command
	for _, want := range []string{"install -d -m 700 -o 1000 -g 1000 '/home/ubuntu/.ssh'", "install -m 600 -o 1000 -g 1000 /dev/null '/home/ubuntu/.ssh/authorized_keys'", "[ -d '/home/ubuntu/.ssh' ] ||", "[ -f '/home/ubuntu/.ssh/authorized_keys' ] ||"} {
		if !strings.Contains(create, want) {
			t.Errorf("create lacks %q: %s", want, create)
		}
	}
	for _, want := range []string{
		"  add       ssh: authorize key " + fingerprint(keyAlice) + " (alice@example.org) for ubuntu",
		"  add       ssh: authorize key " + fingerprint(keyBob) + " (bob@example.org) for ubuntu",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	// The line is appended verbatim, and the sidecar written after it holds
	// both fingerprints once the second key is in.
	last := plan.Steps[2].Command
	if !strings.Contains(last, "printf '%s\\n' '"+keyBob+"' >> '/home/ubuntu/.ssh/authorized_keys'") {
		t.Errorf("bob's key is not appended verbatim: %s", last)
	}
	if !strings.Contains(last, fingerprint(keyAlice)+" alice@example.org\n"+fingerprint(keyBob)+" bob@example.org") &&
		!strings.Contains(last, fingerprint(keyBob)+" bob@example.org\n"+fingerprint(keyAlice)+" alice@example.org") {
		t.Errorf("the sidecar after the last add does not hold both keys: %s", last)
	}
	if !strings.Contains(last, "chmod 600 '/etc/paisans/authorized_keys.ubuntu.paisans-f2a9.owned.paisans-tmp' && mv") {
		t.Errorf("the sidecar is not written 0600 and renamed into place: %s", last)
	}
}

// A listed key cloud-init already put there is adopted, not duplicated, and a
// key nobody listed and host prepare never added is not touched or mentioned.
func TestAnExistingListedKeyIsAdoptedAndOthersLeftAlone(t *testing.T) {
	host := preparedHost(false)
	host.files[authorizedKeys] = keyCloud + "\n" + keyAlice
	delete(host.files, ownedKeys)
	plan, err := hostprep.Build("home-a", withKeys(t, "home-a", keyAlice, keyBob), host)
	if err != nil {
		t.Fatal(err)
	}
	out := printed(plan)
	t.Logf("\n%s", out)
	adopt, add := stepsLabelled(plan, "adopt"), stepsLabelled(plan, "add")
	if len(adopt) != 1 || len(add) != 1 || len(plan.Steps) != 2 {
		t.Fatalf("want one adopt and one add:\n%s", out)
	}
	if !strings.Contains(out, "  adopt     ssh: key "+fingerprint(keyAlice)+" (alice@example.org) is already authorized for ubuntu; record it as host prepare's") {
		t.Errorf("the adoption reads:\n%s", out)
	}
	if strings.Contains(adopt[0].Command, ">> '/home/ubuntu/.ssh/authorized_keys'") {
		t.Errorf("an adoption appends the key again: %s", adopt[0].Command)
	}
	// The file's last line has no newline; the add ends it first.
	if !strings.Contains(add[0].Command, `[ -n "$(tail -c1 '/home/ubuntu/.ssh/authorized_keys')" ]; then echo >>`) {
		t.Errorf("the add can glue a key onto an unterminated line: %s", add[0].Command)
	}
	if strings.Contains(out, fingerprint(keyCloud)) || strings.Contains(out, "cloud-init") {
		t.Errorf("an unlisted, unowned key was mentioned:\n%s", out)
	}
}

// A key host prepare added and the configuration no longer lists is removed,
// last, by its exact line; one it never added is not, listed or not.
func TestAnUnlistedOwnedKeyIsRemovedLast(t *testing.T) {
	host := preparedHost(false)
	host.files["/etc/default/ufw"] = "DEFAULT_INPUT_POLICY=\"ACCEPT\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\n"
	host.files[authorizedKeys] = keyCloud + "\n" + keyAlice + "\n" + keyCarol + "  \n"
	host.files[ownedKeys] = fingerprint(keyAlice) + " alice@example.org\n" + fingerprint(keyCarol) + " carol@example.org\n"
	plan, err := hostprep.Build("home-a", withKeys(t, "home-a", keyAlice, keyBob), host)
	if err != nil {
		t.Fatal(err)
	}
	out := printed(plan)
	t.Logf("\n%s", out)
	last := plan.Steps[len(plan.Steps)-1]
	if last.Label != "remove" || !strings.Contains(last.Describe, "ssh: remove key "+fingerprint(keyCarol)+" (carol@example.org) from /home/ubuntu/.ssh/authorized_keys, which host prepare added and ssh.public_key no longer lists") {
		t.Fatalf("the last step is not carol's removal:\n%s", out)
	}
	// The exact line, trailing spaces and all, so grep -x matches it.
	if !strings.Contains(last.Command, "grep -vxF -e '"+keyCarol+"  ' '/home/ubuntu/.ssh/authorized_keys'") {
		t.Errorf("the removal does not name carol's exact line: %s", last.Command)
	}
	if !strings.Contains(last.Command, "chown --reference='/home/ubuntu/.ssh/authorized_keys'") || !strings.Contains(last.Command, "mv '/home/ubuntu/.ssh/authorized_keys.paisans-tmp' '/home/ubuntu/.ssh/authorized_keys'") {
		t.Errorf("the removal does not keep the owner and rename into place: %s", last.Command)
	}
	if strings.Contains(last.Command, fingerprint(keyCarol)) {
		t.Errorf("the sidecar still lists carol after her removal: %s", last.Command)
	}
	if strings.Contains(last.Command, keyCloud) || strings.Contains(last.Command, keyAlice) {
		t.Errorf("the removal names a line it must keep: %s", last.Command)
	}
	// The add comes before the firewall's default policy, and the removal
	// after it.
	cmds := commands(plan)
	addAt, denyAt := -1, indexOf(cmds, "ufw default deny incoming")
	for i, s := range plan.Steps {
		if s.Label == "add" {
			addAt = i
		}
	}
	if addAt < 0 || denyAt < 0 || addAt > denyAt || denyAt >= len(cmds)-1 {
		t.Errorf("want add < firewall < remove, got add %d deny %d of %d", addAt, denyAt, len(cmds))
	}
}

// A key in the sidecar that someone already deleted is forgotten, so that a
// later hand-added copy is never taken for host prepare's.
func TestAnOwnedKeyGoneFromTheFileIsForgotten(t *testing.T) {
	host := preparedHost(false)
	host.files[ownedKeys] += fingerprint(keyCarol) + " carol@example.org\n"
	plan, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 1 || !strings.Contains(plan.Steps[0].Describe, "ssh: forget key "+fingerprint(keyCarol)) {
		t.Fatalf("got:\n%s", printed(plan))
	}
	if strings.Contains(plan.Steps[0].Command, "authorized_keys'") {
		t.Errorf("forgetting touches authorized_keys: %s", plan.Steps[0].Command)
	}
}

// A listed key present only with options is someone's restriction: it is
// reported and left, and the plain key is not added beside it.
func TestARestrictedListedKeyIsLeftAlone(t *testing.T) {
	host := preparedHost(false)
	restricted := `from="203.0.113.0/24" ` + keyAlice
	host.files[authorizedKeys] = restricted + "\n"
	delete(host.files, ownedKeys)
	plan, err := hostprep.Build("home-a", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("planned over a restricted key:\n%s", printed(plan))
	}
	if !strings.Contains(printed(plan), "present (not paisans) ssh: key "+fingerprint(keyAlice)+" (alice@example.org) authorized for ubuntu with options, by `"+restricted+"`; left as it is") {
		t.Errorf("got:\n%s", printed(plan))
	}
}

// Creating users is out of scope, so a missing one is refused by name.
func TestAMissingUserIsRefused(t *testing.T) {
	host := preparedHost(false)
	host.responses[probePasswd] = "__PAISANS_ABSENT__\n"
	_, err := hostprep.Build("home-a", fixture(t), host)
	if err == nil || !strings.Contains(err.Error(), "ssh.user ubuntu does not exist on fake. host prepare does not create users") {
		t.Fatalf("got %v", err)
	}
}

// The safety property: no plan leaves the user with none of the listed keys.
// Load makes this unreachable through a file, so the configuration is
// emptied by hand to show the check holds on its own.
func TestNoPlanRemovesTheLastListedKey(t *testing.T) {
	host := preparedHost(false)
	host.files[authorizedKeys] = keyCarol + "\n"
	host.files[ownedKeys] = fingerprint(keyCarol) + " carol@example.org\n"
	_, err := hostprep.Build("home-a", withKeys(t, "home-a"), host)
	if err == nil || !strings.Contains(err.Error(), "would leave ubuntu with none of the keys ssh.public_key lists") {
		t.Fatalf("got %v", err)
	}
}

// After the plan's effect, the same host plans nothing for its keys.
func TestPreparedKeysPlanNothing(t *testing.T) {
	host := preparedHost(false)
	host.files[authorizedKeys] = keyCloud + "\n" + keyAlice + "\n" + keyBob + "\n"
	host.files[ownedKeys] = fingerprint(keyBob) + " bob@example.org\n" + fingerprint(keyAlice) + " alice@example.org\n"
	plan, err := hostprep.Build("home-a", withKeys(t, "home-a", keyAlice, keyBob), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("got:\n%s", printed(plan))
	}
	if !strings.Contains(printed(plan), "present   ssh: key "+fingerprint(keyBob)+" (bob@example.org) authorized for ubuntu") {
		t.Errorf("got:\n%s", printed(plan))
	}
}

// A site hosting the monitor lets its containers reach every app port on that
// site's own mesh address, once per port, because the monitor's direct checks
// to a local app arrive on a compose bridge rather than on wg0.
func TestContainerRulesLetTheMonitorReachLocalApps(t *testing.T) {
	cfg := fixture(t)
	status := cfg.Apps["status"]
	status.Placement = config.Placement{Mode: config.PlacementPinned, Site: "home-a"}
	cfg.Apps["status"] = status
	homeA := cfg.Sites["home-a"]
	homeA.Roles = append(homeA.Roles, config.RoleMonitor)
	cfg.Sites["home-a"] = homeA
	var got []string
	for _, r := range hostprep.ContainerRules(cfg, "home-a") {
		if r.Why == "the uptime monitor to apps on this site" {
			if r.Interface != "br-+" || r.To != "10.44.0.1" || r.Proto != "tcp" {
				t.Errorf("rule is wider or other than a bridge to the mesh address: %s", r)
			}
			got = append(got, fmt.Sprint(r.Port))
		}
	}
	// home-a runs the clustered mbin (8080), outline (3000) and pocket-id
	// (1411), and the pinned writefreely (8081) and oauth2-proxy (4180).
	// 3001 is absent: the monitor does not check itself.
	if want := "1411,3000,4180,8080,8081"; strings.Join(got, ",") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
	for _, r := range hostprep.ContainerRules(cfg, "home-b") {
		if r.Why == "the uptime monitor to apps on this site" {
			t.Errorf("home-b hosts no monitor and was given a monitor rule: %s", r)
		}
	}
}

// Docker starts after wg0 at boot, or a container publishing a port on the
// mesh address fails to bind it and is never retried. The drop-in is written
// when missing or different, and installing it reloads systemd without
// restarting Docker, so no running container is touched.
func TestDockerIsOrderedAfterWG0(t *testing.T) {
	for _, current := range []string{"", "[Unit]\nAfter=network.target\n"} {
		host := preparedHost(false)
		if current == "" {
			delete(host.files, dockerDropIn)
		} else {
			host.files[dockerDropIn] = current
		}
		plan, err := hostprep.Build("home-a", fixture(t), host)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Steps) != 1 || plan.Steps[0].File == nil || plan.Steps[0].File.Path != dockerDropIn {
			t.Fatalf("want only the drop-in planned, got %+v", plan.Steps)
		}
		step := plan.Steps[0]
		if !strings.Contains(step.File.Content, "After=wg-quick@wg0.service") {
			t.Errorf("the drop-in does not order docker after wg0:\n%s", step.File.Content)
		}
		if step.Command != "systemctl daemon-reload" {
			t.Errorf("installing the drop-in runs %q; it must not restart docker", step.Command)
		}
	}
}

// On a shared host, prepare adds and removes only its own commented rules.
// The default policy and enabling ufw are host wide, and something else
// lives there whose traffic they would decide.
func TestASharedHostNeverSetsTheFirewallsDefaults(t *testing.T) {
	cfg := fixture(t)
	host := freshHost()
	host.responses[probeFirewall] = "ufw present\nstatus inactive\n" + owned("allow 9999/tcp")
	host.files["/etc/default/ufw"] = "DEFAULT_INPUT_POLICY=\"ACCEPT\"\nDEFAULT_OUTPUT_POLICY=\"DROP\"\n"
	dedicated, err := hostprep.Build("home-a", cfg, host)
	if err != nil {
		t.Fatal(err)
	}
	if indexOf(commands(dedicated), "ufw --force enable") < 0 || indexOf(commands(dedicated), "ufw default deny incoming") < 0 {
		t.Fatalf("the dedicated plan does not set the defaults, so this test proves nothing:\n%s", strings.Join(commands(dedicated), "\n"))
	}
	shared, err := hostprep.Build("home-a", cfg, host, hostprep.Shared())
	if err != nil {
		t.Fatal(err)
	}
	cmds := commands(shared)
	for _, c := range cmds {
		if strings.HasPrefix(c, "ufw default") || strings.Contains(c, "ufw --force enable") || c == "ufw enable" {
			t.Errorf("a shared host's plan runs %q", c)
		}
	}
	for _, want := range []string{
		"ufw allow 22/tcp comment 'paisans-f2a9: ssh, the bootstrap route'",
		"ufw delete allow 9999/tcp comment 'paisans-f2a9: test'",
	} {
		if indexOf(cmds, want) < 0 {
			t.Errorf("a shared host's plan does not manage its own rules, missing %q:\n%s", want, strings.Join(cmds, "\n"))
		}
	}
	if !strings.Contains(printed(shared), "default policy and enabled state left alone") {
		t.Errorf("the plan does not say the defaults were left alone:\n%s", printed(shared))
	}
}

// Docker installed as a snap is refused, not removed, as Ubuntu's docker.io
// is, and whether or not `docker compose` works with it: the toolkit runs
// Docker from Docker's own repository on every host. Removing it would stop
// whatever runs from it.
func TestSnapDockerIsRefusedNotRemoved(t *testing.T) {
	for _, compose := range []string{"compose present", "compose absent"} {
		host := freshHost()
		host.responses[probePackages] = compose + "\nsnap docker\npkg ufw install ok installed\nkeyring absent\narch amd64\n"
		_, err := hostprep.Build("home-a", fixture(t), host)
		if err == nil || !strings.Contains(err.Error(), "snap") || !strings.Contains(err.Error(), "snap remove docker") {
			t.Errorf("%s: got %v", compose, err)
		}
		for _, c := range host.ran {
			if strings.Contains(c, "snap remove") || strings.Contains(c, "apt-get") {
				t.Errorf("%s: prepare changed something: %s", compose, c)
			}
		}
	}
}

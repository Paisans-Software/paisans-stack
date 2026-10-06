package hostprep_test

import (
	"bytes"
	"errors"
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
	probeUnit     = "is-enabled paisans-watchdog.service"
	probeFirewall = "ufw show added"
)

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
// WireGuard tools, no ufw, no watchdog device.
func freshHost() *fakeHost {
	return &fakeHost{
		files: map[string]string{"/etc/os-release": ubuntu2404},
		responses: map[string]string{
			probePackages: "compose absent\npkg docker-ce absent\npkg wireguard-tools absent\npkg ufw absent\nkeyring absent\narch amd64\n",
			probeWatchdog: "device absent\nsystemd 0\ndaemon absent\n",
			probeDocker:   "enabled \nactive \n",
			probeUnit:     "enabled \nactive \n",
			probeFirewall: "ufw absent\n",
		},
	}
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
func preparedHost(gateway bool) *fakeHost {
	firewall := "ufw present\nstatus active\n" + owned("allow 22/tcp", "allow 51820/udp", "allow in on wg0")
	if !gateway {
		firewall += owned("allow in on br-+ to 10.44.0.1 port 5000 proto tcp", "allow in on br-+ to 10.44.0.1 port 3900 proto tcp")
	}
	if gateway {
		firewall += owned("allow 80/tcp", "allow 443/tcp")
	}
	return &fakeHost{
		files: map[string]string{
			"/etc/os-release":                        ubuntu2404,
			"/etc/default/ufw":                       "IPV6=yes\nDEFAULT_INPUT_POLICY=\"DROP\"\nDEFAULT_OUTPUT_POLICY=\"ACCEPT\"\n",
			"/etc/apt/sources.list.d/docker.sources": dockerSources,
		},
		responses: map[string]string{
			probePackages: "compose present\npkg docker-ce install ok installed\npkg wireguard-tools install ok installed\npkg ufw install ok installed\nkeyring present\narch amd64\n",
			probeWatchdog: "device present\nidentity watchdog0 iTCO_wdt\nsystemd 0\ndaemon absent\n",
			probeDocker:   "enabled enabled\nactive active\n",
			probeUnit:     "enabled \nactive \n",
			probeFirewall: firewall,
		},
	}
}

// owned is how `ufw show added` prints rules host prepare added, as probe
// lines. The comment's text after the tag is not compared, so any will do.
func owned(specs ...string) string {
	var out string
	for _, spec := range specs {
		out += "rule " + spec + " comment 'paisans: test'\n"
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
		"watchdog: load softdog at every boot (paisans-watchdog.service)",
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
	if len(host.written) != 2 {
		t.Errorf("want the apt source and the unit written, got %v", host.written)
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
	after.files["/etc/systemd/system/paisans-watchdog.service"] = host.files["/etc/systemd/system/paisans-watchdog.service"]
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
// nothing wider. A gateway only site runs no app containers and gets neither.
func TestContainerRulesReachOnlyWhatAppsUse(t *testing.T) {
	cfg := fixture(t)
	var got []string
	for _, r := range hostprep.ContainerRules(cfg, "home-a") {
		got = append(got, r.String())
	}
	if want := "5000/tcp on br-+ to 10.44.0.1,3900/tcp on br-+ to 10.44.0.1"; strings.Join(got, ",") != want {
		t.Errorf("home-a: got %s, want %s", strings.Join(got, ","), want)
	}
	if rules := hostprep.ContainerRules(cfg, "vm"); len(rules) != 0 {
		t.Errorf("a gateway only site was given container rules: %v", rules)
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
		"ufw allow 22/tcp comment 'paisans: ssh, the bootstrap route'",
		"ufw allow 51820/udp comment 'paisans: WireGuard, the mesh'",
		"ufw allow in on wg0 comment 'paisans: the mesh: etcd, Patroni, Garage, HAProxy'",
		"ufw allow in on br-+ to 10.44.0.1 port 5000 proto tcp comment 'paisans: app containers to the local database proxy'",
	} {
		if indexOf(cmds, want) < 0 {
			t.Errorf("missing %q in:\n%s", want, strings.Join(cmds, "\n"))
		}
	}
	for _, c := range cmds {
		if strings.HasPrefix(c, "ufw allow") && !strings.Contains(c, " comment 'paisans: ") {
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
		"ufw delete allow 80/tcp comment 'paisans: test'",
		"ufw delete allow 443/tcp comment 'paisans: test'",
	}
	if strings.Join(cmds, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(cmds, "\n"), strings.Join(want, "\n"))
	}
	for _, s := range plan.Steps[1:] {
		if s.Label != "remove" {
			t.Errorf("a removal is labelled %q: %s", s.Label, s.Describe)
		}
	}
	if !strings.Contains(printed(plan), "  remove    firewall: delete `ufw allow 80/tcp comment 'paisans: test'`") {
		t.Errorf("the plan does not show the removal as remove:\n%s", printed(plan))
	}
}

// Rules host prepare did not add are never removed or changed. One that bears
// on a derived rule is listed; one that does not is not mentioned at all.
func TestAForeignRuleIsNeverTouched(t *testing.T) {
	cfg := fixture(t)
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
	plan, err := hostprep.Build("vm", fixture(t), host)
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
	plan, err := hostprep.Build("vm", fixture(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 0 || len(plan.Warnings) != 2 {
		t.Fatalf("got:\n%s", printed(plan))
	}

	host.responses[probeFirewall] = "ufw present\nstatus inactive\nrule deny 22/tcp\n"
	if _, err := hostprep.Build("vm", fixture(t), host); err == nil || !strings.Contains(err.Error(), "blocks SSH") {
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
		if s.Label != "adopt" || !strings.HasPrefix(s.Command, "ufw allow ") || !strings.Contains(s.Command, " comment 'paisans: ") {
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
		"rule allow 22/tcp comment 'paisans: ssh, the bootstrap route'\n" +
		owned("allow 51820/udp", "allow in on wg0", "allow in on br-+ to 10.44.0.1 port 5000 proto tcp", "allow in on br-+ to 10.44.0.1 port 3900 proto tcp")
	plan, err := hostprep.Build("home-a", withSSHPort(t, "home-a", 2222), host)
	if err != nil {
		t.Fatal(err)
	}
	out := printed(plan)
	t.Logf("\n%s", out)
	if got := commands(plan); len(got) != 1 || got[0] != "ufw allow 2222/tcp comment 'paisans: ssh, the bootstrap route'" {
		t.Fatalf("want only the new allow, got %q", got)
	}
	if !strings.Contains(out, "present   firewall: `ufw allow 22/tcp comment 'paisans: ssh, the bootstrap route'` kept; it is the SSH allow for an earlier ssh.port, and host prepare never removes an SSH allow. Delete it yourself once SSH on 2222 works") {
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

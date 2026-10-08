package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `version: 1
id: f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01
community:
  name: "Fixture"
  domain: example.org
mesh:
  subnet: 10.44.0.0/24
sites:
  home-a:
    roles: [data, apps]
    address: 10.44.0.1
    ssh:
      host: home-a.local
      user: ubuntu
      public_key: |
        ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
etcd:
  members: [home-a]
apps:
  talk:
    kind: mbin
    hostname: talk.example.org
    placement: cluster
`

// validConfig is a minimal deployment that loads cleanly: a site holding
// data, apps and gateway, an address inside the mesh subnet, an ssh address,
// and the acme block a gateway requires.
const validConfig = `version: 1
id: f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01
community:
  name: "Fixture"
  domain: example.org
mesh:
  subnet: 10.44.0.0/24
acme:
  provider: desec
sites:
  home-a:
    roles: [data, apps, gateway]
    address: 10.44.0.1
    ssh:
      host: home-a.local
      user: ubuntu
      public_key: |
        ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
etcd:
  members: [home-a]
apps:
  talk:
    kind: mbin
    hostname: talk.example.org
    placement: cluster
`

func TestLoadsAMinimalFile(t *testing.T) {
	cfg, err := config.Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Apps["talk"].Placement.Mode != config.PlacementCluster {
		t.Fatalf("placement parsed as %q", cfg.Apps["talk"].Placement.Mode)
	}
	if !cfg.Sites["home-a"].Has(config.RoleData) {
		t.Fatal("the data role was not parsed")
	}
}

func TestPinnedPlacement(t *testing.T) {
	body := strings.Replace(minimal, "placement: cluster", "placement: { pinned: home-a }", 1)
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	placement := cfg.Apps["talk"].Placement
	if placement.Mode != config.PlacementPinned || placement.Site != "home-a" {
		t.Fatalf("pinned placement parsed as %+v", placement)
	}
}

// An unrecognised placement is carried through the decode rather than
// aborting it, so that validate can report it beside every other problem in
// the file.
func TestInvalidPlacementSurvivesTheDecode(t *testing.T) {
	body := strings.Replace(minimal, "placement: cluster", "placement: standby", 1)
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Apps["talk"].Placement.Mode != config.PlacementInvalid {
		t.Fatal("an invalid placement was not recorded as invalid")
	}
}

// Garage requires a unit suffix on the capacity it advertises to its layout,
// so an unset value has to become a usable one rather than an empty string
// that would break the first `layout assign`.
func TestGarageCapacityDefaultsTo100G(t *testing.T) {
	cfg, err := config.Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Garage.Capacity != "100G" {
		t.Fatalf("capacity defaulted to %q, want 100G", cfg.Storage.Garage.Capacity)
	}
}

// An explicit capacity is the operator's call, made because the toolkit
// cannot know how much of a node's disk is meant for objects, and the default
// must never override it.
func TestGarageCapacitySurvivesWhenDeclared(t *testing.T) {
	body := minimal + "storage:\n  garage:\n    capacity: 750G\n"
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Garage.Capacity != "750G" {
		t.Fatalf("capacity was %q, want the declared 750G", cfg.Storage.Garage.Capacity)
	}
}

// Every structural problem in a file is reported at once. Fixing a
// configuration one error per run is miserable.
func TestStructuralProblemsAreReportedTogether(t *testing.T) {
	body := `version: 2
community:
  name: "Fixture"
mesh:
  subnet: 10.44.0.5/24
sites:
  home-a:
    roles: [data, wizard]
    address: not-an-address
apps:
  talk:
    kind: gopher
    hostname: ""
`
	_, err := config.Load(write(t, body))
	if err == nil {
		t.Fatal("a broken file loaded cleanly")
	}
	msg := err.Error()
	for _, want := range []string{
		"version:", "community.domain:", "mesh.subnet:", "sites.home-a.roles:", "sites.home-a.address:",
		"sites.home-a.ssh:", "apps.talk.kind:", "apps.talk.hostname:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not name %s:\n%s", want, msg)
		}
	}
}

// Unknown keys are refused. trusted_proxies is deliberately not in the schema,
// and a typo that silently does nothing is worse than a refusal.
func TestUnknownKeysAreRefused(t *testing.T) {
	body := minimal + "trusted_proxies: 10.44.0.1\n"
	_, err := config.Load(write(t, body))
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if !strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("the error does not name the offending key:\n%v", err)
	}
}

// Certificates are a deployment wide fact rather than a gateway site's
// property, because the gateway role moves between machines by design. A
// deployment with a gateway and no provider cannot obtain a certificate, and
// that is a missing required field rather than a policy question.
func TestACMEProviderIsRequiredWhenAGatewayExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	body := strings.Replace(validConfig, "acme:\n  provider: desec\n", "", 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("a deployment with a gateway and no acme provider loaded")
	}
	if !strings.Contains(err.Error(), "acme.provider") {
		t.Errorf("the error does not name the missing key:\n%v", err)
	}
}

// The provider is read as declared, and an override image is optional.
func TestACMEBlockLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACME.Provider != "desec" {
		t.Errorf("provider is %q, want desec", cfg.ACME.Provider)
	}
	if cfg.ACME.Image != "" {
		t.Errorf("image is %q, and the fixture declares none", cfg.ACME.Image)
	}
}

// The shipped example must load. It is the documentation of the format.
func TestExampleLoads(t *testing.T) {
	if _, err := config.Load(filepath.Join("..", "..", "examples", "paisans.example.yaml")); err != nil {
		t.Fatal(err)
	}
}

// A site that says nothing about its watchdog gets auto, which uses whatever
// device the host has. Making every operator write the default out would put a
// line in every site block that nobody reads.
func TestWatchdogDefaultsToAuto(t *testing.T) {
	cfg, err := config.Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sites["home-a"].WatchdogMode(); got != config.WatchdogAuto {
		t.Fatalf("an undeclared watchdog is %q, want auto", got)
	}
}

// A misspelt mode is refused at load rather than read as auto, because auto
// on a host with no device loads a module the operator may have meant to
// rule out.
func TestUnknownWatchdogModeIsRefused(t *testing.T) {
	body := strings.Replace(minimal, "    address: 10.44.0.1\n", "    address: 10.44.0.1\n    watchdog: hardware\n", 1)
	_, err := config.Load(write(t, body))
	if err == nil || !strings.Contains(err.Error(), "sites.home-a.watchdog") {
		t.Fatalf("an unknown watchdog mode loaded: %v", err)
	}
	for _, mode := range []string{"auto", "required", "softdog", "off"} {
		body := strings.Replace(minimal, "    address: 10.44.0.1\n", "    address: 10.44.0.1\n    watchdog: "+mode+"\n", 1)
		if _, err := config.Load(write(t, body)); err != nil {
			t.Errorf("watchdog %s was refused: %v", mode, err)
		}
	}
}

// Obviously fake keys: ed25519 keys of all zero, all one and all two bytes.
const (
	fakeKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org"
	fakeKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB bob@example.org"
)

// withSSH replaces minimal's ssh section with the given one, indented as a
// site key.
func withSSH(section string) string {
	start := strings.Index(minimal, "    ssh:\n")
	end := strings.Index(minimal, "etcd:\n")
	return minimal[:start] + section + minimal[end:]
}

func loadErr(t *testing.T, body string) string {
	t.Helper()
	_, err := config.Load(write(t, body))
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestSSHSectionLoads(t *testing.T) {
	body := withSSH("    ssh:\n      host: 203.0.113.10\n      user: ubuntu\n      port: 2222\n      public_key: |\n        " + fakeKeyA + "\n\n        " + fakeKeyB + "\n")
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	site := cfg.Sites["home-a"]
	if site.SSHHost() != "203.0.113.10" || site.SSH.User != "ubuntu" || site.SSH.PortOrDefault() != 2222 {
		t.Fatalf("section read as %+v", site.SSH)
	}
	keys, problems := site.SSH.Keys()
	if len(problems) > 0 || len(keys) != 2 {
		t.Fatalf("keys %v, problems %v", keys, problems)
	}
	if keys[0].Line != fakeKeyA || keys[0].Comment != "alice@example.org" || !strings.HasPrefix(keys[0].Fingerprint, "SHA256:") {
		t.Errorf("first key read as %+v", keys[0])
	}
}

func TestSSHDefaultsPortAndHost(t *testing.T) {
	body := withSSH("    public_address: 203.0.113.20\n    ssh:\n      user: ubuntu\n      public_key: " + fakeKeyA + "\n")
	cfg, err := config.Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	site := cfg.Sites["home-a"]
	if site.SSHHost() != "203.0.113.20" {
		t.Errorf("host defaulted to %q, want the public address", site.SSHHost())
	}
	if site.SSH.PortOrDefault() != 22 {
		t.Errorf("port defaulted to %d, want 22", site.SSH.PortOrDefault())
	}
}

// The old destination string is refused, and the refusal shows the section
// that replaces it, with the host carried over.
func TestSSHStringFormIsRefusedWithTheSection(t *testing.T) {
	msg := loadErr(t, withSSH("    ssh: ubuntu@home-a.local\n"))
	for _, want := range []string{"sites.home-a.ssh", "old destination form", "host: home-a.local", "user:", "public_key: |"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q:\n%s", want, msg)
		}
	}
}

func TestSSHSectionRefusals(t *testing.T) {
	key := "      public_key: " + fakeKeyA + "\n"
	cases := []struct {
		name, section, want string
	}{
		{"missing", "", "sites.home-a.ssh: required"},
		{"no user", "    ssh:\n      host: home-a.local\n" + key, "ssh.user: required"},
		{"bad user", "    ssh:\n      host: home-a.local\n      user: \"bob; rm\"\n" + key, "is not a user name"},
		{"port too high", "    ssh:\n      host: home-a.local\n      user: ubuntu\n      port: 70000\n" + key, "ssh.port: 70000 is not a port"},
		{"negative port", "    ssh:\n      host: home-a.local\n      user: ubuntu\n      port: -1\n" + key, "ssh.port: -1 is not a port"},
		{"no host or public address", "    ssh:\n      user: ubuntu\n" + key, "ssh.host: required"},
		{"user in host", "    ssh:\n      host: ubuntu@home-a.local\n      user: ubuntu\n" + key, "neither a hostname nor an IP address"},
		{"no key", "    ssh:\n      host: home-a.local\n      user: ubuntu\n", "ssh.public_key: required"},
		{"not a key", "    ssh:\n      host: home-a.local\n      user: ubuntu\n      public_key: ssh-ed25519 not-base64\n", "line 1 is not an OpenSSH public key"},
		{"private key path", "    ssh:\n      host: home-a.local\n      user: ubuntu\n      public_key: ~/.ssh/id_ed25519\n", "not an OpenSSH public key"},
		{"options", "    ssh:\n      host: home-a.local\n      user: ubuntu\n      public_key: from=\"203.0.113.0/24\" " + fakeKeyA + "\n", "carries options"},
		{"duplicate", "    ssh:\n      host: home-a.local\n      user: ubuntu\n      public_key: |\n        " + fakeKeyA + "\n        " + strings.Replace(fakeKeyA, "alice", "alice-laptop", 1) + "\n", "line 2 is the same key as line 1"},
		{"unknown key", "    ssh:\n      host: home-a.local\n      user: ubuntu\n      public_keys: " + fakeKeyA + "\n", "field public_keys not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := loadErr(t, withSSH(c.section))
			if !strings.Contains(msg, c.want) {
				t.Errorf("want %q in the refusal, got:\n%s", c.want, msg)
			}
		})
	}
}

func TestSSHHostAcceptsHostnamesAndAddresses(t *testing.T) {
	for _, host := range []string{"home-a.local", "vm.example.org", "203.0.113.10", "2001:db8::10", "home-a"} {
		body := withSSH("    ssh:\n      host: \"" + host + "\"\n      user: ubuntu\n      public_key: " + fakeKeyA + "\n")
		if msg := loadErr(t, body); msg != "" {
			t.Errorf("host %s refused: %s", host, msg)
		}
	}
}

const smtpBlock = `smtp:
  host: smtp.example.org
  port: 587
  security: starttls
  username: robot@example.org
  from_address: hello@example.org
  from_name: Fixture
`

// statusApp is an uptime app pinned to site, appended to validConfig's apps.
func statusApp(site, extra string) string {
	return "  status:\n    kind: uptime\n    hostname: status.example.org\n    placement: { pinned: " + site + " }\n" + extra
}

// An app's smtp block replaces only the fields it names; everything else is
// the deployment's, so a different sender does not mean repeating the host.
func TestSMTPForInheritsAndOverridesFieldByField(t *testing.T) {
	cfg, err := config.Load(write(t, validConfig+statusApp("home-a", "    smtp:\n      from_address: alerts@example.org\n      security: tls\n")+smtpBlock))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.SMTPFor("status")
	want := config.SMTP{Host: "smtp.example.org", Port: 587, Security: "tls", Username: "robot@example.org", FromAddress: "alerts@example.org", FromName: "Fixture"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if cfg.SMTPFor("talk") != cfg.SMTP {
		t.Fatal("an app without an override did not get the deployment's block")
	}
}

func TestSMTPPortDefaultsBySecurity(t *testing.T) {
	if p := (config.SMTP{Security: "starttls"}).PortOrDefault(); p != 587 {
		t.Fatalf("starttls: %d", p)
	}
	if p := (config.SMTP{Security: "tls"}).PortOrDefault(); p != 465 {
		t.Fatalf("tls: %d", p)
	}
	if p := (config.SMTP{Security: "tls", Port: 2465}).PortOrDefault(); p != 2465 {
		t.Fatalf("declared: %d", p)
	}
}

func TestSMTPShapeIsChecked(t *testing.T) {
	body := validConfig + statusApp("home-a", "    smtp:\n      security: ssl\n") + "smtp:\n  host: smtp.example.org\n  security: none\n  port: 70000\n"
	_, err := config.Load(write(t, body))
	if err == nil {
		t.Fatal("loaded")
	}
	for _, want := range []string{"smtp.security", "smtp.port", "apps.status.smtp.security"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no problem named %s in %v", want, err)
		}
	}
}

// A site may declare no roles only when an app is pinned to it: then it
// exists to host that app. With nothing pinned it is almost certainly a
// mistake.
func TestARolelessSiteIsAcceptedOnlyWhenSomethingIsPinnedToIt(t *testing.T) {
	site := "  mon:\n    roles: []\n    address: 10.44.0.9\n    ssh:\n      host: mon.local\n      user: ubuntu\n      public_key: |\n        ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org\n"
	withSite := strings.Replace(validConfig, "etcd:\n", site+"etcd:\n", 1)
	if _, err := config.Load(write(t, withSite)); err == nil || !strings.Contains(err.Error(), "sites.mon.roles") {
		t.Fatalf("a role-less site with nothing pinned to it loaded: %v", err)
	}
	if _, err := config.Load(write(t, withSite+statusApp("mon", ""))); err != nil {
		t.Fatalf("a role-less site hosting a pinned app was refused: %v", err)
	}
}

// monitorSite is validConfig with a watch site holding the monitor role and
// extra appended to its entry, before the uptime app is pinned there.
func monitorSite(extra string) string {
	site := "  watch:\n    roles: [monitor]\n    address: 10.44.0.9\n    public_address: 203.0.113.20\n    ssh:\n      host: watch.local\n      user: ubuntu\n      public_key: |\n        ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org\n" + extra
	return strings.Replace(validConfig, "etcd:\n", site+"etcd:\n", 1) + statusApp("watch", "")
}

func TestAMonitorSiteLoadsWithIngress(t *testing.T) {
	cfg, err := config.Load(write(t, monitorSite("    ingress:\n      mode: external\n      listen: 127.0.0.1:8480\n")))
	if err != nil {
		t.Fatal(err)
	}
	watch := cfg.Sites["watch"]
	if !watch.Has(config.RoleMonitor) || watch.IngressMode() != config.IngressExternal {
		t.Fatalf("watch: %+v", watch)
	}
	host, port, ok := watch.Ingress.ListenHostPort()
	if !ok || host != "127.0.0.1" || port != 8480 {
		t.Fatalf("listen: %s %d %v", host, port, ok)
	}
	if watch.RunsCaddy() {
		t.Fatal("an external monitor runs no Caddy")
	}
	if got := cfg.MonitorSites(); len(got) != 1 || got[0] != "watch" {
		t.Fatalf("monitor sites: %v", got)
	}
}

func TestIngressModeDefaultsToPaisansAndRunsCaddy(t *testing.T) {
	cfg, err := config.Load(write(t, monitorSite("")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sites["watch"].IngressMode(); got != config.IngressPaisans || !cfg.Sites["watch"].RunsCaddy() {
		t.Fatalf("mode %s, caddy %v", got, cfg.Sites["watch"].RunsCaddy())
	}
	if !cfg.Sites["home-a"].RunsCaddy() {
		t.Fatal("the gateway runs no Caddy")
	}
}

func TestIngressShapeIsChecked(t *testing.T) {
	for _, tc := range []struct{ ingress, want string }{
		{"    ingress:\n      mode: nginx\n", "sites.watch.ingress.mode: unknown mode"},
		{"    ingress:\n      mode: external\n      listen: localhost:8480\n", "sites.watch.ingress.listen"},
		{"    ingress:\n      mode: external\n      listen: 127.0.0.1:0\n", "sites.watch.ingress.listen"},
		{"    ingress:\n      mode: external\n      listen: 127.0.0.1\n", "sites.watch.ingress.listen"},
		{"    ingress:\n      proxy: nginx\n", "field proxy not found"},
	} {
		if msg := loadErr(t, monitorSite(tc.ingress)); !strings.Contains(msg, tc.want) {
			t.Errorf("%q: %s", tc.ingress, msg)
		}
	}
}

// acme.provider names the module the toolkit's Caddy answers DNS-01 with, so
// it is required wherever that Caddy runs: a gateway, or a monitor serving
// its own apps. A monitor behind the operator's web server runs none.
func TestACMEProviderIsRequiredForAMonitorsOwnCaddy(t *testing.T) {
	strip := func(body string) string {
		body = strings.Replace(body, "acme:\n  provider: desec\n", "", 1)
		return strings.Replace(body, "roles: [data, apps, gateway]", "roles: [data, apps]", 1)
	}
	if msg := loadErr(t, strip(monitorSite(""))); !strings.Contains(msg, "acme.provider: required") {
		t.Fatalf("paisans mode: %s", msg)
	}
	if _, err := config.Load(write(t, strip(monitorSite("    ingress:\n      mode: external\n      listen: 127.0.0.1:8480\n")))); err != nil {
		t.Fatalf("external mode needs no provider: %v", err)
	}
}

// visibility_gate takes three values, and an absent key and `public` mean the
// same thing, so nothing downstream compares against both.
func TestVisibilityGateValues(t *testing.T) {
	for value, want := range map[string]string{"public": "", "member": "member", "provisional": "provisional"} {
		cfg, err := config.Load(write(t, minimal+"    visibility_gate: "+value+"\n"))
		if err != nil {
			t.Fatalf("visibility_gate: %s did not load: %v", value, err)
		}
		if got := cfg.Apps["talk"].Gate(); got != want {
			t.Errorf("visibility_gate: %s is gate %q, want %q", value, got, want)
		}
	}
	cfg, err := config.Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Apps["talk"].Gate(); got != "" {
		t.Errorf("an absent visibility_gate is gate %q, want public", got)
	}
	_, err = config.Load(write(t, minimal+"    visibility_gate: members\n"))
	if err == nil || !strings.Contains(err.Error(), "apps.talk.visibility_gate: unknown value") {
		t.Errorf("visibility_gate: members loaded, or was refused without naming the key: %v", err)
	}
}

// The key this replaced is not read. A deployment is redeployed with the new
// key rather than converted, and an old file fails loudly instead of loading
// with its gate silently dropped.
func TestTheOldGateKeyIsRefused(t *testing.T) {
	_, err := config.Load(write(t, minimal+"    gate: members\n"))
	if err == nil {
		t.Fatal("an app with the old gate key loaded, and would render ungated")
	}
	if !strings.Contains(err.Error(), "gate") {
		t.Fatalf("the error does not name the key:\n%v", err)
	}
}

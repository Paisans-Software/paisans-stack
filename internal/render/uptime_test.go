package render_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

type seedFile struct {
	Settings map[string]any   `json:"settings"`
	Monitors []map[string]any `json:"monitors"`
}

// withMonitor is the fixture with its uptime app on the watch monitor site,
// the secrets it needs, and the given smtp blocks (deployment wide, then the
// app's own).
func withMonitor(t *testing.T, smtp config.SMTP, own *config.SMTP) (*config.Config, *config.Secrets) {
	t.Helper()
	cfg := fixture(t)
	cfg.SMTP = smtp
	cfg.Apps["status"] = config.App{Kind: config.KindUptime, Hostname: "status.example.org",
		Placement: config.Placement{Mode: config.PlacementPinned, Site: "watch"},
		Settings:  map[string]any{"admin_group": "admins"}, SMTP: own}
	secrets := fixtureSecrets(t)
	if secrets.Apps == nil {
		secrets.Apps = map[string]map[string]any{}
	}
	secrets.Apps["status"] = map[string]any{"admin_password": "fixture-admin", "session_secret": "fixture-session"}
	return cfg, secrets
}

func renderSeed(t *testing.T, cfg *config.Config, secrets *config.Secrets) seedFile {
	t.Helper()
	return renderSeedAt(t, cfg, secrets, "watch/srv/paisans/f2a9/status/monitors.json")
}

// renderSeedAt is renderSeed for the seed at path, for a deployment with more
// than one monitor.
func renderSeedAt(t *testing.T, cfg *config.Config, secrets *config.Secrets, path string) seedFile {
	t.Helper()
	plan, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range plan.Files {
		if f.Path != path {
			continue
		}
		if f.Mode != 0o600 {
			t.Errorf("the seed carries the SMTP password and was rendered %o", f.Mode)
		}
		var seed seedFile
		if err := json.Unmarshal([]byte(f.Content), &seed); err != nil {
			t.Fatalf("the seed is not JSON: %v\n%s", err, f.Content)
		}
		return seed
	}
	t.Fatalf("no %s was rendered", path)
	return seedFile{}
}

func byName(seed seedFile) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, m := range seed.Monitors {
		out[m["name"].(string)] = m
	}
	return out
}

// Every app other than the monitor gets a public check at its hostname and a
// direct check per site it runs on, both on the kind's health route; every
// other site gets a ping; the monitor checks neither its own container nor
// its own site.
func TestTheSeedChecksEveryAppTwiceAndPingsEveryOtherSite(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	monitors := byName(renderSeed(t, cfg, secrets))

	talk := monitors["talk — public"]
	if talk == nil || talk["url"] != "https://talk.example.org/" || talk["follow_redirects"] != false {
		t.Fatalf("talk's public check: %v", talk)
	}
	// talk is behind the members gate: forward_auth answers 401 to a request
	// with no session, before the app is asked.
	if talk["expected_status"] != "401" {
		t.Errorf("talk public expects %v", talk["expected_status"])
	}
	auth := monitors["auth — public"]
	if auth == nil || auth["url"] != "https://"+cfg.Apps["auth"].Hostname+"/healthz" || auth["expected_status"] != "204" {
		t.Errorf("pocket-id public check: %v", auth)
	}
	for _, site := range []string{"home-a", "home-b"} {
		d := monitors["talk — direct ("+site+")"]
		if d == nil {
			t.Fatalf("clustered talk has no direct check on %s", site)
		}
		if want := "http://" + cfg.Sites[site].Address + ":8080/"; d["url"] != want {
			t.Errorf("direct %s: url %v, want %s", site, d["url"], want)
		}
		headers, _ := d["request_headers"].(map[string]any)
		if headers["Host"] != "talk.example.org" || headers["X-Forwarded-Proto"] != "https" {
			t.Errorf("direct %s: headers %v", site, headers)
		}
		if d["expected_status"] != "200,302" {
			t.Errorf("direct %s: a direct check bypasses the gate and expects the kind's own codes, got %v", site, d["expected_status"])
		}
	}
	for _, name := range []string{"home-a — ping", "home-b — ping", "vm — ping"} {
		if m := monitors[name]; m == nil || m["monitor_type"] != "ping" {
			t.Errorf("%s: %v", name, m)
		}
	}
	if monitors["watch — ping"] != nil {
		t.Error("the monitor pings its own site")
	}
	for name, m := range monitors {
		if url, _ := m["url"].(string); strings.Contains(url, ":3001") {
			t.Errorf("the monitor checks its own container: %s %s", name, url)
		}
	}
}

// A gated app's public check expects the gate's own answer to a request with
// no session, 401 (oauth2-proxy AuthOnly, oauthproxy.go:1018-1022 at v7.15.4,
// copied back by Caddy's forward_auth): it proves the edge and the gate, and
// the direct check proves the app. An ungated app's expects the kind's codes.
func TestAGatedAppsPublicCheckExpectsTheGatesRefusal(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	monitors := byName(renderSeed(t, cfg, secrets))
	if got := monitors["talk — public"]["expected_status"]; got != "401" {
		t.Errorf("gated mbin public expects %v", got)
	}
	if got := monitors["docs — public"]["expected_status"]; got != "200" {
		t.Errorf("ungated outline public expects %v", got)
	}
	docs := cfg.Apps["docs"]
	docs.Gate = "none"
	cfg.Apps["docs"] = docs
	if got := byName(renderSeed(t, cfg, secrets))["docs — public"]["expected_status"]; got != "200" {
		t.Errorf("gate: none is no gate, yet outline public expects %v", got)
	}
	docs.Gate = "provisional"
	cfg.Apps["docs"] = docs
	if got := byName(renderSeed(t, cfg, secrets))["docs — public"]["expected_status"]; got != "401" {
		t.Errorf("gated outline public expects %v", got)
	}
	if got := monitors["docs — direct (home-a)"]["url"]; got != "http://"+cfg.Sites["home-a"].Address+":3000/_health" {
		t.Errorf("outline direct url %v", got)
	}
}

// The status page is switched off from the seed, and SMTP is carried only
// when a host resolves, with the app's own fields and password winning.
func TestTheSeedCarriesSettings(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	settings := renderSeed(t, cfg, secrets).Settings
	if len(settings) != 1 || settings["status_page_enabled"] != false {
		t.Fatalf("with no smtp the settings are %v", settings)
	}

	cfg, secrets = withMonitor(t,
		config.SMTP{Host: "smtp.example.org", Security: "starttls", Username: "robot", FromAddress: "hello@example.org", FromName: "Example"},
		&config.SMTP{Security: "tls", FromName: "Example Status"})
	secrets.External["smtp_password"] = "shared"
	secrets.Apps["status"]["smtp_password"] = "own"
	settings = renderSeed(t, cfg, secrets).Settings
	want := map[string]any{"status_page_enabled": false, "smtp_host": "smtp.example.org", "smtp_port": float64(465),
		"smtp_secure": true, "smtp_user": "robot", "smtp_pass": "own", "smtp_from_address": "hello@example.org", "smtp_from_name": "Example Status"}
	for k, v := range want {
		if settings[k] != v {
			t.Errorf("settings.%s = %v, want %v", k, settings[k], v)
		}
	}
}

// Monitors are named after app and site keys, never hostnames, so renaming a
// hostname updates a monitor in place instead of replacing it and losing the
// channels admins attached.
func TestMonitorNamesSurviveAHostnameChange(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	before := byName(renderSeed(t, cfg, secrets))
	talk := cfg.Apps["talk"]
	talk.Hostname = "forum.example.org"
	cfg.Apps["talk"] = talk
	after := byName(renderSeed(t, cfg, secrets))
	if len(before) != len(after) {
		t.Fatalf("%d monitors became %d", len(before), len(after))
	}
	for name := range before {
		if after[name] == nil {
			t.Errorf("%s disappeared", name)
		}
	}
	if after["talk — public"]["url"] != "https://forum.example.org/" {
		t.Errorf("the renamed check still points at %v", after["talk — public"]["url"])
	}
}

// The homeserver's public check must reach Synapse through the gateway, which
// routes only /_matrix/* and /_synapse/* to it and everything else to MAS,
// whose listener has no health resource.
func TestTheHomeserversChecksUseARouteTheGatewaySendsToSynapse(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	monitors := byName(renderSeed(t, cfg, secrets))
	if got := monitors["chat — public"]["url"]; got != "https://chat.example.org/_matrix/client/versions" {
		t.Errorf("chat public url %v", got)
	}
	if got := monitors["chat — direct (vm)"]["url"]; got != "http://"+cfg.Sites["vm"].Address+":8008/_matrix/client/versions" {
		t.Errorf("chat direct url %v", got)
	}
}

// The edge refuses the token API, not the monitor's own UI: the dashboard's
// live refresh and the response time chart are session authenticated JSON
// under /api/sites (public/js/dashboard.js:115, site-detail.js:20 in the
// fork), while the token API is /api/v1/*.
func TestTheEdgeRefusesTheTokenAPIButNotTheUIsOwnJSON(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	plan, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	var snippet string
	for _, f := range plan.Files {
		if f.Path == "watch/srv/paisans/f2a9/infra/caddy/snippets/status.caddy" {
			snippet = f.Content
		}
	}
	matcher := ""
	for _, line := range strings.Split(snippet, "\n") {
		if strings.HasPrefix(line, "@refused ") {
			matcher = line
		}
	}
	for _, want := range []string{"/api/v1/*", "/metrics", "/status*", "/badge/*"} {
		if !strings.Contains(matcher, " "+want) {
			t.Errorf("the edge does not refuse %s: %q", want, matcher)
		}
	}
	if strings.Contains(matcher, " /api/*") {
		t.Errorf("the edge refuses the UI's own /api/sites JSON: %q", matcher)
	}
}

// Every site Pocket ID runs on gets a check on its admin reconciler, straight to
// the reconciler's port on the mesh, and nothing public: the gateway does not
// route the reconciler.
func TestEveryPocketIDSiteHasAnAdminReconcilerCheck(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	monitors := byName(renderSeed(t, cfg, secrets))
	sites := render.AppSites(cfg)["auth"]
	if len(sites) == 0 {
		t.Fatal("the fixture runs Pocket ID nowhere")
	}
	for _, site := range sites {
		m := monitors["auth — admin reconciler ("+site+")"]
		if m == nil {
			t.Fatalf("no admin reconciler check on %s", site)
		}
		if want := "http://" + cfg.Sites[site].Address + ":1412/healthz"; m["url"] != want || m["expected_status"] != "200" {
			t.Errorf("%s: url %v expects %v, want %s expecting 200", site, m["url"], m["expected_status"], want)
		}
		if _, ok := m["request_headers"]; ok {
			t.Errorf("%s: the reconciler needs no Host header: %v", site, m["request_headers"])
		}
	}
	for name := range monitors {
		if strings.Contains(name, "admin reconciler") && !strings.HasPrefix(name, "auth — admin reconciler (") {
			t.Errorf("an admin reconciler check for something other than Pocket ID: %s", name)
		}
	}
}

// The monitor cannot report its own death, but whenever it runs it can see
// the path in front of it: DNS, the web server and the certificate, whose
// expiry the fork warns about. So it checks its own public URL, and only that.
func TestTheMonitorChecksItsOwnPublicURL(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	self := byName(renderSeed(t, cfg, secrets))[render.PublicCheckName("status")]
	if self == nil || self["url"] != "https://status.example.org/healthz" || self["expected_status"] != "200" || self["follow_redirects"] != false {
		t.Fatalf("self check: %v", self)
	}
}

// With a second monitor on a second monitor site, each checks the other's
// public URL exactly as it checks its own, so a monitor host that dies is
// noticed by the one still running. Neither gains a second check of itself.
func TestEachMonitorChecksTheOthersPublicURL(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	watchB := cfg.Sites["watch"]
	watchB.Address = "10.44.0.5"
	watchB.Endpoint = "watch-b.example.org:51820"
	watchB.PublicAddress = "203.0.113.21"
	watchB.SSH.Host = "watch-b.example.org"
	cfg.Sites["watch-b"] = watchB
	cfg.Apps["status-b"] = config.App{Kind: config.KindUptime, Hostname: "status-b.example.org",
		Placement: config.Placement{Mode: config.PlacementPinned, Site: "watch-b"},
		Settings:  map[string]any{"admin_group": "admins"}}
	secrets.Sites["watch-b"] = config.SiteSecrets{WireGuardPrivateKey: "REREREREREREREREREREREREREREREREREREREREREQ="}
	secrets.Apps["status-b"] = map[string]any{"admin_password": "fixture-admin-b", "session_secret": "fixture-session-b"}
	if refusals := validate.Check(cfg).Refusals(); len(refusals) > 0 {
		t.Fatalf("two monitors is a configuration the toolkit refuses: %v", refusals)
	}

	for self, other := range map[string]string{"status": "status-b", "status-b": "status"} {
		site := cfg.Apps[self].Placement.Site
		seed := renderSeedAt(t, cfg, secrets, site+"/srv/paisans/f2a9/"+self+"/monitors.json")
		count := map[string]int{}
		for _, m := range seed.Monitors {
			count[m["name"].(string)]++
		}
		for _, name := range []string{render.PublicCheckName(self), render.PublicCheckName(other)} {
			if count[name] != 1 {
				t.Errorf("%s seeds %q %d times", self, name, count[name])
			}
		}
		m := byName(seed)[render.PublicCheckName(other)]
		want := "https://" + cfg.Apps[other].Hostname + "/healthz"
		if m == nil || m["url"] != want || m["expected_status"] != "200" || m["follow_redirects"] != false ||
			m["monitor_type"] != "active" || m["interval_seconds"] != float64(60) {
			t.Errorf("%s's check of %s: %v", self, other, m)
		}
		pinged := false
		for _, m := range seed.Monitors {
			if m["monitor_type"] == "ping" && m["ping_host"] == cfg.Sites[cfg.Apps[other].Placement.Site].Address {
				pinged = true
			}
		}
		if !pinged {
			t.Errorf("%s does not ping %s's site", self, other)
		}
	}
}

// Two monitors on one monitor site share a host, so checking each other
// proves nothing about losing it: each seeds only its own public URL. validate
// refuses this shape today (port-collision in mode paisans, and in mode
// external a monitor site hosts the monitor alone), but Build does not
// validate, so the seed holds the rule itself rather than leaning on that.
func TestMonitorsOnOneSiteDoNotCheckEachOther(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	cfg.Apps["status-b"] = config.App{Kind: config.KindUptime, Hostname: "status-b.example.org",
		Placement: config.Placement{Mode: config.PlacementPinned, Site: "watch"},
		Settings:  map[string]any{"admin_group": "admins"}}
	secrets.Apps["status-b"] = map[string]any{"admin_password": "fixture-admin-b", "session_secret": "fixture-session-b"}
	for self, other := range map[string]string{"status": "status-b", "status-b": "status"} {
		monitors := byName(renderSeedAt(t, cfg, secrets, "watch/srv/paisans/f2a9/"+self+"/monitors.json"))
		if monitors[render.PublicCheckName(self)] == nil {
			t.Errorf("%s does not check its own public URL", self)
		}
		if monitors[render.PublicCheckName(other)] != nil {
			t.Errorf("%s checks %s, which shares its site", self, other)
		}
	}
}

// A deployment with one monitor seeds exactly one uptime check, its own.
func TestASingleMonitorSeedsOnlyItsOwnUptimeCheck(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	var public []string
	for _, m := range renderSeed(t, cfg, secrets).Monitors {
		if url, _ := m["url"].(string); strings.HasPrefix(url, "https://status") {
			public = append(public, m["name"].(string))
		}
	}
	if len(public) != 1 || public[0] != render.PublicCheckName("status") {
		t.Fatalf("uptime checks: %v", public)
	}
}

// The edge refuses /metrics/ as well as /metrics: Express routes both to the
// same handler, its routing being non strict. Caddy's path matcher is case
// insensitive, so /Metrics needs nothing more.
func TestTheEdgeRefusesMetricsWithATrailingSlash(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	plan, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range plan.Files {
		if f.Path == "watch/srv/paisans/f2a9/infra/caddy/snippets/status.caddy" {
			if !strings.Contains(f.Content, "@refused path /status* /badge/* /metrics /metrics/ /api/v1/*") {
				t.Fatalf("%s", f.Content)
			}
			return
		}
	}
	t.Fatal("no snippet")
}

package render_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
)

type seedFile struct {
	Settings map[string]any   `json:"settings"`
	Monitors []map[string]any `json:"monitors"`
}

// withMonitor is the fixture plus an uptime app pinned to vm, the secrets it
// needs, and the given smtp blocks (deployment wide, then the app's own).
func withMonitor(t *testing.T, smtp config.SMTP, own *config.SMTP) (*config.Config, *config.Secrets) {
	t.Helper()
	cfg := fixture(t)
	cfg.SMTP = smtp
	cfg.Apps["status"] = config.App{Kind: config.KindUptime, Hostname: "status.example.org",
		Placement: config.Placement{Mode: config.PlacementPinned, Site: "vm"},
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
	plan, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range plan.Files {
		if f.Path != "vm/srv/status/monitors.json" {
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
	t.Fatal("no vm/srv/status/monitors.json was rendered")
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
// other site gets a ping; the monitor watches neither itself nor its own site.
func TestTheSeedChecksEveryAppTwiceAndPingsEveryOtherSite(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	monitors := byName(renderSeed(t, cfg, secrets))

	talk := monitors["talk — public"]
	if talk == nil || talk["url"] != "https://talk.example.org/" || talk["follow_redirects"] != false {
		t.Fatalf("talk's public check: %v", talk)
	}
	// talk is behind the members gate, which redirects to sign in.
	if talk["expected_status"] != "200,302" {
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
	for _, name := range []string{"home-a — ping", "home-b — ping"} {
		if m := monitors[name]; m == nil || m["monitor_type"] != "ping" {
			t.Errorf("%s: %v", name, m)
		}
	}
	if monitors["vm — ping"] != nil {
		t.Error("the monitor pings its own site")
	}
	for name := range monitors {
		if strings.HasPrefix(name, "status ") {
			t.Errorf("the monitor checks itself: %s", name)
		}
	}
}

// A gated app's public check also accepts the gate's redirect, once; an
// ungated one does not.
func TestOnlyAGatedAppsPublicCheckAcceptsTheRedirect(t *testing.T) {
	cfg, secrets := withMonitor(t, config.SMTP{}, nil)
	monitors := byName(renderSeed(t, cfg, secrets))
	if got := monitors["talk — public"]["expected_status"]; got != "200,302" {
		t.Errorf("gated mbin, whose own codes already include 302, public expects %v", got)
	}
	if got := monitors["docs — public"]["expected_status"]; got != "200" {
		t.Errorf("ungated outline public expects %v", got)
	}
	docs := cfg.Apps["docs"]
	docs.Gate = "members"
	cfg.Apps["docs"] = docs
	if got := byName(renderSeed(t, cfg, secrets))["docs — public"]["expected_status"]; got != "200,302" {
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

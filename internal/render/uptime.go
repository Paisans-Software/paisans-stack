package render

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// The uptime kind's seed file: what the fork reconciles its monitors and
// settings from at every boot. See docs/specs/2026-10-07-uptime-monitoring.md.
//
// Field names are the fork's own column names (src/lib/sitePayload.js
// buildPayload), because the fork feeds each entry through the same builder
// and validator its API uses.
type seedMonitor struct {
	Name             string            `json:"name"`
	MonitorType      string            `json:"monitor_type"`
	URL              string            `json:"url,omitempty"`
	Method           string            `json:"method,omitempty"`
	CheckType        string            `json:"check_type,omitempty"`
	ExpectedStatus   string            `json:"expected_status,omitempty"`
	FollowRedirects  *bool             `json:"follow_redirects,omitempty"`
	RequestHeaders   map[string]string `json:"request_headers,omitempty"`
	PingHost         string            `json:"ping_host,omitempty"`
	IntervalSeconds  int               `json:"interval_seconds"`
	TimeoutMS        int               `json:"timeout_ms"`
	FailureThreshold int               `json:"failure_threshold"`
}

// Two consecutive failures before an incident, so one dropped packet on a
// residential line does not wake anyone.
const (
	seedInterval  = 60
	seedTimeoutMS = 10000
	seedThreshold = 2
)

func (p *planner) uptimeSeed(self string) (string, error) {
	monitorSite := p.cfg.Apps[self].Placement.Site
	placed := AppSites(p.cfg)
	noRedirects := false

	var monitors []seedMonitor
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		if app.Kind == config.KindUptime {
			// A monitor cannot report its own death, and a check that can
			// only ever pass is noise.
			continue
		}
		health, ok := kinds.HealthFor(app.Kind)
		if !ok {
			return "", fmt.Errorf("apps.%s: kind %s has no health route recorded in internal/kinds, so the monitor cannot check it", name, app.Kind)
		}
		public := health.Expect
		if app.Gate != "" && app.Gate != "none" {
			// The gate answers before the app is asked: oauth2-proxy's
			// /oauth2/auth refuses a request with no session with 401
			// (oauthproxy.go:1018-1022 at v7.15.4) and Caddy's forward_auth
			// copies that back. So this check proves the edge and the gate,
			// and the direct check below proves the app.
			public = "401"
		}
		monitors = append(monitors, seedMonitor{
			Name: name + " — public", MonitorType: "active", Method: "GET", CheckType: "status",
			URL: "https://" + app.Hostname + health.Path, ExpectedStatus: public,
			FollowRedirects: &noRedirects,
			IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
		})
		sites := append([]string(nil), placed[name]...)
		sort.Strings(sites)
		for _, site := range sites {
			monitors = append(monitors, seedMonitor{
				Name: fmt.Sprintf("%s — direct (%s)", name, site), MonitorType: "active", Method: "GET", CheckType: "status",
				URL:            fmt.Sprintf("http://%s:%d%s", p.sites[site].Address, appPort[app.Kind], health.Path),
				ExpectedStatus: health.Expect, FollowRedirects: &noRedirects,
				// What the gateway would send, minus the gateway.
				RequestHeaders:  map[string]string{"Host": app.Hostname, "X-Forwarded-Proto": "https"},
				IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
			})
			if app.Kind == config.KindPocketID {
				// The admin reconciler beside each instance: 503 while admins
				// has fewer than two members (docs/specs/2026-10-08-admin-reconciler.md).
				// The gateway does not route it, so there is no public check.
				monitors = append(monitors, seedMonitor{
					Name: fmt.Sprintf("%s — admin reconciler (%s)", name, site), MonitorType: "active", Method: "GET", CheckType: "status",
					URL:            fmt.Sprintf("http://%s:%d/healthz", p.sites[site].Address, reconcilerPort),
					ExpectedStatus: "200", FollowRedirects: &noRedirects,
					IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
				})
			}
		}
	}
	for _, site := range p.order {
		if site == monitorSite {
			continue
		}
		monitors = append(monitors, seedMonitor{
			Name: site + " — ping", MonitorType: "ping", PingHost: p.sites[site].Address,
			IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
		})
	}

	settings := map[string]any{"status_page_enabled": false}
	if smtp := p.smtpFor(self); smtp.Host != "" {
		settings["smtp_host"] = smtp.Host
		settings["smtp_port"] = smtp.Port
		settings["smtp_secure"] = smtp.Secure
		settings["smtp_user"] = smtp.Username
		settings["smtp_pass"] = smtp.Password
		settings["smtp_from_address"] = smtp.FromAddress
		settings["smtp_from_name"] = smtp.FromName
	}
	out, err := json.MarshalIndent(map[string]any{"settings": settings, "monitors": monitors}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// smtpFor is an app's effective SMTP settings with its password: the app's
// own smtp_password secret when set, else external.smtp_password.
func (p *planner) smtpFor(app string) smtpValues {
	s := p.cfg.SMTPFor(app)
	if s.Host == "" {
		return smtpValues{}
	}
	password := p.appSecret(app, "smtp_password")
	if password == "" {
		password = p.secrets.External["smtp_password"]
	}
	return smtpValues{
		Host: s.Host, Port: s.PortOrDefault(), Secure: s.Security == config.SMTPTLS,
		Username: s.Username, Password: password, FromAddress: s.FromAddress, FromName: s.FromName,
	}
}

// smtpValues is an app's resolved mail settings, zero when no host resolves.
type smtpValues struct {
	Host        string
	Port        int
	Secure      bool
	Username    string
	Password    string
	FromAddress string
	FromName    string
}

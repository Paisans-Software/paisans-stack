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
	ExpectedString   string            `json:"expected_string,omitempty"`
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

// The seed's monitor names. The fork reconciles monitors by name, so a format
// that changed would replace every deployed monitor and drop the channels
// admins attached to it; these are the formats the first release shipped.
// Names come from app and site keys, never hostnames, so a renamed hostname
// updates a monitor in place.

// PublicCheckName is the name of an app's check at its public hostname.
func PublicCheckName(app string) string { return app + " — public" }

// GateCheckName is the name of a gated app's check that the gate answers.
func GateCheckName(app string) string { return app + " — gate" }

// SignedFetchCheckName is the name of a gated federating app's check that an
// unsigned ActivityPub read is refused.
func SignedFetchCheckName(app string) string { return app + " — signed fetch" }

func directCheckName(app, site string) string {
	return fmt.Sprintf("%s — direct (%s)", app, site)
}

func pingName(site string) string { return site + " — ping" }

func (p *planner) uptimeSeed(self string) (string, error) {
	monitorSite := p.cfg.Apps[self].Placement.Site
	placed := AppSites(p.cfg)
	noRedirects := false

	var monitors []seedMonitor
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		if app.Kind == config.KindUptime {
			// A monitor cannot report its own death, and a direct check of
			// its own container can only ever pass. Its public URL, and
			// every other monitor's, is checked below, after every other app.
			continue
		}
		health, ok := kinds.HealthFor(app.Kind)
		if !ok {
			return "", fmt.Errorf("apps.%s: kind %s has no health route recorded in internal/kinds, so the monitor cannot check it", name, app.Kind)
		}
		// A gated app is checked three ways through the edge. The public
		// check reaches the app only when its health route is one of the
		// kind's open paths, which a dedicated route is and `/` never is, so a
		// gated app whose health route is its front page has no public check:
		// the gate would answer it, not the app. The gate check proves the
		// edge and the gate are in the path; the signed fetch check proves the
		// app still refuses an unsigned ActivityPub read, which an admin can
		// turn off in the app with nothing in this configuration to say so.
		// See docs/specs/2026-10-08-visibility-gate.md.
		gated := app.Gate() != ""
		if !gated || health.Path != "/" {
			monitors = append(monitors, seedMonitor{
				Name: PublicCheckName(name), MonitorType: "active", Method: "GET", CheckType: "status",
				URL: "https://" + app.Hostname + health.Path, ExpectedStatus: health.Expect,
				FollowRedirects: &noRedirects,
				IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
			})
		}
		if gated {
			// The gate redirects a request with no session to sign in. A
			// string check passes on any status below 400 whose body holds
			// the marker, and only the gate's redirect carries it: the app's
			// own redirect to its sign-in page does not.
			monitors = append(monitors, seedMonitor{
				Name: GateCheckName(name), MonitorType: "active", Method: "GET", CheckType: "string",
				URL: "https://" + app.Hostname + "/", ExpectedString: GateMarker,
				FollowRedirects: &noRedirects,
				IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
			})
			if sf := kinds.SignedFetchFor(app); sf != nil && kinds.Federates(app) {
				monitors = append(monitors, seedMonitor{
					Name: SignedFetchCheckName(name), MonitorType: "active", Method: "GET", CheckType: "status",
					URL: "https://" + app.Hostname + sf.SignedFetchProbe(app.Hostname), ExpectedStatus: sf.Expect,
					FollowRedirects: &noRedirects,
					RequestHeaders:  map[string]string{"Accept": "application/activity+json"},
					IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
				})
			}
		}
		sites := append([]string(nil), placed[name]...)
		sort.Strings(sites)
		for _, site := range sites {
			monitors = append(monitors, seedMonitor{
				Name: directCheckName(name, site), MonitorType: "active", Method: "GET", CheckType: "status",
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
	// The monitor's own public URL. The process is alive whenever it can run
	// this, so what it proves is the path in front of it: DNS, the web
	// server and the certificate, whose expiry the fork warns about 14 days
	// ahead. Behind an operator's own web server it is the only thing that
	// notices a renewal that silently stopped.
	//
	// The public URL of every monitor on another monitor site follows,
	// checked the same way and in AppNames order. A monitor cannot report its
	// own death or its host's, so a monitor on a separate site is what
	// notices either: this check covers the other monitor and the path in
	// front of it, and the ping of its site, seeded below, covers its host.
	// A monitor on this same site dies with this host, so checking it would
	// add nothing about losing the host, and it is left out.
	if health, ok := kinds.HealthFor(config.KindUptime); ok {
		uptimeCheck := func(name string) seedMonitor {
			return seedMonitor{
				Name: PublicCheckName(name), MonitorType: "active", Method: "GET", CheckType: "status",
				URL: "https://" + p.cfg.Apps[name].Hostname + health.Path, ExpectedStatus: health.Expect,
				FollowRedirects: &noRedirects,
				IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
			}
		}
		monitors = append(monitors, uptimeCheck(self))
		for _, name := range p.cfg.AppNames() {
			other := p.cfg.Apps[name]
			if other.Kind == config.KindUptime && other.Placement.Site != monitorSite {
				monitors = append(monitors, uptimeCheck(name))
			}
		}
	}
	for _, site := range p.order {
		if site == monitorSite {
			continue
		}
		monitors = append(monitors, seedMonitor{
			Name: pingName(site), MonitorType: "ping", PingHost: p.sites[site].Address,
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

// Package ingress helps an operator put a monitor site's app behind a web
// server: what that server must do, filled in for this deployment (Show), and
// whether it does it, checked from the operator's machine (Check).
//
// It reaches no host over ssh and changes nothing anywhere. Show reads
// paisans.yaml alone; Check resolves a name, makes two HTTPS requests and
// opens one TCP connection, as any visitor could.
package ingress

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// Target is one app a monitor site serves, and what its ingress needs.
type Target struct {
	App      string
	Site     string
	Hostname string
	Mode     config.IngressMode
	// Listen is where the app is published for the operator's web server,
	// as written in paisans.yaml, and ListenHost and ListenPort the same
	// split. All three are empty in ingress mode paisans.
	Listen     string
	ListenHost string
	ListenPort int
	// PublicAddress and PublicAddress6 are where the hostname must resolve.
	PublicAddress  string
	PublicAddress6 string
	// HealthPath and HealthExpect are the kind's health route and the
	// statuses that mean it is up, from kinds.HealthFor.
	HealthPath   string
	HealthExpect string
}

// External reports whether a web server the operator runs is in front of
// the app.
func (t Target) External() bool { return t.Mode == config.IngressExternal }

// Upstream is the URL the operator's web server proxies to.
func (t Target) Upstream() string { return "http://" + t.Listen }

// For builds the target for an app pinned to a monitor site. Every other app
// is served by the gateway's Caddy, which the toolkit runs, so there is
// nothing to hand off for it.
func For(cfg *config.Config, app string) (Target, error) {
	a, ok := cfg.Apps[app]
	if !ok {
		return Target{}, fmt.Errorf("ingress: %s declares no app %q. Declared apps are %s", cfg.Path, app, strings.Join(cfg.AppNames(), ", "))
	}
	site, ok := render.ServedBy(cfg, app)
	if !ok {
		return Target{}, fmt.Errorf("ingress: %s is served by the gateway's Caddy, which the toolkit runs, so there is no ingress to hand off. Only an app pinned to a site holding the monitor role has one", app)
	}
	s := cfg.Sites[site]
	health, _ := kinds.HealthFor(a.Kind)
	t := Target{
		App: app, Site: site, Hostname: a.Hostname, Mode: s.IngressMode(),
		PublicAddress: s.PublicAddress, PublicAddress6: s.PublicAddress6,
		HealthPath: health.Path, HealthExpect: health.Expect,
	}
	if t.HealthPath == "" {
		t.HealthPath, t.HealthExpect = "/", "200"
	}
	if t.External() && s.Ingress != nil {
		host, port, ok := s.Ingress.ListenHostPort()
		if !ok {
			return Target{}, fmt.Errorf("ingress: sites.%s.ingress.listen %q is not an address and a port", site, s.Ingress.Listen)
		}
		t.Listen, t.ListenHost, t.ListenPort = s.Ingress.Listen, host, port
	}
	return t, nil
}

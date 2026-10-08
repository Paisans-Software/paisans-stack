package render

import (
	"github.com/paisans-software/paisans-stack/internal/config"
)

// ServedBy is the monitor site that serves an app, when the app is pinned to
// a site holding the monitor role. Such an app is left out of the gateway's
// routing and served from the monitor itself, by the toolkit's Caddy there or
// by the operator's own web server: a monitor reached only through the
// gateway goes dark at exactly the moment it is needed. Every other app is
// the gateway's, and ServedBy returns false for it.
func ServedBy(cfg *config.Config, app string) (string, bool) {
	a, ok := cfg.Apps[app]
	if !ok || a.Placement.Mode != config.PlacementPinned {
		return "", false
	}
	site, ok := cfg.Sites[a.Placement.Site]
	if !ok || !site.Has(config.RoleMonitor) {
		return "", false
	}
	return a.Placement.Site, true
}

// ExternalListen is where an app is published for the operator's own web
// server: the listen address of the monitor in ingress mode external that
// the app is pinned to. False for every other app.
func ExternalListen(cfg *config.Config, app string) (string, int, bool) {
	site, ok := ServedBy(cfg, app)
	if !ok {
		return "", 0, false
	}
	s := cfg.Sites[site]
	if s.IngressMode() != config.IngressExternal || s.Ingress == nil {
		return "", 0, false
	}
	return s.Ingress.ListenHostPort()
}

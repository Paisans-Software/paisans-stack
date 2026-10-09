package hostcaddy

import (
	"fmt"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ingress"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// Applies reports whether site is one whose apply looks for the host's own
// web server: a monitor in ingress mode external.
func Applies(cfg *config.Config, site string) bool {
	s, ok := cfg.Sites[site]
	return ok && s.Has(config.RoleMonitor) && s.IngressMode() == config.IngressExternal
}

// Upstream is where the server f found reaches app: the listen address as
// declared when it shares the host's network, and otherwise the app's alias
// on the server's own network, which the app joins (render.HostProxy). The
// published listen is the host's loopback, or an address of the host's, and
// a container on a network of its own reaches neither as the host does.
func Upstream(cfg *config.Config, f *Finding, app string, t ingress.Target) string {
	if f.HostNetwork {
		return t.Listen
	}
	return fmt.Sprintf("%s:%d", render.ProxyAlias(cfg, app), render.AppPort(cfg.Apps[app].Kind))
}

// Proxy is the render option a server on a network of its own needs: the app
// joins that network and trusts the server's address there. False when the
// server shares the host's network, or cannot be added to at all.
func Proxy(f *Finding) (render.HostProxy, bool) {
	if f == nil || f.Unsupported != "" || f.HostNetwork || f.Network == "" {
		return render.HostProxy{}, false
	}
	return render.HostProxy{Network: f.Network, Address: f.Address}, true
}

// Wanted is the site block for every app site serves, each proxying to the
// upstream the server f found reaches. With no server, or one the toolkit
// cannot add to, the blocks proxy to the declared listen, for the owner to
// add by hand.
func Wanted(cfg *config.Config, site string, f *Finding) ([]Site, error) {
	var out []Site
	for _, app := range cfg.PinnedTo(site) {
		served, ok := render.ServedBy(cfg, app)
		if !ok || served != site {
			continue
		}
		t, err := ingress.For(cfg, app)
		if err != nil {
			return nil, err
		}
		upstream := t.Listen
		if f != nil && f.Unsupported == "" {
			upstream = Upstream(cfg, f, app, t)
		}
		out = append(out, Site{App: app, Hostname: t.Hostname, HealthPath: t.HealthPath, Block: ingress.CaddyBlock(t, upstream, false)})
	}
	return out, nil
}

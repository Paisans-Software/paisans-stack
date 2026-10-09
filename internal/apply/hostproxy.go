package apply

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"gopkg.in/yaml.v3"
)

// HostProxyOptions renders every app on a monitor in ingress mode external
// joined to the web server network it was last applied with, as read from
// the app's deployed compose file and environment on its host.
//
// Only `apply` on the monitor's own site looks at that web server and decides
// whether the app joins its network (render.HostProxy). Every other command
// that renders the app, the monitor reseed at the end of a topology change
// above all, renders it without looking, and would otherwise recreate it off
// that network and out of reach of the server in front of it. Reading what
// the last apply deployed keeps the app as that apply left it. A site with no
// transport here, or an app not deployed yet, renders as declared.
func HostProxyOptions(cfg *config.Config, transports map[string]Transport) ([]render.Option, error) {
	var out []render.Option
	d := cfg.Deployment()
	for _, site := range cfg.MonitorSites() {
		t, ok := transports[site]
		if !ok {
			continue
		}
		for _, app := range cfg.PinnedTo(site) {
			if _, _, ok := render.ExternalListen(cfg, app); !ok {
				continue
			}
			compose, found, err := t.ReadFile(d.Compose(app))
			if err != nil {
				return nil, fmt.Errorf("%s: reading %s's compose file for the web server network it joins: %w", site, app, err)
			}
			if !found {
				continue
			}
			network, err := DeployedProxyNetwork(compose)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", site, d.Compose(app), err)
			}
			if network == "" {
				continue
			}
			env, _, err := t.ReadFile(d.Path(app, ".env"))
			if err != nil {
				return nil, fmt.Errorf("%s: reading %s's environment for the web server it trusts: %w", site, app, err)
			}
			out = append(out, render.WithHostProxy(app, render.HostProxy{Network: network, Address: deployedProxyAddress(env)}))
		}
	}
	return out, nil
}

// DeployedProxyNetwork is the external network a deployed compose file joins
// as `proxy`, empty when it joins none.
func DeployedProxyNetwork(compose string) (string, error) {
	var doc struct {
		Networks map[string]struct {
			Name     string `yaml:"name"`
			External bool   `yaml:"external"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		return "", fmt.Errorf("not readable as a compose file: %w", err)
	}
	n, ok := doc.Networks["proxy"]
	if !ok || !n.External {
		return "", nil
	}
	return n.Name, nil
}

// deployedProxyAddress is the web server's address in a deployed
// TRUST_PROXY: the entry after the pinned network's gateway.
func deployedProxyAddress(env string) string {
	for _, line := range strings.Split(env, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "TRUST_PROXY=")
		if !ok {
			continue
		}
		for _, entry := range strings.Split(value, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" && entry != render.IngressGateway+"/32" {
				return strings.TrimSuffix(entry, "/32")
			}
		}
	}
	return ""
}

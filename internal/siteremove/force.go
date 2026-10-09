package siteremove

import (
	"errors"
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
)

// BuildForced plans `site remove --force`: the host at dest cleaned of this
// deployment, and nothing else. No other site is reached, the cluster's
// refusals are not checked, and neither paisans.yaml nor the secrets file is
// edited, because --force is for a host the cluster cannot be asked about or
// no longer counts on (docs/specs/2026-10-09-site-remove-force.md).
//
// dest may be any host. What is removed is only what the host proves is this
// deployment's, by its id or its token, so a host it never used plans
// nothing.
//
// The host stage runs against a copy of the configuration in which the site
// is declared with dest as its ssh destination and the roles its registry
// entry on the host records. Those are what the host was deployed with, which
// is what cleaning it needs, for an undeclared site and for a host the site
// has left alike.
func BuildForced(cfg *config.Config, secrets *config.Secrets, site string, dest config.Destination, t apply.Transport, o Options) (*Plan, error) {
	if o.HostGone {
		return nil, fmt.Errorf("site remove %s: --force cleans one host, and --host-gone reaches none. Drop one of them", site)
	}
	declared, isDeclared := cfg.Sites[site]
	p := &Plan{Site: site, Options: o, secrets: secrets, transports: map[string]apply.Transport{site: t}}
	p.Current = isDeclared && declared.Destination() == dest

	if out, err := t.Run("true"); err != nil {
		if errors.Is(err, apply.ErrUnreachable) {
			return nil, fmt.Errorf("site remove %s: %s does not answer over ssh (%v), so what is on it cannot be read or cleaned. Fix ssh and run again", site, dest, err)
		}
		return nil, fmt.Errorf("site remove %s: %s answered `true` with an error: %v: %s", site, dest, err, lastLines(out, 2))
	}
	s := declared
	s.SSH.User, s.SSH.Host, s.SSH.Port = dest.User, dest.Host, dest.Port
	reg, err := registry.Read(t)
	if err != nil {
		return nil, fmt.Errorf("site remove %s: %w", site, err)
	}
	if e, ok := reg.Deployments[cfg.ID]; ok {
		// The entry names the site the host was deployed as, which is the
		// proof of whose host this is, whatever --ssh spelled.
		if e.Site != site {
			if _, live := cfg.Sites[e.Site]; live {
				return nil, fmt.Errorf("site remove %s: %s is %s's host, which paisans.yaml still declares, by this deployment's entry in its registry. Nothing was changed. To clean it, name that site: paisans site remove %s --force", site, dest, e.Site, e.Site)
			}
		}
		p.Current = isDeclared && e.Site == site
		s.Roles = nil
		for _, r := range strings.Split(e.Roles, ",") {
			if r != "" {
				s.Roles = append(s.Roles, config.Role(r))
			}
		}
	}
	forcedCfg := *cfg
	forcedCfg.Sites = map[string]config.Site{}
	for name, v := range cfg.Sites {
		forcedCfg.Sites[name] = v
	}
	forcedCfg.Sites[site] = s
	p.cfg = &forcedCfg
	if s.Has(config.RoleGateway) {
		// Handing Caddy over reads the snippets of the Caddyfile the
		// gateway was rendered with.
		if p.full, err = p.render(p.cfg); err != nil {
			return nil, fmt.Errorf("site remove %s: rendering the gateway's Caddyfile, which handing Caddy over reads: %w", site, err)
		}
	}

	host, err := p.buildHost()
	if err != nil {
		return nil, err
	}
	if p.Current && len(host.Steps) > 0 {
		host.Steps = append([]Step{{Site: site, Verb: "note", Title: "note the cluster", Text: fmt.Sprintf("%s is still declared and this is its host: its etcd member, Patroni replica and Garage node, and its place in every other site's mesh, stay in the cluster until a full `paisans site remove %s`, and its next `paisans apply --site %s` deploys it again", site, site, site)}}, host.Steps...)
	}
	p.Stages = []*Stage{host}
	return p, nil
}

package siteremove

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
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
	p := &Plan{Site: site, Options: o, secrets: secrets, transports: map[string]apply.Transport{site: t}, forced: true, declared: isDeclared}
	p.Current = isDeclared && declared.Destination() == dest

	if err := answers(site, dest, t); err != nil {
		return nil, err
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

	host, err := p.buildHost()
	if err != nil {
		return nil, err
	}
	if len(host.Steps) > 0 {
		p.notePocketID(host)
	}
	if p.Current && len(host.Steps) > 0 {
		host.Steps = append([]Step{{Site: site, Verb: "note", Title: "note the cluster", Text: fmt.Sprintf("%s is still declared and this is its host: its etcd member, Patroni replica and Garage node, and its place in every other site's mesh, stay in the cluster until a full `paisans site remove %s`, and its next `paisans apply --site %s` deploys it again", site, site, site)}}, host.Steps...)
	}
	p.Stages = []*Stage{host}
	// A site paisans.yaml still declares keeps its records, which are still
	// wanted. One it does not declare is removed from it already.
	if !isDeclared {
		dnsSt, err := p.buildDNS(cfg, cfg.WithoutSite(site))
		if err != nil {
			return nil, err
		}
		p.Stages = append(p.Stages, dnsSt)
	}
	return p, nil
}

// answers checks that dest answers over ssh before anything on it is read.
func answers(what string, dest config.Destination, t apply.Transport) error {
	if out, err := t.Run("true"); err != nil {
		if errors.Is(err, apply.ErrUnreachable) {
			return fmt.Errorf("site remove %s: %s does not answer over ssh (%v), so what is on it cannot be read or cleaned. Fix ssh and run again", what, dest, err)
		}
		return fmt.Errorf("site remove %s: %s answered `true` with an error: %v: %s", what, dest, err, lastLines(out, 2))
	}
	return nil
}

// BuildForcedByID plans `site remove --force --id`: the host at dest cleaned
// of the deployment its registry names by ref, a full id or a token, with no
// paisans.yaml and no secrets file (docs/specs/2026-10-10-remove-without-
// config.md).
//
// The host stage reads the configuration for the id, the site's ssh user and
// destination, and the site's roles, which the registry entry and dest hold.
// So the plan runs against a configuration of just those, and is the forced
// removal's own host stage, without the Pocket ID note, which reads the
// configuration's apps. A gateway's kept Caddy is reduced from the
// Caddyfile on the host, as with a configuration. The remains say what was
// not read.
func BuildForcedByID(dest config.Destination, t apply.Transport, ref string, o Options) (*Plan, error) {
	what := "--id " + ref
	if o.HostGone {
		return nil, fmt.Errorf("site remove %s: --force cleans one host, and --host-gone reaches none. Drop one of them", what)
	}
	if err := answers(what, dest, t); err != nil {
		return nil, err
	}
	reg, err := registry.Read(t)
	if err != nil {
		return nil, fmt.Errorf("site remove %s: %w", what, err)
	}
	id, e, err := registry.Find(reg, ref)
	if err != nil {
		return nil, fmt.Errorf("site remove %s: %s: %w. Nothing was changed", what, dest, err)
	}
	// Every name removed is made from the id, so the entry must be the one
	// a claim by that id writes.
	d := deployment.Deployment{ID: id}
	var wrong []string
	if !deployment.ValidID(id) {
		wrong = append(wrong, "its key is not a deployment id")
	}
	if e.Token != d.Token() {
		wrong = append(wrong, fmt.Sprintf("its token is %q, not %s", e.Token, d.Token()))
	}
	if e.Root != d.Root() {
		wrong = append(wrong, fmt.Sprintf("its root is %q, not %s", e.Root, d.Root()))
	}
	if e.Site == "" {
		wrong = append(wrong, "it names no site")
	}
	// Every name but the label is made from the token, so another entry
	// holding it or the root would share them, whichever id --id spelled.
	var sharing []string
	for other, oe := range reg.Deployments {
		if other != id && (oe.Token == d.Token() || oe.Root == d.Root() || strings.HasPrefix(other, d.Token())) {
			sharing = append(sharing, fmt.Sprintf("entry %s holds its token or root too", other))
		}
	}
	sort.Strings(sharing)
	wrong = append(wrong, sharing...)
	if len(wrong) > 0 {
		return nil, fmt.Errorf("site remove %s: the entry %s in %s on %s is not one a claim writes: %s. Nothing was changed. Look at the registry by hand", what, id, registry.Path, dest, strings.Join(wrong, ", "))
	}

	s := config.Site{}
	s.SSH.User, s.SSH.Host, s.SSH.Port = dest.User, dest.Host, dest.Port
	for _, r := range strings.Split(e.Roles, ",") {
		if r != "" {
			s.Roles = append(s.Roles, config.Role(r))
		}
	}
	cfg := &config.Config{ID: id, Community: config.Community{Domain: e.Domain}, Sites: map[string]config.Site{e.Site: s}}
	p := &Plan{Site: e.Site, Options: o, transports: map[string]apply.Transport{e.Site: t}, forced: true, byID: true, dest: dest, cfg: cfg}
	host, err := p.buildHost()
	if err != nil {
		return nil, err
	}
	if s.Has(config.RoleApps) {
		p.Notes = append(p.Notes, pocketIDNotChecked(e.Site))
	}
	p.Stages = []*Stage{host}
	return p, nil
}

// Confirmation is what --execute says before it asks for the site's name:
// what is cleaned off dest, and what goes with it that the plan alone shows.
func (p *Plan) Confirmation(dest config.Destination) string {
	out := fmt.Sprintf("This cleans deployment %s (%s), site %s, off %s, and nothing says whether it still runs.", p.cfg.ID, p.cfg.Community.Domain, p.Site, dest)
	if len(p.CaddyKept) > 0 {
		out += fmt.Sprintf(" Its Caddy is kept, reduced to the host owner's sites in %s: %s.", render.HostSitesDir, strings.Join(p.CaddyKept, ", "))
	}
	if p.DeleteData {
		out += fmt.Sprintf(" It deletes this deployment's data on %s for good.", dest)
	}
	return out
}

// DeploymentID is the id of the deployment the plan removes.
func (p *Plan) DeploymentID() string { return p.cfg.ID }

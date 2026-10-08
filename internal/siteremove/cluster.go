package siteremove

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// siteChange is what stage 2 does on one remaining site: a scoped apply of
// the files whose render changes with the site taken out, and the one
// command each kind of file needs.
type siteChange struct {
	site    string
	plan    *apply.Plan
	paths   []string
	haproxy bool
	caddy   bool
	// dbApps are the stacks here that reach the database through HAProxy,
	// stopped around its restart.
	dbApps []string
}

// haproxyFile is HAProxy's configuration, in rendered form.
func (p *Plan) haproxyFile() string { return p.dep().RelPath("infra", "haproxy", "haproxy.cfg") }

// isCaddyRouting is a file the toolkit's Caddy reads its routes from, which
// a reload picks up (apply's isRouting).
func (p *Plan) isCaddyRouting(rel string) bool {
	return strings.HasPrefix(rel, p.dep().RelPath("infra", "caddy")+"/") && !strings.HasSuffix(rel, ".env")
}

// changedFiles compares, for one remaining site, the render with the site
// and the render without it: what differs is what names the removed site.
// moved are the files this stage writes; owed are the files it leaves for an
// apply, each with why.
func (p *Plan) changedFiles(site string) (moved []string, owed []string) {
	prefix := site + "/"
	before := map[string]string{}
	for _, f := range p.full.Files {
		if rel, ok := strings.CutPrefix(f.Path, prefix); ok {
			before[rel] = f.Content
		}
	}
	after := map[string]bool{}
	for _, f := range p.rendered.Files {
		rel, ok := strings.CutPrefix(f.Path, prefix)
		if !ok || rel == render.ManifestName {
			continue
		}
		after[rel] = true
		old, had := before[rel]
		switch {
		case had && old == f.Content:
		case !had:
			owed = append(owed, "/"+rel+" is rendered now and was not before")
		case rel == p.dep().WireGuardConf(), rel == p.haproxyFile():
			moved = append(moved, rel)
		case p.isCaddyRouting(rel) && p.end.Sites[site].RunsCaddy():
			moved = append(moved, rel)
		case rel == p.dep().RelPath("infra", "patroni.env"):
			owed = append(owed, "/"+rel+" still names "+p.Site+"'s etcd member. Applying it recreates Patroni there, which on the primary is a failover, and nothing needs it: every member it names but "+p.Site+" stays a voter")
		default:
			owed = append(owed, "/"+rel+" changes with "+p.Site+" gone")
		}
	}
	for rel := range before {
		if rel != render.ManifestName && !after[rel] {
			owed = append(owed, "/"+rel+" is no longer rendered; a whole apply marks it left over")
		}
	}
	sort.Strings(moved)
	sort.Strings(owed)
	return moved, owed
}

// buildCluster is stage 2: the site's etcd member removed, then on every
// remaining site the files that name it, each moved by a scoped apply and
// the one command it needs.
func (p *Plan) buildCluster() (*Stage, error) {
	st := &Stage{Number: 2, Name: "out of the cluster"}
	var gates []string
	if p.etcd.control != "" {
		if p.etcd.member != nil {
			st.Steps = append(st.Steps, Step{Site: p.etcd.control, Verb: "remove", Text: fmt.Sprintf("%s's etcd member %s: %s", p.Site, p.etcd.member.HexID(), apply.Etcdctl(p.dep(), "member remove "+p.etcd.member.HexID()))})
			if !p.HostGone {
				st.Steps = append(st.Steps, Step{Site: p.Site, Verb: "stop", Text: "its etcd, which a removed member cannot rejoin, so it does not restart in a loop: " + p.stopService("etcd")})
			}
		}
		gates = append(gates, fmt.Sprintf("etcd's voters are exactly %s and every member is healthy", strings.Join(sortedCopy(p.end.Etcd.Members), ", ")))
	}

	var changes []*siteChange
	var proxies []string
	key, err := render.PublicKey(p.secrets.Sites[p.Site].WireGuardPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("secrets sites.%s.wireguard_private_key: %w", p.Site, err)
	}
	for _, name := range p.end.SiteNames() {
		moved, owed := p.changedFiles(name)
		for _, o := range owed {
			p.Notes = append(p.Notes, fmt.Sprintf("%s: %s. `paisans apply --site %s` brings it up to date when that is acceptable", name, o, name))
		}
		t := p.transports[name]
		c := &siteChange{site: name}
		if len(moved) > 0 {
			sp, err := apply.Build(name, p.rendered, acme.Module(p.cfg.ACME.Provider), t, apply.Scope(moved...))
			if err != nil {
				return nil, err
			}
			if conflicts := sp.Conflicts(); len(conflicts) > 0 {
				return nil, fmt.Errorf("site remove %s: %s's %s differs from what the last apply recorded, so somebody edited it on the host. Nothing was changed. Restore it, or copy what is wanted into the configuration, and run site remove again", p.Site, name, conflicts[0].Path)
			}
			c.plan, c.paths = sp, moved
			for _, w := range sp.Writes() {
				st.Steps = append(st.Steps, Step{Site: name, Verb: w.Kind.String(), Text: w.Path})
			}
			if sp.WireGuard != apply.WireGuardNone {
				st.Steps = append(st.Steps, Step{Site: name, Verb: "mesh", Text: sp.WireGuard.Describe(p.dep())})
			}
		}
		for _, rel := range moved {
			switch {
			case rel == p.haproxyFile():
				c.haproxy = true
			case p.isCaddyRouting(rel):
				c.caddy = true
			}
		}
		if renders(p.rendered, name, p.haproxyFile()) {
			proxies = append(proxies, name)
			lists, err := p.proxyLists(name)
			if err != nil {
				return nil, err
			}
			if c.haproxy && (len(c.plan.Writes()) > 0 || lists) {
				c.dbApps = p.databaseApps(name)
				if len(c.dbApps) > 0 {
					st.Steps = append(st.Steps, Step{Site: name, Verb: "stop", Text: strings.Join(c.dbApps, ", ") + ": they reach the database through this HAProxy, and an app's open connections do not survive its restart"})
				}
				st.Steps = append(st.Steps, Step{Site: name, Verb: "restart", Text: "HAProxy alone, without " + p.Site + "'s backend: " + p.restartHAProxy()})
				if len(c.dbApps) > 0 {
					st.Steps = append(st.Steps, Step{Site: name, Verb: "start", Text: strings.Join(c.dbApps, ", ") + ", and check each healthy as apply does"})
				}
			} else {
				c.haproxy = false
			}
		}
		if c.caddy {
			st.Steps = append(st.Steps,
				Step{Site: name, Verb: "validate", Text: "the routes without " + p.Site + ": " + validateCaddy(p.dep())},
				Step{Site: name, Verb: "reload", Text: "Caddy: " + reloadCaddy(p.dep())})
		}
		if c.plan != nil {
			changes = append(changes, c)
		}
	}
	gates = append(gates, fmt.Sprintf("no remaining site has %s's WireGuard key as a peer", p.Site))
	if len(proxies) > 0 {
		gates = append(gates, fmt.Sprintf("HAProxy on %s lists exactly %s, the leader UP", strings.Join(proxies, ", "), strings.Join(sortedCopy(p.end.Cluster.Sites), ", ")))
	}
	st.Gate = strings.Join(gates, "; ")
	st.run = func() error {
		if p.etcd.member != nil {
			if err := p.removeEtcdMember(); err != nil {
				return err
			}
			if !p.HostGone {
				if out, err := p.transports[p.Site].Run(p.stopService("etcd")); err != nil {
					return fmt.Errorf("%s: stopping etcd: %w: %s", p.Site, err, lastLines(out, 3))
				}
			}
		}
		for _, c := range changes {
			if err := p.applyChange(c); err != nil {
				return err
			}
		}
		return nil
	}
	st.gate = func() error {
		if p.etcd.control != "" {
			if err := p.etcdGate(); err != nil {
				return err
			}
		}
		if err := p.meshGate(key); err != nil {
			return err
		}
		if len(proxies) > 0 {
			return p.haproxyGate(proxies)
		}
		return nil
	}
	return st, nil
}

func renders(rendered *render.Plan, site, rel string) bool {
	for _, f := range rendered.Files {
		if f.Path == site+"/"+rel {
			return true
		}
	}
	return false
}

// removeEtcdMember removes the site's member by its ID, read again first, so
// a run that removed it and stopped finds nothing to do.
func (p *Plan) removeEtcdMember() error {
	members, err := p.etcdMembers()
	if err != nil {
		return err
	}
	for _, m := range members {
		if apply.EtcdMemberSite(p.cfg, m) != p.Site {
			continue
		}
		p.say("  %-9s %s's etcd member\n", "remove", p.Site)
		if out, err := p.transports[p.etcd.control].Run(apply.Etcdctl(p.dep(), "member remove "+m.HexID())); err != nil {
			return fmt.Errorf("%s: etcdctl member remove %s: %w: %s", p.etcd.control, m.HexID(), err, lastLines(out, 3))
		}
	}
	return nil
}

func (p *Plan) etcdMembers() ([]apply.EtcdMember, error) {
	out, err := p.transports[p.etcd.control].Run(apply.Etcdctl(p.dep(), "member list -w json"))
	if err != nil {
		return nil, fmt.Errorf("%s: etcd's member list: %w: %s", p.etcd.control, err, lastLines(out, 3))
	}
	return apply.ParseEtcdMembers(out)
}

func (p *Plan) etcdGate() error {
	want := strings.Join(sortedCopy(p.end.Etcd.Members), ",")
	err := poll(etcdWait, etcdPoll, func() error {
		members, err := p.etcdMembers()
		if err != nil {
			return err
		}
		var voters []string
		for _, m := range members {
			if m.IsLearner {
				return fmt.Errorf("%s is a learner", apply.EtcdMemberSite(p.cfg, m))
			}
			voters = append(voters, apply.EtcdMemberSite(p.cfg, m))
		}
		sort.Strings(voters)
		if strings.Join(voters, ",") != want {
			return fmt.Errorf("the voters are %s, and the end state's etcd.members is %s", strings.Join(voters, ", "), strings.ReplaceAll(want, ",", ", "))
		}
		out, err := p.transports[p.etcd.control].Run(apply.Etcdctl(p.dep(), "endpoint health --cluster -w json"))
		var health []endpointHealth
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &health); jerr != nil {
			return fmt.Errorf("endpoint health: %s", lastLines(out, 3))
		}
		for _, h := range health {
			if !h.Health {
				return fmt.Errorf("%s is unhealthy: %s", h.Endpoint, h.Error)
			}
		}
		if err != nil {
			return fmt.Errorf("endpoint health: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("etcd after %s: %w", etcdWait, err)
	}
	return nil
}

// applyChange writes one site's files, which hands a changed mesh file to
// the running interface, then restarts HAProxy and reloads Caddy where
// their files changed.
func (p *Plan) applyChange(c *siteChange) error {
	t := p.transports[c.site]
	if len(c.plan.Writes()) > 0 || c.plan.WireGuard != apply.WireGuardNone {
		if err := apply.Execute(c.plan, t); err != nil {
			return err
		}
	}
	if c.haproxy {
		if err := p.restartProxy(c.site, c.dbApps); err != nil {
			return err
		}
	}
	if c.caddy {
		if out, err := t.Run(validateCaddy(p.dep())); err != nil {
			return fmt.Errorf("%s: the routes without %s do not validate, so Caddy was not reloaded and still serves the old ones: %w\n%s", c.site, p.Site, err, out)
		}
		if out, err := t.Run(reloadCaddy(p.dep())); err != nil {
			return fmt.Errorf("%s: reloading Caddy: %w: %s", c.site, err, lastLines(out, 5))
		}
		p.say("  %-9s Caddy on %s\n", "reloaded", c.site)
	}
	return nil
}

func validateCaddy(d deployment.Deployment) string {
	return d.ComposeCmd("infra") + " exec -T caddy caddy validate --config /etc/caddy/Caddyfile"
}

func reloadCaddy(d deployment.Deployment) string {
	return d.ComposeCmd("infra") + " exec -T caddy caddy reload --config /etc/caddy/Caddyfile"
}

// restartHAProxy restarts HAProxy alone: its configuration is a single file
// bind mount, which only a restart reads again (see site add's).
func (p *Plan) restartHAProxy() string { return p.dep().ComposeCmd("infra") + " restart haproxy" }

func statsProbe() string {
	return fmt.Sprintf("curl -fsS --max-time 5 'http://127.0.0.1:%d/stats;csv'", render.HAProxyStatsPort)
}

// databaseApps is the apps on site that reach the cluster's database through
// its HAProxy.
func (p *Plan) databaseApps(site string) []string {
	var out []string
	for _, app := range apply.ClusterDatabaseApps(p.end) {
		if renders(p.rendered, site, p.dep().RelPath(app, "compose.yaml")) {
			out = append(out, app)
		}
	}
	return out
}

// proxyLists reports whether the running HAProxy on site still lists the
// removed site, so a run that wrote the file and stopped before restarting
// restarts it.
func (p *Plan) proxyLists(site string) (bool, error) {
	out, err := p.transports[site].Run(statsProbe())
	if err != nil {
		if errors.Is(err, apply.ErrUnreachable) {
			return false, fmt.Errorf("site remove %s: %s: could not read host state, so whether its HAProxy lists %s is unknown: %w", p.Site, site, p.Site, err)
		}
		return true, nil
	}
	_, ok := backendStatus(out)[p.Site]
	return ok, nil
}

// restartProxy restarts one site's HAProxy with its database apps stopped
// around it, then starts them and gates each as apply does, for the reason
// site add gives: an app's workers may never reconnect after their
// connections close under them.
func (p *Plan) restartProxy(site string, apps []string) error {
	t := p.transports[site]
	for _, app := range apps {
		if out, err := t.Run(p.dep().ComposeCmd(app) + " stop"); err != nil {
			return fmt.Errorf("%s: stopping %s before HAProxy's restart, so HAProxy was not restarted: %w: %s", site, app, err, lastLines(out, 5))
		}
	}
	p.say("  %-9s HAProxy on %s\n", "restart", site)
	out, restartErr := t.Run(p.restartHAProxy())
	for _, app := range apps {
		if out, err := t.Run(p.dep().ComposeCmd(app) + " up -d"); err != nil {
			return fmt.Errorf("%s: starting %s after HAProxy's restart: %w: %s", site, app, err, lastLines(out, 5))
		}
	}
	if restartErr != nil {
		return fmt.Errorf("%s: restarting HAProxy: %w: %s", site, restartErr, lastLines(out, 5))
	}
	for _, app := range apps {
		if err := apply.WaitHealthy(p.dep(), site, app, t, "Site remove stopped here, before HAProxy's gate. The app was started against the new HAProxy; read its logs, fix it, and run site remove again"); err != nil {
			return err
		}
	}
	return nil
}

// backendStatus reads the postgres proxy's servers and their status from the
// stats CSV, by column name (HAProxy 3.0, doc/management.txt, "9.1. CSV
// format").
func backendStatus(csv string) map[string]string {
	out := map[string]string{}
	var cols map[string]int
	for _, line := range strings.Split(csv, "\n") {
		if header, ok := strings.CutPrefix(line, "# "); ok {
			cols = map[string]int{}
			for i, name := range strings.Split(header, ",") {
				cols[name] = i
			}
			continue
		}
		if cols == nil || line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		get := func(name string) string {
			i, ok := cols[name]
			if !ok || i >= len(fields) {
				return ""
			}
			return fields[i]
		}
		if get("pxname") != "postgres" {
			continue
		}
		if sv := get("svname"); sv != "FRONTEND" && sv != "BACKEND" {
			out[sv] = get("status")
		}
	}
	return out
}

func (p *Plan) haproxyGate(sites []string) error {
	want := strings.Join(sortedCopy(p.end.Cluster.Sites), ",")
	err := poll(haproxyWait, haproxyPoll, func() error {
		list, err := p.patroniList()
		if err != nil {
			return err
		}
		leader := ""
		for _, m := range list {
			if m.role() == "Leader" {
				leader = m.name()
			}
		}
		for _, site := range sites {
			out, err := p.transports[site].Run(statsProbe())
			if err != nil {
				return fmt.Errorf("%s: HAProxy's statistics: %w: %s", site, err, lastLines(out, 2))
			}
			servers := backendStatus(out)
			var names []string
			for name := range servers {
				names = append(names, name)
			}
			sort.Strings(names)
			if strings.Join(names, ",") != want {
				return fmt.Errorf("%s: HAProxy's backends are %s, and the end state's cluster.sites is %s", site, strings.Join(names, ", "), strings.ReplaceAll(want, ",", ", "))
			}
			if !strings.HasPrefix(servers[leader], "UP") {
				return fmt.Errorf("%s: HAProxy has the leader %s %s", site, leader, servers[leader])
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("HAProxy after %s: %w", haproxyWait, err)
	}
	return nil
}

// meshGate passes when no remaining site has the removed site's key as a
// WireGuard peer. `wg show <interface> peers` prints one public key per line
// (wireguard-tools, wg(8)).
func (p *Plan) meshGate(key string) error {
	for _, name := range p.end.SiteNames() {
		out, err := p.transports[name].Run("wg show " + p.dep().Interface() + " peers")
		if err != nil {
			return fmt.Errorf("%s: `wg show %s peers`: %w: %s", name, p.dep().Interface(), err, lastLines(out, 2))
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == key {
				return fmt.Errorf("%s still has %s as a WireGuard peer", name, p.Site)
			}
		}
	}
	return nil
}

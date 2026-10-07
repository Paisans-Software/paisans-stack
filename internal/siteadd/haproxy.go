package siteadd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// haproxyFile is HAProxy's configuration, relative to a site's root.
const haproxyFile = "srv/infra/haproxy/haproxy.cfg"

// restartHAProxy restarts HAProxy alone. Not a reload by SIGHUP, which the
// image supports (docker-library/docs, haproxy, "Reloading config"): the
// configuration is a single file bind mount, apply replaces the file by
// renaming a new one over it, and a bind mount of a single file keeps the
// inode it was started with (moby/moby#15793), so a reloaded HAProxy would
// read the old file.
// A restart mounts the path again. Not the whole stack either, which would
// restart etcd and Patroni with it.
const restartHAProxy = "docker compose -f /srv/infra/compose.yaml restart haproxy"

// statsProbe reads HAProxy's statistics as CSV from its loopback listener
// (haproxy.cfg.tmpl, listen stats). curl is on the host: host prepare
// installs it.
func statsProbe() string {
	return fmt.Sprintf("curl -fsS --max-time 5 'http://127.0.0.1:%d/stats;csv'", render.HAProxyStatsPort)
}

// buildHAProxy is stage 6: every site running HAProxy gets the new backend.
func (p *Plan) buildHAProxy(rendered *render.Plan) (*Stage, error) {
	st := &Stage{
		Number: 6,
		Name:   "haproxy",
		Gate:   "HAProxy's statistics on every site running it list every cluster site, with the leader UP and every replica DOWN by health check, so it routes only to the primary",
	}
	plans := map[string]*apply.Plan{}
	var sites []string
	for _, name := range p.cfg.SiteNames() {
		if !renders(rendered, name, haproxyFile) {
			continue
		}
		sites = append(sites, name)
		sp, err := apply.Build(name, rendered, "", p.transports[name], apply.Scope(haproxyFile))
		if err != nil {
			return nil, err
		}
		if c := sp.Conflicts(); len(c) > 0 {
			return nil, fmt.Errorf("site add %s: %s's %s differs from what the last apply recorded, so somebody edited it on the host. Nothing was changed. Restore it, or copy what is wanted into the configuration, and run site add again", p.Site, name, c[0].Path)
		}
		plans[name] = sp
		for _, c := range sp.Writes() {
			st.Steps = append(st.Steps, Step{Site: name, Verb: c.Kind.String(), Text: c.Path})
		}
		if len(sp.Writes()) > 0 || !p.servesEverySite(name) {
			st.Steps = append(st.Steps, Step{Site: name, Verb: "restart", Text: "HAProxy alone: " + restartHAProxy})
		}
	}
	st.run = func() error {
		for _, name := range sites {
			sp := plans[name]
			wrote := len(sp.Writes()) > 0
			if wrote {
				if err := apply.Execute(sp, p.transports[name]); err != nil {
					return err
				}
			}
			if wrote || !p.servesEverySite(name) {
				p.say("  %-9s HAProxy on %s\n", "restart", name)
				if out, err := p.transports[name].Run(restartHAProxy); err != nil {
					return fmt.Errorf("%s: restarting HAProxy: %s", name, lastLines(out, 5))
				}
			}
		}
		return nil
	}
	st.gate = func() error { return p.haproxyGate(sites) }
	return st, nil
}

// servesEverySite reports whether the running HAProxy already lists every
// cluster site, so a run that wrote the file and stopped before restarting
// still restarts it.
func (p *Plan) servesEverySite(site string) bool {
	out, err := p.transports[site].Run(statsProbe())
	if err != nil {
		return false
	}
	servers := backendStatus(out)
	for _, name := range p.cfg.Cluster.Sites {
		if _, ok := servers[name]; !ok {
			return false
		}
	}
	return true
}

// backendStatus reads the postgres proxy's servers and their status from the
// stats CSV. The first line names the columns after a "# " (HAProxy 3.0,
// doc/management.txt, "9.1. CSV format"); pxname, svname and status are
// looked up by name rather than by position.
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
	var problems []string
	n := attempts(haproxyWait, haproxyPoll)
	for i := 0; i < n; i++ {
		problems = nil
		leader := ""
		members, err := p.patroniList()
		if err != nil {
			problems = append(problems, err.Error())
		}
		for _, m := range members {
			if m.role() == "Leader" {
				leader = m.name()
			}
		}
		if leader == "" && err == nil {
			problems = append(problems, "Patroni reports no leader")
		}
		for _, site := range sites {
			out, err := p.transports[site].Run(statsProbe())
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: HAProxy's statistics: %s", site, lastLines(out, 2)))
				continue
			}
			servers := backendStatus(out)
			var names []string
			for name := range servers {
				names = append(names, name)
			}
			sort.Strings(names)
			if strings.Join(names, ",") != strings.Join(sortedCopy(p.cfg.Cluster.Sites), ",") {
				problems = append(problems, fmt.Sprintf("%s: HAProxy's backends are %s, and cluster.sites is %s", site, strings.Join(names, ", "), strings.Join(sortedCopy(p.cfg.Cluster.Sites), ", ")))
				continue
			}
			for _, name := range names {
				status := servers[name]
				switch {
				case name == leader && !strings.HasPrefix(status, "UP"):
					problems = append(problems, fmt.Sprintf("%s: HAProxy has the leader %s %s", site, name, status))
				case name != leader && !strings.HasPrefix(status, "DOWN"):
					problems = append(problems, fmt.Sprintf("%s: HAProxy has the replica %s %s, so it could route to it", site, name, status))
				}
			}
		}
		if len(problems) == 0 {
			return nil
		}
		if i < n-1 {
			sleep(haproxyPoll)
		}
	}
	return fmt.Errorf("HAProxy does not route only to the primary after %s:\n  %s", haproxyWait, strings.Join(problems, "\n  "))
}

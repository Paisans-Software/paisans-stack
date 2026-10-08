package siteadd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// haproxyFile is HAProxy's configuration, in rendered form.
func (p *Plan) haproxyFile() string { return p.dep().RelPath("infra", "haproxy", "haproxy.cfg") }

// restartHAProxy restarts HAProxy alone. Not a reload by SIGHUP, which the
// image supports (docker-library/docs, haproxy, "Reloading config"): the
// configuration is a single file bind mount, apply replaces the file by
// renaming a new one over it, and a bind mount of a single file keeps the
// inode it was started with (moby/moby#15793), so a reloaded HAProxy would
// read the old file.
// A restart mounts the path again. Not the whole stack either, which would
// restart etcd and Patroni with it.
func (p *Plan) restartHAProxy() string { return p.dep().ComposeCmd("infra") + " restart haproxy" }

// statsProbe reads HAProxy's statistics as CSV from its loopback listener
// (haproxy.cfg.tmpl, listen stats). curl is on the host: host prepare
// installs it.
func statsProbe() string {
	return fmt.Sprintf("curl -fsS --max-time 5 'http://127.0.0.1:%d/stats;csv'", render.HAProxyStatsPort)
}

// buildHAProxy is stage 6: every site running HAProxy gets the new backend,
// and the apps that reach the database through it are stopped around its
// restart (see restartProxy).
func (p *Plan) buildHAProxy(rendered *render.Plan) (*Stage, error) {
	st := &Stage{
		Number: 6,
		Name:   "haproxy",
		Gate:   "HAProxy's statistics on every site running it list every cluster site, with the leader UP and every replica DOWN by health check, so it routes only to the primary",
	}
	plans := map[string]*apply.Plan{}
	// restarts holds the sites whose HAProxy this stage restarts, and apps
	// the stacks on each that reach the database through it.
	restarts := map[string]bool{}
	apps := map[string][]string{}
	var sites []string
	for _, name := range p.cfg.SiteNames() {
		if !renders(rendered, name, p.haproxyFile()) {
			continue
		}
		sites = append(sites, name)
		sp, err := apply.Build(name, rendered, "", p.transports[name], apply.Scope(p.haproxyFile()))
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
		serves, err := p.servesEverySite(name)
		if err != nil {
			return nil, err
		}
		if len(sp.Writes()) > 0 || !serves {
			restarts[name] = true
			apps[name] = p.databaseApps(rendered, name)
			list := strings.Join(apps[name], ", ")
			if list != "" {
				st.Steps = append(st.Steps, Step{Site: name, Verb: "stop", Text: list + ": they reach the database through this HAProxy, and an app's open connections do not survive its restart (" + p.composeEach(apps[name], "stop") + ")"})
			}
			st.Steps = append(st.Steps, Step{Site: name, Verb: "restart", Text: "HAProxy alone: " + p.restartHAProxy()})
			if list != "" {
				st.Steps = append(st.Steps, Step{Site: name, Verb: "start", Text: list + " (" + p.composeEach(apps[name], "up -d") + ")"})
				st.Steps = append(st.Steps, Step{Site: name, Verb: "check", Text: list + ": every container running, and healthy where it has a healthcheck, as apply's gate"})
			}
		}
	}
	st.run = func() error {
		for _, name := range sites {
			sp := plans[name]
			if len(sp.Writes()) > 0 {
				if err := apply.Execute(sp, p.transports[name]); err != nil {
					return err
				}
			}
			if restarts[name] {
				if err := p.restartProxy(name, apps[name]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	st.gate = func() error { return p.haproxyGate(sites) }
	return st, nil
}

// databaseApps is the apps on site that reach the cluster's database through
// its HAProxy: apply.ClusterDatabaseApps, where the render places them.
func (p *Plan) databaseApps(rendered *render.Plan, site string) []string {
	var out []string
	for _, app := range apply.ClusterDatabaseApps(p.cfg) {
		if renders(rendered, site, p.dep().RelPath(app, "compose.yaml")) {
			out = append(out, app)
		}
	}
	return out
}

// composeEach is one compose command per stack, as the plan shows them.
func (p *Plan) composeEach(stacks []string, verb string) string {
	var out []string
	for _, stack := range stacks {
		out = append(out, p.appCompose(stack, verb))
	}
	return strings.Join(out, "; ")
}

func (p *Plan) appCompose(stack, verb string) string {
	return p.dep().ComposeCmd(stack) + " " + verb
}

// restartProxy restarts one site's HAProxy with its database apps stopped
// around it, then starts them and gates each as apply does.
//
// The first real join restarted HAProxy under a running Mbin, and Mbin
// answered 500 until it was recreated by hand: its FrankenPHP workers hold
// their connections open, the restart closed them, and the workers never
// reconnect. Its healthcheck does not touch the database, so the container
// stayed healthy throughout. Stopping the apps first means none of them is
// holding a connection when HAProxy goes, and each starts against the new
// HAProxy with fresh ones.
//
// A failed HAProxy restart still starts the apps this stopped, so that the
// error is the only thing the operator has to fix.
func (p *Plan) restartProxy(site string, apps []string) error {
	t := p.transports[site]
	for _, app := range apps {
		p.say("  %-9s %s on %s\n", "stop", app, site)
		if out, err := t.Run(p.appCompose(app, "stop")); err != nil {
			return fmt.Errorf("%s: stopping %s before HAProxy's restart, so HAProxy was not restarted: %s", site, app, lastLines(out, 5))
		}
	}
	p.say("  %-9s HAProxy on %s\n", "restart", site)
	out, restartErr := t.Run(p.restartHAProxy())
	for _, app := range apps {
		p.say("  %-9s %s on %s\n", "start", app, site)
		if out, err := t.Run(p.appCompose(app, "up -d")); err != nil {
			return fmt.Errorf("%s: starting %s after HAProxy's restart: %s", site, app, lastLines(out, 5))
		}
	}
	if restartErr != nil {
		return fmt.Errorf("%s: restarting HAProxy: %s", site, lastLines(out, 5))
	}
	for _, app := range apps {
		if err := apply.WaitHealthy(p.dep(), site, app, t, "Site add stopped here, before HAProxy's gate. The app was started against the new HAProxy; read its logs, fix it, and run site add again"); err != nil {
			return err
		}
		p.say("  %-9s %s on %s healthy\n", "checked", app, site)
	}
	return nil
}

// servesEverySite reports whether the running HAProxy already lists every
// cluster site, so a run that wrote the file and stopped before restarting
// still restarts it.
//
// A probe that never reached the host is an error, not a "no". Read as a no,
// an ssh timeout planned an HAProxy restart, which stops every database app
// on the site, for a proxy that may have been fine. A probe that ran and
// failed (no HAProxy answering its statistics) is still a no: that HAProxy
// does need starting.
func (p *Plan) servesEverySite(site string) (bool, error) {
	out, err := p.transports[site].Run(statsProbe())
	if err != nil {
		if errors.Is(err, apply.ErrUnreachable) {
			return false, fmt.Errorf("site add %s: %s: could not read host state, so whether its HAProxy lists every site is unknown: %w", p.Site, site, err)
		}
		return false, nil
	}
	servers := backendStatus(out)
	for _, name := range p.cfg.Cluster.Sites {
		if _, ok := servers[name]; !ok {
			return false, nil
		}
	}
	return true, nil
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

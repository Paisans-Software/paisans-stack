// Package failover is `paisans failover test`: a controlled switchover of the
// Patroni primary to another data site and back, with the applications
// checked on each side.
//
// A cluster that has never failed over has an untested failover. This makes
// the test a routine, gated operation rather than something learned during an
// outage: every check runs before the first switchover, every switchover ends
// at a gate, and a gate that does not pass stops the test where it is, with
// the evidence, rather than switching back blind.
package failover

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/patroni"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// Options is everything Run needs besides the configuration.
type Options struct {
	// Transports reaches each site, keyed by site name.
	Transports map[string]apply.Transport
	// Client makes the requests to each app through the gateway. Nil is a
	// client with a ten second timeout that does not follow redirects.
	Client *http.Client
	// URL builds the URL for an app's hostname and path. Nil is
	// https://<hostname><path>. Tests point it at a local server.
	URL func(hostname, path string) string
	// Report is where the plan and progress go. Nil reports nothing.
	Report ui.Reporter
	// Execute runs the switchovers. Without it Run checks and lists the plan.
	Execute bool
}

// How long a switchover has to reach its gate, and how often to look. Patroni
// promotes in seconds, but the gate also waits for every app's healthcheck to
// pass again, which on a stack that restarts on a lost connection takes as
// long as its healthcheck's start period.
var (
	gateWait   = 3 * time.Minute
	gatePoll   = 3 * time.Second
	lagPoll    = 2 * time.Second
	lagLooks   = 3
	sleep      = time.Sleep
	appTimeout = 10 * time.Second
)

// SwitchoverCommand is what runs in the leader's Patroni container.
//
// The flags are Patroni v4.1.0's (patroni/ctl.py, `switchover`): --leader
// (alias --primary) names the current leader and is checked against the
// cluster's, so a test planned against a leader that has since changed fails
// rather than switching the wrong way; --candidate names the member to
// promote; --force skips every confirmation, and with no --scheduled it also
// means now (`_do_failover_or_switchover` prompts for a time only without
// --force). Patroni 3.3.3 and 4.0.4, which the other Spilo tags render pins
// ship, accept --leader too. The config path is the one Patroni itself runs with in Spilo
// (zalando/spilo 4.1-p2, postgres-appliance/runit/patroni/run), a link to the
// /run/postgres.yml that configure_spilo.py writes (build_scripts/
// post_build.sh). The same holds at every Spilo tag render pins.
//
// Its exit status is not the verdict. When the REST call answers with an
// error status, patronictl prints "Switchover failed" and returns normally,
// so the gate after it, which reads /cluster, is what decides.
func SwitchoverCommand(d deployment.Deployment, leader, candidate string) string {
	return fmt.Sprintf("%s patronictl -c /home/postgres/postgres.yml switchover --leader %s --candidate %s --force",
		patroni.Exec(d), leader, candidate)
}

// appPath is the path requested from each app: the root, and for Pocket ID
// its OpenID discovery document, which is the first thing every client of it
// fetches and so the answer that matters.
func appPath(kind config.Kind) string {
	if kind == config.KindPocketID {
		return "/.well-known/openid-configuration"
	}
	return "/"
}

type runner struct {
	cfg *config.Config
	o   Options
}

// Run checks, lists the plan, and with Execute switches the primary to the
// candidate and back, gating each switch.
func Run(cfg *config.Config, o Options) error {
	if o.Report == nil {
		o.Report = ui.Discard
	}
	if o.URL == nil {
		o.URL = func(hostname, path string) string { return "https://" + hostname + path }
	}
	if o.Client == nil {
		o.Client = &http.Client{
			Timeout:       appTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
		}
	}
	r := &runner{cfg: cfg, o: o}
	if len(cfg.Cluster.Sites) < 2 {
		return fmt.Errorf("failover test: cluster.sites lists %d site(s), and a switchover needs another data site to switch to", len(cfg.Cluster.Sites))
	}

	o.Report.Section("preflight")
	leader, candidate, err := r.preflight()
	if err != nil {
		return err
	}

	steps := []struct{ from, to string }{{leader, candidate}, {candidate, leader}}
	if !o.Execute {
		r.listPlan(steps[0].from, steps[0].to, steps[1].from, steps[1].to)
		o.Report.Result("Nothing changed. Re-run with --execute to run the test.")
		return nil
	}

	for i, step := range steps {
		o.Report.Section(fmt.Sprintf("switchover %d: %s to %s", i+1, step.from, step.to))
		if err := r.switchover(step.from, step.to); err != nil {
			return err
		}
	}
	o.Report.Result("Failover test passed: %s is the primary again.", leader)
	return nil
}

// cluster reads /cluster from the first cluster site that answers. Any
// member's API describes the whole cluster, and during a switchover the one
// that answers may not be the one that answered last time.
func (r *runner) cluster() (patroni.Cluster, error) {
	var tried []string
	for _, name := range r.cfg.Cluster.Sites {
		t, ok := r.o.Transports[name]
		if !ok {
			tried = append(tried, name+": no transport")
			continue
		}
		api := fmt.Sprintf("%s:%d", r.cfg.Sites[name].Address, render.PatroniAPIPort)
		out, err := t.Run(patroni.ClusterCommand(r.cfg.Deployment(), api))
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s: %s", name, firstLine(out, err)))
			continue
		}
		c, err := patroni.Parse(out)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		return c, nil
	}
	return patroni.Cluster{}, fmt.Errorf("no cluster site's Patroni answered: %s", strings.Join(tried, "; "))
}

// clusterProblems is everything the cluster's state says against a
// switchover: a declared member missing, a leader not running, a replica not
// streaming, no Sync Standby where synchronous mode is declared, or lag.
func (r *runner) clusterProblems(c patroni.Cluster) (leader string, problems []string, lagging bool) {
	l, ok := c.Leader()
	if !ok {
		return "", []string{"no running leader"}, false
	}
	leader = l.Name
	for _, name := range r.cfg.Cluster.Sites {
		m, ok := c.Member(name)
		switch {
		case !ok:
			problems = append(problems, name+" is not a cluster member")
		case name == leader:
		case m.State != "streaming":
			problems = append(problems, fmt.Sprintf("%s is %s, not streaming", name, m.State))
		default:
			if n, ok := m.LagBytes(); !ok || n != 0 {
				problems = append(problems, fmt.Sprintf("%s lags by %s", name, m.LagText()))
				lagging = true
			}
		}
	}
	if r.cfg.Cluster.Synchronous && !c.HasSyncStandby() {
		problems = append(problems, "cluster.synchronous is true and no member is a Sync Standby")
	}
	return leader, problems, lagging
}

// preflight is the spec's step 1. Lag is looked at up to lagLooks times,
// because Patroni compares each replica with the leader's last reported
// position, which is refreshed once per loop; a busy minute shows a few
// kilobytes that are gone on the next look.
func (r *runner) preflight() (leader, candidate string, err error) {
	var problems []string
	var c patroni.Cluster
	for look := 0; look < lagLooks; look++ {
		c, err = r.cluster()
		if err != nil {
			return "", "", fmt.Errorf("failover test refused: %w", err)
		}
		var lagging bool
		leader, problems, lagging = r.clusterProblems(c)
		if !lagging || look == lagLooks-1 {
			break
		}
		sleep(lagPoll)
	}
	s := r.o.Report.Step("check cluster")
	if len(problems) > 0 {
		s.Fail(errors.New(strings.Join(problems, "; ")))
		r.o.Report.Refuse("cluster is not ready for a switchover", strings.Join(problems, "\n"))
	} else {
		s.Detail("%s", r.describeCluster(c))
		s.Done(leader + " leads")
	}

	appProblems := r.appProblems()
	refused := len(problems) > 0 || len(appProblems) > 0
	if refused {
		return "", "", fmt.Errorf("failover test refused: the cluster and every app must be healthy before the primary is moved, and the above is not")
	}
	return leader, r.candidate(c, leader), nil
}

// candidate is the Sync Standby where there is one, since it is the member
// Patroni would promote on a real failure, and otherwise the first other
// cluster site.
func (r *runner) candidate(c patroni.Cluster, leader string) string {
	for _, m := range c.Members {
		if m.Role == "sync_standby" && m.Name != leader {
			return m.Name
		}
	}
	for _, name := range r.cfg.Cluster.Sites {
		if name != leader {
			return name
		}
	}
	return ""
}

func (r *runner) describeCluster(c patroni.Cluster) string {
	var parts []string
	for _, m := range c.Members {
		part := fmt.Sprintf("%s %s %s", m.Name, m.Role, m.State)
		if m.Role != "leader" {
			part += ", lag " + m.LagText()
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// appProblems checks every app stack where it runs, with the same judgement
// as apply's health gate, and asks every app through the gateway. It reports
// one step per stack and per app, and returns what failed.
func (r *runner) appProblems() []string {
	var problems []string
	sites := render.AppSites(r.cfg)
	for _, app := range r.cfg.AppNames() {
		for _, site := range sites[app] {
			t, ok := r.o.Transports[site]
			var err error
			s := r.o.Report.Step(fmt.Sprintf("check %s on %s", app, site))
			if !ok {
				err = fmt.Errorf("stack %s: no transport for %s", app, site)
			} else {
				err = apply.StackHealthy(r.cfg.Deployment(), app, t)
			}
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s on %s: %v", app, site, err))
				s.Fail(err)
				r.o.Report.Refuse(fmt.Sprintf("%s is not healthy on %s", app, site), err.Error())
			} else {
				s.Done("healthy")
			}
		}
	}
	for _, app := range r.cfg.AppNames() {
		s := r.o.Report.Step("check " + app + " answers")
		detail, err := r.answer(app)
		if err != nil {
			problems = append(problems, err.Error())
			s.Fail(err)
			r.o.Report.Refuse(app+" does not answer through the gateway", err.Error())
		} else {
			s.Detail("%s", detail)
			s.Done("answers")
		}
	}
	// A Pocket ID on more than one apps site passes the stack check above
	// whether it is active or standing by, and the gateway answers from
	// whichever site is first, so neither can see two active or none. A
	// switchover drops the active instance's database connection and the
	// gate restarts it, so the handover is checked here, every time.
	for _, app := range apply.StandbyApps(r.cfg) {
		s := r.o.Report.Step("check " + app + " standby")
		list := apply.LookAtInstances(r.cfg, app, r.o.Transports)
		if _, err := apply.OneActive(r.cfg.Deployment(), app, list); err != nil {
			problems = append(problems, err.Error())
			s.Fail(err)
			r.o.Report.Refuse(app+" does not have exactly one active instance", fmt.Sprintf("%v: %s", err, summary(list)))
		} else {
			s.Done(summary(list))
		}
	}
	return problems
}

// summary is each site's instance state on one line.
func summary(list []apply.Instance) string {
	var parts []string
	for _, in := range list {
		parts = append(parts, fmt.Sprintf("%s %s", in.Site, in.State))
	}
	return strings.Join(parts, ", ")
}

// answer requests one app through the gateway. Anything under 500 means the
// gateway reached an app that could answer, which a redirect to a login page
// is; a 5xx, or no answer, means it did not.
func (r *runner) answer(app string) (string, error) {
	declared := r.cfg.Apps[app]
	url := r.o.URL(declared.Hostname, appPath(declared.Kind))
	resp, err := r.o.Client.Get(url)
	if err != nil {
		return "", fmt.Errorf("%s: %v", url, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	return fmt.Sprintf("%s answered %d", url, resp.StatusCode), nil
}

// databaseStack is one app stack on one apps site that reaches the database
// through that site's HAProxy. that reach the database
// through that site's HAProxy, as apply.ClusterDatabaseApps names them.
type databaseStack struct{ site, app string }

func (r *runner) databaseStacks() []databaseStack {
	var out []databaseStack
	placed := render.AppSites(r.cfg)
	apps := apply.ClusterDatabaseApps(r.cfg)
	for _, site := range r.cfg.AppsSites() {
		for _, app := range apps {
			for _, s := range placed[app] {
				if s == site {
					out = append(out, databaseStack{site: site, app: app})
				}
			}
		}
	}
	return out
}

func restartCommand(d deployment.Deployment, app string) string {
	return d.ComposeCmd(app) + " restart"
}

// listPlan is the dry run: each switchover as the steps Execute reports,
// titled alike, with the command it runs and the cost of it as details.
func (r *runner) listPlan(leader, candidate, back, home string) {
	rep := r.o.Report
	stacks := r.databaseStacks()
	for i, step := range []struct{ from, to string }{{leader, candidate}, {back, home}} {
		rep.Section(fmt.Sprintf("switchover %d: %s to %s", i+1, step.from, step.to))
		rep.Step("switch primary").End(ui.Pending, "")
		rep.Detail("on %s: %s", step.from, SwitchoverCommand(r.cfg.Deployment(), step.from, step.to))
		rep.Step("wait for cluster").End(ui.Pending, "")
		rep.Detail("gate: %s leads, %s streams from it", step.to, step.from)
		if len(stacks) > 0 {
			rep.Detail("restart the apps that use the cluster database, on every apps site: the leader change dropped their connections")
		}
		for _, st := range stacks {
			rep.Step(fmt.Sprintf("restart %s on %s", st.app, st.site)).End(ui.Pending, "")
			rep.Detail("on %s: %s", st.site, restartCommand(r.cfg.Deployment(), st.app))
		}
		rep.Step("wait for apps").End(ui.Pending, "")
		rep.Detail("gate: every app stack healthy and answering")
	}
	rep.Warn("writes fail twice, for several seconds each", "expected interruption, twice: writes fail from the moment the old primary demotes until the new one is promoted")
	rep.Detail("expected interruption, twice: writes fail from the moment the old primary")
	rep.Detail("demotes until the new one is promoted and each site's HAProxy marks it up.")
	rep.Detail("HAProxy asks every member's /primary every 3 s and needs 2 passes (inter 3s,")
	rep.Detail("rise 2 in haproxy.cfg), so allow several seconds after promotion. Connections")
	rep.Detail("to the old primary are closed when it is marked down (on-marked-down")
	rep.Detail("shutdown-sessions), so apps must reconnect. Reads through HAProxy pause too: it")
	rep.Detail("routes only to the primary.")
	if len(stacks) > 0 {
		rep.Detail("Not every app reconnects (Mbin's workers do not), so each app above is")
		rep.Detail("restarted after each switch, a further outage of a few seconds per app.")
	}
	for _, app := range apply.StandbyApps(r.cfg) {
		rep.Detail("Restarting %s's active Pocket ID hands it to a standby site, which takes over", app)
		rep.Detail("at its next retry (15 s by default); sign in pauses until then. Each gate")
		rep.Detail("waits for exactly one active instance.")
	}
}

// switchover runs one switch and waits for its gate.
func (r *runner) switchover(from, to string) error {
	t, ok := r.o.Transports[from]
	if !ok {
		return fmt.Errorf("no transport for %s", from)
	}
	s := r.o.Report.Step("switch primary")
	s.Detail("on %s: %s", from, SwitchoverCommand(r.cfg.Deployment(), from, to))
	out, err := t.Run(SwitchoverCommand(r.cfg.Deployment(), from, to))
	r.o.Report.Trace("patronictl switchover", out)
	if err != nil {
		s.Fail(err)
		return fmt.Errorf("switchover from %s to %s did not run: %v. Nothing after it was started; read `patronictl list` on %s", from, to, err, from)
	}
	s.Done("")
	return r.gate(from, to)
}

// gate waits until the candidate leads and the old leader streams from it,
// restarts every app that uses the cluster database on every apps site, then
// waits until every app is healthy and answering. It polls rather than
// looking once, because each of those settles a few seconds apart, and stops
// at gateWait with what was still failing.
//
// The restart is on every apps site, not only the old primary's: a leader
// change closes every client connection to the old primary wherever the client
// runs, and an app whose workers never reconnect (Mbin's FrankenPHP workers,
// found after a real HAProxy restart) answers 500 until it is restarted, with
// a container its own healthcheck still calls healthy. It waits for the
// cluster first, so that the apps start against the new primary.
func (r *runner) gate(from, to string) error {
	wait := r.o.Report.Step("wait for cluster")
	wait.Detail("%s leads, %s streams", to, from)
	if err := r.poll(from, to, func() []string { return r.clusterGate(from, to) }); err != nil {
		wait.Fail(err)
		return err
	}
	wait.Done("")
	for _, st := range r.databaseStacks() {
		s := r.o.Report.Step(fmt.Sprintf("restart %s on %s", st.app, st.site))
		t, ok := r.o.Transports[st.site]
		if !ok {
			err := fmt.Errorf("failover test stopped after switching %s to %s: no transport for %s to restart %s, so nothing after it ran", from, to, st.site, st.app)
			s.Fail(err)
			return err
		}
		s.Detail("%s", restartCommand(r.cfg.Deployment(), st.app))
		out, err := t.Run(restartCommand(r.cfg.Deployment(), st.app))
		if err != nil {
			err = fmt.Errorf("failover test stopped after switching %s to %s: restarting %s on %s failed, so nothing after it ran: %s", from, to, st.app, st.site, firstLine(out, err))
			s.Fail(err)
			return err
		}
		s.Done("")
	}
	apps := r.o.Report.Step("wait for apps")
	if err := r.poll(from, to, func() []string { return r.gateProblems(from, to) }); err != nil {
		apps.Fail(err)
		return err
	}
	apps.Done("")
	return nil
}

// poll looks at problems until there are none or gateWait has passed.
func (r *runner) poll(from, to string, problems func() []string) error {
	attempts := int(gateWait / gatePoll)
	if attempts < 1 {
		attempts = 1
	}
	var last []string
	for i := 0; i < attempts; i++ {
		last = problems()
		if len(last) == 0 {
			return nil
		}
		if i < attempts-1 {
			sleep(gatePoll)
		}
	}
	sort.Strings(last)
	return fmt.Errorf("failover test stopped: the gate after switching %s to %s did not pass within %s, so nothing after it ran:\n  %s\nRead `patronictl list` in the Patroni container on any cluster site before acting; the primary may now be either site",
		from, to, gateWait, strings.Join(last, "\n  "))
}

// gateProblems is one look at the gate's conditions, quietly: progress lines
// for every poll would bury the one that matters.
func (r *runner) gateProblems(from, to string) []string {
	problems := r.clusterGate(from, to)
	quiet := *r
	quiet.o.Report = ui.Discard
	return append(problems, quiet.appProblems()...)
}

// clusterGate is the cluster's half of the gate: the candidate leads and the
// old leader streams from it.
func (r *runner) clusterGate(from, to string) []string {
	c, err := r.cluster()
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	if l, ok := c.Leader(); !ok || l.Name != to {
		got := "none"
		if ok {
			got = l.Name
		}
		problems = append(problems, fmt.Sprintf("leader is %s, not %s", got, to))
	}
	if m, ok := c.Member(from); !ok || m.State != "streaming" {
		state := "absent"
		if ok {
			state = m.State
		}
		problems = append(problems, fmt.Sprintf("%s is %s, not streaming", from, state))
	}
	return problems
}

func firstLine(out string, err error) string {
	s := strings.TrimSpace(out)
	if s == "" {
		s = err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

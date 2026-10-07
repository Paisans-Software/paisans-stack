// Package siteadd joins a new data site to a running cluster: `paisans site
// add`. It is staged, and every stage ends at a gate that stops the command
// with its evidence when it does not pass; nothing after a failed gate runs.
//
// The configuration is the end state. Build reads the live deployment (the
// WireGuard files and handshakes on every site, etcd's member list, Patroni's
// member list and dynamic configuration, HAProxy's backend list) and plans
// only what differs, so a run after a fixed problem resumes at the first stage
// whose gate does not yet pass. docs/specs/2026-10-07-site-add.md is the
// approved design; README.md, "The staged gate, and the half-joined site", is
// the argument for it.
package siteadd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/preflight"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// Step is one thing a stage will do, for the plan an operator reads.
type Step struct {
	Site string
	Verb string
	Text string
}

// Stage is one stage of the join: its steps, then its gate.
type Stage struct {
	Number int
	Name   string
	Steps  []Step
	// Gate says what must hold before the next stage may start.
	Gate string
	// OnFailure says what a failed gate undoes, empty when it undoes nothing.
	OnFailure string

	run      func() error
	gate     func() error
	rollback func() error
}

// Plan is a whole join, decided from the live deployment.
type Plan struct {
	Site      string
	Stages    []*Stage
	Preflight preflight.Report
	// Notes are things the join leaves for later, on purpose.
	Notes []string
	// Progress receives each stage as it starts and each gate as it passes.
	// Nil discards it.
	Progress io.Writer

	cfg        *config.Config
	secrets    *config.Secrets
	transports map[string]apply.Transport
	// initial is how each etcd member was first started, by site: what its
	// host records, and, for a member this join adds, what it is given.
	initial map[string]render.EtcdInitial
	// control is a site whose etcd is a running voter, where every etcdctl
	// command runs.
	control string
	// patroni is a site whose Patroni answers, where every patronictl
	// command runs.
	patroni string
	// preflightSkipped says why stage 1 is not run, empty when it is.
	preflightSkipped string
}

func (p *Plan) say(format string, args ...any) {
	if p.Progress != nil {
		fmt.Fprintf(p.Progress, format, args...)
	}
}

// Timing is every wait the gates make. Each wait is a number of polls, the
// wait divided by the interval, so a test that replaces sleep with nothing
// still ends.
var (
	sleep = time.Sleep

	// The mesh: a peer with an endpoint and PersistentKeepalive 25 handshakes
	// within half a minute of being configured; a minute and a half allows
	// for a slow first ping. A handshake older than two minutes is a peer
	// that has stopped answering: WireGuard rekeys every two minutes while
	// traffic flows (WireGuard whitepaper, section 6.1, REKEY_AFTER_TIME).
	meshWait        = 90 * time.Second
	meshPoll        = 5 * time.Second
	handshakeMaxAge = int64(120)

	// etcd: a learner has to start and publish its name before it can be
	// promoted, and etcd refuses the promotion until its log has caught up
	// with the leader's, so promotion is retried with a doubling delay.
	learnerWait       = 2 * time.Minute
	learnerPoll       = 3 * time.Second
	promoteAttempts   = 10
	promoteFirstDelay = 2 * time.Second
	promoteMaxDelay   = 30 * time.Second
	etcdWait          = 60 * time.Second
	etcdPoll          = 5 * time.Second

	// Patroni: pg_basebackup copies the whole database, so streaming can be
	// an hour away on a large one. A run that times out resumes here, and
	// Patroni carries on cloning in the meantime.
	streamWait  = 60 * time.Minute
	streamPoll  = 10 * time.Second
	lagSamples  = 3
	lagInterval = 5 * time.Second
	syncWait    = 2 * time.Minute
	syncPoll    = 5 * time.Second

	// HAProxy: a server is marked UP after two good checks three seconds
	// apart (haproxy.cfg.tmpl, inter 3s rise 2).
	haproxyWait = 60 * time.Second
	haproxyPoll = 3 * time.Second
)

func attempts(wait, poll time.Duration) int {
	if poll <= 0 {
		return 1
	}
	if n := int(wait / poll); n > 0 {
		return n
	}
	return 1
}

// Build decides the join of newSite, reading every site and changing none.
//
// transports maps every declared site's name to how it is reached.
func Build(cfg *config.Config, secrets *config.Secrets, newSite string, transports map[string]apply.Transport) (*Plan, error) {
	if err := checkScope(cfg, newSite, transports); err != nil {
		return nil, err
	}
	p := &Plan{
		Site:       newSite,
		cfg:        cfg,
		secrets:    secrets,
		transports: transports,
		initial:    map[string]render.EtcdInitial{},
	}

	for _, name := range cfg.Etcd.Members {
		in, found, err := apply.ReadEtcdInitial(transports[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if found {
			p.initial[name] = in
		}
	}

	live, err := p.probeEtcd()
	if err != nil {
		return nil, err
	}

	// Preflight checks a blank host, and once stage 2 has run the new site is
	// not one: its own wg0 holds 51820/udp, which the ports check refuses.
	// So a join that has started is recognised from live state and stage 1
	// is not run again; it passed on the run that started the join, which
	// could not have got further otherwise.
	started, why, err := p.joinStarted(live)
	if err != nil {
		return nil, err
	}
	if started {
		p.preflightSkipped = why
	} else {
		report, err := runPreflight(cfg, newSite, transports)
		if err != nil {
			return nil, fmt.Errorf("preflight: %w", err)
		}
		p.Preflight = report
	}
	rendered, err := p.render()
	if err != nil {
		return nil, err
	}

	p.Stages = append(p.Stages, p.buildPreflight())
	mesh, err := p.buildMesh(rendered)
	if err != nil {
		return nil, err
	}
	p.Stages = append(p.Stages, mesh)
	p.Stages = append(p.Stages, p.buildEtcd(live))
	members, err := p.probePatroni()
	if err != nil {
		return nil, err
	}
	p.Stages = append(p.Stages, p.buildReplica(members))
	cluster, err := p.buildClusterConfig()
	if err != nil {
		return nil, err
	}
	p.Stages = append(p.Stages, cluster)
	proxy, err := p.buildHAProxy(rendered)
	if err != nil {
		return nil, err
	}
	p.Stages = append(p.Stages, proxy)

	if err := p.noteOwed(rendered); err != nil {
		return nil, err
	}
	return p, nil
}

// checkScope refuses a join this command does not build, each for the reason
// the spec gives.
func checkScope(cfg *config.Config, newSite string, transports map[string]apply.Transport) error {
	site, ok := cfg.Sites[newSite]
	if !ok {
		return fmt.Errorf("site add: the configuration declares no site %q. Declare it at its end state first, Eg: roles [data], an address, an endpoint and an ssh section", newSite)
	}
	if len(site.Roles) != 1 || !site.Has(config.RoleData) {
		return fmt.Errorf("site add %s: the site's roles must be exactly [data]. Apps on a second site need a placement decision first, since Pocket ID allows one instance per database, and a second gateway or witness is a separate change", newSite)
	}
	if !contains(cfg.Cluster.Sites, newSite) {
		return fmt.Errorf("site add %s: cluster.sites does not list it, so there is no replica to add. List it there", newSite)
	}
	if !contains(cfg.Etcd.Members, newSite) {
		return fmt.Errorf("site add %s: etcd.members does not list it. A data site is an etcd voter, so list it there, and a witness with it if the cluster has one member today", newSite)
	}
	for _, name := range cfg.SiteNames() {
		if cfg.Sites[name].Endpoint == "" {
			return fmt.Errorf("site add %s: sites.%s has no endpoint. site add joins sites that all dial each other directly; a site behind NAT needs the relay README.md describes under \"`site add`: a second data site, and the one hard problem\", which is not built yet", newSite, name)
		}
		if _, ok := transports[name]; !ok {
			return fmt.Errorf("site add %s: no way to reach site %s. Every site is read, and most are changed", newSite, name)
		}
	}
	return nil
}

func (p *Plan) render() (*render.Plan, error) {
	var opts []render.Option
	for site, in := range p.initial {
		opts = append(opts, render.WithEtcdInitial(site, in))
	}
	return render.Build(p.cfg, p.secrets, opts...)
}

// renders reports whether the render has a file at rel for site.
func renders(rendered *render.Plan, site, rel string) bool {
	for _, f := range rendered.Files {
		if f.Path == site+"/"+rel {
			return true
		}
	}
	return false
}

func renderedFile(rendered *render.Plan, site, rel string) string {
	for _, f := range rendered.Files {
		if f.Path == site+"/"+rel {
			return f.Content
		}
	}
	return ""
}

// joinStarted reports whether an earlier run got past stage 1, from what
// only stage 2 or later leaves behind: a wg0.conf on the new site, which host
// prepare does not write and stage 2 does, or the new site in etcd's
// membership.
func (p *Plan) joinStarted(live []apply.EtcdMember) (bool, string, error) {
	_, found, err := p.transports[p.Site].ReadFile("/" + wireguardFile)
	if err != nil {
		return false, "", fmt.Errorf("%s: %w", p.Site, err)
	}
	if found {
		return true, fmt.Sprintf("%s already has /%s, which stage 2 wrote", p.Site, wireguardFile), nil
	}
	for _, m := range live {
		if apply.EtcdMemberSite(p.cfg, m) == p.Site {
			return true, fmt.Sprintf("%s is already in etcd's membership", p.Site), nil
		}
	}
	return false, "", nil
}

func (p *Plan) buildPreflight() *Stage {
	st := &Stage{Number: 1, Name: "preflight", Gate: "every check passes"}
	if p.preflightSkipped != "" {
		st.Gate = "passed on the run that started this join"
		st.Steps = append(st.Steps, Step{Site: p.Site, Verb: "skip", Text: "preflight is not run again: " + p.preflightSkipped + ", and preflight's checks are for a host the join has not touched (its own wg0 now holds 51820/udp)"})
		return st
	}
	for _, c := range p.Preflight.Checks {
		verb := "pass"
		switch {
		case c.Refused:
			verb = "refuse"
		case c.Warned:
			verb = "warn"
		}
		st.Steps = append(st.Steps, Step{Site: c.Site, Verb: verb, Text: c.Name + ": " + c.Detail})
	}
	st.gate = func() error {
		if !p.Preflight.Refused() {
			return nil
		}
		var lines []string
		for _, c := range p.Preflight.Checks {
			if c.Refused {
				lines = append(lines, fmt.Sprintf("%s: %s: %s", c.Site, c.Name, c.Detail))
			}
		}
		return fmt.Errorf("preflight refused:\n  %s", strings.Join(lines, "\n  "))
	}
	return st
}

// noteOwed lists what the join leaves on purpose. An existing data site's
// patroni.env names every etcd member after the join, and applying that
// recreates its Patroni, which on the primary is a failover. The join does not
// need it: that Patroni keeps working against the etcd member it started
// with, which stays a voter throughout.
func (p *Plan) noteOwed(rendered *render.Plan) error {
	for _, name := range p.cfg.Cluster.Sites {
		if name == p.Site {
			continue
		}
		want := renderedFile(rendered, name, "srv/infra/patroni.env")
		have, found, err := p.transports[name].ReadFile("/srv/infra/patroni.env")
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if found && have != want {
			p.Notes = append(p.Notes, fmt.Sprintf(
				"%s's patroni.env will still name the etcd members it started with. `paisans apply --site %s` brings it up to date and recreates its Patroni, which on the primary is a failover; run it when that is acceptable, Eg: once the leader has been switched to another site",
				name, name))
		}
	}
	return nil
}

// Execute runs the stages in order. A stage's steps run only when it has
// any; its gate always runs, so a resumed join proves each stage again before
// moving past it. A failed gate stops everything, after undoing what that
// stage undoes.
func Execute(p *Plan) error {
	for _, st := range p.Stages {
		p.say("stage %d, %s\n", st.Number, st.Name)
		if st.run != nil && len(st.Steps) > 0 {
			if err := st.run(); err != nil {
				return p.fail(st, err)
			}
		}
		if st.gate != nil {
			if err := st.gate(); err != nil {
				return p.fail(st, fmt.Errorf("gate: %w", err))
			}
		}
		p.say("  %-9s %s\n", "passed", st.Gate)
	}
	return nil
}

func (p *Plan) fail(st *Stage, err error) error {
	msg := fmt.Sprintf("site add %s stopped at stage %d (%s), and nothing after it ran: %v", p.Site, st.Number, st.Name, err)
	if st.rollback != nil {
		if rerr := st.rollback(); rerr != nil {
			return fmt.Errorf("%s\nRolling back failed too, so the mesh may hold the new peer on some sites: %v", msg, rerr)
		}
		msg += "\n" + st.OnFailure + ". Done."
	}
	return fmt.Errorf("%s\nFix the cause and run site add again: it resumes at the first stage whose gate does not pass", msg)
}

// Print writes the plan as an operator reads it.
func (p *Plan) Print(w io.Writer) {
	fmt.Fprintf(w, "site add %s\n", p.Site)
	for _, st := range p.Stages {
		fmt.Fprintf(w, "\n%d. %s\n", st.Number, st.Name)
		if len(st.Steps) == 0 {
			fmt.Fprintf(w, "  %-9s nothing to do here, and the gate is still checked\n", "nothing")
		}
		for _, step := range st.Steps {
			fmt.Fprintf(w, "  %-9s %s: %s\n", step.Verb, step.Site, step.Text)
		}
		fmt.Fprintf(w, "  %-9s %s\n", "gate", st.Gate)
		if st.OnFailure != "" {
			fmt.Fprintf(w, "  %-9s %s\n", "rollback", st.OnFailure)
		}
	}
	for _, note := range p.Notes {
		fmt.Fprintf(w, "\nnote: %s\n", note)
	}
}

// Pending reports whether any stage has steps to run.
func (p *Plan) Pending() bool {
	for _, st := range p.Stages {
		if st.Number > 1 && len(st.Steps) > 0 {
			return true
		}
	}
	return false
}

// acmeModule is what a whole apply of the new site is handed. It runs no
// gateway, so the module is never asked for.
func (p *Plan) acmeModule() string { return acme.Module(p.cfg.ACME.Provider) }

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// runPreflight is stage 1. It is a variable so that this package's tests, which
// drive stages 2 to 6 against fake hosts, can stand in a passing report:
// preflight's own checks are tested in internal/preflight, against hosts shaped
// for them.
var runPreflight = preflight.Run

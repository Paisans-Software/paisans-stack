// Package siteremove takes one site out of a running deployment: `paisans
// site remove`. It is the reverse of internal/siteadd and is built the same
// way: staged, every stage ending at a gate that stops the command with its
// evidence, and every stage planned from live state, so a run after a fixed
// problem resumes at the first stage with anything left to do.
//
// The site stays declared in paisans.yaml while the command runs, because its
// ssh section is how its host is reached and its roles say what it holds.
// Everything else is computed from config.WithoutSite, the end state, which
// the last stage writes back to the file. When that end state would leave
// one data site and a witness, two etcd voters, the witness leaves etcd in
// the same removal (see EndState).
//
// On the site's host it touches only what is provably this deployment's: a
// Docker object with the deployment label carrying this id, a file whose
// hash matches this deployment's manifest, a unit named for its token, a ufw
// rule with its owner tag, a key its record lists, and its own registry
// entry. Everything else found is reported as kept. docs/specs/
// 2026-10-08-site-remove.md is the approved design.
package siteremove

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/ui"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// Options is what the operator asked for besides the site.
type Options struct {
	// HostGone skips cleaning the site's host, which is not reached at all.
	HostGone bool
	// DeleteData deletes the deployment root and the named volumes on the
	// host. The caller asks for the site's name at a terminal first.
	DeleteData bool
	// ConfigPath is the paisans.yaml the last stage edits.
	ConfigPath string
}

// Step is one thing a stage will do, for the plan an operator reads.
type Step struct {
	Site string
	Verb string
	// Text is the whole step, commands included. A dry run shows it only with
	// --verbose.
	Text string
	// Title is the step as a line of its own. It is empty for a step that
	// has none worth the name, which then reads as its verb and site. Steps
	// that follow each other with one title are listed as one item.
	Title string
}

// title is the line a dry run lists the step as.
func (s Step) title() string {
	if s.Title != "" {
		return s.Title
	}
	return s.Verb + " " + s.Site
}

// Stage is one stage of the removal: its steps, then its gate.
type Stage struct {
	Number int
	Name   string
	Steps  []Step
	// Gate says what must hold before the next stage may start.
	Gate string
	// Short names the gate in a few words, as the title of its step. The long
	// text is that step's detail.
	Short string
	// Skipped says why the stage does nothing, empty when it runs.
	Skipped string

	run  func() error
	gate func() error
}

// Plan is a whole removal, decided from the live deployment.
type Plan struct {
	Site string
	Options
	Stages []*Stage
	// Notes are things the removal leaves for later, on purpose.
	Notes []string
	// Kept is what the removal leaves on the host, with why: planned at
	// Build and added to as the host stage runs.
	Kept []string
	// Report receives each stage as a section, the work in it as steps and
	// each gate as a step of its own. Nil discards it.
	Report ui.Reporter

	cfg *config.Config
	end *config.Config
	// witness is the site that leaves etcd and loses the witness role with
	// this removal, empty when none does (see EndState).
	witness    string
	secrets    *config.Secrets
	transports map[string]apply.Transport
	initial    map[string]render.EtcdInitial
	full       *render.Plan
	rendered   *render.Plan

	etcd    etcdState
	patroni patroniState
	garage  garageState
	host    *hostPlan
	// open is the step the running stage is in the middle of, ended when the
	// next one starts or the stage's work does.
	open ui.Step
}

func (p *Plan) reporter() ui.Reporter {
	if p.Report == nil {
		return ui.Discard
	}
	return p.Report
}

// work starts the step a stage's run is now doing and ends the one before it,
// so a stage's work reads as a list of steps and a failure marks the one that
// failed.
func (p *Plan) work(title string) ui.Step {
	p.idle()
	p.open = p.reporter().Step(title)
	return p.open
}

// idle ends the open step as done. A step that calls into apply ends its own
// first, since the apply reports steps of its own and two cannot be open.
func (p *Plan) idle() {
	if p.open != nil {
		p.open.Done("")
		p.open = nil
	}
}

// stop ends the open step as failed, when there is one.
func (p *Plan) stop(err error) {
	if p.open != nil {
		p.open.Fail(err)
		p.open = nil
	}
}

// dep is the deployment every name and path here belongs to.
func (p *Plan) dep() deployment.Deployment { return p.cfg.Deployment() }

// Timing is every wait the gates make, as a count of polls, so a test that
// replaces sleep with nothing still ends.
var (
	sleep = time.Sleep

	// Patroni: a switchover completes in seconds, and a member key that is
	// deleted disappears from the list at once, and one left to expire
	// goes with its TTL, 30 seconds by default.
	switchWait  = 3 * time.Minute
	switchPoll  = 3 * time.Second
	patroniWait = 2 * time.Minute
	patroniPoll = 5 * time.Second

	// Garage moves every partition the node held to the nodes that remain,
	// which on a large store takes hours. A run that times out stops, and
	// the next resumes at this gate while Garage carries on.
	garageWait    = 60 * time.Minute
	garagePoll    = 30 * time.Second
	garageSamples = 3
	garageGap     = 10 * time.Second

	etcdWait = 60 * time.Second
	etcdPoll = 5 * time.Second

	// A replica's Patroni recreated for its patroni.env replays the WAL it
	// missed while it restarted, which is seconds of writes, not a clone.
	replicaWait = 5 * time.Minute
	replicaPoll = 5 * time.Second

	// HAProxy marks a server UP after two good checks three seconds apart.
	haproxyWait = 60 * time.Second
	haproxyPoll = 3 * time.Second

	// The handed over Caddy has started once its container runs and its
	// configuration validates inside it.
	handoverWait = 60 * time.Second
	handoverPoll = 3 * time.Second
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

// poll calls check until it returns nil or the wait runs out, and returns
// its last error.
func poll(wait, every time.Duration, check func() error) error {
	n := attempts(wait, every)
	var err error
	for i := 0; i < n; i++ {
		if err = check(); err == nil {
			return nil
		}
		if errors.Is(err, apply.ErrUnreachable) {
			return err
		}
		if i < n-1 {
			sleep(every)
		}
	}
	return err
}

// Refusal is why site cannot be removed from this configuration, or nil. It
// reads no host, so it runs before any is reached.
func Refusal(cfg *config.Config, site string, o Options) error {
	s, ok := cfg.Sites[site]
	if !ok {
		return fmt.Errorf("site remove: the configuration declares no site %q. Declared sites are %s", site, strings.Join(cfg.SiteNames(), ", "))
	}
	if o.HostGone && o.DeleteData {
		return fmt.Errorf("site remove %s: --delete-data deletes data on the host, and --host-gone does not reach it. Drop one of them", site)
	}
	if s.Has(config.RoleGateway) && len(cfg.GatewaySites()) == 1 {
		return fmt.Errorf("site remove %s: it is the only gateway, so nothing would answer for the community's hostnames. Give another site the gateway role and apply it first (README, \"Moving the gateway\")", site)
	}
	if s.Has(config.RoleApps) && len(cfg.AppsSites()) == 1 {
		var placed []string
		for _, name := range cfg.AppNames() {
			if cfg.Apps[name].Placement.Mode == config.PlacementCluster {
				placed = append(placed, name)
			}
		}
		if len(placed) > 0 {
			return fmt.Errorf("site remove %s: it is the only apps site, and %s placed cluster would run nowhere. Give another site the apps role and apply it first", site, strings.Join(placed, ", "))
		}
	}
	if pinned := cfg.PinnedTo(site); len(pinned) > 0 {
		return fmt.Errorf("site remove %s: %s pinned to it, and would run nowhere. Move each first (README, \"Moving a pinned app\")", site, strings.Join(pinned, ", "))
	}
	if contains(cfg.Cluster.Sites, site) && len(cfg.Cluster.Sites) == 1 || s.Has(config.RoleData) && len(cfg.DataSites()) == 1 {
		return fmt.Errorf("site remove %s: it is the only data site, so the database would have nowhere to live. Add another data site first (`paisans site add`)", site)
	}
	if len(cfg.Storage.Garage.Sites) > 0 && cfg.Storage.Garage.Sites[0] == site && len(cfg.AppNames()) > 0 {
		return fmt.Errorf("site remove %s: it is first in storage.garage.sites, the Garage node every app writes its objects through, and the apps would lose it the moment its node stops. Move another Garage site to the front of the list and apply every site running an app first, then run this again", site)
	}
	if contains(cfg.Storage.Garage.Sites, site) {
		left := len(cfg.Storage.Garage.Sites) - 1
		if left < cfg.Storage.Garage.Replication {
			return fmt.Errorf("site remove %s: it is a Garage site, and without it %d node(s) would hold objects at storage.garage.replication %d, so the data on it would have nowhere to go. Add a Garage site, or lower the replication factor, with `paisans storage add` first", site, left, cfg.Storage.Garage.Replication)
		}
	}
	end, _, err := EndState(cfg, site)
	if err != nil {
		return err
	}
	if result := validate.Check(end); result.Refused() {
		var lines []string
		twoVoters := false
		for _, f := range result.Refusals() {
			lines = append(lines, f.Key+": "+f.Message)
			twoVoters = twoVoters || f.Rule == "two-etcd-voters"
		}
		advice := "site remove takes one site out and changes nothing else, so the end state has to validate as it is"
		if twoVoters {
			advice = "Two data sites would stay as etcd voters, and neither can leave etcd while it holds data. Add a witness first, a site that holds no data with the witness role, as an etcd member, so that three voters remain without " + site
		}
		return fmt.Errorf("site remove %s: the configuration without it is refused:\n  %s\n%s", site, strings.Join(lines, "\n  "), advice)
	}
	return nil
}

// EndState is the configuration once site is removed, and the witness that
// leaves etcd with it, empty when none does.
//
// Taking one data site out of two data sites and a witness leaves two etcd
// voters, which validation refuses: a majority of two is two, so either
// failing stops the cluster. One voter is strictly better, and with one data
// site the witness has nothing left to break a tie between. So when the end
// state's etcd members would be exactly the one remaining data site and a site
// that is a member only because it is the witness, that site leaves etcd too,
// and loses the witness role while keeping every other. A site whose only
// role was the witness would be left with none, which the configuration allows
// only for a site an app is pinned to.
func EndState(cfg *config.Config, site string) (*config.Config, string, error) {
	end := cfg.WithoutSite(site)
	if !contains(cfg.Etcd.Members, site) || len(end.Etcd.Members) != 2 || len(end.Cluster.Sites) != 1 {
		return end, "", nil
	}
	data := end.Cluster.Sites[0]
	var witness string
	for _, name := range end.Etcd.Members {
		s := end.Sites[name]
		if name != data && s.Has(config.RoleWitness) && !s.Has(config.RoleData) {
			witness = name
		}
	}
	if witness == "" || !contains(end.Etcd.Members, data) {
		return end, "", nil
	}
	w := end.Sites[witness]
	var roles []config.Role
	for _, r := range w.Roles {
		if r != config.RoleWitness {
			roles = append(roles, r)
		}
	}
	if len(roles) == 0 && len(end.PinnedTo(witness)) == 0 {
		return nil, "", fmt.Errorf("site remove %s: without it, %s and the witness %s would be two etcd voters, so %s leaves etcd and loses the witness role in the same removal. Witness is its only role, and a site with none is allowed only when an app is pinned to it. Give %s another role and apply it first, or run `paisans site remove %s` once this removal is done", site, data, witness, witness, witness, witness)
	}
	w.Roles = roles
	end.Sites[witness] = w
	end.Etcd.Members = []string{data}
	return end, witness, nil
}

// Build decides the removal of site, reading every site it reaches and
// changing none. transports maps every remaining site's name to how it is
// reached, and the site's own unless o.HostGone.
func Build(cfg *config.Config, secrets *config.Secrets, site string, transports map[string]apply.Transport, o Options) (*Plan, error) {
	if err := Refusal(cfg, site, o); err != nil {
		return nil, err
	}
	end, witness, err := EndState(cfg, site)
	if err != nil {
		return nil, err
	}
	p := &Plan{
		Site:       site,
		Options:    o,
		cfg:        cfg,
		end:        end,
		witness:    witness,
		secrets:    secrets,
		transports: map[string]apply.Transport{},
		initial:    map[string]render.EtcdInitial{},
	}
	for _, name := range p.end.SiteNames() {
		t, ok := transports[name]
		if !ok {
			return nil, fmt.Errorf("site remove %s: no way to reach site %s. Every remaining site is read, and most are changed", site, name)
		}
		p.transports[name] = t
	}
	if !o.HostGone {
		t, ok := transports[site]
		if !ok {
			return nil, fmt.Errorf("site remove %s: no way to reach its host", site)
		}
		if out, err := t.Run("true"); err != nil {
			if errors.Is(err, apply.ErrUnreachable) {
				return nil, fmt.Errorf("site remove %s: its host does not answer over ssh (%v), so what is on it cannot be read or cleaned. Fix ssh and run again, or, if the host is never coming back, run with --host-gone: the cluster stages and the configuration edit run, and the host is left as it is", site, err)
			}
			return nil, fmt.Errorf("site remove %s: its host answered `true` with an error: %v: %s", site, err, lastLines(out, 2))
		}
		p.transports[site] = t
	}

	for _, name := range cfg.Etcd.Members {
		t, ok := p.transports[name]
		if !ok {
			continue
		}
		in, found, err := apply.ReadEtcdInitial(t, p.dep())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if found {
			p.initial[name] = in
		}
	}
	if p.full, err = p.render(cfg); err != nil {
		return nil, err
	}
	if p.rendered, err = p.render(p.end); err != nil {
		return nil, err
	}

	if err := p.probeEtcd(); err != nil {
		return nil, err
	}
	if err := p.probePatroni(); err != nil {
		return nil, err
	}
	if err := p.probeGarage(); err != nil {
		return nil, err
	}

	p.Stages = append(p.Stages, p.buildData())
	cluster, err := p.buildCluster()
	if err != nil {
		return nil, err
	}
	p.Stages = append(p.Stages, cluster)
	host, err := p.buildHost()
	if err != nil {
		return nil, err
	}
	p.notePocketID(host)
	p.Stages = append(p.Stages, host)
	p.Stages = append(p.Stages, p.buildConfig())
	monitor, err := p.buildMonitor()
	if err != nil {
		return nil, err
	}
	p.Stages = append(p.Stages, monitor)
	return p, nil
}

// notePocketID says, at the head of stage 3, when the site holds the active
// instance of a Pocket ID app that stands by elsewhere: stage 3 stops it, and
// sign in is unavailable until a standby takes over (README, "Pocket ID runs
// on every apps site, and one of them is active"). The stop is clean, so that
// is a standby's next retry, a few seconds; an instance that does not stop
// cleanly leaves its registration to age for 90 seconds first. Only the site's own
// instance is asked, since it is the only one this stops.
func (p *Plan) notePocketID(st *Stage) {
	if p.HostGone || st.Skipped != "" {
		return
	}
	var notes []Step
	for _, app := range apply.StandbyApps(p.cfg) {
		for _, in := range apply.LookAtInstances(p.cfg, app, map[string]apply.Transport{p.Site: p.transports[p.Site]}) {
			if in.Site == p.Site && in.State == apply.Active {
				notes = append(notes, Step{Site: p.Site, Verb: "note", Title: "note Pocket ID failover", Text: fmt.Sprintf("it holds the active instance of Pocket ID %s, so once this stage stops it, sign in is unavailable for a few seconds while a standby on another site takes over, up to about 90 seconds if the instance does not stop cleanly", app)})
			}
		}
	}
	st.Steps = append(notes, st.Steps...)
}

func (p *Plan) render(cfg *config.Config) (*render.Plan, error) {
	var opts []render.Option
	for site, in := range p.initial {
		if _, ok := cfg.Sites[site]; ok {
			opts = append(opts, render.WithEtcdInitial(site, in))
		}
	}
	return render.Build(cfg, p.secrets, opts...)
}

// buildConfig is stage 4: the site out of paisans.yaml.
func (p *Plan) buildConfig() *Stage {
	st := &Stage{
		Number: 4,
		Name:   "config",
		Gate:   fmt.Sprintf("%s loads, and declares no site %s", p.ConfigPath, p.Site),
		Short:  "configuration loads without " + p.Site,
		Steps:  []Step{{Site: p.Site, Verb: "remove", Title: "remove " + p.Site + " from " + p.ConfigPath, Text: fmt.Sprintf("sites.%s from %s, and its name from cluster.sites, etcd.members, storage.garage.sites and storage.garage.capacities, keeping every comment", p.Site, p.ConfigPath)}},
	}
	if p.witness != "" {
		st.Gate += fmt.Sprintf(", etcd.members does not list %s, and %s has no witness role", p.witness, p.witness)
		st.Steps = append(st.Steps, Step{Site: p.witness, Verb: "remove", Title: "remove witness " + p.witness, Text: fmt.Sprintf("%s from etcd.members and witness from sites.%s.roles, in the same write", p.witness, p.witness)})
	}
	st.run = func() error {
		p.work("remove " + p.Site + " from " + p.ConfigPath)
		if p.ConfigPath == "" {
			return fmt.Errorf("no configuration file to edit")
		}
		if p.witness != "" {
			return config.RemoveSiteAndWitness(p.ConfigPath, p.Site, p.witness)
		}
		return config.RemoveSite(p.ConfigPath, p.Site)
	}
	st.gate = func() error {
		cfg, err := config.Load(p.ConfigPath)
		if err != nil {
			return err
		}
		if _, ok := cfg.Sites[p.Site]; ok {
			return fmt.Errorf("%s still declares %s", p.ConfigPath, p.Site)
		}
		if p.witness != "" && (contains(cfg.Etcd.Members, p.witness) || cfg.Sites[p.witness].Has(config.RoleWitness)) {
			return fmt.Errorf("%s still has %s as the witness or in etcd.members", p.ConfigPath, p.witness)
		}
		return nil
	}
	return st
}

// Execute runs the stages in order. A stage's steps run only when it has
// any; its gate always runs, so a resumed removal proves each stage again
// before moving past it.
func Execute(p *Plan) error {
	r := p.reporter()
	for _, st := range p.Stages {
		r.Section(stageTitle(st))
		if st.Skipped != "" {
			s := r.Step("skip " + st.Name)
			s.Detail("%s", st.Skipped)
			s.Done("skipped")
			continue
		}
		if st.run != nil && len(st.Steps) > 0 {
			err := st.run()
			if err != nil {
				p.stop(err)
				return p.fail(st, err)
			}
			p.idle()
		}
		g := r.Step("gate: " + st.Short)
		g.Detail("%s", st.Gate)
		if st.gate != nil {
			if err := st.gate(); err != nil {
				g.Fail(err)
				return p.fail(st, fmt.Errorf("gate: %w", err))
			}
		}
		g.Done("passed")
	}
	return nil
}

func stageTitle(st *Stage) string { return fmt.Sprintf("stage %d, %s", st.Number, st.Name) }

func (p *Plan) fail(st *Stage, err error) error {
	if st.Number == monitorStage {
		// The configuration no longer declares the site, so there is no
		// site remove to run again; the stage's own error names the apply.
		return fmt.Errorf("site remove %s stopped at stage %d (%s): %v", p.Site, st.Number, st.Name, err)
	}
	return fmt.Errorf("site remove %s stopped at stage %d (%s), and nothing after it ran: %v\nFix the cause and run site remove again: it resumes at the first stage with anything left to do", p.Site, st.Number, st.Name, err)
}

// Show lists the plan as an operator reads it: a section per stage, an item
// per step and one for the gate, with the whole text of each as its detail.
// What the removal leaves for later is Remains.
func (p *Plan) Show(r ui.Reporter) {
	for _, st := range p.Stages {
		r.Section(stageTitle(st))
		if st.Skipped != "" {
			r.Item("skip " + st.Name)
			r.Detail("%s", st.Skipped)
			continue
		}
		if len(st.Steps) == 0 {
			r.Item("nothing to do here")
			r.Detail("nothing to do here, and the gate is still checked")
		}
		last := ""
		for _, step := range st.Steps {
			if t := step.title(); t != last {
				r.Item(t)
				last = t
			}
			r.Detail("%s: %s", step.Site, step.Text)
		}
		r.Item("gate: " + st.Short)
		r.Detail("%s", st.Gate)
	}
}

// Remains is what the operator still has to see to once the plan has run:
// the site's secrets, its DNS records, what a scoped apply did not move, and
// everything kept on the host, with why. Nothing here is changed by this
// command.
func (p *Plan) Remains() []string {
	var out []string
	if p.secrets != nil {
		if _, ok := p.secrets.Sites[p.Site]; ok {
			out = append(out, fmt.Sprintf("secrets: remove sites.%s (its WireGuard key) from the secrets file with sops. This command never edits it, so remove it once nothing needs it", p.Site))
		}
	}
	if addr := p.cfg.Sites[p.Site].PublicAddress; addr != "" {
		out = append(out, fmt.Sprintf("DNS: records dns init made pointing at %s stay until `paisans dns prune --execute` deletes them", addr))
	} else {
		out = append(out, "DNS: any record dns init made pointing at this host stays until `paisans dns prune --execute` deletes it")
	}
	out = append(out, p.Notes...)
	out = append(out, p.Kept...)
	return out
}

// Pending reports whether any stage has steps to run.
func (p *Plan) Pending() bool {
	for _, st := range p.Stages {
		if st.Skipped == "" && len(st.Steps) > 0 {
			return true
		}
	}
	return false
}

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

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func sortedCopy(list []string) []string {
	out := append([]string(nil), list...)
	sort.Strings(out)
	return out
}

// Package storageadd joins every Garage site the configuration lists into one
// cluster: `paisans storage add`. It is staged like `site add`, and every
// stage ends at a gate that stops the command with its evidence when it does
// not pass; nothing after a failed gate runs.
//
// The configuration is the end state. Build reads every Garage node (its
// node ID, the replication factor its garage.toml was deployed with, its view
// of the layout and of its peers) and plans only what differs, so a run after
// a fixed problem, or after Garage has finished copying data, resumes at the
// first stage whose gate does not yet pass. Two gates wait on Garage rather
// than on the toolkit; by default a run exits at them with ErrWaiting instead
// of blocking, and the next run carries on. Everything a resumed run needs is
// on the hosts, never on the operator's machine.
//
// docs/specs/2026-10-07-multisite-garage.md is the approved design, with the
// founder's decisions and the Garage v1.0.1 source behind every gate.
package storageadd

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/garage"
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
	// Waits marks a gate that waits on Garage rather than on anything the
	// toolkit does. A run that finds one not yet passing exits with
	// ErrWaiting, unless it was asked to wait.
	Waits bool
	// Verifies marks a stage that only proves the result, run on every
	// execute, so its steps never make a plan pending.
	Verifies bool

	run  func() error
	gate func() error
}

// Options are what the operator asked for beyond the configuration.
type Options struct {
	// ChangeReplication allows the reset a different replication factor
	// needs. Garage calls the procedure unsupported, so it is never implied.
	ChangeReplication bool
	// Wait is how long a waiting gate polls before the run exits. Zero reads
	// it once.
	Wait time.Duration
	// StopTest stops Garage on the last listed site during the smoke stage,
	// to prove reads and uploads survive it, then starts it again. Refused
	// unless uploads survive one node down.
	StopTest bool
	// SharedSites are the sites the host check found shared. Every apply
	// storage add runs on one keeps its images, as apply itself does there,
	// because an image the toolkit renders may be what something else runs
	// from.
	SharedSites map[string]bool
}

// keepImages is whether the applies on site keep superseded images.
func (p *Plan) keepImages(site string) bool { return p.opts.SharedSites[site] }

// applyOptions are opts, plus KeepImages on a shared site.
func (p *Plan) applyOptions(site string, opts ...apply.Option) []apply.Option {
	if p.keepImages(site) {
		opts = append(opts, apply.KeepImages())
	}
	return opts
}

// Plan is a whole join, decided from the live deployment.
type Plan struct {
	Stages []*Stage
	// Notes are things the join leaves for later, on purpose.
	Notes []string
	// Progress receives each stage as it starts and each gate as it passes.
	// Nil discards it.
	Progress io.Writer

	cfg        *config.Config
	secrets    *config.Secrets
	transports map[string]apply.Transport
	opts       Options
	rendered   *render.Plan
	nodes      []*node
	// anchor is the site whose Garage every cluster command runs on.
	anchor string
	// gateway is the site holding the gateway role, empty when none.
	gateway string
	// needsChange says the factor differs on some node. reset says stages 2
	// and 3 run: for that, or to finish a reset an earlier run left with
	// nodes stopped.
	needsChange bool
	reset       bool
}

// ErrWaiting is what Execute returns, wrapped, when a gate is waiting on
// Garage. It is not a failure: the next run resumes there.
var ErrWaiting = errors.New("waiting on Garage")

// Waiting is the error a waiting gate stops a run with.
type Waiting struct {
	Stage  *Stage
	Detail string
}

func (w *Waiting) Error() string {
	return fmt.Sprintf("storage add is waiting at stage %d (%s): %s. Nothing failed. Run `paisans storage add` to look again, or with --execute to carry on; --wait <duration> polls instead of exiting", w.Stage.Number, w.Stage.Name, w.Detail)
}

func (w *Waiting) Unwrap() error { return ErrWaiting }

// Timing is every wait the gates make, as polls of an interval, so a test
// that replaces sleep with nothing still ends.
var (
	sleep = time.Sleep

	// A connect is answered at once, but a node learns the peers of its
	// peers on Garage's discovery loop, every 60 seconds
	// (src/rpc/system.rs, DISCOVERY_INTERVAL).
	connectWait = 90 * time.Second
	connectPoll = 3 * time.Second
	// A new layout version reaches every node over the same RPC, in seconds.
	layoutWait = 60 * time.Second
	layoutPoll = 3 * time.Second
	// A node answers once its container is up and its RPC is listening.
	startWait = 60 * time.Second
	startPoll = 2 * time.Second
	// The waiting gates read every node this often when asked to wait, and
	// an empty resync queue must hold for this many reads in a row.
	syncPoll    = 10 * time.Second
	syncSamples = 3
	sampleGap   = 5 * time.Second
	// A probe written through one node is read through the others, which at
	// consistency dangerous may not have it yet.
	probeWait = 30 * time.Second
	probePoll = 2 * time.Second
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

func (p *Plan) say(format string, args ...any) {
	if p.Progress != nil {
		fmt.Fprintf(p.Progress, format, args...)
	}
}

// Build decides the join, reading every Garage site and the gateway and
// changing none.
//
// transports maps a site's name to how it is reached; every Garage site and
// the gateway site must have one.
func Build(cfg *config.Config, secrets *config.Secrets, transports map[string]apply.Transport, opts Options) (*Plan, error) {
	garageSites := cfg.Storage.Garage.Sites
	if len(garageSites) == 0 {
		return nil, fmt.Errorf("storage add: storage.garage.sites lists no site, so there is no cluster to build")
	}
	for _, name := range garageSites {
		if _, ok := cfg.Sites[name]; !ok {
			return nil, fmt.Errorf("storage add: storage.garage.sites lists %q, which the configuration does not declare", name)
		}
		if _, ok := transports[name]; !ok {
			return nil, fmt.Errorf("storage add: no way to reach Garage site %s", name)
		}
	}
	p := &Plan{cfg: cfg, secrets: secrets, transports: transports, opts: opts}
	for _, name := range cfg.SiteNames() {
		if cfg.Sites[name].Has(config.RoleGateway) {
			p.gateway = name
			break
		}
	}
	if p.gateway != "" {
		if _, ok := transports[p.gateway]; !ok {
			return nil, fmt.Errorf("storage add: no way to reach the gateway site %s, whose media routes name the Garage nodes", p.gateway)
		}
	}

	rendered, err := render.Build(cfg, secrets)
	if err != nil {
		return nil, err
	}
	p.rendered = rendered

	for _, name := range garageSites {
		n, err := p.readNode(name)
		if err != nil {
			return nil, err
		}
		p.nodes = append(p.nodes, n)
	}
	p.anchor = p.chooseAnchor()
	if opts.StopTest {
		if err := p.stopTestAllowed(); err != nil {
			return nil, fmt.Errorf("storage add: %w", err)
		}
	}
	want := p.replication()
	for _, n := range p.nodes {
		if n.deployed && n.factor != want {
			p.needsChange = true
		}
	}
	p.reset = p.needsChange
	if !p.reset {
		// A reset that stopped the nodes and rewrote every garage.toml, but
		// was interrupted before starting them all, leaves no factor that
		// differs. Its counts file is what says it is unfinished.
		_, counted, err := transports[p.anchor].ReadFile(countsFile(p.dep()))
		if err != nil {
			// An unread counts file is not an absent one: reading it as
			// absent would skip resuming a reset that is half done.
			return nil, fmt.Errorf("storage add: could not read host state on %s, so whether a reset is unfinished is unknown: %w", p.anchor, err)
		}
		if counted {
			for _, n := range p.nodes {
				if n.deployed && !n.answers() {
					p.reset = true
				}
			}
		}
	}

	p.Stages = append(p.Stages, p.buildNodes())
	if p.reset {
		reset, err := p.buildReset()
		if err != nil {
			return nil, err
		}
		p.Stages = append(p.Stages, p.buildSettle(), reset)
	}
	p.Stages = append(p.Stages, p.buildConnect(), p.buildLayout(), p.buildSync())
	provision, err := p.buildProvision()
	if err != nil {
		return nil, err
	}
	p.Stages = append(p.Stages, provision)
	media, err := p.buildMedia()
	if err != nil {
		return nil, err
	}
	if media != nil {
		p.Stages = append(p.Stages, media)
	}
	if smoke := p.buildSmoke(); smoke != nil {
		p.Stages = append(p.Stages, smoke)
	}
	for i, st := range p.Stages {
		st.Number = i + 1
	}
	if err := p.noteOwed(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Plan) replication() int {
	if p.cfg.Storage.Garage.Replication == 0 {
		return 1
	}
	return p.cfg.Storage.Garage.Replication
}

// chooseAnchor is the first listed site whose node holds a role in its live
// layout, or the first listed when none does: a cluster's commands run where
// the cluster already is.
func (p *Plan) chooseAnchor() string {
	for _, n := range p.nodes {
		if n.hasRole {
			return n.site
		}
	}
	return p.nodes[0].site
}

func (p *Plan) node(site string) *node {
	for _, n := range p.nodes {
		if n.site == site {
			return n
		}
	}
	return nil
}

// noteOwed lists the apps sites whose rendered files differ from what is on
// them. S3_ENDPOINT follows the first listed Garage site, so reordering
// storage.garage.sites changes every app's .env; that apply recreates the app
// and is the operator's to time, so the join does not run it.
func (p *Plan) noteOwed() error {
	for _, name := range p.cfg.SiteNames() {
		site := p.cfg.Sites[name]
		if !site.Has(config.RoleApps) {
			continue
		}
		t, ok := p.transports[name]
		files := p.appEnvFiles(name)
		if !ok || len(files) == 0 {
			continue
		}
		sp, err := apply.Build(name, p.rendered, acme.Module(p.cfg.ACME.Provider), t, p.applyOptions(name, apply.Scope(files...))...)
		if err != nil {
			return err
		}
		if n := len(sp.Writes()); n > 0 {
			p.Notes = append(p.Notes, fmt.Sprintf(
				"%s's app configuration differs from the render in %d file(s), Eg: S3_ENDPOINT after storage.garage.sites was reordered. `paisans apply --site %s` brings it up to date and recreates those apps; run it when that is acceptable", name, n, name))
		}
	}
	return nil
}

// appEnvFiles is every app .env the render places on site.
func (p *Plan) appEnvFiles(site string) []string {
	var out []string
	prefix := site + "/"
	for _, f := range p.rendered.Files {
		rel, ok := strings.CutPrefix(f.Path, prefix)
		if !ok || !strings.HasPrefix(rel, p.dep().Rel()+"/") || strings.HasPrefix(rel, p.dep().RelPath("infra")+"/") {
			continue
		}
		if strings.HasSuffix(rel, "/.env") {
			out = append(out, rel)
		}
	}
	return out
}

// Execute runs the stages in order. A stage's steps run only when it has
// any; its gate always runs, so a resumed join proves each stage again before
// moving past it. A failed gate stops everything; a waiting one stops it with
// a *Waiting.
func Execute(p *Plan) error {
	for _, st := range p.Stages {
		p.say("stage %d, %s\n", st.Number, st.Name)
		if st.run != nil && len(st.Steps) > 0 {
			if err := st.run(); err != nil {
				return p.fail(st, err)
			}
		}
		if st.gate != nil {
			if err := p.check(st); err != nil {
				var w *Waiting
				if errors.As(err, &w) {
					return err
				}
				return p.fail(st, fmt.Errorf("gate: %w", err))
			}
		}
		p.say("  %-9s %s\n", "passed", st.Gate)
	}
	return nil
}

// check runs a stage's gate, polling a waiting gate for as long as the
// operator asked.
func (p *Plan) check(st *Stage) error {
	if !st.Waits {
		return st.gate()
	}
	deadline := attempts(p.opts.Wait, syncPoll)
	if p.opts.Wait <= 0 {
		deadline = 1
	}
	var last error
	for i := 0; i < deadline; i++ {
		if i > 0 {
			sleep(syncPoll)
		}
		last = st.gate()
		if last == nil {
			return nil
		}
		var w *Waiting
		if !errors.As(last, &w) {
			return last
		}
		p.say("  %-9s %s\n", "waiting", w.Detail)
	}
	return last
}

func (p *Plan) fail(st *Stage, err error) error {
	return fmt.Errorf("storage add stopped at stage %d (%s), and nothing after it ran: %v\nFix the cause and run storage add again: it resumes at the first stage whose gate does not pass", st.Number, st.Name, err)
}

// Print writes the plan as an operator reads it.
func (p *Plan) Print(w io.Writer) {
	fmt.Fprintf(w, "storage add: %s at replication %d, consistency %s\n",
		strings.Join(p.cfg.Storage.Garage.Sites, ", "), p.replication(), p.consistency())
	for _, st := range p.Stages {
		fmt.Fprintf(w, "\n%d. %s\n", st.Number, st.Name)
		if len(st.Steps) == 0 {
			fmt.Fprintf(w, "  %-9s nothing to do here, and the gate is still checked\n", "nothing")
		}
		for _, step := range st.Steps {
			fmt.Fprintf(w, "  %-9s %s: %s\n", step.Verb, step.Site, step.Text)
		}
		gate := "gate"
		if st.Waits {
			gate = "waits"
		}
		fmt.Fprintf(w, "  %-9s %s\n", gate, st.Gate)
	}
	for _, note := range p.Notes {
		fmt.Fprintf(w, "\nnote: %s\n", note)
	}
}

// Pending reports whether any stage after the read has steps to run.
func (p *Plan) Pending() bool {
	for _, st := range p.Stages {
		if st.Number > 1 && !st.Verifies && len(st.Steps) > 0 {
			return true
		}
	}
	return false
}

func (p *Plan) consistency() string {
	if p.cfg.Storage.Garage.Consistency == "" {
		return config.GarageConsistent
	}
	return p.cfg.Storage.Garage.Consistency
}

// gcmd is one garage CLI command, run in the site's own Garage container.
func gcmd(d deployment.Deployment, args string) string { return garage.Command(d) + " " + args }

// dep is the deployment every path and command here belongs to.
func (p *Plan) dep() deployment.Deployment { return p.cfg.Deployment() }

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// poll runs check up to n times with gap between, returning its last error.
func poll(n int, gap time.Duration, check func() error) error {
	var err error
	for i := 0; i < n; i++ {
		if i > 0 {
			sleep(gap)
		}
		if err = check(); err == nil {
			return nil
		}
	}
	return err
}

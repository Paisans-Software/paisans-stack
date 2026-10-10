package apply

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// MonitorReseed is one monitor site's uptime stack applied on its own, with
// Only, so that the monitors.json it seeds from follows a topology change.
//
// The seed is rendered from the whole deployment (render's uptimeSeed): a
// ping per site, a direct check per site an app runs on, and a check per
// app. It is correct whenever the monitor site is applied, and nothing else
// applies it: `apply --site` is one site at a time, and a command that joins or
// removes a site, or an app, moves files on the sites it changes and not on
// the monitor's. Without this the monitor keeps checking a site that is gone,
// alerting for as long as nobody applies it by hand, and never checks a site
// that joined. So every command that changes the topology ends with one of
// these per monitor site, as its last stage (README.md, "Topology commands
// reseed the monitor"). The fork reconciles managed monitors by name at every
// start and deletes the ones the seed no longer has, which is what makes a
// restart with the new file enough.
type MonitorReseed struct {
	Site string
	App  string
	// KeepImages leaves superseded images on the site, as apply's
	// --keep-images does, for a host the host check found shared. Execute
	// reads it, so a caller may set it after planning.
	KeepImages bool

	plan     *Plan
	rendered *render.Plan
	acme     string
	t        Transport
}

// PlanMonitorReseeds plans the reseed of every uptime app pinned to a site
// holding the monitor role, reading each monitor site and changing none. A
// deployment with no monitor site plans none. Every monitor site must have a
// transport: the monitor is written to, so a command that cannot reach it
// cannot finish.
func PlanMonitorReseeds(cfg *config.Config, rendered *render.Plan, acmeModule string, transports map[string]Transport) ([]*MonitorReseed, error) {
	var out []*MonitorReseed
	for _, site := range cfg.MonitorSites() {
		for _, app := range cfg.PinnedTo(site) {
			if cfg.Apps[app].Kind != config.KindUptime {
				continue
			}
			t, ok := transports[site]
			if !ok {
				return nil, fmt.Errorf("no way to reach the monitor site %s, whose %s is reseeded with the sites and apps as they are after this change", site, app)
			}
			m := &MonitorReseed{Site: site, App: app, rendered: rendered, acme: acmeModule, t: t}
			if err := m.build(); err != nil {
				return nil, err
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// build plans the stack's apply as it stands now.
func (m *MonitorReseed) build() error {
	opts := []Option{Only(m.App)}
	if m.KeepImages {
		opts = append(opts, KeepImages())
	}
	p, err := Build(m.Site, m.rendered, m.acme, m.t, opts...)
	if err != nil {
		return err
	}
	if c := p.Conflicts(); len(c) > 0 {
		return fmt.Errorf("%s's %s differs from what the last apply recorded, so somebody edited it on the host. Nothing was changed. Restore it, or copy what is wanted into the configuration, and run again", m.Site, c[0].Path)
	}
	m.plan = p
	return nil
}

func (m *MonitorReseed) dep() deployment.Deployment { return m.rendered.Deployment }

// Seed is the seed file's path on the host.
func (m *MonitorReseed) Seed() string { return m.dep().Path(m.App, "monitors.json") }

// Pending reports whether the reseed has anything to write or run.
func (m *MonitorReseed) Pending() bool {
	return len(m.plan.Writes()) > 0 || len(m.plan.Actions) > 0
}

// MonitorStep is one thing Execute does, for a plan.
type MonitorStep struct{ Verb, Text string }

// Steps is what Execute does, in order, for a plan: nothing when the seed on
// the host already matches the render and the stack owes no action.
func (m *MonitorReseed) Steps() []MonitorStep {
	var out []MonitorStep
	for _, w := range m.plan.Writes() {
		out = append(out, MonitorStep{w.Kind.String(), w.Path})
	}
	for _, a := range m.plan.Actions {
		verb := "restart"
		if a.Recreate {
			verb = "recreate"
		}
		out = append(out, MonitorStep{verb, fmt.Sprintf("%s, so the monitor reconciles its managed monitors from the seed, adding what is new and deleting what is gone: %s", m.App, a.Command(m.dep()))})
	}
	if len(out) > 0 {
		out = append(out, MonitorStep{"check", fmt.Sprintf("%s runs and is healthy, as apply's gate, and %s matches the render", m.App, m.Seed())})
	}
	return out
}

// Execute applies the stack. It is built again rather than run as planned:
// a partial apply keeps the manifest it read for every file outside its
// stacks, and an earlier stage may have written this site's manifest since
// (the mesh file, in site add and site remove).
func (m *MonitorReseed) Execute() error {
	if err := m.build(); err != nil {
		return err
	}
	if !m.Pending() {
		return nil
	}
	return Execute(m.plan, m.t)
}

// Gate passes when the seed on the host is the rendered one and the stack is
// healthy. It is checked on every run, so a run with nothing to do still
// proves the monitor watches the deployment as it is.
func (m *MonitorReseed) Gate() error {
	want := ""
	for _, f := range m.rendered.Files {
		if f.Path == m.Site+"/"+m.dep().RelPath(m.App, "monitors.json") {
			want = f.Content
		}
	}
	have, found, err := m.t.ReadFile(m.Seed())
	if err != nil {
		return fmt.Errorf("%s: reading %s: %w", m.Site, m.Seed(), err)
	}
	if !found {
		return fmt.Errorf("%s: %s is not on the host", m.Site, m.Seed())
	}
	if have != want {
		return fmt.Errorf("%s: %s does not match the render", m.Site, m.Seed())
	}
	return WaitHealthy(m.dep(), m.Site, m.App, m.t, "The seed is written; the monitor is not running on it")
}

// Command is the apply that does the same as the reseed, for a failure to
// name: the one command that brings the monitor up to date on its own.
func (m *MonitorReseed) Command() string {
	return fmt.Sprintf("paisans apply --site %s --only %s --execute", m.Site, m.App)
}

// MonitorCommands is every reseed's Command, joined for a message.
func MonitorCommands(list []*MonitorReseed) string {
	var out []string
	for _, m := range list {
		out = append(out, "`"+m.Command()+"`")
	}
	return strings.Join(out, " and ")
}

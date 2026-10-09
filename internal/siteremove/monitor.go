package siteremove

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
)

// monitorStage is the stage number of the monitor's reseed, the last.
const monitorStage = 5

// buildMonitor is stage 5, the last: every remaining monitor site's uptime
// stack applied on its own, so that its monitors.json, rendered from the end
// state, no longer names the removed site, and the restarted monitor deletes
// the site's ping and direct checks (see apply.MonitorReseed). It runs after
// the configuration edit because the removal is done without it: an out of
// date monitor alerts for a site that is gone, which is a nuisance, and holding
// paisans.yaml at a state the cluster has left is worse. A failure here names
// the apply that finishes it, since site remove cannot be run again for a site
// the configuration no longer declares.
func (p *Plan) buildMonitor() (*Stage, error) {
	st := &Stage{Number: monitorStage, Name: "monitor", Short: "monitor matches the render"}
	reseeds, err := apply.PlanMonitorReseeds(p.end, p.rendered, acme.Module(p.cfg.ACME.Provider), p.transports)
	if err != nil {
		return nil, fmt.Errorf("site remove %s: %w", p.Site, err)
	}
	if len(reseeds) == 0 {
		// A monitor site itself is never the one removed: its uptime app is
		// pinned to it, which Refusal stops.
		st.Short = "no monitor to reseed"
		st.Skipped = "no site holds the monitor role, so nothing watches this deployment and there is no monitor to reseed"
		return st, nil
	}
	var sites []string
	for _, m := range reseeds {
		sites = append(sites, m.Site)
		for _, s := range m.Steps() {
			st.Steps = append(st.Steps, Step{Site: m.Site, Verb: s.Verb, Text: s.Text})
		}
	}
	st.Gate = fmt.Sprintf("the monitor on %s runs on a monitors.json that matches the render without %s, so its ping and direct checks are deleted", strings.Join(sites, ", "), p.Site)
	byHand := func(err error) error {
		return fmt.Errorf("%v\nThe removal itself is done: every earlier gate passed, %s no longer declares %s, and only the monitor is out of date. Fix the cause and run %s, which does the same as this stage", err, p.ConfigPath, p.Site, apply.MonitorCommands(reseeds))
	}
	st.run = func() error {
		for _, m := range reseeds {
			p.work("apply "+m.App+" on "+m.Site).Detail("%s on %s, on the seed rendered without %s", m.App, m.Site, p.Site)
			if err := m.Execute(); err != nil {
				return byHand(err)
			}
		}
		return nil
	}
	st.gate = func() error {
		for _, m := range reseeds {
			if err := m.Gate(); err != nil {
				return byHand(err)
			}
		}
		return nil
	}
	return st, nil
}

// reseeded reports whether a rendered file on site is the monitor's own
// stack, which stage 5 applies whole rather than stage 2 listing it as owed.
func (p *Plan) reseeded(site, rel string) bool {
	if !p.end.Sites[site].Has(config.RoleMonitor) {
		return false
	}
	for _, app := range p.end.PinnedTo(site) {
		if p.end.Apps[app].Kind == config.KindUptime && strings.HasPrefix(rel, p.dep().RelPath(app)+"/") {
			return true
		}
	}
	return false
}

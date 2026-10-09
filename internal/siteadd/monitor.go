package siteadd

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// buildMonitor is stage 8, the last: every monitor site's uptime stack
// applied on its own, so that its monitors.json, rendered from the end
// state, gains the new site's ping and direct checks (see
// apply.MonitorReseed). It runs last because the join is done without it:
// a monitor that is out of date is a monitor that misses a site, not a
// cluster that is wrong, so a failure here stops nothing that already
// passed and names the apply that finishes it by hand.
func (p *Plan) buildMonitor(rendered *render.Plan) (*Stage, error) {
	st := &Stage{Number: 8, Name: "monitor"}
	reseeds, err := apply.PlanMonitorReseeds(p.cfg, rendered, p.acmeModule(), p.transports)
	if err != nil {
		return nil, fmt.Errorf("site add %s: %w", p.Site, err)
	}
	if len(reseeds) == 0 {
		st.Gate = "none: no site holds the monitor role, so nothing watches this deployment and there is no monitor to reseed"
		return st, nil
	}
	var sites []string
	for _, m := range reseeds {
		sites = append(sites, m.Site)
		for _, s := range m.Steps() {
			st.Steps = append(st.Steps, Step{Site: m.Site, Verb: s.Verb, Text: s.Text})
		}
	}
	st.Gate = fmt.Sprintf("the monitor on %s runs on a monitors.json that matches the render, so %s's ping and direct checks exist", strings.Join(sites, ", "), p.Site)
	byHand := func(err error) error {
		return fmt.Errorf("%v\nThe join itself is done: every earlier gate passed, and only the monitor is out of date. Fix the cause and run %s, which does the same as this stage", err, apply.MonitorCommands(reseeds))
	}
	st.run = func() error {
		for _, m := range reseeds {
			m.KeepImages = p.SharedSites[m.Site]
			p.say("  %-9s %s on %s, on the seed rendered with %s\n", "apply", m.App, m.Site, p.Site)
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

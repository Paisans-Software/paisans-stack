package storageadd

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
)

// buildMonitor is the last stage: every monitor site's uptime stack applied
// on its own, so that its monitors.json, rendered from the configuration, has
// a ping for every site the join brought in, Eg: a site with the storage role
// that exists for Garage alone (see apply.MonitorReseed). It runs last
// because the join is done without it, so a failure here stops nothing that
// already passed and names the apply that finishes it by hand.
func (p *Plan) buildMonitor() (*Stage, error) {
	st := &Stage{Name: "monitor"}
	reseeds, err := apply.PlanMonitorReseeds(p.cfg, p.rendered, acme.Module(p.cfg.ACME.Provider), p.transports)
	if err != nil {
		return nil, fmt.Errorf("storage add: %w", err)
	}
	if len(reseeds) == 0 {
		st.Gate = "none: no site holds the monitor role, so nothing watches this deployment and there is no monitor to reseed"
		return st, nil
	}
	var sites []string
	for _, m := range reseeds {
		m.KeepImages = p.keepImages(m.Site)
		sites = append(sites, m.Site)
		for _, s := range m.Steps() {
			st.Steps = append(st.Steps, Step{Site: m.Site, Verb: s.Verb, Text: s.Text})
		}
	}
	st.Gate = fmt.Sprintf("the monitor on %s runs on a monitors.json that matches the render, so every site has its ping and every app its checks", strings.Join(sites, ", "))
	byHand := func(err error) error {
		return fmt.Errorf("%v\nThe join itself is done: every earlier gate passed, and only the monitor is out of date. Fix the cause and run %s, which does the same as this stage", err, apply.MonitorCommands(reseeds))
	}
	st.run = func() error {
		for _, m := range reseeds {
			p.say("  %-9s %s on %s, on the seed rendered now\n", "apply", m.App, m.Site)
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

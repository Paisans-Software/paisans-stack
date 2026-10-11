package main

import (
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// hostGate runs the host check on one site before a command changes
// anything, in a dry run as well as with --execute, as one step whose result
// is the host's class, with what the check found marked under it. It returns
// the refusal for a conflict, or for a shared host whose firewall is not
// already up and denying by default; otherwise the report, whose Shared
// tells the command to touch only what is the toolkit's.
//
// It is the first thing host prepare, apply, site add and prune do on the
// site they change. There is no flag to skip it: what holds a claim is
// moved, or the configuration is changed.
func hostGate(r ui.Reporter, cfg *config.Config, site string, t hostcheck.Transport) (*hostcheck.Report, error) {
	return gate(r, "host check", cfg, site, t)
}

// gate is hostGate under a given step title.
func gate(r ui.Reporter, title string, cfg *config.Config, site string, t hostcheck.Transport) (*hostcheck.Report, error) {
	s := r.Step(title)
	report, err := hostcheck.Run(cfg, site, t)
	if err != nil {
		s.Fail(err)
		return nil, err
	}
	// The refusal carries the conflicts itself, so the step only marks the
	// line and the error, returned for main to print, says what to move.
	// What the check found is marked under the step's line, with --verbose.
	refusal := report.Refusal()
	if refusal != nil {
		s.Fail(refusal)
	} else {
		s.Done(report.Class.String())
	}
	report.Show(r)
	if refusal != nil {
		return nil, refusal
	}
	return report, nil
}

// gateSites runs hostGate on each site a command is about to change, before
// it changes any of them, and returns which the host check found shared. A
// refusal on any site stops the command with nothing changed anywhere. Each
// step names its site, since the commands that check several report them
// under one section.
func gateSites(r ui.Reporter, cfg *config.Config, sites []string, reach func(site string) hostcheck.Transport) (map[string]bool, error) {
	shared := map[string]bool{}
	for _, site := range sites {
		report, err := gate(r, "host check "+site, cfg, site, reach(site))
		if err != nil {
			return nil, err
		}
		shared[site] = report.Shared()
	}
	return shared, nil
}

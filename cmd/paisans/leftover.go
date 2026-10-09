package main

import (
	"fmt"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/ownership"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// printLeftovers is apply's "left over" section: this deployment's stacks
// still on the host that the site no longer renders, and its files that no
// apply renders any more. A whole plan's left over files are what it is about
// to record; a scoped or partial plan marks nothing, so the manifest's marks
// from the last whole apply are shown. Each is a warning naming `paisans app
// remove` for the app it belongs to, which is what takes it off, so the
// remedy shows at any verbosity. It reports nothing when nothing is left
// over.
func printLeftovers(r ui.Reporter, cfg *config.Config, site string, inv *hostcheck.Inventory, plan *apply.Plan) error {
	if inv == nil {
		return nil
	}
	manifest := inv.ManifestFiles
	if plan.Whole() {
		manifest = plan.Leftovers
	}
	report, err := ownership.Classify(cfg, site, inv, manifest)
	if err != nil {
		return err
	}
	lines := report.LeftoverLines()
	if len(lines) == 0 {
		return nil
	}
	r.Detail("left over: this deployment's, no longer rendered for %s. apply leaves each one in place.", site)
	// LeftoverLines describes the stacks, then the files, in that order.
	hint := func(what, app string) string {
		if app == "" {
			return "left over: " + what
		}
		return fmt.Sprintf("left over: %s (`paisans app remove %s`)", what, app)
	}
	for i, s := range report.Stacks {
		app := s.Name
		if app == "infra" {
			app = ""
		}
		r.Warn(hint("stack "+s.Name, app), lines[i])
	}
	for i, f := range report.Files {
		r.Warn(hint("file /"+f.Path, ownership.FileApp(f.Path)), lines[len(report.Stacks)+i])
	}
	for _, line := range removeAdvice(report) {
		r.Detail("%s", line)
	}
	return nil
}

// removeAdvice names the command that takes each app's leftovers off, once
// a whole apply has run on every site the app was on.
func removeAdvice(report ownership.Report) []string {
	var out []string
	for _, app := range report.Apps() {
		out = append(out, fmt.Sprintf("`paisans app remove %s` takes %s's off every site, once a whole apply has run on each; it keeps its data unless given --delete-data", app, app))
	}
	return out
}

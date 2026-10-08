package main

import (
	"fmt"
	"io"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/ownership"
)

// printLeftovers is apply's "left over" section: this deployment's stacks
// still on the host that the site no longer renders, and its files that no
// apply renders any more. A whole plan's left over files are what it is about
// to record; a scoped or partial plan marks nothing, so the manifest's marks
// from the last whole apply are shown. It names `paisans app remove` for each
// app they belong to, which is what takes them off. It prints nothing when
// nothing is left over.
func printLeftovers(w io.Writer, cfg *config.Config, site string, inv *hostcheck.Inventory, plan *apply.Plan) error {
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
	fmt.Fprintf(w, "\nleft over: this deployment's, no longer rendered for %s. apply leaves each one in place.\n", site)
	for _, line := range lines {
		fmt.Fprintf(w, "  %-9s %s\n", "leftover", line)
	}
	for _, line := range removeAdvice(report) {
		fmt.Fprintf(w, "  %s\n", line)
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

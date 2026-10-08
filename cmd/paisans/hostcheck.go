package main

import (
	"fmt"
	"io"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
)

// hostGate runs the host check on one site before a command changes
// anything, in a dry run as well as with --execute, and prints its report.
// It returns the refusal for a conflict, or for a shared host whose firewall
// is not already up and denying by default; otherwise the report, whose
// Shared tells the command to touch only what is the toolkit's.
//
// It is the first thing host prepare, apply, site add and prune do on the
// site they change. There is no flag to skip it: what holds a claim is
// moved, or the configuration is changed.
func hostGate(w io.Writer, cfg *config.Config, site string, t hostcheck.Transport) (*hostcheck.Report, error) {
	report, err := hostcheck.Run(cfg, site, t)
	if err != nil {
		return nil, err
	}
	report.Print(w)
	fmt.Fprintln(w)
	if err := report.Refusal(); err != nil {
		return nil, err
	}
	return report, nil
}

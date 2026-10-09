package main

import (
	"flag"
	"os"

	"github.com/paisans-software/paisans-stack/internal/ui"
)

// commonFlags registers the flags every command takes, and returns the
// reporter for the command's stdout once the flags are parsed. -v and
// --verbose show the detail behind each step: reasons, configuration values
// and raw command output. Without them a command prints one line per step.
func commonFlags(fs *flag.FlagSet) func() ui.Reporter {
	verbose := fs.Bool("verbose", false, "show the detail behind each step: reasons, values and command output")
	fs.BoolVar(verbose, "v", false, "short for --verbose")
	return func() ui.Reporter {
		if reporterOverride != nil {
			return reporterOverride
		}
		return ui.New(os.Stdout, *verbose)
	}
}

// reporterOverride, when set, is the reporter every command uses in place of
// stdout's. A test sets it to a ui.Recorder, so it asserts on the steps and
// items a command reports rather than on how they are drawn.
var reporterOverride ui.Reporter

// errReporter is r's verbosity on stderr, for a command whose stdout carries
// data that a script reads.
func errReporter(r ui.Reporter) ui.Reporter { return ui.New(os.Stderr, r.Verbose()) }

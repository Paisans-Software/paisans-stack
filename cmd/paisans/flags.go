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
	return func() ui.Reporter { return ui.New(os.Stdout, *verbose) }
}

// errReporter is r's verbosity on stderr, for a command whose stdout carries
// data that a script reads.
func errReporter(r ui.Reporter) ui.Reporter { return ui.New(os.Stderr, r.Verbose()) }

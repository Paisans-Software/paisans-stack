package main

import (
	"flag"
	"os"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
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
		r := reporterOverride
		if r == nil {
			r = ui.New(os.Stdout, *verbose)
		}
		routeRetries(r)
		return r
	}
}

// routeRetries sends an ssh retry to r as a detail of the open step: the
// error that ends the command says in full if the retries ran out. A command
// that moves its report to stderr (errReporter) moves the retries with it,
// because stdout there carries data a script reads and a retry line beside
// it would corrupt it.
func routeRetries(r ui.Reporter) { apply.SetRetryLog(detailWriter{r}) }

// detailWriter turns each line written to it into a detail of the reporter.
type detailWriter struct{ r ui.Reporter }

func (d detailWriter) Write(p []byte) (int, error) {
	d.r.Detail("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// reporterOverride, when set, is the reporter every command uses in place of
// stdout's. A test sets it to a ui.Recorder, so it asserts on the steps and
// items a command reports rather than on how they are drawn.
var reporterOverride ui.Reporter

// errReporter is r's verbosity on stderr, for a command whose stdout carries
// data that a script reads.
func errReporter(r ui.Reporter) ui.Reporter {
	if reporterOverride != nil {
		return reporterOverride
	}
	e := ui.New(os.Stderr, r.Verbose())
	routeRetries(e)
	return e
}

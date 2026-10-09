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
		// An ssh retry is the detail of whatever step is open: the error
		// that ends the command says in full if the retries ran out.
		apply.SetRetryLog(detailWriter{r})
		return r
	}
}

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
	return ui.New(os.Stderr, r.Verbose())
}

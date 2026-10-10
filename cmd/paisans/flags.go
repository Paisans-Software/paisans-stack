package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// flagsOnly, set by a test, stops each command parseFlags parses for once its
// flags parse, so a test can prove a command accepts the arguments apply
// without --site hands it without running it.
var flagsOnly bool

// errFlagsOnly is what a command returns when flagsOnly stopped it.
var errFlagsOnly = errors.New("flags parsed; stopped before doing anything")

// parseFlags is fs.Parse for each command apply without --site runs as a
// step. With flagsOnly set it returns a bad flag or a stray argument as an
// error rather than exiting, and errFlagsOnly once the flags parse.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if !flagsOnly {
		return fs.Parse(args)
	}
	fs.Init(fs.Name(), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: %q is extra", fs.Name(), fs.Arg(0))
	}
	return errFlagsOnly
}

// commonFlags registers the flags every command takes, and returns the
// reporter for the command's stdout once the flags are parsed. -v and
// --verbose show the detail behind each step: reasons, configuration values,
// requests and retries. Without them a command prints one line per step.
func commonFlags(fs *flag.FlagSet) func() ui.Reporter {
	verbose := fs.Bool("verbose", false, "show the detail behind each step: reasons, values, requests and retries")
	fs.BoolVar(verbose, "v", false, "short for --verbose")
	return func() ui.Reporter {
		r := reporterOverride
		if r == nil {
			r = ui.New(os.Stdout, *verbose)
		}
		routeRetries(r)
		routeHolds(r)
		return r
	}
}

// routeRetries sends an ssh retry to r as a detail of the open step: the
// error that ends the command says in full if the retries ran out. A command
// that moves its report to stderr (errReporter) moves the retries with it,
// because stdout there carries data a script reads and a retry line beside
// it would corrupt it.
func routeRetries(r ui.Reporter) {
	if c, ok := r.(interface{ RetryLog() io.Writer }); ok {
		apply.SetRetryLog(c.RetryLog())
		return
	}
	apply.SetRetryLog(detailWriter{r})
}

// holdOutput pauses the drawing of the reporter a command reports through,
// for as long as something else asks on the terminal. routeHolds points it at
// the reporter that draws, as routeRetries does for the retry lines, so that
// a command that moved its report to stderr holds the spinner it actually
// shows.
var holdOutput = func() (resume func()) { return func() {} }

// routeHolds holds r around the sudo password prompt and around ssh's first
// connection to each host, where ssh itself may ask to accept a host key.
func routeHolds(r ui.Reporter) {
	hold := func() (resume func()) { return ui.Hold(r) }
	holdOutput = hold
	apply.SetPromptHold(hold)
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
	e := ui.New(os.Stderr, r.Verbose())
	routeRetries(e)
	routeHolds(e)
	return e
}

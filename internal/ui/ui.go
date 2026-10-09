// Package ui is how every paisans command reports to the operator. Commands
// and libraries say what happened (a step started, ended, failed; a warning;
// a detail) and the reporter decides how it looks, so the toolkit reads like
// one installer rather than a dozen programs with their own habits.
//
// There are two levels. By default an operator sees one line per step and
// its result. With --verbose they also see every Detail and Trace: the
// reasoning, configuration values and raw command output that explain a
// step, which matter when something is wrong and are noise when it is not.
// Errors are never a detail: a failed command must not need a second run to
// say why it failed.
package ui

import (
	"io"
	"os"

	"golang.org/x/term"
)

type Reporter interface {
	// Section starts a block: a site, a configuration file, a stage.
	Section(title string)
	// Step starts something that ends in success or failure.
	Step(title string) Step
	// Item is one line of a dry run's plan: a step that would run.
	Item(title string)
	// Warn shows hint always and detail only when verbose.
	Warn(hint, detail string)
	// Refuse shows hint and explanation always: a refusal stops the command,
	// and the operator needs the reason to fix it.
	Refuse(hint, explanation string)
	// Detail is shown only when verbose. While a step is open it belongs to
	// that step and prints after the step's line.
	Detail(format string, args ...any)
	// Trace is raw output from a command or an API call, shown only when
	// verbose.
	Trace(label, output string)
	// Result is the closing line of a command.
	Result(format string, args ...any)
	// Verbose reports whether details are shown, for callers that would
	// otherwise do work only to produce one.
	Verbose() bool
}

type Step interface {
	// Done ends the step. result is shown in the second column; empty means
	// the elapsed time, when the step took a second or more.
	Done(result string)
	// Fail ends the step as failed. The error itself is the caller's to
	// return and print; Fail marks the line.
	Fail(err error)
	// Detail attaches a verbose line to this step.
	Detail(format string, args ...any)
}

// New reports to w, drawing with colour and a spinner when w is a terminal
// that allows it.
func New(w io.Writer, verbose bool) Reporter {
	return newWriter(w, verbose, isTerminal(w))
}

// NewPlain reports to w without any terminal drawing.
func NewPlain(w io.Writer, verbose bool) Reporter {
	return newWriter(w, verbose, false)
}

// Discard reports nothing.
var Discard Reporter = discard{}

// isTerminal is whether w is a terminal that should be drawn on. NO_COLOR
// (no-color.org) and TERM=dumb turn drawing off, so an operator or a CI
// system can ask for plain lines.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return false
	}
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

type discard struct{}

func (discard) Section(string)        {}
func (discard) Step(string) Step      { return discardStep{} }
func (discard) Item(string)           {}
func (discard) Warn(string, string)   {}
func (discard) Refuse(string, string) {}
func (discard) Detail(string, ...any) {}
func (discard) Trace(string, string)  {}
func (discard) Result(string, ...any) {}
func (discard) Verbose() bool         { return false }

type discardStep struct{}

func (discardStep) Done(string)           {}
func (discardStep) Fail(error)            {}
func (discardStep) Detail(string, ...any) {}

package main

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// holdingReporter is a reporter that draws between lines, as a terminal's
// does, and records whether it is held.
type holdingReporter struct {
	ui.Recorder
	held, holds int
}

func (h *holdingReporter) Hold() func() {
	h.held++
	h.holds++
	return func() { h.held-- }
}

// The sudo password is asked while a step's spinner is drawing (the first
// sudo command runs inside "check mesh subnet"), so the prompt is read with
// the reporter that draws held: otherwise the spinner's next frame erases the
// prompt and the operator sees a spinner silently waiting on them.
func TestSudoPromptHoldsTheReporter(t *testing.T) {
	saved, savedRead := holdOutput, readPassword
	t.Cleanup(func() { holdOutput, readPassword = saved, savedRead; routeHolds(ui.Discard) })
	r := &holdingReporter{}
	routeHolds(r)

	readPassword = func(host string) (string, error) {
		if r.held != 1 {
			t.Errorf("the password was asked with %d holds in place, want 1", r.held)
		}
		return "secret", nil
	}
	password, err := promptOnTerminal("home-a")
	if err != nil || password != "secret" {
		t.Fatalf("got %q, %v", password, err)
	}
	if r.held != 0 || r.holds != 1 {
		t.Errorf("after the prompt: %d holds in place, %d taken; want 0 and 1", r.held, r.holds)
	}
}

// The reporter a command moves to stderr is the one drawing, so the hold
// moves with it.
func TestErrReporterTakesTheHold(t *testing.T) {
	saved := holdOutput
	t.Cleanup(func() { holdOutput = saved; routeHolds(ui.Discard); apply.SetRetryLog(nil) })
	first := &holdingReporter{}
	routeHolds(first)
	errReporter(first)
	holdOutput()()
	if first.holds != 0 {
		t.Error("the hold still points at the stdout reporter after errReporter")
	}
}

// The sudo password prompt is followed by a blank line, so the next question
// or the report starts apart from it.
func TestSudoPromptIsFollowedByABlankLine(t *testing.T) {
	savedRead, savedBlank := readPassword, blankLineOnTerminal
	t.Cleanup(func() { readPassword, blankLineOnTerminal = savedRead, savedBlank })
	blanks := 0
	readPassword = func(string) (string, error) { return "secret", nil }
	blankLineOnTerminal = func() { blanks++ }
	if _, err := promptOnTerminal("home-a"); err != nil {
		t.Fatal(err)
	}
	if blanks != 1 {
		t.Errorf("%d blank lines after the prompt, want 1", blanks)
	}
}

package main

import (
	"bytes"
	"errors"
	"strings"
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

// The sudo password prompt is erased once answered, right or wrong: the
// newline that ends it, a carriage return, then a cursor up and a line clear
// for each row it took. Nothing else is written, so no blank line is left.
func TestSudoPromptIsErasedOnceAnswered(t *testing.T) {
	prompt := sudoPrompt("site home-a (ubuntu@192.0.2.10, host home-a-01)")
	for _, c := range []struct {
		password string
		err      error
	}{{"secret", nil}, {"", errors.New("EOF")}} {
		var b bytes.Buffer
		got, err := askPassword(&b, prompt, 80, true, func() ([]byte, error) { return []byte(c.password), c.err })
		if got != c.password || !errors.Is(err, c.err) {
			t.Errorf("got %q, %v; want %q, %v", got, err, c.password, c.err)
		}
		if want := prompt + "\n\r\x1b[1A\x1b[2K"; b.String() != want {
			t.Errorf("err %v: wrote %q, want %q", c.err, b.String(), want)
		}
	}
}

// A prompt that wrapped at the terminal's width is erased over every row it
// took.
func TestALongSudoPromptIsErasedWhole(t *testing.T) {
	prompt := sudoPrompt("site " + strings.Repeat("x", 90)) // 115 characters
	for _, c := range []struct{ cols, rows int }{{40, 3}, {115, 1}, {114, 2}, {200, 1}, {0, 1}} {
		if got := promptRows(prompt, c.cols); got != c.rows {
			t.Errorf("%d columns: %d rows, want %d", c.cols, got, c.rows)
		}
	}
	var b bytes.Buffer
	if _, err := askPassword(&b, prompt, 40, true, func() ([]byte, error) { return []byte("secret"), nil }); err != nil {
		t.Fatal(err)
	}
	if want := prompt + "\n\r" + strings.Repeat("\x1b[1A\x1b[2K", 3); b.String() != want {
		t.Errorf("wrote %q, want %q", b.String(), want)
	}
}

// A terminal that is not drawn on (TERM=dumb, NO_COLOR) gets no cursor
// movement: the prompt stays, and a blank line sets what follows apart.
func TestASudoPromptIsNotErasedWithoutDrawing(t *testing.T) {
	var b bytes.Buffer
	if _, err := askPassword(&b, "sudo password for home-a: ", 80, false, func() ([]byte, error) { return []byte("secret"), nil }); err != nil {
		t.Fatal(err)
	}
	if want := "sudo password for home-a: \n\n"; b.String() != want {
		t.Errorf("wrote %q, want %q", b.String(), want)
	}
	for _, c := range []struct {
		term, noColor string
		erase         bool
	}{{"xterm-256color", "", true}, {"dumb", "", false}, {"xterm-256color", "1", false}} {
		t.Setenv("TERM", c.term)
		t.Setenv("NO_COLOR", c.noColor)
		if got := promptErasable(); got != c.erase {
			t.Errorf("TERM=%s NO_COLOR=%q: erasable %v, want %v", c.term, c.noColor, got, c.erase)
		}
	}
}

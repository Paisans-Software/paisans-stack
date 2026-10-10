package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// sudoAuths holds one SudoAuth per destination for the whole run, so a site
// whose transport is built several times (a map of every site, then the one
// a stage needs) asks for its sudo password once.
var (
	sudoAuthsMu sync.Mutex
	sudoAuths   = map[string]*apply.SudoAuth{}
)

func sudoAuthFor(destination string) *apply.SudoAuth {
	sudoAuthsMu.Lock()
	defer sudoAuthsMu.Unlock()
	if auth, ok := sudoAuths[destination]; ok {
		return auth
	}
	var prompt func(string) (string, error)
	if terminalAvailable() {
		prompt = promptOnTerminal
	}
	auth := apply.NewSudoAuth(prompt)
	sudoAuths[destination] = auth
	return auth
}

// openTTY opens the controlling terminal. The password is read there rather
// than from stdin, because stdin may already carry something else (`secrets
// set` and `app admin create` read a value from it), and because a run with
// no controlling terminal, from cron or CI, is exactly the run that must not
// be asked. Tests replace it.
var openTTY = func() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

func terminalAvailable() bool {
	tty, err := openTTY()
	if err != nil {
		return false
	}
	defer tty.Close()
	return isTerminal(tty)
}

// promptOnTerminal reads a sudo password from the controlling terminal with
// echo off, with the reporter's drawing held. The first sudo command runs
// inside an open step, so without the hold the spinner's next frame would
// erase the prompt and leave the operator watching a spinner that waits on
// them without saying so. The prompt is erased once answered, so the
// spinner redraws on the row it took.
func promptOnTerminal(host string) (string, error) {
	resume := holdOutput()
	defer resume()
	return readPassword(host)
}

// readPassword asks on the controlling terminal. Tests replace it, since a
// test has no terminal to type into.
var readPassword = func(host string) (string, error) {
	tty, err := openTTY()
	if err != nil {
		return "", err
	}
	defer tty.Close()
	fd := int(tty.Fd())
	cols, _, err := term.GetSize(fd)
	if err != nil {
		cols = 0
	}
	return askPassword(tty, sudoPrompt(host), cols, func() ([]byte, error) { return term.ReadPassword(fd) })
}

func sudoPrompt(host string) string { return "sudo password for " + host + ": " }

// askPassword writes prompt to tty, a terminal cols wide, reads the answer
// with read, and erases the prompt, whether the answer was read or not:
// what follows says whether it was right. Echo is off while read waits, so
// the newline that ends the prompt is written here, and then, from the row
// below the prompt, a carriage return and, for each row the prompt took, a
// cursor up and a line clear. The cursor ends at the start of the prompt's
// first row, which is empty, where the held spinner redraws.
func askPassword(tty io.Writer, prompt string, cols int, read func() ([]byte, error)) (string, error) {
	fmt.Fprint(tty, prompt)
	password, err := read()
	fmt.Fprintln(tty)
	fmt.Fprint(tty, "\r"+strings.Repeat("\x1b[1A\x1b[2K", promptRows(prompt, cols)))
	if err != nil {
		return "", err
	}
	return string(password), nil
}

// promptRows is how many rows prompt takes on a terminal cols wide: one
// when the width is not known, since a terminal too narrow to say is also
// too unusual to guess for.
func promptRows(prompt string, cols int) int {
	n := utf8.RuneCountInString(prompt)
	if cols <= 0 || n <= cols {
		return 1
	}
	return (n + cols - 1) / cols
}

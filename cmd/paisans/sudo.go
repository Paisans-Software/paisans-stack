package main

import (
	"fmt"
	"os"
	"sync"

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
// them without saying so.
func promptOnTerminal(host string) (string, error) {
	resume := holdOutput()
	defer resume()
	password, err := readPassword(host)
	blankLineOnTerminal()
	return password, err
}

// blankLineOnTerminal writes an empty line to the controlling terminal after
// the password prompt, so the next question or the report starts apart from
// it. Tests replace it.
var blankLineOnTerminal = func() {
	tty, err := openTTY()
	if err != nil {
		return
	}
	defer tty.Close()
	fmt.Fprintln(tty)
}

// readPassword asks on the controlling terminal. Tests replace it, since a
// test has no terminal to type into.
var readPassword = func(host string) (string, error) {
	tty, err := openTTY()
	if err != nil {
		return "", err
	}
	defer tty.Close()
	fmt.Fprintf(tty, "sudo password for %s: ", host)
	password, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return "", err
	}
	return string(password), nil
}

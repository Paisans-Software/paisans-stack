// Package apply pushes rendered artifacts to a host and takes the narrowest
// action that makes them live.
//
// Everything here is staged and every stage is a gate that stops rather than
// warning, because the blast radius lands on somebody else's stack. The order
// is fixed: read what is on the host, refuse if anybody edited it, write, check
// the assembled gateway configuration, and only then restart or recreate
// anything.
package apply

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Transport is the one place this package talks to a machine. Everything else
// works in terms of files and commands, which is what makes the whole planner
// testable without a host in the room.
type Transport interface {
	// Run executes a command and returns its combined output. A non zero exit
	// is an error: a caller that wants to tolerate one says so by inspecting
	// the error, never by ignoring it.
	Run(command string) (string, error)
	// ReadFile returns a file's contents. A missing file is reported through
	// the second result rather than as an error, because "not there yet" is the
	// ordinary case on a first apply.
	ReadFile(path string) (content string, found bool, err error)
	// WriteFile writes content at path with mode, creating parent directories.
	WriteFile(path string, content string, mode uint32) error
	// Describe names the destination, for messages.
	Describe() string
}

// SSHTransport runs commands through the operator's own ssh binary.
//
// Shelling out rather than embedding an SSH client is deliberate, and it is the
// opposite of the choice made for sops and age. Authentication is the
// operator's: their agent, their keys, their `~/.ssh/config` with its jump
// hosts and per host users, their `known_hosts`. An embedded client would have
// to reimplement all of that or, far worse, invite a toolkit specific way to
// hand it a private key. sops is embedded because its alternative is asking an
// operator to install a binary; ssh is not, because every operator already has
// one and it already knows things this toolkit should never learn.
type SSHTransport struct {
	// Destination is whatever ssh accepts: a host, a user@host, or an alias
	// from the operator's config.
	Destination string
	// Sudo prefixes every command. The paths written here are under /srv and
	// /etc, which a deploy user does not own.
	Sudo bool
}

func (t SSHTransport) Describe() string { return t.Destination }

func (t SSHTransport) Run(command string) (string, error) {
	if t.Sudo {
		command = "sudo sh -c " + shellQuote(command)
	}
	cmd := exec.Command("ssh", t.Destination, command)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s: %s: %w\n%s", t.Destination, firstLine(command), err, out.String())
	}
	return out.String(), nil
}

func (t SSHTransport) ReadFile(path string) (string, bool, error) {
	out, err := t.Run(fmt.Sprintf("if [ -f %s ]; then cat %s; else echo __PAISANS_ABSENT__; fi", shellQuote(path), shellQuote(path)))
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(out) == "__PAISANS_ABSENT__" {
		return "", false, nil
	}
	return out, true, nil
}

// WriteFile sends content over stdin rather than as part of the command.
//
// A rendered file carries credentials, and a command line is visible in `ps`
// to every user on the host for as long as it runs. Feeding it through stdin
// keeps it out of the process table, and writing to a temporary file first
// means a failed transfer leaves the previous file intact rather than a
// truncated one that an application will happily read.
func (t SSHTransport) WriteFile(path, content string, mode uint32) error {
	dir := parentDir(path)
	script := fmt.Sprintf(
		"set -e; mkdir -p %s; tmp=%s.paisans-tmp; cat > $tmp; chmod %04o $tmp; mv $tmp %s",
		shellQuote(dir), shellQuote(path), mode, shellQuote(path))
	if t.Sudo {
		script = "sudo sh -c " + shellQuote(script)
	}
	cmd := exec.Command("ssh", t.Destination, script)
	cmd.Stdin = strings.NewReader(content)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: writing %s: %w\n%s", t.Destination, path, err, out.String())
	}
	return nil
}

func parentDir(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "/"
}

// shellQuote wraps a value in single quotes for /bin/sh, escaping any single
// quote inside it. Paths here come from a configuration an operator wrote, so
// they are not hostile, but they are not guaranteed to be free of spaces
// either.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

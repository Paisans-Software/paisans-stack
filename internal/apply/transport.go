// Package apply pushes rendered artifacts to a host and takes the narrowest
// action that makes them live.
//
// Everything here is staged and every stage is a gate that stops rather than
// warning, because the blast radius lands on somebody else's stack. The order
// is fixed: read what is on the host, refuse if anybody edited it, write, bring
// up the mesh, check the assembled gateway configuration, act on the
// infrastructure stack, create clustered apps' databases, and only then
// restart or recreate any app. Every stack acted on must come up healthy
// before the next one moves.
package apply

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// RunInput is Run with stdin. It exists for input that must not appear
	// in a command line, such as SQL carrying a password: a command line is
	// visible in `ps` to every user on the host, and stdin is not.
	RunInput(command, stdin string) (string, error)
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
//
// A site's ssh section supplies User, Host, Port and PublicKeys. Destination
// is the `--ssh` escape hatch: when it is set it is passed to ssh verbatim, and
// the other four are not used at all, so an operator whose route the section
// cannot describe still has one.
type SSHTransport struct {
	// Destination, when set, is whatever ssh accepts (a host, a user@host,
	// or an alias from the operator's config), passed as given.
	Destination string
	// User and Host make the destination when Destination is empty.
	User string
	Host string
	// Port is ssh's -p. Zero means 22.
	Port int
	// PublicKeys are authorized_keys lines, one key each. Each is written to
	// a file of its own and given to ssh with -i, under IdentitiesOnly, so
	// ssh offers exactly these keys and finds their private halves in the
	// operator's agent. See SSHArgs.
	PublicKeys []string
	// Sudo prefixes every command. The paths written here are under /srv and
	// /etc, which a deploy user does not own.
	Sudo bool
}

func (t SSHTransport) Describe() string {
	if t.Destination != "" {
		return t.Destination
	}
	out := t.User + "@" + t.Host
	if p := t.port(); p != 22 {
		out += fmt.Sprintf(" port %d", p)
	}
	return out
}

func (t SSHTransport) port() int {
	if t.Port == 0 {
		return 22
	}
	return t.Port
}

// SSHArgs is ssh's argument list for one remote command, with the public key
// files at keyFiles (in the order of PublicKeys).
//
// `-i` names a public key file, not a private one. ssh(1), on -i, and
// ssh_config(5), on IdentityFile, both say a public key file may be given "to
// use the corresponding private key that is loaded in ssh-agent(1) when the
// private key file is not present locally", and ssh_config(5) on IdentityFile
// says it "may be used in conjunction with IdentitiesOnly to select which
// identities in an agent are offered during authentication" (OpenSSH 9.9p2's
// pages). IdentitiesOnly=yes is what makes the selection a selection: without
// it ssh also offers every other key the agent holds, and a server that allows
// six attempts can refuse the operator before reaching the right one. Several
// -i are allowed and tried in order, so every listed key is offered, and the
// operator's private key, whichever of them it is, is never named here.
//
// Keys from the operator's own IdentityFile lines are still offered, since
// IdentitiesOnly restricts to configured files rather than to the command
// line's, and every other option in ~/.ssh/config (ProxyJump, known hosts)
// still applies. A -p and a user@ given here take precedence over that file's
// Port and User, because command line options are read first.
//
// BatchMode is not set, as before: a first connection may still ask the
// operator to accept a host key, and refusing that would push them to
// disable host key checking instead.
func (t SSHTransport) SSHArgs(keyFiles []string, command string) []string {
	if t.Destination != "" {
		return []string{t.Destination, command}
	}
	args := []string{"-p", strconv.Itoa(t.port()), "-o", "IdentitiesOnly=yes"}
	for _, f := range keyFiles {
		args = append(args, "-i", f)
	}
	return append(args, t.User+"@"+t.Host, command)
}

// ssh builds the command for one remote command, with the key files it
// needs, and a cleanup that removes them.
//
// The files go in a fresh directory per invocation, made by os.MkdirTemp,
// which creates it 0700; each file is 0600. A public key is not a secret, but
// a world writable path ssh reads an identity from would let another local
// user choose which key it offers. Writing them per invocation rather than
// once per transport means nothing outlives the command that used it, even
// when the caller never calls a Close.
func (t SSHTransport) ssh(command string) (*exec.Cmd, func(), error) {
	if t.Destination != "" {
		return exec.Command("ssh", t.SSHArgs(nil, command)...), func() {}, nil
	}
	if len(t.PublicKeys) == 0 {
		return nil, nil, fmt.Errorf("%s: no public key to offer. List one under the site's ssh.public_key, or pass --ssh", t.Describe())
	}
	dir, err := os.MkdirTemp("", "paisans-ssh-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	files, err := writeKeyFiles(dir, t.PublicKeys)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return exec.Command("ssh", t.SSHArgs(files, command)...), cleanup, nil
}

// writeKeyFiles writes one public key per file, 0600, named in order.
func writeKeyFiles(dir string, keys []string) ([]string, error) {
	var files []string
	for i, key := range keys {
		path := filepath.Join(dir, fmt.Sprintf("key-%d.pub", i+1))
		if err := os.WriteFile(path, []byte(strings.TrimSpace(key)+"\n"), 0o600); err != nil {
			return nil, err
		}
		files = append(files, path)
	}
	return files, nil
}

func (t SSHTransport) Run(command string) (string, error) {
	if t.Sudo {
		command = "sudo sh -c " + shellQuote(command)
	}
	cmd, cleanup, err := t.ssh(command)
	if err != nil {
		return "", err
	}
	defer cleanup()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %s: %w\n%s", t.Describe(), firstLine(command), err, out.String())
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
	cmd, cleanup, err := t.ssh(script)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdin = strings.NewReader(content)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: writing %s: %w\n%s", t.Describe(), path, err, out.String())
	}
	return nil
}

// RunInput runs a command with stdin, for the same reason WriteFile sends
// content that way: what goes in carries credentials, and only the command
// line is visible in the process table.
func (t SSHTransport) RunInput(command, stdin string) (string, error) {
	if t.Sudo {
		command = "sudo sh -c " + shellQuote(command)
	}
	cmd, cleanup, err := t.ssh(command)
	if err != nil {
		return "", err
	}
	defer cleanup()
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %s: %w\n%s", t.Describe(), firstLine(command), err, out.String())
	}
	return out.String(), nil
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

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
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paisans-software/paisans-stack/internal/ui"
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
	// Site is the site's name in paisans.yaml, when the transport reaches
	// one. It is never part of the ssh command; it names the site where the
	// operator is asked something, so a prompt says which site it is for
	// rather than only an address the operator may not recognise.
	Site string
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
	// Auth is how sudo is satisfied on this host, shared by every copy of
	// the transport so a site is asked once. Nil runs sudo as it is, which
	// works only where it asks for no password.
	Auth *SudoAuth
	// ConnectTimeout, in seconds, is ssh's -o ConnectTimeout. Zero leaves
	// ssh's own, which is the operating system's TCP connect timeout and can
	// be over a minute for a host that is switched off. A command that asks
	// every site whether it is up (`paisans doctor`) sets it, so a dead
	// site costs seconds rather than minutes per attempt. It is given on the
	// command line, so it takes precedence over ~/.ssh/config. Like every
	// other field here it is not used with Destination, which stays verbatim.
	ConnectTimeout int
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
//
// The command is sent as RemoteCommand wraps it, on both routes.
func (t SSHTransport) SSHArgs(keyFiles []string, command string) []string {
	command = RemoteCommand(command)
	if t.Destination != "" {
		return []string{t.Destination, command}
	}
	args := []string{"-p", strconv.Itoa(t.port()), "-o", "IdentitiesOnly=yes"}
	if t.ConnectTimeout > 0 {
		args = append(args, "-o", "ConnectTimeout="+strconv.Itoa(t.ConnectTimeout))
	}
	for _, f := range keyFiles {
		args = append(args, "-i", f)
	}
	return append(args, t.User+"@"+t.Host, command)
}

// RemoteCommand is what ssh sends for command: command run by sh, whatever
// the login user's shell is.
//
// sshd hands the string ssh sends to the login shell (`$SHELL -c`), and the
// commands built here are sh syntax, quoted for sh. A login shell with other
// quoting rules misreads them: inside single quotes fish reads `\'` as an
// escaped quote, so the way shellQuote closes, escapes and reopens a quote
// leaves fish's quotes unbalanced, and it fails before anything runs. So the
// command travels as base64, which has no quote, backslash or space, inside a
// string every common shell parses the same way: `sh -c`, then one single
// quoted word holding only `eval`, a double quoted `$(...)`, `printf %s`, the
// base64 and `base64 -d`. sh decodes it and evaluates it as the command
// itself, so its output and exit status are the command's, and stdin reaches
// the command untouched, which sudo's password and WriteFile's content need.
//
// The string is a third longer than the command, and it is still one
// argument: Linux allows 128 KiB in one (MAX_ARG_STRLEN), and anything large
// here (rendered files, SQL) goes on stdin, not in the command.
func RemoteCommand(command string) string {
	return `sh -c 'eval "$(printf %s ` + base64.StdEncoding.EncodeToString([]byte(command)) + ` | base64 -d)"'`
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
	command, stdin, err := t.sudo(command, nil)
	if err != nil {
		return "", err
	}
	out, err := t.run(command, stdin)
	if err != nil {
		return out, fmt.Errorf("%s: %s: %w\n%s", t.Describe(), firstLine(command), err, strings.TrimRight(out, "\n"))
	}
	return out, nil
}

// How often a connection that never opened is tried again, and how long to
// wait before each retry: three attempts in all. Package variables so that a
// test can shrink them.
var sshRetryDelays = []time.Duration{2 * time.Second, 4 * time.Second}

// runSSH runs one built ssh command. A package variable so that a test can
// stand in for the ssh binary and choose its output and exit status.
var runSSH = func(cmd *exec.Cmd) error { return cmd.Run() }

// retryLog receives one line per retry. Nothing by default: a retry that
// then succeeds is not news, and one that runs out is reported in full by the
// error that ends the command, so the lines are only worth showing to
// someone who asked for detail. SetRetryLog lets a command route them there.
var retryLog io.Writer = io.Discard

// SetRetryLog sends the notice of each ssh retry to w, or nowhere for nil.
func SetRetryLog(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	retryLog = w
}

// promptHold pauses the operator's progress display while ssh may be asking
// on the terminal, and returns what resumes it. Nothing by default, since a
// run with nothing drawn has nothing to pause. SetPromptHold lets a command
// connect it to the reporter that draws.
var promptHold = func() (resume func()) { return func() {} }

// SetPromptHold pauses with hold around an ssh that may ask the operator
// something, or with nothing for nil.
func SetPromptHold(hold func() (resume func())) {
	if hold == nil {
		hold = func() (resume func()) { return func() {} }
	}
	promptHold = hold
}

// contacted is every destination ssh has reached in this run. ssh asks on
// the terminal itself, not through the toolkit, when it meets a host key it
// does not know, and that happens on a first connection. Until a host has
// answered once, each attempt is made with the display held, so a spinner
// redrawing its line cannot erase the question; after that the spinner runs.
var (
	contactedMu sync.Mutex
	contacted   = map[string]bool{}
)

func firstContact(destination string) bool {
	contactedMu.Lock()
	defer contactedMu.Unlock()
	return !contacted[destination]
}

func markContacted(destination string) {
	contactedMu.Lock()
	defer contactedMu.Unlock()
	contacted[destination] = true
}

// hostKeyKnown reports whether ssh already knows t's host key, so connecting
// asks nothing. It asks ssh -G where the host and its known_hosts files are,
// then ssh-keygen -F in each file. Anything it cannot tell counts as known:
// the cost of a wrong answer is one missing blank line. Tests replace it.
var hostKeyKnown = func(t SSHTransport) bool {
	args := t.SSHArgs(nil, "true")
	out, err := exec.Command("ssh", append([]string{"-G"}, args[:len(args)-1]...)...).Output()
	if err != nil {
		return true
	}
	host, port := "", "22"
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "hostname":
			host = value
		case "port":
			port = value
		case "userknownhostsfile", "globalknownhostsfile":
			files = append(files, strings.Fields(value)...)
		}
	}
	if host == "" {
		return true
	}
	name := host
	if port != "22" {
		name = "[" + host + "]:" + port
	}
	home, _ := os.UserHomeDir()
	for _, f := range files {
		if strings.HasPrefix(f, "~/") && home != "" {
			f = filepath.Join(home, f[2:])
		}
		if exec.Command("ssh-keygen", "-F", name, "-f", f).Run() == nil {
			return true
		}
	}
	return false
}

// blankLineOnTerminal writes an empty line to the controlling terminal, after
// ssh's question about a host key, so what follows starts apart from the
// answer. Without a terminal there was no question. Tests replace it.
var blankLineOnTerminal = func() {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer tty.Close()
	fmt.Fprintln(tty)
}

// ErrUnreachable marks a command that never reached the host: ssh could not
// connect, on every attempt. A caller asking the host a question can tell
// "the host said no" from "the host was never asked" with errors.Is, and must
// never read the second as an answer.
var ErrUnreachable = errors.New("ssh could not connect to the host")

// connectionFailures are what ssh prints when the connection itself failed,
// before any command could run. "Connection closed by" is the pre
// authentication form ("Connection closed by 203.0.113.10 port 22"); once a
// session is open ssh says "Connection to <host> closed by remote host"
// instead, which is not matched, because by then the command may have run.
var connectionFailures = []string{
	"Operation timed out",
	"Connection timed out",
	"Connection refused",
	"Connection reset",
	"No route to host",
	"kex_exchange_identification",
	"Connection closed by",
}

// connectionFailed reports whether a failed ssh never reached the remote
// command. Both halves are needed. ssh exits 255 on its own errors, but a
// remote command may exit 255 too, so the status alone proves nothing. And
// the strings alone prove nothing either, since a remote command can print
// "Connection refused" about some other connection. ssh's own error is its
// last line, after anything the remote side printed, so only that line is
// read, and a session that had opened (client_loop, which ssh prints only
// once one exists) is never a connection failure.
func connectionFailed(err error, out string) bool {
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 255 {
		return false
	}
	last := lastLine(out)
	if strings.Contains(last, "client_loop") {
		return false
	}
	for _, s := range connectionFailures {
		if strings.Contains(last, s) {
			return true
		}
	}
	return false
}

// authProblem is the Problem for a connection ssh opened and then would not
// use: the host's key is not the one known, or is not known and ssh could
// not ask, or the host accepted none of the operator's keys. ssh says each
// on its own last line and exits 255, as for connectionFailed. Nil for
// anything else.
func (t SSHTransport) authProblem(err error, out string) error {
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 255 {
		return nil
	}
	last := lastLine(out)
	switch {
	case strings.Contains(last, "Host key verification failed") && strings.Contains(out, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		return &ui.Problem{
			Hint:    t.Describe() + " offered a host key other than the one known",
			Explain: fmt.Sprintf("Either the host was reinstalled, or something is answering in its place. Find out which before going on. Once you know it is the same host, remove the old key with ssh-keygen -R %s, connect once with %s to accept the new one, and run again.", shellQuote(t.knownHostsName()), t.connectCommand()),
			Cause:   err,
		}
	case strings.Contains(last, "Host key verification failed"):
		return &ui.Problem{
			Hint:    t.Describe() + "'s host key is not known",
			Explain: fmt.Sprintf("ssh could not ask whether to trust it. Connect once with %s, check the fingerprint it shows, accept it, and run again.", t.connectCommand()),
			Cause:   err,
		}
	case strings.Contains(last, "Permission denied (") || strings.Contains(last, "Too many authentication failures"):
		return &ui.Problem{
			Hint:    t.Describe() + " did not accept your ssh key",
			Explain: "Check that your key is loaded in your ssh agent (ssh-add -l), that its public half is in the site's ssh.public_key, and that the host's authorized_keys for that user holds it.",
			Cause:   err,
		}
	}
	return nil
}

// connectCommand is the ssh command an operator runs to reach the host by
// hand.
func (t SSHTransport) connectCommand() string {
	if t.Destination != "" {
		return "ssh " + t.Destination
	}
	if p := t.port(); p != 22 {
		return fmt.Sprintf("ssh -p %d %s@%s", p, t.User, t.Host)
	}
	return "ssh " + t.User + "@" + t.Host
}

// knownHostsName is the host as known_hosts names it.
func (t SSHTransport) knownHostsName() string {
	if t.Destination != "" {
		_, host, found := strings.Cut(t.Destination, "@")
		if !found {
			return t.Destination
		}
		return host
	}
	if p := t.port(); p != 22 {
		return fmt.Sprintf("[%s]:%d", t.Host, p)
	}
	return t.Host
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// run runs one remote command, with stdin when it is not nil, and retries a
// connection that never opened.
//
// Only that failure is retried. A command that ran and failed is reported at
// once, whatever its status: running it again could repeat whatever it did
// before failing, and the failure is the host's answer. A connection that
// never opened ran nothing, so trying again cannot do anything twice. The
// first real storage add met exactly this twice, a connect timeout on a host
// that answered seconds later, and stopped a multi stage operation half way
// for it. WriteFile is retried the same way and is safe to be: it writes a
// temporary file and renames it, so even a transfer cut off part way leaves
// the old file whole and a second attempt starts over.
//
// Three attempts, two and four seconds apart, is enough for a blip and short
// enough that a host which is really down is reported within seconds rather
// than hidden behind a long wait.
func (t SSHTransport) run(command string, stdin *string) (string, error) {
	for attempt := 1; ; attempt++ {
		cmd, cleanup, err := t.ssh(command)
		if err != nil {
			return "", err
		}
		if stdin != nil {
			cmd.Stdin = strings.NewReader(*stdin)
		}
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		resume := func() {}
		asks := false
		if firstContact(t.Describe()) {
			resume = promptHold()
			asks = !hostKeyKnown(t)
		}
		err = runSSH(cmd)
		if asks && (err == nil || !connectionFailed(err, out.String())) {
			// ssh asked about the host's key and was answered. The blank
			// line goes before the display resumes, so the spinner redraws
			// below it rather than leaving a frame above it.
			blankLineOnTerminal()
		}
		resume()
		cleanup()
		if err == nil {
			markContacted(t.Describe())
			return out.String(), nil
		}
		if !connectionFailed(err, out.String()) {
			// The host answered, so any question about its key is settled.
			markContacted(t.Describe())
			if p := t.authProblem(err, out.String()); p != nil {
				return out.String(), p
			}
			return out.String(), err
		}
		if attempt > len(sshRetryDelays) {
			return out.String(), &ui.Problem{
				Hint:    t.Describe() + " cannot be reached over ssh",
				Explain: fmt.Sprintf("ssh could not connect in %d attempts: %s. Check that the host is up, and that %s from this machine reaches it.", attempt, strings.TrimSuffix(lastLine(out.String()), "."), t.connectCommand()),
				Cause:   fmt.Errorf("%w after %d attempts: %w", ErrUnreachable, attempt, err),
			}
		}
		delay := sshRetryDelays[attempt-1]
		fmt.Fprintf(retryLog, "%s: ssh could not connect (%s), retrying in %s (attempt %d of %d)\n",
			t.Describe(), lastLine(out.String()), delay, attempt+1, len(sshRetryDelays)+1)
		sleep(delay)
	}
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
	script, stdin, err := t.sudo(script, &content)
	if err != nil {
		return err
	}
	if out, err := t.run(script, stdin); err != nil {
		return fmt.Errorf("%s: writing %s: %w\n%s", t.Describe(), path, err, strings.TrimRight(out, "\n"))
	}
	return nil
}

// RunInput runs a command with stdin, for the same reason WriteFile sends
// content that way: what goes in carries credentials, and only the command
// line is visible in the process table.
func (t SSHTransport) RunInput(command, stdin string) (string, error) {
	command, in, err := t.sudo(command, &stdin)
	if err != nil {
		return "", err
	}
	out, err := t.run(command, in)
	if err != nil {
		return out, fmt.Errorf("%s: %s: %w\n%s", t.Describe(), firstLine(command), err, strings.TrimRight(out, "\n"))
	}
	return out, nil
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

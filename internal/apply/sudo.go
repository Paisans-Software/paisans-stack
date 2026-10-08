package apply

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// sudoProbe asks whether sudo runs without a password. -n makes sudo fail at
// once instead of waiting for one, and it prints "a password is required"
// when that is the only thing missing (sudo 1.9).
const sudoProbe = "sudo -n true"

// sudoWithPassword is sudo reading its password from the first line of
// stdin. -S reads it from stdin rather than a terminal; -p ” prints no
// prompt, since the output is read by the toolkit; and -k ignores any cached
// credential, so sudo reads that line on every command. Without -k, a command
// run while the credential was cached would get the password line as the top
// of its own input, and WriteFile would write it into a file.
//
// sudo reads the password one byte at a time up to the newline, so whatever
// follows it on stdin reaches the command unchanged.
const sudoWithPassword = "sudo -k -S -p ''"

// SudoAuth is how sudo is satisfied on one host. It asks the host once
// whether sudo needs a password and, if it does, asks the operator once, and
// every copy of the transport sharing it reuses the answer. The password is
// kept in memory only, and reaches the host on the stdin of each ssh
// session, never on a command line.
type SudoAuth struct {
	// Prompt asks the operator for the password of the host it names. Nil
	// means there is no terminal to ask on, and a host that wants a password
	// is refused.
	Prompt func(host string) (string, error)

	mu       sync.Mutex
	settled  bool
	password string
	err      error
}

// NewSudoAuth returns a SudoAuth that asks with prompt, or refuses a host
// that wants a password when prompt is nil.
func NewSudoAuth(prompt func(host string) (string, error)) *SudoAuth {
	return &SudoAuth{Prompt: prompt}
}

// sudo wraps a command to run under sudo, when the transport says to, and
// returns the stdin to send with it.
func (t SSHTransport) sudo(command string, stdin *string) (string, *string, error) {
	if !t.Sudo {
		return command, stdin, nil
	}
	if t.Auth == nil {
		return "sudo sh -c " + shellQuote(command), stdin, nil
	}
	password, err := t.Auth.resolve(t)
	if err != nil {
		return "", nil, err
	}
	if password == "" {
		return "sudo sh -c " + shellQuote(command), stdin, nil
	}
	in := password + "\n"
	if stdin != nil {
		in += *stdin
	}
	return sudoWithPassword + " sh -c " + shellQuote(command), &in, nil
}

// resolve returns the password sudo wants on this host, or "" when it wants
// none, asking the host and the operator only the first time.
//
// A refusal is kept, so a wrong password is never sent twice: repeated
// failures can lock the account where the host counts them. A probe that
// never reached the host is not kept, because it is not an answer.
func (a *SudoAuth) resolve(t SSHTransport) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.settled {
		return a.password, a.err
	}

	out, err := t.run(sudoProbe, nil)
	if err == nil {
		a.settled = true
		return "", nil
	}
	if errors.Is(err, ErrUnreachable) {
		return "", fmt.Errorf("%s: asking whether sudo needs a password: %w\n%s", t.Describe(), err, out)
	}
	if !strings.Contains(out, "a password is required") {
		return "", a.refuse(fmt.Errorf("%s: sudo refused before asking for any password, so none was asked for:\n%s", t.Describe(), strings.TrimSpace(out)))
	}
	if a.Prompt == nil {
		return "", a.refuse(fmt.Errorf("%s: sudo asks for a password, and there is no terminal to ask on. paisans asks for a sudo password only on a terminal, so an unattended run needs sudo without one: a sudoers rule giving the ssh user NOPASSWD. That makes the ssh key alone enough for root on this host, which is a decision about the host rather than about one run", t.Describe()))
	}
	password, err := a.Prompt(t.Describe())
	if err != nil {
		return "", a.refuse(fmt.Errorf("%s: reading the sudo password: %w", t.Describe(), err))
	}
	if password == "" {
		return "", a.refuse(fmt.Errorf("%s: no sudo password was given", t.Describe()))
	}

	in := password + "\n"
	out, err = t.run(sudoWithPassword+" true", &in)
	if errors.Is(err, ErrUnreachable) {
		return "", fmt.Errorf("%s: checking the sudo password: %w\n%s", t.Describe(), err, out)
	}
	if err != nil {
		return "", a.refuse(fmt.Errorf("%s: sudo did not accept the password, and it is not tried again in this run, since repeated failures can lock the account:\n%s", t.Describe(), strings.TrimSpace(out)))
	}
	a.settled, a.password = true, password
	return password, nil
}

// ErrSudo marks a host where sudo cannot be used in this run: it refused the
// user, or wants a password nobody can give it here, or did not accept the
// one given. A caller tells it from a host that never answered with
// errors.Is.
var ErrSudo = errors.New("sudo cannot be used on this host")

type sudoRefused struct{ error }

func (e sudoRefused) Unwrap() error        { return e.error }
func (e sudoRefused) Is(target error) bool { return target == ErrSudo }

func (a *SudoAuth) refuse(err error) error {
	a.settled, a.err = true, sudoRefused{err}
	return a.err
}

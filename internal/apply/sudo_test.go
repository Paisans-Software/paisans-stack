package apply_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// sudoHost is a fake host whose sudo either needs no password (password
// empty) or wants the given one. It answers the probe, the check and every
// command after them, and records each remote command with its stdin.
type sudoHost struct {
	password string
	// refuse is what `sudo -n true` prints when sudo is not allowed at all.
	refuse string
	// hostname is what `uname -n` prints. Empty means box-01; "-" means
	// uname fails.
	hostname string
	commands []string
	stdins   []string
}

func (h *sudoHost) install(t *testing.T) {
	t.Helper()
	restore := apply.FakeSSH(func(args []string, stdin string) (string, int) {
		command := args[len(args)-1]
		h.commands = append(h.commands, command)
		h.stdins = append(h.stdins, stdin)
		switch {
		case command == "uname -n":
			switch h.hostname {
			case "":
				return "box-01\n", 0
			case "-":
				return "uname: not found\n", 127
			}
			return h.hostname + "\n", 0
		case command == "sudo -n true":
			if h.refuse != "" {
				return h.refuse, 1
			}
			if h.password != "" {
				return "sudo: a password is required\n", 1
			}
			return "", 0
		case strings.HasPrefix(command, "sudo -k -S -p '' "):
			line, _, _ := strings.Cut(stdin, "\n")
			if line != h.password {
				return "Sorry, try again.\nsudo: 1 incorrect password attempt\n", 1
			}
			if strings.Contains(command, "fail") {
				return "it failed\n", 1
			}
			return "ok\n", 0
		}
		return "ok\n", 0
	}, nil, func(_ time.Duration) {}, &strings.Builder{})
	t.Cleanup(restore)
}

type prompter struct {
	answer string
	asked  []string
}

func (p *prompter) prompt(who string) (string, error) {
	p.asked = append(p.asked, who)
	return p.answer, nil
}

func sudoTransport(auth *apply.SudoAuth) apply.SSHTransport {
	return apply.SSHTransport{Destination: "box", Sudo: true, Auth: auth}
}

// A host whose sudo needs no password is probed once and then used exactly
// as before: no prompt, and stdin reaches the command untouched.
func TestSudoWithoutAPasswordIsProbedOnceAndNeverPrompts(t *testing.T) {
	h := &sudoHost{}
	h.install(t)
	p := &prompter{answer: "unused"}
	tr := sudoTransport(apply.NewSudoAuth(p.prompt))

	if _, err := tr.Run("echo one"); err != nil {
		t.Fatal(err)
	}
	if err := tr.WriteFile("/srv/x", "content", 0o600); err != nil {
		t.Fatal(err)
	}
	if len(p.asked) != 0 {
		t.Errorf("prompted on a host that needs no password: %q", p.asked)
	}
	if h.commands[0] != "sudo -n true" {
		t.Errorf("first command %q, want the probe", h.commands[0])
	}
	probes := 0
	for _, c := range h.commands {
		if c == "sudo -n true" {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("probed %d times, want once: %q", probes, h.commands)
	}
	if want := "sudo sh -c 'echo one'"; h.commands[1] != want {
		t.Errorf("command %q, want %q", h.commands[1], want)
	}
	if h.stdins[2] != "content" {
		t.Errorf("WriteFile sent %q, want the content alone", h.stdins[2])
	}
}

// A host that wants a password is asked for it once, the password is checked
// before any real command runs, and every later command gets it as the first
// line of stdin with -k, so sudo always reads exactly that line and what
// follows reaches the command.
func TestSudoWithAPasswordPromptsOnceAndSendsItOnStdin(t *testing.T) {
	h := &sudoHost{password: "hunter2"}
	h.install(t)
	p := &prompter{answer: "hunter2"}
	tr := sudoTransport(apply.NewSudoAuth(p.prompt))

	if _, err := tr.Run("echo one"); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RunInput("cat", "input"); err != nil {
		t.Fatal(err)
	}
	if err := tr.WriteFile("/srv/x", "content", 0o600); err != nil {
		t.Fatal(err)
	}
	if len(p.asked) != 1 || p.asked[0] != "box, host box-01" {
		t.Fatalf("asked %q, want once for box, host box-01", p.asked)
	}
	if want := []string{"sudo -n true", "uname -n", "sudo -k -S -p '' true", "sudo -k -S -p '' sh -c 'echo one'", "sudo -k -S -p '' sh -c 'cat'"}; !equalPrefix(h.commands, want) {
		t.Fatalf("commands %q, want them to begin %q", h.commands, want)
	}
	if !strings.HasPrefix(h.commands[5], "sudo -k -S -p '' sh -c ") {
		t.Errorf("WriteFile ran %q, want it under sudo -k -S", h.commands[5])
	}
	want := []string{"", "", "hunter2\n", "hunter2\n", "hunter2\ninput", "hunter2\ncontent"}
	for i, s := range want {
		if h.stdins[i] != s {
			t.Errorf("call %d (%q) sent stdin %q, want %q", i, h.commands[i], h.stdins[i], s)
		}
	}
	for _, c := range h.commands {
		if strings.Contains(c, "hunter2") {
			t.Errorf("the password is on a command line: %q", c)
		}
	}
}

// Copies of a transport share one SudoAuth, so a site is asked once however
// many times its transport is copied.
func TestCopiesOfATransportShareOnePrompt(t *testing.T) {
	h := &sudoHost{password: "hunter2"}
	h.install(t)
	p := &prompter{answer: "hunter2"}
	auth := apply.NewSudoAuth(p.prompt)
	for i := 0; i < 3; i++ {
		if _, err := sudoTransport(auth).Run("true"); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.asked) != 1 {
		t.Errorf("asked %d times, want once", len(p.asked))
	}
}

// Without a terminal there is nobody to ask, so a host that wants a password
// is refused before any command runs, and the refusal says what an unattended
// run needs and what that costs.
func TestSudoWithAPasswordAndNoTerminalIsRefused(t *testing.T) {
	h := &sudoHost{password: "hunter2"}
	h.install(t)
	tr := sudoTransport(apply.NewSudoAuth(nil))

	_, err := tr.Run("echo one")
	if err == nil {
		t.Fatal("ran with no way to give sudo its password")
	}
	for _, want := range []string{"NOPASSWD", "terminal", "root"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	if len(h.commands) != 1 {
		t.Errorf("ran %q after the probe, want nothing", h.commands[1:])
	}
}

// A wrong password is reported once and never tried again: not by this
// command, and not by any later one, since repeated failures can lock the
// account.
func TestAWrongPasswordIsNeverRetried(t *testing.T) {
	h := &sudoHost{password: "hunter2"}
	h.install(t)
	p := &prompter{answer: "wrong"}
	tr := sudoTransport(apply.NewSudoAuth(p.prompt))

	_, first := tr.Run("echo one")
	if first == nil || !strings.Contains(first.Error(), "password") {
		t.Fatalf("want a refusal about the password, got %v", first)
	}
	_, second := tr.Run("echo two")
	if second == nil {
		t.Fatal("a later command ran after the password was refused")
	}
	if len(p.asked) != 1 {
		t.Errorf("asked %d times, want once", len(p.asked))
	}
	if len(h.commands) != 3 {
		t.Errorf("ran %q, want only the probe, the hostname and one check", h.commands)
	}
	if strings.Contains(first.Error(), "wrong") {
		t.Errorf("the refusal carries the password: %v", first)
	}
}

// A user sudo will not serve at all is reported as such, with sudo's own
// words, and nobody is asked for a password that could not help.
func TestSudoThatRefusesTheUserIsReportedWithoutAPrompt(t *testing.T) {
	h := &sudoHost{refuse: "ubuntu is not in the sudoers file.\n"}
	h.install(t)
	p := &prompter{answer: "x"}
	_, err := sudoTransport(apply.NewSudoAuth(p.prompt)).Run("true")
	if err == nil || !strings.Contains(err.Error(), "not in the sudoers file") {
		t.Fatalf("want sudo's refusal, got %v", err)
	}
	if len(p.asked) != 0 {
		t.Errorf("prompted for a password sudo would not accept: %q", p.asked)
	}
}

// A probe that never reached the host is not an answer, so nothing is
// concluded from it and the next command asks again.
func TestAnUnreachableProbeConcludesNothing(t *testing.T) {
	s := &sshScript{answers: []sshAnswer{{sshTimeout, 255}, {sshTimeout, 255}, {sshTimeout, 255}, {"", 0}}}
	s.install(t)
	p := &prompter{answer: "x"}
	tr := sudoTransport(apply.NewSudoAuth(p.prompt))
	if _, err := tr.Run("true"); !errors.Is(err, apply.ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
	if _, err := tr.Run("true"); err != nil {
		t.Fatalf("a reachable host was still treated as unreachable: %v", err)
	}
	if len(p.asked) != 0 {
		t.Errorf("prompted after an unreachable probe: %q", p.asked)
	}
}

// A command that fails under a password still reports its own failure, and
// the error never carries the password.
func TestAFailedCommandDoesNotCarryThePassword(t *testing.T) {
	h := &sudoHost{password: "hunter2"}
	h.install(t)
	p := &prompter{answer: "hunter2"}
	_, err := sudoTransport(apply.NewSudoAuth(p.prompt)).Run("fail")
	if err == nil {
		t.Fatal("a failed command reported success")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error carries the password: %v", err)
	}
}

func equalPrefix(got, want []string) bool {
	if len(got) < len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The prompt names the site from paisans.yaml and the name the host gives
// itself beside the address, so an operator who does not recognise an
// address can still tell which site and which machine is asking.
func TestThePromptNamesTheSiteAndTheHost(t *testing.T) {
	h := &sudoHost{password: "hunter2", hostname: "home-a-01"}
	h.install(t)
	p := &prompter{answer: "hunter2"}
	tr := apply.SSHTransport{Site: "home-a", Destination: "ubuntu@192.0.2.10", Sudo: true, Auth: apply.NewSudoAuth(p.prompt)}

	if _, err := tr.Run("true"); err != nil {
		t.Fatal(err)
	}
	if want := "site home-a (ubuntu@192.0.2.10, host home-a-01)"; len(p.asked) != 1 || p.asked[0] != want {
		t.Fatalf("asked %q, want %q", p.asked, want)
	}
}

// A hostname the host does not give, or gives with anything a hostname does
// not contain, is left out of the prompt: the prompt is printed on the
// operator's terminal, and a host must not write escapes into it.
func TestThePromptLeavesOutAHostnameItCannotTrust(t *testing.T) {
	for _, hostname := range []string{"-", "evil\x1b]0;owned\x07", "two words"} {
		h := &sudoHost{password: "hunter2", hostname: hostname}
		h.install(t)
		p := &prompter{answer: "hunter2"}
		tr := apply.SSHTransport{Site: "home-a", Destination: "box", Sudo: true, Auth: apply.NewSudoAuth(p.prompt)}

		if _, err := tr.Run("true"); err != nil {
			t.Fatal(err)
		}
		if want := "site home-a (box)"; len(p.asked) != 1 || p.asked[0] != want {
			t.Errorf("hostname %q: asked %q, want %q", hostname, p.asked, want)
		}
	}
}

// Each way sudo cannot be used is a Problem that names the host and what to
// do, and is still ErrSudo to a caller that asks.
func TestSudoRefusalsAreProblems(t *testing.T) {
	for _, c := range []struct {
		name    string
		host    *sudoHost
		prompt  func(string) (string, error)
		hint    string
		explain string
	}{
		{"no terminal", &sudoHost{password: "hunter2"}, nil, "sudo on box needs a password, and there is no terminal to ask on", "NOPASSWD"},
		{"wrong password", &sudoHost{password: "hunter2"}, (&prompter{answer: "wrong"}).prompt, "sudo on box did not accept the password", "not tried again"},
		{"no password", &sudoHost{password: "hunter2"}, (&prompter{answer: ""}).prompt, "no sudo password was given for box", "Run again"},
		{"not a sudoer", &sudoHost{refuse: "ubuntu is not in the sudoers file.\n"}, (&prompter{answer: "x"}).prompt, "sudo on box refused the ssh user", "not in the sudoers file"},
	} {
		c.host.install(t)
		_, err := sudoTransport(apply.NewSudoAuth(c.prompt)).Run("true")
		if !errors.Is(err, apply.ErrSudo) {
			t.Errorf("%s: not ErrSudo: %v", c.name, err)
		}
		p := problemOf(t, err)
		if p.Hint != c.hint || !strings.Contains(p.Explain, c.explain) {
			t.Errorf("%s: got %q\n%q", c.name, p.Hint, p.Explain)
		}
		if strings.Contains(err.Error(), "hunter2") || c.name == "wrong password" && strings.Contains(err.Error(), "wrong") {
			t.Errorf("%s: the error carries a password: %v", c.name, err)
		}
	}
}

package apply_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// Obviously fake: an ed25519 key whose 32 bytes are all zero, and all one.
const (
	keyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org"
	keyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB bob@example.org"
)

// The argument list is the whole of what the transport decides about a
// connection, so it is pinned exactly.
func TestSSHArgsForASection(t *testing.T) {
	tr := apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10", Port: 2222, PublicKeys: []string{keyA, keyB}}
	got := tr.SSHArgs([]string{"/tmp/k/key-1.pub", "/tmp/k/key-2.pub"}, "uptime")
	want := []string{"-p", "2222", "-o", "IdentitiesOnly=yes", "-i", "/tmp/k/key-1.pub", "-i", "/tmp/k/key-2.pub", "ubuntu@203.0.113.10", apply.RemoteCommand("uptime")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ssh args:\n got %q\nwant %q", got, want)
	}
	if d := tr.Describe(); d != "ubuntu@203.0.113.10 port 2222" {
		t.Errorf("Describe() = %q", d)
	}
}

// A connect timeout is one more option before the keys, and only when set.
func TestSSHArgsConnectTimeout(t *testing.T) {
	tr := apply.SSHTransport{User: "ubuntu", Host: "vm.example.org", PublicKeys: []string{keyA}, ConnectTimeout: 10}
	got := tr.SSHArgs([]string{"k1"}, "true")
	want := []string{"-p", "22", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=10", "-i", "k1", "ubuntu@vm.example.org", apply.RemoteCommand("true")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ssh args:\n got %q\nwant %q", got, want)
	}
}

func TestSSHArgsDefaultToPort22(t *testing.T) {
	tr := apply.SSHTransport{User: "ubuntu", Host: "vm.example.org", PublicKeys: []string{keyA}}
	got := tr.SSHArgs([]string{"k1"}, "true")
	if got[0] != "-p" || got[1] != "22" {
		t.Fatalf("an undeclared port is not 22: %q", got)
	}
	if d := tr.Describe(); d != "ubuntu@vm.example.org" {
		t.Errorf("Describe() = %q, want no port when it is 22", d)
	}
}

// --ssh is an escape hatch: given verbatim, with nothing of the section added.
func TestSSHArgsDestinationOverrideIsVerbatim(t *testing.T) {
	tr := apply.SSHTransport{Destination: "jump-alias", User: "ubuntu", Host: "203.0.113.10", Port: 2222, PublicKeys: []string{keyA}}
	if got, want := tr.SSHArgs(nil, "true"), []string{"jump-alias", apply.RemoteCommand("true")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("override args: got %q, want %q", got, want)
	}
	args, cleanup, err := apply.SSHCommand(tr, "true")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if want := []string{"ssh", "jump-alias", apply.RemoteCommand("true")}; !reflect.DeepEqual(args, want) {
		t.Fatalf("override command: got %q, want %q", args, want)
	}
}

// The key files exist, private, for as long as the command needs them, and
// not after.
func TestSSHCommandWritesPrivateKeyFilesAndRemovesThem(t *testing.T) {
	tr := apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10", PublicKeys: []string{keyA, keyB}}
	args, cleanup, err := apply.SSHCommand(tr, "true")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for i, a := range args {
		if a == "-i" {
			files = append(files, args[i+1])
		}
	}
	if len(files) != 2 {
		t.Fatalf("want one -i per key, got %q", args)
	}
	dir := filepath.Dir(files[0])
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("key directory mode %o, want 700", info.Mode().Perm())
	}
	for i, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o, want 600", f, info.Mode().Perm())
		}
		body, _ := os.ReadFile(f)
		if want := []string{keyA, keyB}[i] + "\n"; string(body) != want {
			t.Errorf("%s holds %q, want %q", f, body, want)
		}
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("key directory survives cleanup: %v", err)
	}
}

func TestSSHCommandRefusesASectionWithoutKeys(t *testing.T) {
	_, _, err := apply.SSHCommand(apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10"}, "true")
	if err == nil || !strings.Contains(err.Error(), "no public key") {
		t.Fatalf("want a refusal naming the missing key, got %v", err)
	}
}

// sshScript is a fake ssh that answers each call in turn, and records what it
// was given.
type sshScript struct {
	answers []sshAnswer
	calls   int
	stdins  []string
	slept   []time.Duration
	log     bytes.Buffer
}

type sshAnswer struct {
	out  string
	exit int
}

func (s *sshScript) install(t *testing.T) {
	t.Helper()
	restore := apply.FakeSSH(func(args []string, stdin string) (string, int) {
		s.stdins = append(s.stdins, stdin)
		a := s.answers[len(s.answers)-1]
		if s.calls < len(s.answers) {
			a = s.answers[s.calls]
		}
		s.calls++
		return a.out, a.exit
	}, []time.Duration{2 * time.Second, 4 * time.Second}, func(d time.Duration) { s.slept = append(s.slept, d) }, &s.log)
	t.Cleanup(restore)
}

const sshTimeout = "ssh: connect to host 203.0.113.10 port 22: Operation timed out\n"

// A connection that never opened is tried again, at most three times in all,
// two and then four seconds apart, with one line naming the destination for
// each retry. Every entry point retries: Run, RunInput, ReadFile, WriteFile.
func TestAConnectionThatNeverOpenedIsRetried(t *testing.T) {
	tr := apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10", PublicKeys: []string{keyA}}
	for name, call := range map[string]func() error{
		"Run":      func() error { _, err := tr.Run("true"); return err },
		"RunInput": func() error { _, err := tr.RunInput("cat", "in"); return err },
		"ReadFile": func() error { _, _, err := tr.ReadFile("/srv/x"); return err },
		"WriteFile": func() error {
			return tr.WriteFile("/srv/x", "content", 0o600)
		},
	} {
		s := &sshScript{answers: []sshAnswer{{sshTimeout, 255}, {"kex_exchange_identification: Connection closed by remote host\n", 255}, {"ok\n", 0}}}
		s.install(t)
		if err := call(); err != nil {
			t.Errorf("%s: a connection that opened on the third attempt still failed: %v", name, err)
		}
		if s.calls != 3 {
			t.Errorf("%s: ssh ran %d times, want 3", name, s.calls)
		}
		if want := []time.Duration{2 * time.Second, 4 * time.Second}; !reflect.DeepEqual(s.slept, want) {
			t.Errorf("%s: waited %v, want %v", name, s.slept, want)
		}
		if lines := strings.Count(s.log.String(), "\n"); lines != 2 || !strings.Contains(s.log.String(), "ubuntu@203.0.113.10") {
			t.Errorf("%s: want one line per retry naming the destination, got %q", name, s.log.String())
		}
		if name == "WriteFile" || name == "RunInput" {
			for _, in := range s.stdins {
				if in == "" {
					t.Errorf("%s: a retry sent no stdin: %q", name, s.stdins)
				}
			}
		}
	}
}

// Three attempts, then the failure is reported as unreachable, which a caller
// can tell from an answer.
func TestAHostThatStaysUnreachableIsReportedAsSuch(t *testing.T) {
	s := &sshScript{answers: []sshAnswer{{sshTimeout, 255}}}
	s.install(t)
	tr := apply.SSHTransport{Destination: "vm"}
	_, err := tr.Run("true")
	if !errors.Is(err, apply.ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
	if s.calls != 3 {
		t.Errorf("ssh ran %d times, want 3", s.calls)
	}
}

// A command that ran and failed is the host's answer, and running it again
// could repeat what it did. Exit 255 without a connection error is a remote
// command's own status; a connection error in the output with another status
// is something the command printed; and a session that had opened before it
// dropped may have run the command.
func TestOnlyAConnectionFailureIsRetried(t *testing.T) {
	for _, a := range []sshAnswer{
		{"mv: cannot move\n", 1},
		{"the remote command exited 255 itself\n", 255},
		{"curl: (7) Failed to connect: Connection refused\n", 7},
		{"partial output\nclient_loop: send disconnect: Connection reset by peer\n", 255},
		{"Connection to 203.0.113.10 closed by remote host.\n", 255},
	} {
		s := &sshScript{answers: []sshAnswer{a}}
		s.install(t)
		_, err := apply.SSHTransport{Destination: "vm"}.Run("true")
		if err == nil {
			t.Fatalf("%q: a failure was reported as success", a.out)
		}
		if s.calls != 1 {
			t.Errorf("%q (exit %d) was retried: ssh ran %d times", a.out, a.exit, s.calls)
		}
		if errors.Is(err, apply.ErrUnreachable) {
			t.Errorf("%q (exit %d) was called unreachable", a.out, a.exit)
		}
	}
}

// ssh asks on the terminal itself on a first connection: a host key to
// accept, or a key's passphrase. The progress display is held around every
// attempt until the host has answered once, so the question stays readable,
// and is not held again for that host, so the spinner runs for the rest.
func TestTheFirstConnectionToAHostHoldsTheDisplay(t *testing.T) {
	apply.ForgetContacts()
	t.Cleanup(apply.ForgetContacts)
	held := false
	apply.SetPromptHold(func() func() {
		held = true
		return func() { held = false }
	})
	t.Cleanup(func() { apply.SetPromptHold(nil) })
	var during []bool
	answers := []sshAnswer{{sshTimeout, 255}, {"ok\n", 0}, {"ok\n", 0}, {"ok\n", 0}}
	calls := 0
	restore := apply.FakeSSH(func([]string, string) (string, int) {
		during = append(during, held)
		a := answers[calls]
		calls++
		return a.out, a.exit
	}, []time.Duration{time.Second}, func(time.Duration) {}, io.Discard)
	t.Cleanup(restore)

	a := apply.SSHTransport{Destination: "home-a"}
	if _, err := a.Run("true"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run("true"); err != nil {
		t.Fatal(err)
	}
	if _, err := (apply.SSHTransport{Destination: "home-b"}).Run("true"); err != nil {
		t.Fatal(err)
	}
	if want := []bool{true, true, false, true}; !reflect.DeepEqual(during, want) {
		t.Errorf("held during each ssh: %v, want %v (retried first contact, later command, another host's first)", during, want)
	}
	if held {
		t.Error("the display was left held")
	}
}

// ssh's question about a host key it does not know is followed by a blank
// line, so the next question or the report starts apart from the answer. A
// host whose key is known asks nothing and gets no blank line.
func TestAHostKeyQuestionIsFollowedByABlankLine(t *testing.T) {
	apply.ForgetContacts()
	t.Cleanup(apply.ForgetContacts)
	blanks := 0
	t.Cleanup(apply.FakeHostKeys(func(t apply.SSHTransport) bool { return t.Destination == "known" }, &blanks))
	t.Cleanup(apply.FakeSSH(func([]string, string) (string, int) { return "ok\n", 0 }, nil, func(time.Duration) {}, io.Discard))

	for _, d := range []string{"new", "new", "known"} {
		if _, err := (apply.SSHTransport{Destination: d}).Run("true"); err != nil {
			t.Fatal(err)
		}
	}
	if blanks != 1 {
		t.Errorf("%d blank lines, want 1: after the new host's first connection only", blanks)
	}
}

// A failed command's error ends with ssh's last line, not a newline, so a
// caller that adds a sentence after it keeps it on the same line.
func TestARunErrorEndsWithoutANewline(t *testing.T) {
	apply.ForgetContacts()
	t.Cleanup(apply.ForgetContacts)
	t.Cleanup(apply.FakeSSH(func([]string, string) (string, int) { return "Host key verification failed.\n", 1 }, nil, func(time.Duration) {}, io.Discard))
	_, err := (apply.SSHTransport{Destination: "home-a"}).Run("true")
	if err == nil || strings.HasSuffix(err.Error(), "\n") {
		t.Errorf("err = %q", err)
	}
}

func problemOf(t *testing.T, err error) *ui.Problem {
	t.Helper()
	var p *ui.Problem
	if !errors.As(err, &p) {
		t.Fatalf("not a ui.Problem: %v", err)
	}
	return p
}

// A host that stays unreachable is said as such, with ssh's own reason, and
// is still ErrUnreachable to a caller that asks.
func TestUnreachableIsAProblem(t *testing.T) {
	s := &sshScript{answers: []sshAnswer{{sshTimeout, 255}}}
	s.install(t)
	_, err := apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10", PublicKeys: []string{keyA}}.Run("true")
	if !errors.Is(err, apply.ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
	p := problemOf(t, err)
	if p.Hint != "ubuntu@203.0.113.10 cannot be reached over ssh" {
		t.Errorf("hint: %q", p.Hint)
	}
	for _, want := range []string{"Operation timed out", "3 attempts", "is up"} {
		if !strings.Contains(p.Explain, want) {
			t.Errorf("the explanation lacks %q: %q", want, p.Explain)
		}
	}
}

// ssh refusing the host's key, not knowing it, or the host refusing the
// operator's key each say what to do, and none is retried or called
// unreachable: the host answered.
func TestSSHAuthenticationFailuresAreProblems(t *testing.T) {
	changed := "@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\n@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\nHost key verification failed.\n"
	for _, c := range []struct {
		out, hint, explain string
	}{
		{changed, "ubuntu@203.0.113.10 port 2222 offered a host key other than the one known", "ssh-keygen -R '[203.0.113.10]:2222'"},
		{"Host key verification failed.\n", "ubuntu@203.0.113.10 port 2222's host key is not known", "ssh -p 2222 ubuntu@203.0.113.10"},
		{"ubuntu@203.0.113.10: Permission denied (publickey).\n", "ubuntu@203.0.113.10 port 2222 did not accept your ssh key", "authorized_keys"},
	} {
		apply.ForgetContacts()
		s := &sshScript{answers: []sshAnswer{{c.out, 255}}}
		s.install(t)
		_, err := apply.SSHTransport{User: "ubuntu", Host: "203.0.113.10", Port: 2222, PublicKeys: []string{keyA}}.Run("true")
		if s.calls != 1 || errors.Is(err, apply.ErrUnreachable) {
			t.Errorf("%q: retried or called unreachable: %d calls, %v", c.out, s.calls, err)
		}
		p := problemOf(t, err)
		if p.Hint != c.hint || !strings.Contains(p.Explain, c.explain) {
			t.Errorf("%q: got %q\n%q", c.out, p.Hint, p.Explain)
		}
	}
	apply.ForgetContacts()
}

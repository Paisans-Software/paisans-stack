package apply_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
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
	want := []string{"-p", "2222", "-o", "IdentitiesOnly=yes", "-i", "/tmp/k/key-1.pub", "-i", "/tmp/k/key-2.pub", "ubuntu@203.0.113.10", "uptime"}
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
	want := []string{"-p", "22", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=10", "-i", "k1", "ubuntu@vm.example.org", "true"}
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
	if got, want := tr.SSHArgs(nil, "true"), []string{"jump-alias", "true"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("override args: got %q, want %q", got, want)
	}
	args, cleanup, err := apply.SSHCommand(tr, "true")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if want := []string{"ssh", "jump-alias", "true"}; !reflect.DeepEqual(args, want) {
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

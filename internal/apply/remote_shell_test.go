package apply_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// What every login shell parses: nothing inside the single quotes that any
// shell's quoting rules could read differently, and the command recoverable
// exactly.
func TestRemoteCommandQuotesNothing(t *testing.T) {
	command := `sudo -k -S -p '' sh -c 'printf '\''%s\n'\'' "$HOME"; exit 3'`
	remote := apply.RemoteCommand(command)
	body, ok := strings.CutPrefix(remote, "sh -c '")
	if !ok || !strings.HasSuffix(body, "'") {
		t.Fatalf("not one single quoted word after sh -c: %s", remote)
	}
	body = strings.TrimSuffix(body, "'")
	if strings.ContainsAny(body, `'\`) {
		t.Errorf("the quoted word holds a quote or a backslash: %s", body)
	}
	if got, ok := apply.InnerCommand(remote); !ok || got != command {
		t.Errorf("decoded %q, want %q", got, command)
	}
}

// quote is shellQuote, for building a command the way the transport does.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sshd runs the string through the login shell as `$SHELL -c`. Whichever
// shell that is, the command runs in sh, with its output, its exit status
// and stdin passed through. Each shell installed here is tried. The command
// is quoted twice over, as sudo's wrapper around a command with its own
// quoted paths is, which fish misreads when it is sent bare.
func TestRemoteCommandRunsInShUnderAnyLoginShell(t *testing.T) {
	command := "sh -c " + quote("sh -c "+quote(`printf '%s\n' "it's"; cat; exit 3`))
	want := "it's\nfrom stdin\n"
	run := func(shell, command string) (string, error) {
		cmd := exec.Command(shell, "-c", command)
		cmd.Stdin = strings.NewReader("from stdin\n")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if fish, err := exec.LookPath("fish"); err == nil {
		if out, _ := run(fish, command); out == want {
			t.Fatal("fish ran the bare command correctly, so this test shows nothing")
		}
	}
	tried := 0
	for _, shell := range []string{"sh", "bash", "dash", "zsh", "fish", "csh", "tcsh"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		tried++
		out, err := run(path, apply.RemoteCommand(command))
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3 {
			t.Errorf("%s: exit %v, want status 3\n%s", shell, err, out)
		}
		if out != want {
			t.Errorf("%s: output %q, want %q", shell, out, want)
		}
	}
	if tried == 0 {
		t.Skip("no shell found")
	}
}

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/storageadd"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// withoutAgeKey leaves sops nothing to find a key in: no variable naming
// one, and a home and config directory with none in them.
func withoutAgeKey(t *testing.T) {
	t.Helper()
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("XDG_CONFIG_HOME", empty)
	for _, name := range []string{"SOPS_AGE_KEY", "SOPS_AGE_KEY_FILE", "SOPS_AGE_KEY_CMD", "SOPS_AGE_SSH_PRIVATE_KEY_FILE", "SOPS_AGE_SSH_PRIVATE_KEY_CMD"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// encryptedSecrets writes a secrets file encrypted to a key nobody holds, at
// dir/staging/secrets.enc.yaml.
func encryptedSecrets(t *testing.T, dir string) string {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "staging", "secrets.enc.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteSecrets(path, &config.Secrets{Version: 1}, []string{identity.Recipient().String()}); err != nil {
		t.Fatal(err)
	}
	return path
}

// The secrets file no key here opens, end to end: render loads it as apply
// does, and main's printer says what is wrong and what to do in a few short
// lines, with sops' own words and the full path only behind -v.
func TestSecretsErrorThroughMainsPrinter(t *testing.T) {
	dir := t.TempDir()
	path := encryptedSecrets(t, dir)
	withoutAgeKey(t)
	cfg, err := filepath.Abs(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	err = runRender([]string{
		"--config", cfg,
		"--secrets", path,
		"--out", t.TempDir(),
	})
	if err == nil {
		t.Fatal("a file no key opens was decrypted")
	}

	var b strings.Builder
	if code := reportError(&b, err, false); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	out := b.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[0] != "FAIL staging/secrets.enc.yaml cannot be decrypted: no usable age key was found" {
		t.Errorf("hint line: %q", lines[0])
	}
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{"Set SOPS_AGE_KEY_CMD to a command that prints your key", "never stored in a file or the environment", "SOPS_AGE_KEY_FILE", "SOPS_AGE_KEY (the key itself)", "sops/age/keys.txt", "recipient in .sops.yaml", "sops updatekeys"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the explanation lacks %q:\n%s", want, out)
		}
	}
	for _, hidden := range []string{"successful groups", dir, "paisans:"} {
		if strings.Contains(out, hidden) {
			t.Errorf("default output shows %q:\n%s", hidden, out)
		}
	}
	for _, l := range lines {
		if len(l) > 80 {
			t.Errorf("a line is wider than 80: %q", l)
		}
		if l != lines[0] && !strings.HasPrefix(l, "  ") {
			t.Errorf("an explanation line is not indented: %q", l)
		}
	}

	b.Reset()
	reportError(&b, err, true)
	for _, want := range []string{"Error getting data key", path} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("-v lacks %q:\n%s", want, b.String())
		}
	}
}

// A wait on Garage exits 75 however it is wrapped: by itself, as apply
// without --site reports it, or under another Problem.
func TestAGarageWaitStillExits75(t *testing.T) {
	wait := &storageadd.Waiting{Stage: &storageadd.Stage{Number: 3, Name: "sync"}, Detail: "home-b is still resyncing 120 items"}
	for _, err := range []error{
		wait,
		fmt.Errorf("storage add: %w", wait),
		&ui.Problem{Hint: "apply is waiting at storage add", Explain: "Run it again later.", Cause: fmt.Errorf("x: %w", wait)},
	} {
		var b strings.Builder
		if code := reportError(&b, err, false); code != 75 {
			t.Errorf("%v: exit code %d, want 75", err, code)
		}
		if !strings.HasPrefix(b.String(), "FAIL ") {
			t.Errorf("not in the error form: %q", b.String())
		}
	}
	var b strings.Builder
	if code := reportError(&b, errors.New("boom"), false); code != 1 {
		t.Errorf("an ordinary error exits %d, want 1", code)
	}
}

// A command line paisans cannot run says so in the error form, then the
// usage, and exits 2.
func TestUsageErrorIsInTheErrorForm(t *testing.T) {
	var b strings.Builder
	if code := usageError(&b, "host takes one subcommand, prepare or deployments"); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.HasPrefix(b.String(), "FAIL host takes one subcommand, prepare or deployments\n\n"+usage[:20]) {
		t.Errorf("got:\n%s", b.String()[:200])
	}
}

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/garage"
)

// garage-key-is-malformed has to stop `render`, not only `init`: `render` and
// `apply` both call config.LoadSecrets directly and never call
// secretsgen.Fill, so a key hand edited into the secrets file after the last
// `init` would otherwise reach the rendered artifacts, and from there a host,
// with nothing ever having looked at it. This test proves the wiring, not
// just the check: it drives runRender exactly as the CLI would, over a
// fixture where the configuration is otherwise fine and only the secret is
// broken.
func TestRunRenderRefusesAMalformedGarageKey(t *testing.T) {
	out := t.TempDir()
	err := runRender([]string{
		"--config", filepath.Join("..", "..", "internal", "render", "testdata", "deployment.yaml"),
		"--secrets", filepath.Join("testdata", "garage-key-is-malformed-secrets.yaml"),
		"--out", out,
	})
	if err == nil {
		t.Fatal("expected render to refuse a configuration with a malformed Garage key")
	}
	if !strings.Contains(err.Error(), "garage-key-is-malformed") {
		t.Fatalf("the error should name the rule garage-key-is-malformed, got: %v", err)
	}
	if !strings.Contains(err.Error(), "docs.s3_access_key_id") {
		t.Fatalf("the error should name the offending field, got: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("render wrote %d file(s) to --out before refusing the key", len(entries))
	}
}

// storage init is the path CheckGarageKeys exists for: a malformed key that
// reaches `garage key import` fails partway through provisioning, after
// earlier keys in the same run are already imported and cannot be imported
// again. This proves the check runs before anything reaches a transport, by
// using an ssh destination that cannot be dialled and confirming the error
// still names the malformed key rather than a connection failure.
func TestRunStorageInitRefusesAMalformedGarageKeyBeforeReachingAHost(t *testing.T) {
	err := runStorageInit([]string{
		"--config", filepath.Join("..", "..", "internal", "render", "testdata", "deployment.yaml"),
		"--secrets", filepath.Join("testdata", "garage-key-is-malformed-secrets.yaml"),
		"--site", "home-a",
		"--ssh", "nonexistent.invalid",
	})
	if err == nil {
		t.Fatal("expected storage init to refuse a configuration with a malformed Garage key")
	}
	if !strings.Contains(err.Error(), "garage-key-is-malformed") {
		t.Fatalf("the error should name the rule garage-key-is-malformed, got: %v", err)
	}
	if strings.Contains(err.Error(), "nonexistent.invalid") {
		t.Fatalf("the check should stop before ssh is ever tried, got: %v", err)
	}
}

// printGaragePlan is the one thing that puts a provisioning plan on the
// operator's terminal, and a key import step's command carries that app's S3
// secret as a positional argument because `garage key import` takes no other
// form. Printing Step.Command would put the secret in a scrollback buffer and
// a log every time an operator ran a dry run, which is the ordinary case
// rather than a failure. It prints Describe, and this test is what keeps it
// that way: a future field added to the plan's output has to be checked here
// before it reaches a screen.
func TestPrintGaragePlanPrintsNoSecret(t *testing.T) {
	const secret = "e1d9a0a2e1d9a0a2e1d9a0a2e1d9a0a2e1d9a0a2e1d9a0a2e1d9a0a2e1d9a0a2"
	plan := &garage.Plan{
		Site: "home-a",
		Steps: []garage.Step{{
			Describe: "import the S3 key for talk",
			Command:  "docker compose -f /srv/paisans/f2a9/infra/compose.yaml exec -T garage /garage key import GKfacadefacadefacadefacade " + secret + " --yes -n talk",
			Secret:   secret,
		}},
		Present: []string{"bucket: talk-uploads already exists"},
	}

	printed := captureStdout(t, func() { printGaragePlan(plan) })

	if strings.Contains(printed, secret) {
		t.Errorf("printGaragePlan put an S3 secret on the terminal:\n%s", printed)
	}
	if !strings.Contains(printed, "import the S3 key for talk") {
		t.Errorf("printGaragePlan must still say what the step does:\n%s", printed)
	}
	if !strings.Contains(printed, "bucket: talk-uploads already exists") {
		t.Errorf("printGaragePlan must still report what was already there:\n%s", printed)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// was written, restoring os.Stdout afterwards. printGaragePlan writes to
// os.Stdout directly, so this is the only way to see what an operator sees.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	w.Close()
	return <-done
}

// dns init has to refuse offline wherever it can: a configuration that cannot
// name its records, or a provider whose record management does not exist,
// must stop before the secrets are used to contact anything. The fixture
// declares desec and gives the gateway no public_address.
func TestRunDNSInitRefusesBeforeContactingAProvider(t *testing.T) {
	fixture := filepath.Join("..", "..", "internal", "render", "testdata", "deployment.yaml")
	secrets := filepath.Join("..", "..", "internal", "render", "testdata", "secrets.fixture.yaml")

	err := runDNSInit([]string{"--config", fixture, "--secrets", secrets})
	if err == nil || !strings.Contains(err.Error(), "sites.vm.public_address is not set") {
		t.Fatalf("want a refusal naming the missing public_address, got %v", err)
	}

	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	withAddress := strings.Replace(string(data), "    endpoint: vm.example.org:51820\n",
		"    endpoint: vm.example.org:51820\n    public_address: 203.0.113.10\n", 1)
	if withAddress == string(data) {
		t.Fatal("the fixture no longer has the line this test edits")
	}
	dir := t.TempDir()
	edited := filepath.Join(dir, "paisans.yaml")
	if err := os.WriteFile(edited, []byte(withAddress), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runDNSInit([]string{"--config", edited, "--secrets", secrets})
	if err == nil || !strings.Contains(err.Error(), "desec record management is not implemented yet") {
		t.Fatalf("want the unimplemented provider refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "fixture-not-a-secret-desec") {
		t.Fatalf("the token leaked into an error: %v", err)
	}

	// dns prune shares the same setup, so it refuses in the same places.
	err = runDNSPrune([]string{"--config", fixture, "--secrets", secrets})
	if err == nil || !strings.Contains(err.Error(), "sites.vm.public_address is not set") {
		t.Fatalf("prune: want a refusal naming the missing public_address, got %v", err)
	}
	err = runDNSPrune([]string{"--config", edited, "--secrets", secrets})
	if err == nil || !strings.Contains(err.Error(), "desec record management is not implemented yet") {
		t.Fatalf("prune: want the unimplemented provider refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "fixture-not-a-secret-desec") {
		t.Fatalf("prune: the token leaked into an error: %v", err)
	}
}

//go:build garage_integration

// This file drives a real dxflrs/garage:v1.0.1 container through the whole
// provisioning sequence Build and Execute plan. Everything provision.go
// assumes about Garage, the key and secret formats, the forced ordering
// between a layout and a key, and which commands are idempotent, was learned
// by running that image once by hand. Nothing here is learned from Garage's
// documentation, and this file is what keeps that knowledge honest: a future
// Garage release that changes any of it fails this test rather than failing
// silently on an operator's host.
package garage

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
)

const garageImage = "dxflrs/garage:v1.0.1"

// dockerTransport satisfies Transport by execing straight into a container
// this test started, rather than through the `docker compose ... exec`
// form garageCmd hard codes for a deployed host. Build's commands all carry
// that compose prefix; Run strips it off and replaces it with a plain
// `docker exec <container> /garage`, which is the part of the command that
// actually matters here. That is also why the command prefix this test cares
// about is `/garage`, not the compose form: once the prefix is stripped, that
// is what is left.
type dockerTransport struct {
	container string
}

func (d dockerTransport) Describe() string { return "container " + d.container }

func (d dockerTransport) Run(command string) (string, error) {
	rest := strings.TrimPrefix(command, garageCmd)
	if rest == command {
		return "", fmt.Errorf("command %q does not start with the expected prefix %q", command, garageCmd)
	}
	args := append([]string{"exec", d.container, "/garage"}, strings.Fields(rest)...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// hexString returns n random lowercase hex characters, used for both the RPC
// secret Garage's own config needs and the S3 secrets this test hands it.
func hexString(t *testing.T, n int) string {
	t.Helper()
	if n%2 != 0 {
		t.Fatalf("hexString: %d is not even", n)
	}
	buf := make([]byte, n/2)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generating %d random hex characters: %v", n, err)
	}
	return hex.EncodeToString(buf)
}

// garageKeyID returns an S3 access key ID in the shape Garage accepts: the
// literal "GK" followed by 24 lowercase hex characters. Established by
// running dxflrs/garage:v1.0.1; see internal/secretsgen for where the
// toolkit's own generator produces the same shape.
func garageKeyID(t *testing.T) string {
	t.Helper()
	return "GK" + hexString(t, 24)
}

// requireDocker skips the test with a clear message when docker is not on
// PATH, so the integration suite degrades gracefully on a workstation that
// does not have it, rather than failing in a way that looks like a real bug.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH; skipping the Garage integration test")
	}
}

// startGarage starts dxflrs/garage:v1.0.1 as a single node, replication
// factor 1, configured entirely from a garage.toml written to t.TempDir. It
// waits for `garage status` to answer rather than sleeping a fixed time,
// because how long the node takes to come up is not this test's business to
// guess, and registers t.Cleanup to remove the container whether the test
// passes or fails.
func startGarage(t *testing.T) dockerTransport {
	t.Helper()
	requireDocker(t)

	dir := t.TempDir()
	rpcSecret := hexString(t, 64)
	adminToken := hexString(t, 64)
	tomlPath := dir + "/garage.toml"
	toml := fmt.Sprintf(`metadata_dir = "/tmp/meta"
data_dir = "/tmp/data"
db_engine = "sqlite"

replication_factor = 1

rpc_bind_addr = "127.0.0.1:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "%s"

[s3_api]
s3_region = "garage"
api_bind_addr = "127.0.0.1:3900"
root_domain = ".s3.garage.localhost"

[admin]
api_bind_addr = "127.0.0.1:3903"
admin_token = "%s"
`, rpcSecret, adminToken)
	if err := os.WriteFile(tomlPath, []byte(toml), 0o644); err != nil {
		t.Fatalf("writing garage.toml: %v", err)
	}

	container := fmt.Sprintf("paisans-garage-integration-%d", time.Now().UnixNano())
	run := exec.Command("docker", "run", "-d", "--name", container,
		"-v", tomlPath+":/etc/garage.toml",
		garageImage)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("starting %s: %v\n%s", garageImage, err, out)
	}
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", container).Run()
	})

	transport := dockerTransport{container: container}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := exec.Command("docker", "exec", container, "/garage", "status").CombinedOutput(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
			t.Fatalf("garage did not answer `garage status` within 30s:\n%s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return transport
}

// TestRealGarageIsProvisionedAndIsIdempotent drives Build and Execute against
// a live dxflrs/garage:v1.0.1 container through the whole sequence: a fresh
// node with no layout, two apps that store objects, and a second Build run
// that must find everything already there.
//
// The property that matters most, and the one no unit test can reach, is
// asserted explicitly rather than left implicit in Execute succeeding: the
// key ID this test hands Garage for an app is read back out of Garage itself
// with `garage key info`, not compared against the toolkit's own idea of what
// it generated. If the two ever drifted, every upload from that app would
// fail with an opaque signature error and nothing before this test would say
// a word.
func TestRealGarageIsProvisionedAndIsIdempotent(t *testing.T) {
	transport := startGarage(t)

	const site = "home-a"
	talkKeyID := garageKeyID(t)
	talkSecret := hexString(t, 64)
	docsKeyID := garageKeyID(t)
	docsSecret := hexString(t, 64)

	cfg := &config.Config{
		Storage: config.Storage{
			Garage: config.Garage{
				Sites:    []string{site},
				Capacity: "100G",
			},
		},
		Apps: map[string]config.App{
			"talk": {Kind: config.KindMbin},
			"docs": {Kind: config.KindOutline},
		},
	}
	secrets := &config.Secrets{
		Apps: map[string]map[string]any{
			"talk": {
				"s3_access_key_id":     talkKeyID,
				"s3_secret_access_key": talkSecret,
			},
			"docs": {
				"s3_access_key_id":     docsKeyID,
				"s3_secret_access_key": docsSecret,
			},
		},
	}

	plan, err := Build(site, cfg, secrets, transport)
	if err != nil {
		t.Fatalf("building the first plan: %v", err)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("a fresh node should have something to do")
	}
	var firstCommands []string
	for _, s := range plan.Steps {
		firstCommands = append(firstCommands, s.Command)
	}
	joined := strings.Join(firstCommands, "\n")
	for _, want := range []string{"layout assign", "layout apply", "key import", "bucket create", "bucket allow"} {
		if !strings.Contains(joined, want) {
			t.Errorf("a fresh node's first plan should contain %q, got:\n%s", want, joined)
		}
	}

	if err := Execute(plan, transport); err != nil {
		t.Fatalf("executing the first plan: %v", err)
	}

	// The property that matters: the key ID the toolkit generated is the key
	// ID Garage actually holds, read back from Garage rather than compared
	// against the toolkit's own record of what it sent.
	for name, keyID := range map[string]string{"talk": talkKeyID, "docs": docsKeyID} {
		out, err := transport.Run(garageCmd + " key info " + keyID)
		if err != nil {
			t.Fatalf("reading back %s's key %s from Garage: %v\n%s", name, keyID, err, out)
		}
		if !strings.Contains(out, "Key ID: "+keyID) {
			t.Errorf("Garage's own record of %s's key does not carry the ID the toolkit generated, got:\n%s", name, out)
		}
		if !strings.Contains(out, "Key name: "+name) {
			t.Errorf("Garage's key for %s is not named %s, got:\n%s", name, name, out)
		}
	}

	// Both buckets exist.
	for _, bucket := range []string{"talk-uploads", "docs-uploads"} {
		out, err := transport.Run(garageCmd + " bucket info " + bucket)
		if err != nil {
			t.Fatalf("reading back bucket %s from Garage: %v\n%s", bucket, err, out)
		}
		if !strings.Contains(out, bucket) {
			t.Errorf("bucket info for %s does not name itself, got:\n%s", bucket, out)
		}
	}

	// One key per app is the whole point of this design: a key is allowed on
	// its own bucket and not on the other app's. Prove both directions.
	talkBucket, err := transport.Run(garageCmd + " bucket info talk-uploads")
	if err != nil {
		t.Fatalf("reading talk-uploads: %v", err)
	}
	if !strings.Contains(talkBucket, talkKeyID) {
		t.Errorf("talk's key %s must be allowed on talk-uploads, got:\n%s", talkKeyID, talkBucket)
	}
	if strings.Contains(talkBucket, docsKeyID) {
		t.Errorf("docs's key %s must not be allowed on talk-uploads, got:\n%s", docsKeyID, talkBucket)
	}

	docsBucket, err := transport.Run(garageCmd + " bucket info docs-uploads")
	if err != nil {
		t.Fatalf("reading docs-uploads: %v", err)
	}
	if !strings.Contains(docsBucket, docsKeyID) {
		t.Errorf("docs's key %s must be allowed on docs-uploads, got:\n%s", docsKeyID, docsBucket)
	}
	if strings.Contains(docsBucket, talkKeyID) {
		t.Errorf("talk's key %s must not be allowed on docs-uploads, got:\n%s", talkKeyID, docsBucket)
	}

	// The same cross check from the key's own point of view: `key info`
	// lists the buckets a key is authorized on, and it should be exactly the
	// one bucket that belongs to it.
	talkKeyInfo, err := transport.Run(garageCmd + " key info " + talkKeyID)
	if err != nil {
		t.Fatalf("reading talk's key info: %v", err)
	}
	if !strings.Contains(talkKeyInfo, "talk-uploads") {
		t.Errorf("talk's key should be authorized on talk-uploads, got:\n%s", talkKeyInfo)
	}
	if strings.Contains(talkKeyInfo, "docs-uploads") {
		t.Errorf("talk's key must not be authorized on docs-uploads, got:\n%s", talkKeyInfo)
	}

	// Idempotency, proven against the real thing: a second Build finds
	// everything already there and plans none of the one-shot steps. bucket
	// allow is idempotent and is always planned, so it is not asserted away
	// here.
	secondPlan, err := Build(site, cfg, secrets, transport)
	if err != nil {
		t.Fatalf("building the second plan: %v", err)
	}
	var secondCommands []string
	for _, s := range secondPlan.Steps {
		secondCommands = append(secondCommands, s.Command)
	}
	secondJoined := strings.Join(secondCommands, "\n")
	for _, unwanted := range []string{"key import", "bucket create", "layout"} {
		if strings.Contains(secondJoined, unwanted) {
			t.Errorf("a fully provisioned node's second plan should not contain %q, got:\n%s", unwanted, secondJoined)
		}
	}
	if len(secondPlan.Present) == 0 {
		t.Error("a fully provisioned node should report what it found, not look like it found nothing")
	}
}

// TestRenderedGarageTOMLStartsGarage boots dxflrs/garage:v1.0.1 against the
// garage.toml this toolkit actually renders, byte for byte, rather than
// against one a test wrote for itself.
//
// That distinction is the whole point of this test and the reason it exists.
// startGarage above writes its own garage.toml with a correct rpc_secret, so
// it proved the provisioning sequence and could never prove the rendered file
// was startable. It was not: `rpc_secret` was filled by `password()`, which is
// base64, and Garage parses that field as a hex encoded 32 byte key. Every
// rendered file died at startup with "Invalid RPC secret key: expected 32 bits
// of entropy", and six task reviews did not see it because nothing had ever
// started the file that reaches a host.
//
// The rendered addresses are mesh addresses and do not exist inside a
// container, so the node is started with net.ipv4.ip_nonlocal_bind. That
// changes what the kernel permits, not one byte of the configuration under
// test.
func TestRenderedGarageTOMLStartsGarage(t *testing.T) {
	requireDocker(t)

	const goldenPath = "srv/infra/garage/garage.toml"
	toml := renderedGarageTOML(t)

	dir := t.TempDir()
	tomlPath := dir + "/garage.toml"
	if err := os.WriteFile(tomlPath, []byte(toml), 0o644); err != nil {
		t.Fatalf("writing the rendered %s: %v", goldenPath, err)
	}

	container := fmt.Sprintf("paisans-garage-rendered-%d", time.Now().UnixNano())
	run := exec.Command("docker", "run", "-d", "--name", container,
		"--sysctl", "net.ipv4.ip_nonlocal_bind=1",
		"-v", tomlPath+":/etc/garage.toml",
		garageImage)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("starting %s: %v\n%s", garageImage, err, out)
	}
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", container).Run()
	})

	// "listening" is the proof: Garage reads the whole configuration, opens
	// its database and initializes RPC before it binds anything, so a node
	// that is listening is a node that accepted every value in this file.
	const listening = "S3 API server listening"
	var logs string
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, _ := exec.Command("docker", "logs", container).CombinedOutput()
		logs = string(out)
		if strings.Contains(logs, listening) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rendered %s did not bring Garage up within 60s. Its output was:\n%s", goldenPath, logs)
		}
		if state, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", container).CombinedOutput(); err == nil && strings.TrimSpace(string(state)) == "false" {
			t.Fatalf("Garage exited rather than starting against the rendered %s. Its output was:\n%s", goldenPath, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Named explicitly rather than left to the "listening" check, so that a
	// regression to a base64 rpc_secret fails with the message that says what
	// happened rather than with a timeout.
	if strings.Contains(logs, "Invalid RPC secret key") {
		t.Errorf("the rendered %s carries an rpc_secret Garage refuses:\n%s", goldenPath, logs)
	}
}

// renderedGarageTOML renders the repository's own fixture deployment and
// returns the garage.toml it produced, unmodified. It goes through
// render.Build rather than reading the checked in golden tree so that the file
// under test is the output of the code, not a copy of it that could have gone
// stale.
func renderedGarageTOML(t *testing.T) string {
	t.Helper()
	base := filepath.Join("..", "render", "testdata")
	cfg, err := config.Load(filepath.Join(base, "deployment.yaml"))
	if err != nil {
		t.Fatalf("loading the fixture configuration: %v", err)
	}
	secrets, err := config.LoadSecrets(filepath.Join(base, "secrets.fixture.yaml"))
	if err != nil {
		t.Fatalf("loading the fixture secrets: %v", err)
	}
	plan, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatalf("rendering the fixture: %v", err)
	}
	for _, f := range plan.Files {
		if strings.HasSuffix(f.Path, "srv/infra/garage/garage.toml") {
			return f.Content
		}
	}
	t.Fatal("the fixture rendered no garage.toml, so there is nothing to boot")
	return ""
}

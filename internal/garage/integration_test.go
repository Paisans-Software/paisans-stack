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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
)

const garageImage = "dxflrs/garage:v1.0.1"

// dockerTransport satisfies Transport by execing straight into a container
// this test started, rather than through the `docker compose ... exec`
// form Command hard codes for a deployed host. Build's commands all carry
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
	rest := strings.TrimPrefix(command, Command)
	if rest == command {
		return "", fmt.Errorf("command %q does not start with the expected prefix %q", command, Command)
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
		out, err := transport.Run(Command + " key info " + keyID)
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
		out, err := transport.Run(Command + " bucket info " + bucket)
		if err != nil {
			t.Fatalf("reading back bucket %s from Garage: %v\n%s", bucket, err, out)
		}
		if !strings.Contains(out, bucket) {
			t.Errorf("bucket info for %s does not name itself, got:\n%s", bucket, out)
		}
	}

	// One key per app is the whole point of this design: a key is allowed on
	// its own bucket and not on the other app's. Prove both directions.
	talkBucket, err := transport.Run(Command + " bucket info talk-uploads")
	if err != nil {
		t.Fatalf("reading talk-uploads: %v", err)
	}
	if !strings.Contains(talkBucket, talkKeyID) {
		t.Errorf("talk's key %s must be allowed on talk-uploads, got:\n%s", talkKeyID, talkBucket)
	}
	if strings.Contains(talkBucket, docsKeyID) {
		t.Errorf("docs's key %s must not be allowed on talk-uploads, got:\n%s", docsKeyID, talkBucket)
	}

	docsBucket, err := transport.Run(Command + " bucket info docs-uploads")
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
	talkKeyInfo, err := transport.Run(Command + " key info " + talkKeyID)
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

// fixtureDeployment loads the repository's own fixture deployment, the same
// pair of files internal/render's tests render. Returning both halves from
// one place keeps the configuration the provisioner is driven with and the
// configuration the files under test were rendered from identical by
// construction rather than by coincidence.
func fixtureDeployment(t *testing.T) (*config.Config, *config.Secrets) {
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
	return cfg, secrets
}

// renderedFile renders the fixture deployment and returns one file's content,
// unmodified, matched on its exact rendered path. It goes through
// render.Build rather than reading the checked in golden tree so that the
// file under test is the output of the code, not a copy of it that could have
// gone stale.
//
// The path is exact rather than a suffix because two sites render a
// garage.toml and they are not the same file: each binds its own mesh
// address. A suffix match would silently pick whichever sorted first.
func renderedFile(t *testing.T, path string) string {
	t.Helper()
	cfg, secrets := fixtureDeployment(t)
	plan, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatalf("rendering the fixture: %v", err)
	}
	for _, f := range plan.Files {
		if f.Path == path {
			return f.Content
		}
	}
	var rendered []string
	for _, f := range plan.Files {
		rendered = append(rendered, f.Path)
	}
	t.Fatalf("the fixture rendered no %s, so there is nothing to test. It rendered:\n%s", path, strings.Join(rendered, "\n"))
	return ""
}

// renderedGarageTOML returns the garage.toml rendered for home-a, the first
// Garage site in the fixture and the one whose address the rendered media
// snippet proxies to.
func renderedGarageTOML(t *testing.T) string {
	t.Helper()
	return renderedFile(t, "home-a/srv/infra/garage/garage.toml")
}

// ---------------------------------------------------------------------------
// The anonymous read, and the presigned one, each on its app's own media
// hostname.
// ---------------------------------------------------------------------------

// mediaHosts are the fixture's media hostnames, one per app that stores
// objects, each derived as <label>-media.<domain> from that app's own
// hostname. The Host a request carries is the only thing that selects which
// app's media routing it gets, so every request below sets one.
var mediaHosts = map[string]string{
	"talk": "talk-media.example.org",
	"blog": "blog-media.example.org",
	"docs": "docs-media.example.org",
}

const (
	// meshSubnet is mesh.subnet in the fixture deployment. The rendered
	// garage.toml binds a mesh address and the rendered media snippets proxy
	// to one, so the Docker network this test builds has to carry the same
	// subnet or neither rendered file would be usable as written.
	meshSubnet = "10.44.0.0/24"
	// meshGateway is an address in that subnet that no node claims. Docker
	// insists on a gateway inside the subnet, and the fixture's own addresses
	// start at .1, so the gateway is put at the far end.
	meshGateway = "10.44.0.254"
	// s3Region is s3_region in the rendered garage.toml. A SigV4 signature
	// covers it, so a mismatch here is an opaque signature failure.
	s3Region = "garage"
)

// garageNodes are the fixture's Garage sites and the mesh address each one's
// own rendered garage.toml binds.
//
// Both are started, and that is not thoroughness. The fixture declares
// replication 2, and dxflrs/garage:v1.0.1 refuses to apply a layout whose
// node count is below the replication factor: "The number of nodes with
// positive capacity (1) is smaller than the replication factor (2)". A single
// node cannot be provisioned from the rendered configuration at all, so a
// test that used the rendered garage.toml and one node would be testing a
// deployment that cannot exist.
var garageNodes = []struct {
	Site    string
	Address string
}{
	{Site: "home-a", Address: "10.44.0.1"},
	{Site: "home-b", Address: "10.44.0.2"},
}

// mediaCluster is the thing under test: two Garage nodes started from their
// own rendered garage.toml, and a gateway started from the rendered media
// snippets, on one network that carries the fixture's mesh subnet.
type mediaCluster struct {
	// Nodes is each site's transport into its own container.
	Nodes map[string]dockerTransport
	// S3 is host:port reaching Garage's S3 API from this test process,
	// published off the home-a container. Uploads go here directly: they are
	// how an application puts an object, and they are not what the gateway
	// is being tested for.
	S3 string
	// Caddy is host:port reaching the gateway from this test process. Every
	// assertion in this test goes through here.
	Caddy string
}

// dockerPort returns the host address Docker published a container port on.
func dockerPort(t *testing.T, container string, port int) string {
	t.Helper()
	out, err := exec.Command("docker", "port", container, strconv.Itoa(port)).CombinedOutput()
	if err != nil {
		t.Fatalf("reading the published address for %s port %d: %v\n%s", container, port, err, out)
	}
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	if first == "" {
		t.Fatalf("docker published nothing for %s port %d", container, port)
	}
	return first
}

// waitForLog blocks until every marker appears in a container's output, and
// fails if the container exits first or the deadline passes. It is used
// rather than a sleep because how long an image takes to come up is not this
// test's business to guess.
func waitForLog(t *testing.T, container string, within time.Duration, markers ...string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		out, _ := exec.Command("docker", "logs", container).CombinedOutput()
		logs := string(out)
		missing := ""
		for _, m := range markers {
			if !strings.Contains(logs, m) {
				missing = m
				break
			}
		}
		if missing == "" {
			return
		}
		if state, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", container).CombinedOutput(); err == nil && strings.TrimSpace(string(state)) == "false" {
			t.Fatalf("%s exited before it logged %q. Its output was:\n%s", container, missing, logs)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not log %q within %s. Its output was:\n%s", container, missing, within, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// renderedCaddyImage reads the gateway image out of the rendered infra
// compose file rather than naming it here, so this test runs the image an
// operator receives and cannot drift from it when the pin moves.
//
// PAISANS_TEST_CADDY_IMAGE replaces it, and the test logs that it did. It
// exists for a workstation that cannot pull the pinned image, which is
// published to a registry that needs a login. Nothing the snippets under test
// use comes from the modules that image adds, so a stock Caddy of the same
// release exercises the same directives; it is still not the pinned bytes, and
// a run that used it has to say so.
func renderedCaddyImage(t *testing.T) string {
	t.Helper()
	if override := os.Getenv("PAISANS_TEST_CADDY_IMAGE"); override != "" {
		t.Logf("PAISANS_TEST_CADDY_IMAGE is set: running %s rather than the rendered pin", override)
		return override
	}
	compose := renderedFile(t, "vm/srv/infra/compose.yaml")
	match := regexp.MustCompile(`image:\s*(\S*/caddy:\S+)`).FindStringSubmatch(compose)
	if match == nil {
		t.Fatalf("the rendered infra compose file names no Caddy image, so there is nothing to start:\n%s", compose)
	}
	return match[1]
}

// startMediaCluster brings up the two Garage nodes and the gateway, joins the
// nodes into one cluster, and registers cleanup for every container and the
// network.
func startMediaCluster(t *testing.T) *mediaCluster {
	t.Helper()
	requireDocker(t)

	caddyImage := renderedCaddyImage(t)
	stamp := time.Now().UnixNano()
	dir := t.TempDir()

	network := fmt.Sprintf("paisans-permedia-%d", stamp)
	// Registered before any container, so that LIFO cleanup removes the
	// containers first and the network last. A network with a container still
	// attached cannot be removed.
	t.Cleanup(func() {
		exec.Command("docker", "network", "rm", network).Run()
	})
	if out, err := exec.Command("docker", "network", "create",
		"--subnet", meshSubnet, "--gateway", meshGateway, network).CombinedOutput(); err != nil {
		t.Fatalf("creating a Docker network on %s: %v\n%s", meshSubnet, err, out)
	}

	cluster := &mediaCluster{Nodes: map[string]dockerTransport{}}
	for _, node := range garageNodes {
		toml := renderedFile(t, node.Site+"/srv/infra/garage/garage.toml")
		tomlPath := filepath.Join(dir, node.Site+".garage.toml")
		if err := os.WriteFile(tomlPath, []byte(toml), 0o644); err != nil {
			t.Fatalf("writing %s's rendered garage.toml: %v", node.Site, err)
		}

		container := fmt.Sprintf("paisans-permedia-%s-%d", node.Site, stamp)
		args := []string{"run", "-d", "--name", container,
			"--network", network, "--ip", node.Address,
			"-v", tomlPath + ":/etc/garage.toml"}
		if node.Site == garageNodes[0].Site {
			// Docker on macOS cannot route to a container address, so the S3
			// API is published to loopback for the uploads below. Nothing
			// about the configuration under test changes: Garage still binds
			// only the mesh address in its own namespace, and the gateway
			// still reaches it there.
			args = append(args, "-p", "127.0.0.1::3900")
		}
		args = append(args, garageImage)
		t.Cleanup(func() {
			exec.Command("docker", "rm", "-f", container).Run()
		})
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			t.Fatalf("starting %s for %s: %v\n%s", garageImage, node.Site, err, out)
		}
		// Both endpoints, named separately. The web endpoint is the one this
		// test exists for, and a garage.toml that dropped [s3_web] would
		// still log the S3 API line.
		waitForLog(t, container, 60*time.Second, "S3 API server listening", "Web server listening")
		cluster.Nodes[node.Site] = dockerTransport{container: container}
		if node.Site == garageNodes[0].Site {
			cluster.S3 = dockerPort(t, container, 3900)
		}
	}

	// Joining the nodes is this test's own scaffolding. Nothing rendered
	// carries bootstrap_peers, Consul discovery or Kubernetes discovery: a
	// shared rpc_secret authenticates a peer, it does not find one, so two
	// nodes started from these very files sit alone until something joins
	// them.
	//
	// `paisans storage add` is what performs this join on a real
	// deployment; this test does it by hand because what it tests is the
	// routing, not the join.
	first := cluster.Nodes[garageNodes[0].Site]
	for _, node := range garageNodes[1:] {
		idOut, err := cluster.Nodes[node.Site].Run(Command + " node id -q")
		if err != nil {
			t.Fatalf("reading %s's node ID: %v\n%s", node.Site, err, idOut)
		}
		if out, err := first.Run(Command + " node connect " + strings.TrimSpace(idOut)); err != nil {
			t.Fatalf("joining %s to the cluster: %v\n%s", node.Site, err, out)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := first.Run(Command + " status")
		if err == nil && strings.Count(out, ":3901") >= len(garageNodes) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %d Garage nodes did not all appear in `garage status` within 30s:\n%s", len(garageNodes), out)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The gateway, from the rendered snippets. The Caddyfile is minimal on
	// purpose: the rendered one would try to obtain certificates for every
	// hostname from a DNS provider. What it would contribute is one site block
	// per media hostname, three lines each and reproduced here over plain
	// http, and what is under test is the snippet each one imports.
	snippets := filepath.Join(dir, "snippets")
	if err := os.MkdirAll(snippets, 0o755); err != nil {
		t.Fatalf("creating the snippet directory: %v", err)
	}
	caddyfile := "{\n\tadmin off\n\tauto_https off\n}\n"
	for _, app := range []string{"blog", "docs", "talk"} {
		file := app + "-media.caddy"
		snippet := renderedFile(t, "vm/srv/infra/caddy/snippets/"+file)
		if err := os.WriteFile(filepath.Join(snippets, file), []byte(snippet), 0o644); err != nil {
			t.Fatalf("writing the rendered %s: %v", file, err)
		}
		caddyfile += fmt.Sprintf("\nhttp://%s {\n\timport /etc/caddy/snippets/%s\n}\n", mediaHosts[app], file)
	}
	caddyfilePath := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(caddyfilePath, []byte(caddyfile), 0o644); err != nil {
		t.Fatalf("writing the minimal Caddyfile: %v", err)
	}

	gateway := fmt.Sprintf("paisans-permedia-gateway-%d", stamp)
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", gateway).Run()
	})
	out, err := exec.Command("docker", "run", "-d", "--name", gateway,
		"--network", network,
		"-p", "127.0.0.1::80",
		"-v", caddyfilePath+":/etc/caddy/Caddyfile:ro",
		"-v", snippets+":/etc/caddy/snippets:ro",
		caddyImage).CombinedOutput()
	if err != nil {
		t.Fatalf("starting %s: %v\n%s", caddyImage, err, out)
	}
	// "serving initial configuration" is the proof that Caddy adapted the
	// whole Caddyfile, snippet included. A snippet it could not parse makes
	// it exit instead, which waitForLog reports with the parse error.
	waitForLog(t, gateway, 60*time.Second, "serving initial configuration")
	cluster.Caddy = dockerPort(t, gateway, 80)

	return cluster
}

// provision lays both nodes out, then drives the real garage.Build and
// garage.Execute against home-a for the keys, buckets, grants and website
// access.
//
// The layout is this test's own scaffolding, as the join above is. A node
// joining a cluster of several is `paisans storage add`'s job, which this
// package's planner refuses and names; storage add's own Docker test
// (internal/storageadd, tag garage_integration) drives that path end to end.
// What is under test here is the routing, so the layout is assigned the way
// storage add assigns it, both roles in one version, zone named after site.
// Keys and buckets live in tables every node with a role holds, so planning
// them against one node provisions the cluster.
func (c *mediaCluster) provision(t *testing.T, cfg *config.Config, secrets *config.Secrets) {
	t.Helper()
	first := c.Nodes[garageNodes[0].Site]
	for _, node := range garageNodes {
		idOut, err := c.Nodes[node.Site].Run(Command + " node id -q")
		if err != nil {
			t.Fatalf("reading %s's node ID: %v\n%s", node.Site, err, idOut)
		}
		id := strings.TrimSpace(idOut)
		if at := strings.Index(id, "@"); at >= 0 {
			id = id[:at]
		}
		if out, err := first.Run(Command + " layout assign -z " + node.Site + " -c 1G " + id); err != nil {
			t.Fatalf("assigning %s a role: %v\n%s", node.Site, err, out)
		}
	}
	if out, err := first.Run(Command + " layout apply --version 1"); err != nil {
		t.Fatalf("applying the layout: %v\n%s", err, out)
	}
	plan, err := Build(garageNodes[0].Site, cfg, secrets, first)
	if err != nil {
		t.Fatalf("building %s's plan: %v", garageNodes[0].Site, err)
	}
	if err := Execute(plan, first); err != nil {
		t.Fatalf("provisioning %s: %v", garageNodes[0].Site, err)
	}
	// The second node holds the same keys and buckets without being asked.
	plan, err = Build(garageNodes[1].Site, cfg, secrets, c.Nodes[garageNodes[1].Site])
	if err != nil {
		t.Fatalf("building %s's plan: %v", garageNodes[1].Site, err)
	}
	if len(plan.Steps) != 0 {
		var left []string
		for _, s := range plan.Steps {
			left = append(left, s.Describe)
		}
		t.Fatalf("%s should find every key, bucket and grant present, being in the same cluster; it planned: %s", garageNodes[1].Site, strings.Join(left, "; "))
	}
}

// hmacSHA256 and the three helpers below are a complete SigV4 implementation
// in about fifty lines of standard library. The AWS SDK is in go.mod as an
// indirect dependency of sops and making it a direct one to sign two requests
// would change what this repository depends on in order to test it.
func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sigv4Key derives the date, region and service scoped signing key.
func sigv4Key(secret, dateStamp string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	k = hmacSHA256(k, s3Region)
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

// signSigV4 signs a request in the header form, in place. The signature
// covers the Host header, which is the reason the gateway forwards Host
// unchanged on its fallback route and the reason a presigned Outline URL
// would stop working if it did not.
func signSigV4(req *http.Request, keyID, secret, body string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	payloadHash := sha256Hex(body)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n"
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + s3Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex(canonicalRequest),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(sigv4Key(secret, dateStamp), stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+keyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

// presignGet returns the query a presigned GET carries, signed for the given
// host. This is the form Outline issues to a browser: the credential is in
// the URL, so the only header the signature covers is Host, and the host it
// is signed with is the media hostname rather than anything inside the mesh.
func presignGet(keyID, secret, host, path string) string {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := dateStamp + "/" + s3Region + "/s3/aws4_request"

	query := url.Values{}
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", keyID+"/"+scope)
	query.Set("X-Amz-Date", amzDate)
	query.Set("X-Amz-Expires", "300")
	query.Set("X-Amz-SignedHeaders", "host")

	canonicalRequest := strings.Join([]string{
		"GET",
		path,
		query.Encode(),
		"host:" + host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex(canonicalRequest),
	}, "\n")
	query.Set("X-Amz-Signature", hex.EncodeToString(hmacSHA256(sigv4Key(secret, dateStamp), stringToSign)))
	return query.Encode()
}

// appCredential reads one app's rendered S3 credential and bucket out of the
// fixture, so that the object this test uploads is uploaded with the same key
// the provisioner granted and into the same bucket the gateway routes.
func appCredential(t *testing.T, cfg *config.Config, secrets *config.Secrets, app string) (bucket, keyID, secret string) {
	t.Helper()
	keyID, ok := SecretString(secrets, app, "s3_access_key_id")
	if !ok || keyID == "" {
		t.Fatalf("the fixture holds no S3 access key for %s", app)
	}
	secret, ok = SecretString(secrets, app, "s3_secret_access_key")
	if !ok || secret == "" {
		t.Fatalf("the fixture holds no S3 secret for %s", app)
	}
	return BucketName(cfg.Apps[app], app), keyID, secret
}

// putObject uploads one object straight to Garage's S3 API with a signed
// request, the way the application itself would.
func putObject(t *testing.T, endpoint, bucket, key, keyID, secret, body string) {
	t.Helper()
	target := "http://" + endpoint + "/" + bucket + "/" + key
	req, err := http.NewRequest(http.MethodPut, target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building the upload request for %s: %v", target, err)
	}
	req.ContentLength = int64(len(body))
	signSigV4(req, keyID, secret, body)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("uploading %s/%s: %v", bucket, key, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("uploading %s/%s returned %d, want 200:\n%s", bucket, key, resp.StatusCode, out)
	}
}

// throughGateway issues one request to the gateway with Host set to a media
// hostname, and returns the status, the response headers and the body. The
// Host is the only thing that selects an app's media routing, so it is set on
// every call rather than left to the address the request was dialled on.
//
// No credential is added here. A caller that wants one passes a presigned
// query; there is no code path in this helper that could add an Authorization
// header, which is what makes the anonymous assertions below mean what they
// say.
func throughGateway(t *testing.T, cluster *mediaCluster, host, path, rawQuery string) (int, http.Header, string) {
	t.Helper()
	target := "http://" + cluster.Caddy + path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("building the request for %s: %v", target, err)
	}
	req.Host = host
	if req.Header.Get("Authorization") != "" {
		t.Fatal("this helper must never carry a credential")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetching %s%s through the gateway: %v", host, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body for %s%s: %v", host, path, err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// bucketWebsiteAccess reads one bucket's website flag back out of Garage.
func bucketWebsiteAccess(t *testing.T, node dockerTransport, bucket string) string {
	t.Helper()
	out, err := node.Run(Command + " bucket info " + bucket)
	if err != nil {
		t.Fatalf("reading bucket %s: %v\n%s", bucket, err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Website access:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("`bucket info %s` reported no website access at all, so this test cannot tell either way:\n%s", bucket, out)
	return ""
}

// publicMediaPolicy is the Content-Security-Policy every public media
// hostname must send, exactly as the rendered snippet sets it.
const publicMediaPolicy = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

// TestEachAppsMediaHostnameServesOnlyItsOwnBucket is the assertion the media
// routing exists for, and it is made against nothing this test wrote itself.
// Two Garage nodes are started from their own rendered garage.toml, a Caddy
// from the rendered media snippets, the buckets and keys come from the real
// garage.Build and garage.Execute, and the objects are uploaded with the
// rendered credential.
//
// Every fetch goes through the same gateway, and the Host it carries is the
// only thing that differs:
//
//  1. Mbin's object on talk-media, with no credential of any kind. 200 and the
//     bytes, with the sandboxing policy and nosniff. The path is the object
//     key with no bucket in it, because the hostname is the bucket.
//  2. The blog's object on blog-media, the same way. The wisp fork never reads
//     its bucket back under S3, so a reader's browser has to be able to.
//  3. Outline's object on docs-media, no credential. Refused. The absence of
//     the object's bytes is asserted alongside the status, because a 200
//     carrying an error document would satisfy a status check alone.
//  4. Outline's object on docs-media, presigned for that hostname. 200 and the
//     bytes. The signature covers Host, so this is what proves the gateway
//     forwards it unchanged to the S3 API rather than to the web endpoint.
//  5. Outline's key asked for on talk-media. Not Outline's object: talk-media
//     resolves to talk's bucket whatever the path says, so one app's media
//     hostname cannot be used to reach another app's bucket.
func TestEachAppsMediaHostnameServesOnlyItsOwnBucket(t *testing.T) {
	cfg, secrets := fixtureDeployment(t)
	cluster := startMediaCluster(t)
	cluster.provision(t, cfg, secrets)

	talkBucket, talkKeyID, talkSecret := appCredential(t, cfg, secrets, "talk")
	blogBucket, blogKeyID, blogSecret := appCredential(t, cfg, secrets, "blog")
	docsBucket, docsKeyID, docsSecret := appCredential(t, cfg, secrets, "docs")

	// Website access, stated rather than implied. A public bucket must have it
	// or no anonymous read is possible; the private bucket must not, or the
	// refusal below would be an accident of routing rather than a property of
	// the bucket.
	for _, bucket := range []string{talkBucket, blogBucket} {
		if got := bucketWebsiteAccess(t, cluster.Nodes["home-a"], bucket); got != "true" {
			t.Errorf("%s serves objects publicly, so its website access must be true, got %q", bucket, got)
		}
	}
	if got := bucketWebsiteAccess(t, cluster.Nodes["home-a"], docsBucket); got != "false" {
		t.Errorf("%s must never be readable without a credential, so its website access must be false, got %q", docsBucket, got)
	}

	const key = "media/permedia-read.svg"
	talkBody := "mbin media bytes " + hexString(t, 16)
	blogBody := "blog image bytes " + hexString(t, 16)
	docsBody := "outline private bytes " + hexString(t, 16)
	putObject(t, cluster.S3, talkBucket, key, talkKeyID, talkSecret, talkBody)
	putObject(t, cluster.S3, blogBucket, key, blogKeyID, blogSecret, blogBody)
	putObject(t, cluster.S3, docsBucket, key, docsKeyID, docsSecret, docsBody)

	// 1 and 2. Anonymous, public buckets, each on its own hostname.
	for _, tc := range []struct{ app, body string }{{"talk", talkBody}, {"blog", blogBody}} {
		host := mediaHosts[tc.app]
		status, header, body := throughGateway(t, cluster, host, "/"+key, "")
		t.Logf("anonymous GET %s/%s: HTTP %d", host, key, status)
		if status != http.StatusOK {
			t.Errorf("an anonymous fetch on %s must return 200, got %d:\n%s", host, status, body)
		}
		if body != tc.body {
			t.Errorf("an anonymous fetch on %s must return %s's object, got %q", host, tc.app, body)
		}
		if got := header.Values("Content-Security-Policy"); len(got) != 1 || got[0] != publicMediaPolicy {
			t.Errorf("%s must send exactly one Content-Security-Policy, %q, got %q", host, publicMediaPolicy, got)
		}
		if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s must send X-Content-Type-Options: nosniff, got %q", host, got)
		}
	}

	// 3. Anonymous, private bucket, on Outline's hostname.
	docsHost := mediaHosts["docs"]
	docsPath := "/" + docsBucket + "/" + key
	status, _, body := throughGateway(t, cluster, docsHost, docsPath, "")
	t.Logf("anonymous GET %s%s: HTTP %d", docsHost, docsPath, status)
	if status != http.StatusForbidden {
		t.Errorf("an anonymous fetch on %s should be refused with 403 by Garage's S3 API, got %d:\n%s", docsHost, status, body)
	}
	if strings.Contains(body, docsBody) {
		t.Errorf("an anonymous fetch on %s returned the object's bytes in a %d response, which is the failure a status check alone would miss:\n%s", docsHost, status, body)
	}

	// 4. Presigned for Outline's hostname, path style, the form Outline issues.
	query := presignGet(docsKeyID, docsSecret, docsHost, docsPath)
	status, header, body := throughGateway(t, cluster, docsHost, docsPath, query)
	t.Logf("presigned GET %s%s: HTTP %d", docsHost, docsPath, status)
	if status != http.StatusOK {
		t.Errorf("a presigned fetch on %s must return 200, got %d:\n%s", docsHost, status, body)
	}
	if body != docsBody {
		t.Errorf("a presigned fetch on %s must return the object's bytes, got %q", docsHost, body)
	}
	if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("%s should send X-Content-Type-Options: nosniff, got %q", docsHost, got)
	}
	if got := header.Get("Content-Security-Policy"); got != "" {
		t.Errorf("%s should carry no sandboxing policy, which would break Outline's embedded PDF previews, got %q", docsHost, got)
	}

	// 5. Another app's key on talk's hostname.
	status, _, body = throughGateway(t, cluster, mediaHosts["talk"], docsPath, "")
	t.Logf("anonymous GET %s%s: HTTP %d", mediaHosts["talk"], docsPath, status)
	if status == http.StatusOK || strings.Contains(body, docsBody) {
		t.Errorf("talk's media hostname must not reach Outline's bucket, got %d:\n%s", status, body)
	}

	// 4. The first listed node gone. The media routes name every Garage node
	// in storage.garage.sites order, so the gateway must find the object on
	// the second: the request that meets the dead node is retried there
	// (lb_try_duration), and every node holds a copy at replication 2.
	// Asserted at once, without waiting for Garage to notice the node is
	// down, because a reader does not wait either.
	if out, err := exec.Command("docker", "stop", cluster.Nodes[garageNodes[0].Site].container).CombinedOutput(); err != nil {
		t.Fatalf("stopping %s: %v\n%s", garageNodes[0].Site, err, out)
	}
	for i := 0; i < 3; i++ {
		status, body = throughGateway(t, cluster, "/"+publicBucket+"/"+publicKey, "")
		t.Logf("anonymous GET /%s/%s with %s stopped, attempt %d: HTTP %d", publicBucket, publicKey, garageNodes[0].Site, i+1, status)
		if status != http.StatusOK || body != publicBody {
			t.Errorf("with %s stopped, the gateway must serve %s from %s, got %d:\n%s", garageNodes[0].Site, publicBucket, garageNodes[1].Site, status, body)
		}
	}
}

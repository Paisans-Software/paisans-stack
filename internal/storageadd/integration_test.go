//go:build garage_integration

// This file drives the real storage add planner against real
// dxflrs/garage:v1.0.1 containers: one node provisioned at replication 1 and
// holding an object, a second node applied at replication 2, then
// `storage add --change-replication` with consistency dangerous, the shape
// the first deployment moves to. Everything the unit tests assume about
// Garage's output and behaviour is exercised here against the real thing:
// the refusal to start at another factor, the layout set aside, the join,
// one layout version, the sync gate, provisioning found present, the object
// still readable through both nodes, and the probe.
//
// Run with: go test -tags garage_integration ./internal/storageadd/
package storageadd_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/storageadd"
)

const (
	garageImage = "dxflrs/garage:v1.0.1"
	// curlImage runs the probe's requests inside the Docker network: Docker
	// on macOS cannot route to a container address, and the Garage image
	// has no shell or curl of its own. A real host has curl from host
	// prepare.
	curlImage = "curlimages/curl:8.10.1"
	// A subnet of this test's own, so it cannot collide with the Garage
	// package's Docker test, which uses the fixture's 10.44.0.0/24 and may
	// run at the same time.
	subnet  = "10.47.0.0/24"
	gateway = "10.47.0.254"
)

// dockerHost is one site: a directory standing in for its filesystem, with
// /srv/infra/garage/garage.toml and meta/ bind mounted into its Garage
// container, which is created stopped and driven by the commands storage add
// runs.
type dockerHost struct {
	t         *testing.T
	site      string
	root      string
	container string
	network   string
}

func (h *dockerHost) Describe() string { return h.site }

func (h *dockerHost) local(path string) string { return filepath.Join(h.root, path) }

func (h *dockerHost) ReadFile(path string) (string, bool, error) {
	b, err := os.ReadFile(h.local(path))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	return string(b), err == nil, err
}

// WriteFile writes in place, keeping the inode: garage.toml is a single file
// bind mount, which keeps the inode it was started with.
func (h *dockerHost) WriteFile(path, content string, mode uint32) error {
	if err := os.MkdirAll(filepath.Dir(h.local(path)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(h.local(path), []byte(content), os.FileMode(mode))
}

func (h *dockerHost) Run(command string) (string, error) { return h.RunInput(command, "") }

func (h *dockerHost) RunInput(command, stdin string) (string, error) {
	var cmd *exec.Cmd
	switch {
	case strings.HasPrefix(command, garage.Command+" "):
		args := append([]string{"exec", h.container, "/garage"}, strings.Fields(strings.TrimPrefix(command, garage.Command+" "))...)
		cmd = exec.Command("docker", args...)
	case command == "docker compose -f /srv/infra/compose.yaml stop garage":
		cmd = exec.Command("docker", "stop", h.container)
	case command == "docker compose -f /srv/infra/compose.yaml up -d garage":
		cmd = exec.Command("docker", "start", h.container)
	case strings.HasPrefix(command, "curl "):
		cmd = exec.Command("docker", "run", "--rm", "-i", "--network", h.network, "--entrypoint", "sh", curlImage, "-c", command)
	default:
		// Shell work on the host's own files: ls, mv, rm. Every absolute
		// /srv path is this host's directory.
		cmd = exec.Command("sh", "-c", strings.ReplaceAll(command, " /srv/", " "+h.root+"/srv/"))
	}
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gatewayHost stands in for the gateway: its files on disk, Caddy's validate
// and reload accepted, and the media read answered by the first node's web
// endpoint. The gateway's routing through a real Caddy is the Garage
// package's Docker test; this one is about the join.
type gatewayHost struct {
	dockerHost
	first string
}

func (g *gatewayHost) Run(command string) (string, error) {
	switch {
	case strings.Contains(command, "caddy validate"), strings.Contains(command, "caddy reload"):
		return "", nil
	case strings.Contains(command, "--resolve "):
		m := regexp.MustCompile(`https://[^/]+/([^/]+)/(\S+)$`).FindStringSubmatch(command)
		return g.dockerHost.Run(fmt.Sprintf("curl -fsS --max-time 20 -H 'Host: %s.web.garage.internal' http://%s:3902/%s", m[1], g.first, m[2]))
	}
	return g.dockerHost.Run(command)
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker is not running: %s", out)
	}
}

func mustRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// renderFor renders cfg and puts each named site's garage.toml on its host,
// with the manifest apply would have left, as `paisans apply` would.
func renderFor(t *testing.T, cfg *config.Config, secrets *config.Secrets, hosts map[string]*dockerHost, sites ...string) *render.Plan {
	t.Helper()
	rendered, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range sites {
		h := hosts[site]
		for _, f := range rendered.Files {
			if f.Path == site+"/"+apply.GarageConfig || f.Path == site+"/srv/infra/caddy/snippets/media.caddy" {
				rel := strings.TrimPrefix(f.Path, site+"/")
				if err := h.WriteFile("/"+rel, f.Content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		recordManifest(t, h)
	}
	return rendered
}

// recordManifest records every file under the host's /srv/infra as apply's
// own, which is what an apply leaves behind.
func recordManifest(t *testing.T, h *dockerHost) {
	t.Helper()
	var m render.Manifest
	m.Version = 1
	for _, rel := range []string{apply.GarageConfig, "srv/infra/caddy/snippets/media.caddy"} {
		content, found, _ := h.ReadFile("/" + rel)
		if !found {
			continue
		}
		m.Files = append(m.Files, render.ManifestFile{Path: rel, SHA256: sha(content), Mode: "0600"})
	}
	data, _ := jsonMarshal(m)
	if err := h.WriteFile("/srv/.paisans-manifest.json", data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStorageAddResetsAndJoinsRealGarage(t *testing.T) {
	requireDocker(t)
	cfg, secrets := fixture(t)
	cfg.Mesh.Subnet = subnet
	addresses := map[string]string{"home-a": "10.47.0.1", "home-b": "10.47.0.2", "vm": "10.47.0.3"}
	for name, addr := range addresses {
		s := cfg.Sites[name]
		s.Address = addr
		cfg.Sites[name] = s
	}

	stamp := time.Now().UnixNano()
	network := fmt.Sprintf("paisans-storageadd-%d", stamp)
	t.Cleanup(func() { exec.Command("docker", "network", "rm", network).Run() })
	mustRun(t, "docker", "network", "create", "--subnet", subnet, "--gateway", gateway, network)

	dir := t.TempDir()
	hosts := map[string]*dockerHost{}
	for _, site := range []string{"home-a", "home-b", "vm"} {
		h := &dockerHost{t: t, site: site, root: filepath.Join(dir, site), container: fmt.Sprintf("paisans-storageadd-%s-%d", site, stamp), network: network}
		for _, d := range []string{"srv/infra/garage/meta", "srv/infra/garage/data"} {
			if err := os.MkdirAll(h.local("/"+d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		hosts[site] = h
	}

	// Before: Garage on home-a alone, at replication 1.
	cfg.Storage.Garage.Sites = []string{"home-a"}
	cfg.Storage.Garage.Replication = 1
	renderFor(t, cfg, secrets, hosts, "home-a", "vm")
	create := func(h *dockerHost) {
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", h.container).Run() })
		mustRun(t, "docker", "create", "--name", h.container, "--network", h.network, "--ip", cfg.Sites[h.site].Address,
			"-v", h.local("/srv/infra/garage/garage.toml")+":/etc/garage.toml",
			"-v", h.local("/srv/infra/garage/meta")+":/var/lib/garage/meta",
			"-v", h.local("/srv/infra/garage/data")+":/var/lib/garage/data",
			garageImage)
		mustRun(t, "docker", "start", h.container)
	}
	create(hosts["home-a"])
	waitAnswers(t, hosts["home-a"])
	plan, err := garage.Build("home-a", cfg, secrets, hosts["home-a"])
	if err != nil {
		t.Fatal(err)
	}
	if err := garage.Execute(plan, hosts["home-a"]); err != nil {
		t.Fatalf("provisioning home-a alone: %v", err)
	}

	// An object written at replication 1, which must survive the reset.
	keyID, _ := garage.SecretString(secrets, "talk", "s3_access_key_id")
	secret, _ := garage.SecretString(secrets, "talk", "s3_secret_access_key")
	bucket := garage.BucketName(cfg.Apps["talk"], "talk")
	s3 := func(h *dockerHost, method, node, body string) (string, error) {
		conf := fmt.Sprintf("url = \"http://%s:3900/%s/before-the-reset\"\nrequest = \"%s\"\nuser = \"%s:%s\"\naws-sigv4 = \"aws:amz:garage:s3\"\nheader = \"x-amz-content-sha256: UNSIGNED-PAYLOAD\"\n", node, bucket, method, keyID, secret)
		if body != "" {
			conf += fmt.Sprintf("data-binary = \"%s\"\n", body)
		}
		return h.RunInput("curl -fsS --max-time 20 -K -", conf)
	}
	if out, err := s3(hosts["home-a"], "PUT", addresses["home-a"], "written at replication 1"); err != nil {
		t.Fatalf("writing the object at replication 1: %v\n%s", err, out)
	}

	// After: the configuration the founder chose, and home-b applied at it.
	cfg.Storage.Garage.Sites = []string{"home-a", "home-b"}
	cfg.Storage.Garage.Replication = 2
	cfg.Storage.Garage.Consistency = "dangerous"
	renderFor(t, cfg, secrets, hosts, "home-b")
	create(hosts["home-b"])
	waitAnswers(t, hosts["home-b"])

	transports := map[string]apply.Transport{
		"home-a": hosts["home-a"],
		"home-b": hosts["home-b"],
		"vm":     &gatewayHost{dockerHost: *hosts["vm"], first: addresses["home-a"]},
	}

	// Without the flag: refused, nothing changed.
	p, err := storageadd.Build(cfg, secrets, transports, storageadd.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := storageadd.Execute(p); err == nil || !strings.Contains(err.Error(), "--change-replication") {
		t.Fatalf("expected the refusal without --change-replication, got %v", err)
	}

	p, err = storageadd.Build(cfg, secrets, transports, storageadd.Options{ChangeReplication: true, Wait: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var progress strings.Builder
	p.Print(&progress)
	p.Progress = &progress
	start := time.Now()
	if err := storageadd.Execute(p); err != nil {
		t.Fatalf("storage add: %v\n%s", err, progress.String())
	}
	t.Logf("storage add took %s:\n%s", time.Since(start).Round(time.Second), progress.String())

	if _, found, _ := hosts["home-a"].ReadFile("/srv/infra/garage/meta/cluster_layout.rf1"); !found {
		t.Error("home-a's replication 1 layout was not set aside")
	}
	if _, found, _ := hosts["home-a"].ReadFile(storageadd.CountsFile); found {
		t.Error("the counts file outlived the provision gate")
	}
	for _, site := range []string{"home-a", "home-b"} {
		out, err := s3(hosts[site], "GET", addresses[site], "")
		if err != nil || out != "written at replication 1" {
			t.Errorf("the object written before the reset, read through %s: %v %q", site, err, out)
		}
	}

	// A second run finds nothing to do and passes every gate again.
	p, err = storageadd.Build(cfg, secrets, transports, storageadd.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Pending() {
		var b strings.Builder
		p.Print(&b)
		t.Errorf("a joined cluster still plans work:\n%s", b.String())
	}
	if err := storageadd.Execute(p); err != nil {
		t.Fatalf("a second run: %v", err)
	}
}

func waitAnswers(t *testing.T, h *dockerHost) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if out, err := h.Run(garage.Command + " node id -q"); err == nil && strings.Contains(out, "@") {
			return
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", h.container).CombinedOutput()
			t.Fatalf("%s's Garage did not answer within 60s:\n%s", h.site, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

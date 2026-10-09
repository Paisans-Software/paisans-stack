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
	"github.com/paisans-software/paisans-stack/internal/ui"
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
	"github.com/paisans-software/paisans-stack/internal/kinds"
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
// /srv/paisans/f2a9/infra/garage/garage.toml and meta/ bind mounted into its Garage
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
	case strings.HasPrefix(command, garage.Command(storageadd.Fixture)+" "):
		args := append([]string{"exec", h.container, "/garage"}, strings.Fields(strings.TrimPrefix(command, garage.Command(storageadd.Fixture)+" "))...)
		cmd = exec.Command("docker", args...)
	case command == "docker compose -f /srv/paisans/f2a9/infra/compose.yaml stop garage":
		cmd = exec.Command("docker", "stop", h.container)
	case command == "docker compose -f /srv/paisans/f2a9/infra/compose.yaml up -d garage":
		cmd = exec.Command("docker", "start", h.container)
	case strings.HasPrefix(command, "curl "):
		cmd = exec.Command("docker", "run", "--rm", "-i", "--network", h.network, "--entrypoint", "sh", curlImage, "-c", command)
	default:
		// Shell work on the host's own files: ls, mv, rm. Every absolute
		// /srv path is this host's directory.
		cmd = exec.Command("sh", "-c", strings.ReplaceAll(command, " /srv/paisans/f2a9/", " "+h.root+"/srv/paisans/f2a9/"))
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
	// buckets maps each app's media hostname to its bucket.
	buckets map[string]string
}

func (g *gatewayHost) Run(command string) (string, error) {
	switch {
	case strings.Contains(command, "caddy validate"), strings.Contains(command, "caddy reload"):
		return "", nil
	case strings.Contains(command, "--resolve "):
		// An app's media hostname is its alone and the path is the object
		// key, so the hostname says which bucket's web vhost to ask.
		m := regexp.MustCompile(`https://([^/]+)/(\S+)$`).FindStringSubmatch(command)
		return g.dockerHost.Run(fmt.Sprintf("curl -fsS --max-time 20 -H 'Host: %s.web.garage.internal' http://%s:3902/%s", g.buckets[m[1]], g.first, m[2]))
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
			if f.Path == site+"/"+"srv/paisans/f2a9/infra/garage/garage.toml" || (strings.HasPrefix(f.Path, site+"/srv/paisans/f2a9/infra/caddy/snippets/") && strings.HasSuffix(f.Path, "-media.caddy")) {
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

// recordManifest records every file under the host's /srv/paisans/f2a9/infra as apply's
// own, which is what an apply leaves behind.
func recordManifest(t *testing.T, h *dockerHost) {
	t.Helper()
	var m render.Manifest
	m.Version = 1
	_ = filepath.Walk(h.local("/srv/paisans/f2a9/infra"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.Contains(path, "/garage/meta/") || strings.Contains(path, "/garage/data/") {
			return nil
		}
		rel, _ := filepath.Rel(h.root, path)
		content, _ := os.ReadFile(path)
		m.Files = append(m.Files, render.ManifestFile{Path: rel, SHA256: sha(string(content)), Mode: "0600"})
		return nil
	})
	data, _ := jsonMarshal(m)
	if err := h.WriteFile("/srv/paisans/f2a9/.paisans-manifest.json", data, 0o644); err != nil {
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
		for _, d := range []string{"srv/paisans/f2a9/infra/garage/meta", "srv/paisans/f2a9/infra/garage/data"} {
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
			"-v", h.local("/srv/paisans/f2a9/infra/garage/garage.toml")+":/etc/garage.toml",
			"-v", h.local("/srv/paisans/f2a9/infra/garage/meta")+":/var/lib/garage/meta",
			"-v", h.local("/srv/paisans/f2a9/infra/garage/data")+":/var/lib/garage/data",
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
		"vm":     &gatewayHost{dockerHost: *hosts["vm"], first: addresses["home-a"], buckets: mediaBuckets(cfg)},
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
	rec := &ui.Recorder{Verbose_: true}
	p.Show(rec)
	p.Report = rec
	start := time.Now()
	if err := storageadd.Execute(p); err != nil {
		t.Fatalf("storage add: %v\n%s", err, rec.Lines())
	}
	t.Logf("storage add took %s:\n%s", time.Since(start).Round(time.Second), rec.Lines())

	if _, found, _ := hosts["home-a"].ReadFile("/srv/paisans/f2a9/infra/garage/meta/cluster_layout.rf1"); !found {
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
		rec := &ui.Recorder{}
		p.Show(rec)
		t.Errorf("a joined cluster still plans work:\n%s", rec.Lines())
	}
	if err := storageadd.Execute(p); err != nil {
		t.Fatalf("a second run: %v", err)
	}
}

func waitAnswers(t *testing.T, h *dockerHost) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if out, err := h.Run(garage.Command(storageadd.Fixture) + " node id -q"); err == nil && strings.Contains(out, "@") {
			return
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", h.container).CombinedOutput()
			t.Fatalf("%s's Garage did not answer within 60s:\n%s", h.site, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func mediaBuckets(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		if media := kinds.MediaHostname(app, cfg.Community.Domain); media != "" {
			out[media] = garage.BucketName(app, name)
		}
	}
	return out
}

// TestStorageAddGrowsToThreeWithAStorageSite is the founder's growth path:
// Garage on home-a alone at replication 1, then home-b and a host with only
// the storage role and its own capacity, at replication 3, joined in one
// reset, and the stop test proving reads and an upload survive the storage
// host going down.
func TestStorageAddGrowsToThreeWithAStorageSite(t *testing.T) {
	requireDocker(t)
	cfg, secrets := fixture(t)
	const sub, gw = "10.48.0.0/24", "10.48.0.254"
	cfg.Mesh.Subnet = sub
	addresses := map[string]string{"home-a": "10.48.0.1", "home-b": "10.48.0.2", "vm": "10.48.0.3", "store": "10.48.0.4"}
	cfg.Sites["store"] = config.Site{Roles: []config.Role{config.RoleStorage}, Endpoint: "203.0.113.40:51820"}
	secrets.Sites["store"] = config.SiteSecrets{WireGuardPrivateKey: "Q0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0M=", HeartbeatToken: "5555555555555555555555555555eeee"}
	for name, addr := range addresses {
		s := cfg.Sites[name]
		s.Address = addr
		cfg.Sites[name] = s
	}

	stamp := time.Now().UnixNano()
	network := fmt.Sprintf("paisans-storageadd3-%d", stamp)
	t.Cleanup(func() { exec.Command("docker", "network", "rm", network).Run() })
	mustRun(t, "docker", "network", "create", "--subnet", sub, "--gateway", gw, network)

	dir := t.TempDir()
	hosts := map[string]*dockerHost{}
	for _, site := range []string{"home-a", "home-b", "vm", "store"} {
		h := &dockerHost{t: t, site: site, root: filepath.Join(dir, site), container: fmt.Sprintf("paisans-storageadd3-%s-%d", site, stamp), network: network}
		for _, d := range []string{"srv/paisans/f2a9/infra/garage/meta", "srv/paisans/f2a9/infra/garage/data"} {
			if err := os.MkdirAll(h.local("/"+d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		hosts[site] = h
	}
	create := func(h *dockerHost) {
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", h.container).Run() })
		mustRun(t, "docker", "create", "--name", h.container, "--network", h.network, "--ip", cfg.Sites[h.site].Address,
			"-v", h.local("/srv/paisans/f2a9/infra/garage/garage.toml")+":/etc/garage.toml",
			"-v", h.local("/srv/paisans/f2a9/infra/garage/meta")+":/var/lib/garage/meta",
			"-v", h.local("/srv/paisans/f2a9/infra/garage/data")+":/var/lib/garage/data",
			garageImage)
		mustRun(t, "docker", "start", h.container)
		waitAnswers(t, h)
	}

	// Before: home-a alone at replication 1, provisioned, with an object.
	cfg.Storage.Garage.Sites = []string{"home-a"}
	cfg.Storage.Garage.Replication = 1
	cfg.Storage.Garage.Capacity = "3G"
	renderFor(t, cfg, secrets, hosts, "home-a", "vm")
	create(hosts["home-a"])
	plan, err := garage.Build("home-a", cfg, secrets, hosts["home-a"])
	if err != nil {
		t.Fatal(err)
	}
	if err := garage.Execute(plan, hosts["home-a"]); err != nil {
		t.Fatal(err)
	}
	keyID, _ := garage.SecretString(secrets, "talk", "s3_access_key_id")
	secret, _ := garage.SecretString(secrets, "talk", "s3_secret_access_key")
	bucket := garage.BucketName(cfg.Apps["talk"], "talk")
	get := func(node string) (string, error) {
		conf := fmt.Sprintf("url = \"http://%s:3900/%s/before\"\nrequest = \"GET\"\nuser = \"%s:%s\"\naws-sigv4 = \"aws:amz:garage:s3\"\nheader = \"x-amz-content-sha256: UNSIGNED-PAYLOAD\"\n", node, bucket, keyID, secret)
		return hosts["home-a"].RunInput("curl -fsS --max-time 20 -K -", conf)
	}
	put := fmt.Sprintf("url = \"http://%s:3900/%s/before\"\nrequest = \"PUT\"\nuser = \"%s:%s\"\naws-sigv4 = \"aws:amz:garage:s3\"\nheader = \"x-amz-content-sha256: UNSIGNED-PAYLOAD\"\ndata-binary = \"written at replication 1\"\n", addresses["home-a"], bucket, keyID, secret)
	if out, err := hosts["home-a"].RunInput("curl -fsS --max-time 20 -K -", put); err != nil {
		t.Fatalf("writing the object: %v\n%s", err, out)
	}

	// After: three sites at replication 3, the storage host last and smaller.
	cfg.Storage.Garage.Sites = []string{"home-a", "home-b", "store"}
	cfg.Storage.Garage.Replication = 3
	cfg.Storage.Garage.Capacities = map[string]string{"store": "2G"}
	renderFor(t, cfg, secrets, hosts, "home-b", "store")
	create(hosts["home-b"])
	create(hosts["store"])

	transports := map[string]apply.Transport{
		"home-a": hosts["home-a"], "home-b": hosts["home-b"], "store": hosts["store"],
		"vm": &gatewayHost{dockerHost: *hosts["vm"], first: addresses["home-a"], buckets: mediaBuckets(cfg)},
	}
	p, err := storageadd.Build(cfg, secrets, transports, storageadd.Options{ChangeReplication: true, StopTest: true, Wait: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	rec := &ui.Recorder{Verbose_: true}
	p.Show(rec)
	p.Report = rec
	if err := storageadd.Execute(p); err != nil {
		t.Fatalf("storage add: %v\n%s", err, rec.Lines())
	}
	t.Logf("storage add:\n%s", rec.Lines())

	out, err := hosts["home-a"].Run(garage.Command(storageadd.Fixture) + " layout show")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)\sstore\s+2\.0 GB`).MatchString(out) || !regexp.MustCompile(`(?m)\shome-b\s+3\.0 GB`).MatchString(out) {
		t.Errorf("the layout does not give each site its capacity:\n%s", out)
	}
	for _, site := range []string{"home-a", "home-b", "store"} {
		if body, err := get(addresses[site]); err != nil || body != "written at replication 1" {
			t.Errorf("the object written at replication 1, read through %s: %v %q", site, err, body)
		}
	}
	if running, _ := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", hosts["store"].container).CombinedOutput(); strings.TrimSpace(string(running)) != "true" {
		t.Error("the stop test left the storage host stopped")
	}
}

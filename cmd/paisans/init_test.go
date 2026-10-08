package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/mesh"
	"github.com/paisans-software/paisans-stack/internal/registry"
)

// initFake is one site as init sees it: its registry, its networks, or no
// answer at all.
type initFake struct {
	name     string
	registry string
	routes   string
	down     bool
}

func (f *initFake) Run(command string) (string, error) {
	if f.down {
		return "ssh: connect to host " + f.name + " port 22: Operation timed out", errors.New("exit status 255")
	}
	switch command {
	case mesh.RouteCommand:
		if f.routes != "" {
			return f.routes, nil
		}
		return `[{"dst":"default","dev":"eth0"}]`, nil
	case mesh.AddrCommand, mesh.NetworksCommand:
		return "[]", nil
	}
	return "", errors.New("unexpected command " + command)
}

func (f *initFake) ReadFile(path string) (string, bool, error) {
	if f.down {
		return "", false, errors.New("ssh: connect to host " + f.name + " port 22: Operation timed out")
	}
	if path == registry.Path && f.registry != "" {
		return f.registry, true, nil
	}
	return "", false, nil
}

func (f *initFake) Describe() string { return "ubuntu@" + f.name }

const (
	exampleID = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"
	otherID   = "0c1d2e3f-4a5b-4c6d-8e7f-8091a2b3c4d5"
)

// registryHolding is a registry with one deployment on it.
func registryHolding(t *testing.T, id, token, subnet string) string {
	t.Helper()
	data, err := registry.Encode(registry.Registry{Version: registry.Version, Deployments: map[string]registry.Entry{
		id: {Token: token, Root: "/srv/paisans/" + token, Domain: "example.net", Site: "home-a", Interface: "psns-" + token, Subnet: subnet},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// initWorld copies the example declaration into a directory of its own and
// points init at fakes for its sites and at random.
func initWorld(t *testing.T, edit func(string) string, sites map[string]*initFake, random []byte) (string, *bytes.Buffer) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "paisans.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if edit != nil {
		body = edit(body)
	}
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	savedHost, savedRandom, savedOut := initHost, meshRandom, initOut
	initHost = func(site config.Site, _ bool) registry.Runner {
		for _, f := range sites {
			if f.name == site.SSH.Host {
				return f
			}
		}
		t.Fatalf("init reached an unexpected host %q", site.SSH.Host)
		return nil
	}
	meshRandom = bytes.NewReader(random)
	var out bytes.Buffer
	initOut = &out
	t.Cleanup(func() { initHost, meshRandom, initOut = savedHost, savedRandom, savedOut })
	return path, &out
}

func fakeSites() map[string]*initFake {
	return map[string]*initFake{
		"home-a": {name: "home-a.local"},
		"home-b": {name: "home-b.local"},
		"vm":     {name: "vm.example.org"},
	}
}

func runInitQuietly(t *testing.T, path string) error {
	t.Helper()
	var err error
	captureStdout(t, func() { err = runInit([]string{"--config", path, "--sudo=false"}) })
	return err
}

// The declared subnet overlaps another deployment's mesh on home-a, so init
// rolls; the first roll lands inside a route on vm and the second is clear.
// mesh.subnet and every address move, host numbers and every comment stay,
// and the output says why.
func TestInitRollsASubnetClearOfEveryHost(t *testing.T) {
	sites := fakeSites()
	sites["home-a"].registry = registryHolding(t, otherID, "0c1d", "10.44.0.0/24")
	sites["vm"].routes = `[{"dst":"10.1.0.0/16","dev":"tun0"}]`
	path, out := initWorld(t, nil, sites, []byte{1, 2, 212, 37})
	before, _ := os.ReadFile(path)

	if err := runInitQuietly(t, path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	want := strings.NewReplacer(
		"subnet: 10.44.0.0/24", "subnet: 10.212.37.0/24",
		"address: 10.44.0.1 ", "address: 10.212.37.1 ",
		"address: 10.44.0.2\n", "address: 10.212.37.2\n",
		"address: 10.44.0.3\n", "address: 10.212.37.3\n",
	).Replace(string(before))
	if string(after) != want {
		t.Errorf("init wrote:\n%s", after)
	}
	for _, line := range []string{
		"rolled mesh.subnet 10.212.37.0/24 (10.44.0.0/24 overlapped deployment " + otherID + " (example.net)'s mesh 10.44.0.0/24 on home-a; 10.1.2.0/24 overlapped route 10.1.0.0/16 dev tun0 on vm)",
		"sites.home-a.address 10.44.0.1 -> 10.212.37.1",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("init did not say %q:\n%s", line, out)
		}
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.Mesh.Subnet != "10.212.37.0/24" {
		t.Fatalf("the rewritten file loads as %v, %v", cfg, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "secrets.enc.yaml")); err != nil {
		t.Errorf("secrets were not generated after the roll: %v", err)
	}
}

// With no subnet declared, init rolls one and adds it.
func TestInitAddsAMissingSubnet(t *testing.T) {
	path, out := initWorld(t, func(s string) string {
		i := strings.Index(s, "mesh:\n")
		j := strings.Index(s, "  subnet: 10.44.0.0/24\n")
		return s[:i] + s[j+len("  subnet: 10.44.0.0/24\n"):]
	}, fakeSites(), []byte{212, 37})
	if err := runInitQuietly(t, path); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mesh.Subnet != "10.212.37.0/24" || cfg.Sites["vm"].Address != "10.212.37.3" {
		t.Errorf("subnet %s, vm %s", cfg.Mesh.Subnet, cfg.Sites["vm"].Address)
	}
	if !strings.Contains(out.String(), "(no mesh.subnet was declared)") {
		t.Errorf("init did not say why it rolled:\n%s", out)
	}
}

// A declared subnet that overlaps nothing is kept, and the file is not
// touched.
func TestInitKeepsAClearSubnet(t *testing.T) {
	path, out := initWorld(t, nil, fakeSites(), nil)
	before, _ := os.ReadFile(path)
	if err := runInitQuietly(t, path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || !strings.Contains(out.String(), "overlaps nothing on home-a, home-b, vm; kept") {
		t.Errorf("a clear subnet was not kept as it was:\n%s", out)
	}
}

// A site that does not answer makes init refuse with nothing written: no
// subnet, and no secrets either.
func TestInitRefusesWhenASiteIsUnreachable(t *testing.T) {
	sites := fakeSites()
	sites["home-b"].down = true
	path, _ := initWorld(t, nil, sites, []byte{212, 37})
	before, _ := os.ReadFile(path)
	err := runInitQuietly(t, path)
	if err == nil || !strings.Contains(err.Error(), "init reaches every site") || !strings.Contains(err.Error(), "home-b") || !strings.Contains(err.Error(), "Nothing was written") {
		t.Fatalf("init = %v, want a refusal naming home-b", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("init changed paisans.yaml although a site was unreachable")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "secrets.enc.yaml")); err == nil {
		t.Error("init generated secrets although it refused")
	}
}

// Once any site's registry records this deployment, its subnet is deployed:
// init leaves it alone even where it now overlaps something, and an
// unreachable site no longer matters to it.
func TestInitLeavesADeployedSubnetAlone(t *testing.T) {
	sites := fakeSites()
	sites["vm"].registry = registryHolding(t, exampleID, "f2a9", "10.44.0.0/24")
	sites["home-a"].routes = `[{"dst":"10.44.0.0/16","dev":"eth1"}]`
	sites["home-b"].down = true
	path, out := initWorld(t, nil, sites, []byte{212, 37})
	before, _ := os.ReadFile(path)
	if err := runInitQuietly(t, path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Errorf("init changed a deployed subnet:\n%s", after)
	}
	if !strings.Contains(out.String(), "is deployed (this deployment is on vm)") {
		t.Errorf("init did not say the subnet is deployed:\n%s", out)
	}
}

// Every roll overlapping something is a refusal naming what blocked it, and
// writes nothing.
func TestInitGivesUpAfterMaxRolls(t *testing.T) {
	sites := fakeSites()
	sites["home-a"].routes = `[{"dst":"10.0.0.0/8","dev":"tun0"}]`
	path, _ := initWorld(t, nil, sites, bytes.Repeat([]byte{7, 7}, mesh.MaxRolls))
	before, _ := os.ReadFile(path)
	err := runInitQuietly(t, path)
	if err == nil || !strings.Contains(err.Error(), "route 10.0.0.0/8 dev tun0 on home-a") {
		t.Fatalf("init = %v, want a refusal naming the route", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("init wrote paisans.yaml although it found no subnet")
	}
}

// apply and host prepare refuse a host where something outside this
// deployment's own interface overlaps the mesh, naming it, and pass one whose
// only mesh route is their own.
func TestCheckMeshLive(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	own := &initFake{name: "home-a.local", routes: `[{"dst":"10.44.0.0/24","dev":"psns-f2a9"}]`}
	if err := checkMeshLive(cfg, "home-a", own); err != nil {
		t.Errorf("the deployment's own route was refused: %v", err)
	}
	clash := &initFake{name: "home-a.local", routes: `[{"dst":"10.44.0.0/24","dev":"psns-f2a9"},{"dst":"10.44.0.0/23","dev":"psns-0c1d"}]`}
	err = checkMeshLive(cfg, "home-a", clash)
	if err == nil || !strings.Contains(err.Error(), "route 10.44.0.0/23 dev psns-0c1d on home-a") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("checkMeshLive = %v, want a refusal naming the other route", err)
	}
}

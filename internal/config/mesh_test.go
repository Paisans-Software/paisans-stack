package config_test

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/paisans-software/paisans-stack/internal/config"
)

func TestListenPortIsTheEndpointsPort(t *testing.T) {
	for endpoint, want := range map[string]int{
		"vm.example.org:51820":  51820,
		"vm.example.org:51821":  51821,
		"[2001:db8::1]:4500":    4500,
		"":                      0,
		"vm.example.org":        0,
		"vm.example.org:99999":  0,
		"vm.example.org:wg-udp": 0,
	} {
		if got := (config.Site{Endpoint: endpoint}).ListenPort(); got != want {
			t.Errorf("ListenPort(%q) = %d, want %d", endpoint, got, want)
		}
	}
}

func TestLoadRefusesAnEndpointWithoutAPort(t *testing.T) {
	body := strings.Replace(minimal, "    address: 10.44.0.1\n", "    address: 10.44.0.1\n    endpoint: home-a.example.org\n", 1)
	_, err := config.Load(write(t, body))
	if err == nil || !strings.Contains(err.Error(), "sites.home-a.endpoint") {
		t.Fatalf("Load = %v, want a refusal of the endpoint", err)
	}
}

func TestAMissingSubnetSendsTheOperatorToInit(t *testing.T) {
	body := strings.Replace(minimal, "mesh:\n  subnet: 10.44.0.0/24\n", "", 1)
	path := write(t, body)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "mesh.subnet: required. Run `paisans init`") {
		t.Fatalf("Load = %v, want a refusal sending the operator to init", err)
	}
	cfg, err := config.LoadForInit(path)
	if err != nil {
		t.Fatalf("LoadForInit refused a file whose only gap is the subnet: %v", err)
	}
	if cfg.Mesh.Subnet != "" {
		t.Errorf("subnet = %q", cfg.Mesh.Subnet)
	}
	bad := strings.Replace(minimal, "10.44.0.0/24", "10.44.0.0", 1)
	if _, err := config.LoadForInit(write(t, bad)); err == nil {
		t.Error("LoadForInit accepted a malformed subnet")
	}
}

const meshFile = `version: 1
id: f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01   # never changes

# the private network
mesh:
  # picked by init
  subnet: 10.44.0.0/24   # keep
sites:
  home-a:
    roles: [data]
    address: 10.44.0.1          # WireGuard address
  vm:
    address: "10.44.0.3"
    endpoint: vm.example.org:51820
`

func TestSetMeshKeepsEveryOtherByte(t *testing.T) {
	path := write(t, meshFile)
	if err := config.SetMesh(path, "10.212.37.0/24", map[string]string{"home-a": "10.212.37.1", "vm": "10.212.37.3"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := strings.NewReplacer(
		"subnet: 10.44.0.0/24", "subnet: 10.212.37.0/24",
		"address: 10.44.0.1 ", "address: 10.212.37.1 ",
		`address: "10.44.0.3"`, `address: "10.212.37.3"`,
	).Replace(meshFile)
	if string(got) != want {
		t.Errorf("SetMesh wrote:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetMeshAddsAMissingSubnet(t *testing.T) {
	for name, tc := range map[string]struct{ from, to string }{
		"no mesh block": {
			from: "mesh:\n  # picked by init\n  subnet: 10.44.0.0/24   # keep\n",
			to:   "",
		},
		"an empty mesh key": {
			from: "  # picked by init\n  subnet: 10.44.0.0/24   # keep\n",
			to:   "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(meshFile, tc.from, tc.to, 1)
			path := write(t, body)
			if err := config.SetMesh(path, "10.212.37.0/24", map[string]string{"home-a": "10.212.37.1"}); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			var cfg config.Config
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				t.Fatalf("the result does not parse: %v\n%s", err, data)
			}
			if cfg.Mesh.Subnet != "10.212.37.0/24" || cfg.Sites["home-a"].Address != "10.212.37.1" || cfg.Sites["vm"].Address != "10.44.0.3" {
				t.Errorf("loaded %+v", cfg)
			}
			if !strings.Contains(string(data), "# WireGuard address") || !strings.Contains(string(data), "# never changes") {
				t.Errorf("a comment was lost:\n%s", data)
			}
		})
	}
}

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
)

// inventoryHost answers only the host check's probes, and records every
// command, so a test can see that nothing but the inventory ran.
type inventoryHost struct {
	answers map[string]string
	ran     []string
}

func (h *inventoryHost) Describe() string { return "ubuntu@host.example.org" }
func (h *inventoryHost) Run(command string) (string, error) {
	h.ran = append(h.ran, command)
	for key, out := range h.answers {
		if strings.Contains(command, key) {
			return out, nil
		}
	}
	return "", fmt.Errorf("not an inventory probe: %s", command)
}
func (h *inventoryHost) ReadFile(string) (string, bool, error) { return "", false, nil }

// webHost runs somebody's web server on 80 and 443, outside Docker.
func webHost(ufw string) *inventoryHost {
	return &inventoryHost{answers: map[string]string{
		"id -u":                  "0\n",
		"docker version":         "absent\n",
		"dpkg-query":             "",
		"ss -Hltnup":             "tcp LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:((\"nginx\",pid=900,fd=6))\ntcp LISTEN 0 511 0.0.0.0:443 0.0.0.0:* users:((\"nginx\",pid=900,fd=7))\n",
		"/proc/":                 "900 0::/system.slice/nginx.service \n",
		"ip -o link":             "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536\n2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500\n",
		"ip -j route":            `[{"dst":"default","dev":"eth0"}]`,
		"ufw status verbose":     ufw,
		"is-active firewalld":    "inactive\n",
		"docker inspect":         "",
		"docker volume inspect":  "",
		"docker network inspect": "",
	}}
}

func hostCheckFixture(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("../../internal/hostcheck/testdata/hostcheck.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const ufwUp = "Status: active\nDefault: deny (incoming), allow (outgoing), disabled (routed)\n"

// A conflict is refused with its report printed, and nothing but the
// inventory's own probes reached the host.
func TestTheGateRefusesAConflictHavingOnlyLooked(t *testing.T) {
	h := webHost(ufwUp)
	var out bytes.Buffer
	_, err := hostGate(&out, hostCheckFixture(t), "edge", h)
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(out.String(), "CONFLICT  *:80/tcp (Caddy): claimed by sites.edge.roles (gateway), held by process nginx (pid 900)") {
		t.Errorf("the report does not name the conflict:\n%s", out.String())
	}
	probes := []string{"id -u", "docker version", "dpkg-query", "ss -Hltnup", "/proc/", "ip -o link", "ip -j route", "ufw status verbose", "is-active firewalld"}
	for _, c := range h.ran {
		known := false
		for _, p := range probes {
			known = known || strings.Contains(c, p)
		}
		if !known {
			t.Errorf("the gate ran something other than the inventory: %s", c)
		}
	}
}

// A shared host is let through only with its firewall already up.
func TestTheGateNeedsASharedHostsFirewall(t *testing.T) {
	cfg := hostCheckFixture(t)
	if _, err := hostGate(&bytes.Buffer{}, cfg, "home-a", webHost("Status: inactive\n")); err == nil || !strings.Contains(err.Error(), "ufw is inactive") {
		t.Errorf("got %v", err)
	}
	report, err := hostGate(&bytes.Buffer{}, cfg, "home-a", webHost(ufwUp))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Shared() {
		t.Error("a host with a foreign web server is not shared")
	}
}

// A command that changes several sites checks each before any changes,
// and learns which are shared.
func TestGateSitesChecksEachAndNamesTheShared(t *testing.T) {
	cfg := hostCheckFixture(t)
	hosts := map[string]*inventoryHost{"home-a": webHost(ufwUp), "edge": webHost(ufwUp)}
	reach := func(site string) hostcheck.Transport { return hosts[site] }
	if _, err := gateSites(&bytes.Buffer{}, cfg, []string{"home-a", "edge"}, reach); err == nil {
		t.Fatal("a conflict on edge was not refused")
	}
	shared, err := gateSites(&bytes.Buffer{}, cfg, []string{"home-a"}, reach)
	if err != nil {
		t.Fatal(err)
	}
	if !shared["home-a"] {
		t.Errorf("shared %v", shared)
	}
}

// rotate-key's switch is an apply, and on a shared site it keeps images as
// apply does there.
func TestTheRotateKeySwitchKeepsImagesOnASharedSite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(freshSite), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &config.Secrets{Version: 1}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatal(err)
	}
	transports := map[string]apply.Transport{"home-a": emptyHost{}}
	for shared, want := range map[bool]bool{true: true, false: false} {
		s := applySwitch{cfg: cfg, app: "talk", transports: transports, shared: map[string]bool{"home-a": shared}}
		plan, err := s.plan("home-a", secrets)
		if err != nil {
			t.Fatal(err)
		}
		if plan.KeepImages != want {
			t.Errorf("shared %v: keep images %v", shared, plan.KeepImages)
		}
	}
}

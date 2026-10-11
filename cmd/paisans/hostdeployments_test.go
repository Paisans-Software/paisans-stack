package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

const (
	monitorID = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"
	neighbour = "0c1d2e3f-4a5b-4c6d-8e7f-8091a2b3c4d5"
	strayID   = "dead0000-1111-4222-8333-444455556666"
)

// listedHost is a host that answers its registry, `true` and the
// deployments probe, and fails anything else, which no command here may run.
type listedHost struct {
	t        *testing.T
	registry string
	probe    string
	commands []string
	// readErr fails the registry's read, and probeErr the probe, which
	// then answers probe as its output.
	readErr, probeErr error
}

func (h *listedHost) Describe() string { return "admin@192.0.2.30" }
func (h *listedHost) ReadFile(path string) (string, bool, error) {
	if path == registry.Path {
		return h.registry, h.registry != "", h.readErr
	}
	return "", false, nil
}
func (h *listedHost) WriteFile(string, string, uint32) error {
	h.t.Error("a file was written")
	return nil
}
func (h *listedHost) RunInput(command, _ string) (string, error) { return h.Run(command) }
func (h *listedHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	switch {
	case command == "true":
		return "", nil
	case command == deploymentsProbe:
		return h.probe, h.probeErr
	}
	h.t.Errorf("unexpected command %q", command)
	return "", nil
}

func twoDeployments(t *testing.T) string {
	data, err := registry.Encode(registry.Registry{Version: registry.Version, Deployments: map[string]registry.Entry{
		monitorID: {Token: "f2a9", Root: "/srv/paisans/f2a9", Domain: "example.org", Site: "watch", Roles: "monitor", ClaimedAt: "2026-10-01T00:00:00Z", Interface: "psns-f2a9", Subnet: "10.44.0.0/24", Address: "10.44.0.6"},
		neighbour: {Token: "0c1d", Root: "/srv/paisans/0c1d", Domain: "example.net", Site: "edge", Roles: "apps,gateway", ClaimedAt: "2026-10-01T00:00:00Z", Interface: "psns-0c1d", Subnet: "10.45.0.0/24", Address: "10.45.0.2"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// withHost stands h in for every host a command without a configuration
// reaches.
func withHost(t *testing.T, h apply.Transport) {
	t.Helper()
	saved := reachDestination
	reachDestination = func(config.Destination, bool) apply.Transport { return h }
	t.Cleanup(func() { reachDestination = saved })
}

func plainOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	reporterOverride = ui.NewPlain(&b, false)
	t.Cleanup(func() { reporterOverride = nil })
	return &b
}

// host deployments lists each registry entry in full, then what of a
// deployment is on the host with no entry, and changes nothing.
func TestHostDeploymentsListsEntriesAndLeftovers(t *testing.T) {
	h := &listedHost{t: t, registry: twoDeployments(t), probe: strings.Join([]string{
		"root f2a9", "root 0c1d", "root dead",
		"container\t" + monitorID + "\tpaisans-f2a9-status\tapp",
		"container\t" + strayID + "\tpaisans-dead-talk\tapp",
		"container\t" + strayID + "\tpaisans-dead-talk\tapp",
		"end",
	}, "\n") + "\n"}
	withHost(t, h)
	out := plainOutput(t)
	if err := runHostDeployments([]string{"--ssh", "admin@192.0.2.30"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	// The host is read in a step, so a spinner shows while ssh answers.
	if !strings.Contains(got, "ok   read admin@192.0.2.30:22") {
		t.Errorf("the read is not a step:\n%s", got)
	}
	for _, want := range []string{
		monitorID, "token f2a9", "example.org", "site watch", "roles monitor", "root /srv/paisans/f2a9",
		neighbour, "token 0c1d", "example.net", "site edge", "roles apps,gateway", "root /srv/paisans/0c1d",
		"  ok   " + monitorID + ": token f2a9",
		"  ok   " + neighbour + ": token 0c1d",
		"  WARN /srv/paisans/dead: no registry entry",
		"  WARN compose project paisans-dead-talk: no registry entry",
		"--id <id or token>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "paisans-dead-talk") != 1 {
		t.Errorf("a project is listed once per container:\n%s", got)
	}
	if strings.Contains(got, "/srv/paisans/f2a9: no registry entry") || strings.Contains(got, "paisans-f2a9-status") {
		t.Errorf("a registered deployment is listed as a leftover:\n%s", got)
	}
}

func TestHostDeploymentsOnAnEmptyHost(t *testing.T) {
	withHost(t, &listedHost{t: t, probe: "end\n"})
	out := plainOutput(t)
	if err := runHostDeployments([]string{"--ssh", "admin@192.0.2.30", "--sudo=false"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No deployment") {
		t.Errorf("printed:\n%s", out.String())
	}
}

func TestHostDeploymentsRefusals(t *testing.T) {
	noSiteHosts(t)
	for args, want := range map[string]string{
		"":                         "--ssh",
		"--ssh myalias":            "not user@host",
		"--ssh admin@192.0.2.30 x": "extra",
	} {
		err := runHostDeployments(strings.Fields(args))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", args, err, want)
		}
	}
}

// --id needs no configuration, and every refusal that needs no host reaches
// none.
func TestSiteRemoveByIDRefusesBeforeReachingAHost(t *testing.T) {
	noSiteHosts(t)
	for args, want := range map[string]string{
		"--ssh admin@192.0.2.30 --id f2a9": "add --force",
		"--force --id f2a9":                "--ssh",
		"--force --ssh admin@192.0.2.30 --id f2a9 --config paisans.yaml":   "--config",
		"--force --ssh admin@192.0.2.30 --id f2a9 --secrets s.enc.yaml":    "--secrets",
		"watch --force --ssh admin@192.0.2.30 --id f2a9":                   "names the site",
		"--force --ssh admin@192.0.2.30 --id f2a":                          "Eg: --id f2a9",
		"--force --ssh admin@192.0.2.30 --id F2A9":                         "Eg: --id f2a9",
		"--force --ssh myalias --id f2a9":                                  "not user@host",
		"--force --ssh admin@192.0.2.30 --id f2a9 --host-gone":             "Drop one of them",
		"--force --ssh admin@192.0.2.30 --id f2a9 --execute":               "terminal",
		"--force --ssh admin@192.0.2.30 --id f2a9 --execute --delete-data": "terminal",
	} {
		err := runSiteRemove(strings.Fields(args), strings.NewReader("watch\n"), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
		// Each is a short hint and an explanation, as main prints it.
		var p *ui.Problem
		if !errors.As(err, &p) || len(p.Hint) > 80 || p.Explain == "" || strings.HasPrefix(p.Hint, "site remove") {
			t.Errorf("%s: not the Problem main prints: %#v", args, p)
		}
	}
}

// No match and several are refused with what the host holds, and nothing
// past the registry is read.
func TestSiteRemoveByIDRefusesNoMatchAndSeveral(t *testing.T) {
	h := &listedHost{t: t, registry: twoDeployments(t)}
	withHost(t, h)
	plainOutput(t)
	err := runSiteRemove([]string{"--force", "--ssh", "admin@192.0.2.30", "--id", "beef"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no deployment") || !strings.Contains(err.Error(), neighbour) || !strings.Contains(err.Error(), monitorID) {
		t.Errorf("no match: %v", err)
	}
	r, _ := registry.Parse([]byte(h.registry))
	r.Deployments["f2a91111-2222-4333-8444-555566667777"] = registry.Entry{Token: "f2a9", Root: "/srv/paisans/f2a9", Domain: "example.com", Site: "y", Roles: "monitor", ClaimedAt: "2026-10-01T00:00:00Z", Interface: "psns-f2a9", Subnet: "10.46.0.0/24", Address: "10.46.0.2"}
	data, _ := registry.Encode(r)
	h.registry = string(data)
	err = runSiteRemove([]string{"--force", "--ssh", "admin@192.0.2.30", "--id", "f2a9"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "full id") {
		t.Errorf("several: %v", err)
	}
	for _, c := range h.commands {
		if c != "true" {
			t.Errorf("ran %q", c)
		}
	}
}

// A host whose registry or probe cannot be read is an error naming what
// failed, and nothing is listed: an empty or partial listing would read as a
// host holding nothing.
func TestHostDeploymentsFailsRatherThanListingPartly(t *testing.T) {
	good := "root f2a9\nend\n"
	for name, tc := range map[string]struct {
		h    *listedHost
		want string
	}{
		"registry unreadable":    {&listedHost{readErr: errors.New("exit status 1: Permission denied"), probe: good}, registry.Path},
		"registry malformed":     {&listedHost{registry: "not json", probe: good}, registry.Path},
		"registry dir hidden":    {&listedHost{probe: "cannot read /var/lib/paisans\n", probeErr: errors.New("exit status 3")}, "cannot read /var/lib/paisans"},
		"deployments dir hidden": {&listedHost{probe: "cannot read /srv/paisans\n", probeErr: errors.New("exit status 3")}, "cannot read /srv/paisans"},
		"docker ps failed":       {&listedHost{probe: "root f2a9\ndocker ps failed: Cannot connect to the Docker daemon\n", probeErr: errors.New("exit status 4")}, "docker ps failed"},
		"cut short":              {&listedHost{probe: "root f2a9\n"}, "cut short"},
		"unreadable line":        {&listedHost{probe: "root f2a9\nsomething else\nend\n"}, "something else"},
	} {
		tc.h.t = t
		if tc.h.registry == "" && tc.h.readErr == nil {
			tc.h.registry = twoDeployments(t)
		}
		withHost(t, tc.h)
		out := plainOutput(t)
		err := runHostDeployments([]string{"--ssh", "admin@192.0.2.30"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		// The read is a step, so its failed line shows; nothing is listed.
		if strings.Contains(out.String(), "token ") || strings.Contains(out.String(), "no registry entry") {
			t.Errorf("%s: printed a listing:\n%s", name, out.String())
		}
	}
}

// An entry whose Caddy is kept shows it, and the owner's files it serves.
func TestHostDeploymentsShowsAKeptCaddy(t *testing.T) {
	r, _ := registry.Parse([]byte(twoDeployments(t)))
	r.Deployments[neighbour] = registry.KeepCaddy(r.Deployments[neighbour])
	data, _ := registry.Encode(r)
	withHost(t, &listedHost{t: t, registry: string(data), probe: "root 0c1d\nsite a.caddy\nsite b.caddy\ncontainer\t" + neighbour + "\tpaisans-0c1d-infra\tcaddy\nend\n"})
	out := plainOutput(t)
	if err := runHostDeployments([]string{"--ssh", "admin@192.0.2.30"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "caddy kept: serves /srv/caddy.d sites (a.caddy, b.caddy)") {
		t.Errorf("no kept Caddy in:\n%s", got)
	}
	if strings.Count(got, "caddy kept") != 1 {
		t.Errorf("the kept Caddy is shown for another entry too:\n%s", got)
	}
}

// A Caddy labelled for a deployment with no registry entry, while the
// owner's sites are there, is an orphaned Caddy; the same Caddy with its
// entry is shown with the entry, and nothing else changes.
func TestHostDeploymentsFlagsAnOrphanedCaddy(t *testing.T) {
	probe := "root dead\nsite a.caddy\nsite b.caddy\ncontainer\t" + strayID + "\tpaisans-dead-infra\tcaddy\ncontainer\t" + strayID + "\tpaisans-dead-talk\tapp\nend\n"
	withHost(t, &listedHost{t: t, registry: twoDeployments(t), probe: probe})
	out := plainOutput(t)
	if err := runHostDeployments([]string{"--ssh", "admin@192.0.2.30"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		strayID + " orphaned Caddy: serves /srv/caddy.d sites (a.caddy, b.caddy); its registry entry was removed, so remove it by hand once those sites have moved",
		"compose project paisans-dead-talk: no registry entry",
		"/srv/paisans/dead: no registry entry",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "compose project paisans-dead-infra") {
		t.Errorf("the orphaned Caddy is also listed as a plain leftover:\n%s", got)
	}

	// No site of the owner's: a plain leftover, not a Caddy kept for anything.
	withHost(t, &listedHost{t: t, registry: twoDeployments(t), probe: "container\t" + strayID + "\tpaisans-dead-infra\tcaddy\nend\n"})
	out = plainOutput(t)
	if err := runHostDeployments([]string{"--ssh", "admin@192.0.2.30"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "orphaned") || !strings.Contains(got, "compose project paisans-dead-infra: no registry entry") {
		t.Errorf("with no site of the owner's:\n%s", got)
	}

	// Its entry is there: shown with the entry, not as orphaned.
	r, _ := registry.Parse([]byte(twoDeployments(t)))
	r.Deployments[neighbour] = registry.KeepCaddy(r.Deployments[neighbour])
	data, _ := registry.Encode(r)
	withHost(t, &listedHost{t: t, registry: string(data), probe: "root 0c1d\nsite a.caddy\ncontainer\t" + neighbour + "\tpaisans-0c1d-infra\tcaddy\nend\n"})
	out = plainOutput(t)
	if err := runHostDeployments([]string{"--ssh", "admin@192.0.2.30"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "orphaned") || strings.Contains(got, "not in the registry") || !strings.Contains(got, "caddy kept: serves /srv/caddy.d sites (a.caddy)") {
		t.Errorf("a kept Caddy with its entry:\n%s", got)
	}
}

// A kept Caddy whose sites are gone says the next removal takes it.
func TestHostDeploymentsKeptCaddyWithNoSitesLeft(t *testing.T) {
	r, _ := registry.Parse([]byte(twoDeployments(t)))
	r.Deployments[neighbour] = registry.KeepCaddy(r.Deployments[neighbour])
	data, _ := registry.Encode(r)
	withHost(t, &listedHost{t: t, registry: string(data), probe: "root 0c1d\nend\n"})
	out := plainOutput(t)
	if err := runHostDeployments([]string{"--ssh", "admin@192.0.2.30"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "caddy kept, but /srv/caddy.d holds no site now: paisans site remove --force --ssh admin@192.0.2.30 --id 0c1d removes it") {
		t.Errorf("printed:\n%s", got)
	}
}

// A host named only by --ssh is reached the way the operator's own ssh
// reaches it, with their agent, keys and config: there are no declared keys
// to offer, since there is no configuration.
func TestReachDestinationUsesTheOperatorsSSH(t *testing.T) {
	for in, want := range map[string]string{
		"ubuntu@192.0.2.10":         "ubuntu@192.0.2.10",
		"ubuntu@192.0.2.10:2222":    "ssh://ubuntu@192.0.2.10:2222",
		"ubuntu@[2001:db8::1]:2222": "ssh://ubuntu@[2001:db8::1]:2222",
	} {
		dest, err := config.ParseDestination(in)
		if err != nil {
			t.Fatal(err)
		}
		tr, ok := reachDestination(dest, true).(apply.SSHTransport)
		if !ok || tr.Destination != want || !tr.Sudo || tr.Auth == nil {
			t.Errorf("%s: %+v, want destination %s with sudo", in, tr, want)
		}
	}
}

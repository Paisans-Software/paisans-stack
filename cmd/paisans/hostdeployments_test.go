package main

import (
	"bytes"
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
}

func (h *listedHost) Describe() string { return "admin@192.0.2.30" }
func (h *listedHost) ReadFile(path string) (string, bool, error) {
	if path == registry.Path {
		return h.registry, h.registry != "", nil
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
		return h.probe, nil
	}
	h.t.Errorf("unexpected command %q", command)
	return "", nil
}

func twoDeployments(t *testing.T) string {
	data, err := registry.Encode(registry.Registry{Version: registry.Version, Deployments: map[string]registry.Entry{
		monitorID: {Token: "f2a9", Root: "/srv/paisans/f2a9", Domain: "example.org", Site: "watch", Roles: "monitor"},
		neighbour: {Token: "0c1d", Root: "/srv/paisans/0c1d", Domain: "example.net", Site: "edge", Roles: "apps,gateway"},
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
		"container " + monitorID + " paisans-f2a9-status",
		"container " + strayID + " paisans-dead-talk",
		"container " + strayID + " paisans-dead-talk",
	}, "\n") + "\n"}
	withHost(t, h)
	out := plainOutput(t)
	if err := runHostDeployments([]string{"--ssh", "admin@192.0.2.30"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		monitorID, "token f2a9", "example.org", "site watch", "roles monitor", "root /srv/paisans/f2a9",
		neighbour, "token 0c1d", "example.net", "site edge", "roles apps,gateway", "root /srv/paisans/0c1d",
		"/srv/paisans/dead: no registry entry",
		"compose project paisans-dead-talk: no registry entry",
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
	withHost(t, &listedHost{t: t})
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
	r.Deployments["f2a91111-2222-4333-8444-555566667777"] = registry.Entry{Token: "f2a9", Root: "/srv/paisans/f2a9", Domain: "example.com", Site: "y"}
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

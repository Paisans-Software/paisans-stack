package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// A gateway's apply creates the host's own Caddy site directory before the
// first container that mounts it, with a command that leaves an existing one
// alone, and never writes into it or records it. A site without the gateway
// role never asks.
func TestTheGatewayCreatesTheHostsSiteDirectoryAndNothingInIt(t *testing.T) {
	host := newHost()
	p, err := apply.Build("vm", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if !p.HostSites {
		t.Fatal("the gateway's plan does not create the host's Caddy site directory")
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	ensure, validate := -1, -1
	for i, c := range host.commands {
		if c == apply.HostSitesCommand() && ensure < 0 {
			ensure = i
		}
		if strings.Contains(c, "caddy validate") && validate < 0 {
			validate = i
		}
	}
	if ensure < 0 || validate < 0 || ensure > validate {
		t.Fatalf("the directory is not created before the validation mounts it: %q", host.commands)
	}
	if want := "[ -d " + render.HostSitesDir + " ] || install -d -m 755 -o root -g root " + render.HostSitesDir; apply.HostSitesCommand() != want {
		t.Errorf("HostSitesCommand = %q, want %q", apply.HostSitesCommand(), want)
	}
	for path := range host.files {
		if strings.HasPrefix(path, render.HostSitesDir+"/") {
			t.Errorf("apply wrote %s into the host's own directory", path)
		}
	}
	if strings.Contains(host.files["/srv/paisans/f2a9/.paisans-manifest.json"], render.HostSitesDir) {
		t.Error("the manifest records the host's own directory")
	}

	other := newHost()
	q, err := apply.Build("home-a", plan(t), acmeModule(t), other)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(q, other); err != nil {
		t.Fatal(err)
	}
	if q.HostSites || other.ran(render.HostSitesDir) {
		t.Error("a site without the gateway role touched the host's Caddy site directory")
	}
}

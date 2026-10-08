package siteremove_test

import (
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

// gatewayWorld has a second gateway on home-c, so vm, a gateway and the
// witness, may go; ownerSites puts a site block of the host owner's on vm.
func gatewayWorld(t *testing.T, ownerSites bool) *world {
	w := newWorld(t,
		replace("  home-c:\n    roles: [data]", "  home-c:\n    roles: [data, gateway]"),
		replace("placement: { pinned: vm }", "placement: { pinned: box }"),
		replace("  vm:\n    roles: [gateway, witness]", "  box:\n    roles: [apps]\n    address: 10.44.0.6\n    ssh:\n      host: box.local\n      user: ubuntu\n      public_key: |\n        "+alice+"\n  vm:\n    roles: [gateway, witness]"),
	)
	t.Cleanup(siteremove.SetFast())
	t.Cleanup(siteremove.SetInspect(w.inspect))
	t.Cleanup(siteremove.SetNow(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)))
	vm := w.hosts["vm"]
	vm.containers = append(vm.containers, hostcheck.Container{Name: "paisans-f2a9-infra-caddy-1", Project: "paisans-f2a9-infra", Service: "caddy", Deployment: ourID, PID: 12, Networks: []string{"host"}})
	vm.files[root+"/infra/caddy/data/caddy/certificates/blog.example.net.crt"] = "CERT"
	vm.files[root+"/infra/caddy/config/caddy/autosave.json"] = "{}"
	if ownerSites {
		vm.files["/srv/caddy.d/blog.caddy"] = "blog.example.net {\n\timport upstream_unavailable\n\trespond \"hi\"\n}\n"
	}
	return w
}

func TestAGatewaysCaddyIsHandedOverWhenSomebodyReliesOnIt(t *testing.T) {
	w := gatewayWorld(t, true)
	p := w.mustBuild("vm", siteremove.Options{})
	if !hasStep(p, 3, "vm", "hand over", "/srv/caddy.d/blog.caddy") || !hasStep(p, 3, "vm", "copy", "DNS provider token") {
		t.Fatalf("no hand over planned:\n%s", printed(p))
	}
	t.Log("\n" + printed(p) + "\n" + strings.Join(p.Remains(), "\n"))
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	vm := w.hosts["vm"]
	compose, caddyfile := vm.files["/srv/caddy/compose.yaml"], vm.files["/srv/caddy/Caddyfile"]
	for _, want := range []string{"name: caddy\n", "network_mode: host", "/srv/caddy.d:/etc/caddy.d:ro", "env_file:", "image: "} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose.yaml has no %q:\n%s", want, compose)
		}
	}
	if strings.Contains(compose, "community.paisans.deployment") {
		t.Error("the handed over Caddy carries the deployment label")
	}
	for _, want := range []string{"acme_dns desec", "(upstream_unavailable) {", "(upstream_single) {", "import /etc/caddy.d/*.caddy", "never reads or\n# changes this directory again"} {
		if !strings.Contains(caddyfile, want) {
			t.Errorf("the Caddyfile has no %q:\n%s", want, caddyfile)
		}
	}
	if vm.files["/srv/caddy/data/caddy/certificates/blog.example.net.crt"] != "CERT" || vm.under(root+"/infra/caddy/data") {
		t.Error("the certificates did not move")
	}
	if _, ok := vm.files["/srv/caddy/caddy.env"]; !ok {
		t.Error("the token was not copied")
	}
	if m := vm.files["/srv/caddy/HANDED-OVER"]; !strings.Contains(m, "deployment: "+ourID+"\n") || !strings.Contains(m, "site: vm\n") || !strings.Contains(m, "handed_over_at: 2026-10-08T12:00:00Z") {
		t.Errorf("the marker:\n%s", m)
	}
	labelled, handed := 0, 0
	for _, c := range vm.containers {
		if c.Deployment == ourID {
			labelled++
		}
		if c.Project == "caddy" {
			handed++
		}
	}
	if labelled != 0 || handed != 1 {
		t.Errorf("containers after: %v", vm.containers)
	}
	if _, ok := vm.files["/srv/caddy.d/blog.caddy"]; !ok {
		t.Error("the owner's site block was touched")
	}
	if !strings.Contains(strings.Join(p.Remains(), "\n"), "a credential this deployment no longer controls") {
		t.Error("the report does not say the token was left behind")
	}
}

func TestAGatewayNobodyReliesOnIsRemovedPlainly(t *testing.T) {
	w := gatewayWorld(t, false)
	p := w.mustBuild("vm", siteremove.Options{})
	if hasStep(p, 3, "vm", "hand over", "") {
		t.Fatalf("a hand over is planned with nothing relying on Caddy")
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	for path := range w.hosts["vm"].files {
		if strings.HasPrefix(path, "/srv/caddy/") {
			t.Errorf("%s was written", path)
		}
	}
	if w.hosts["vm"].ran("/srv/caddy/compose.yaml") != 0 {
		t.Error("a handed over Caddy was started")
	}
}

// A handed over Caddy that does not come up is undone: the certificates go
// back and this deployment's Caddy is started again, and the next run
// resumes.
func TestAFailedHandOverIsUndoneAndResumes(t *testing.T) {
	w := gatewayWorld(t, true)
	w.failOnce = "/srv/caddy/compose.yaml up -d"
	err := siteremove.Execute(w.mustBuild("vm", siteremove.Options{}))
	if err == nil || !strings.Contains(err.Error(), "started again") {
		t.Fatalf("err = %v", err)
	}
	vm := w.hosts["vm"]
	if !vm.under(root+"/infra/caddy/data") || vm.under("/srv/caddy/data") {
		t.Error("the certificates were not put back")
	}
	if vm.ran("/srv/paisans/f2a9/infra/compose.yaml up -d caddy") == 0 {
		t.Error("this deployment's Caddy was not started again")
	}
	if _, ok := vm.files["/srv/caddy/HANDED-OVER"]; ok {
		t.Error("a failed hand over left a marker")
	}
	if err := siteremove.Execute(w.mustBuild("vm", siteremove.Options{})); err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if _, ok := vm.files["/srv/caddy/HANDED-OVER"]; !ok {
		t.Error("the resumed hand over left no marker")
	}
}

// A configuration that does not validate stops the hand over before
// anything is stopped.
func TestAnInvalidHandOverStopsNothing(t *testing.T) {
	w := gatewayWorld(t, true)
	w.caddyBroken = true
	if err := siteremove.Execute(w.mustBuild("vm", siteremove.Options{})); err == nil || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Fatalf("err = %v", err)
	}
	if w.hosts["vm"].ran("stop caddy") != 0 || w.hosts["vm"].ran("docker ps -aq") != 0 {
		t.Error("something was stopped")
	}
}

// /srv/caddy holding something the hand over does not write is somebody
// else's, and so is a marker by another deployment.
func TestAHandOverIntoSomethingElseIsRefused(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"a foreign file":   {"/srv/caddy/notes.txt": "mine"},
		"a foreign marker": {"/srv/caddy/HANDED-OVER": "deployment: " + otherID + "\n"},
		"an edited file":   {"/srv/caddy/Caddyfile": "{\n}\n"},
	} {
		t.Run(name, func(t *testing.T) {
			w := gatewayWorld(t, true)
			for p, c := range files {
				w.hosts["vm"].files[p] = c
			}
			if _, err := w.build("vm", siteremove.Options{}); err == nil || !strings.Contains(err.Error(), "Nothing was changed") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

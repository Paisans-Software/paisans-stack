package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/dns"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// noSiteHosts fails the test if `site remove` reaches for any host.
func noSiteHosts(t *testing.T) {
	t.Helper()
	saved := removeSiteHost
	removeSiteHost = func(string, config.Site, bool) apply.Transport {
		t.Fatal("a host was reached")
		return nil
	}
	t.Cleanup(func() { removeSiteHost = saved })
	savedDest := reachDestination
	reachDestination = func(config.Destination, bool) apply.Transport {
		t.Fatal("a host was reached")
		return nil
	}
	t.Cleanup(func() { reachDestination = savedDest })
}

// Deleting member data needs a person at a terminal, and there is no flag
// standing in for one. Refused before any host is read.
func TestSiteRemoveDeleteDataNeedsATerminal(t *testing.T) {
	noSiteHosts(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString("home-b\n")
	w.Close()
	defer r.Close()
	err = runSiteRemove([]string{"home-b", "--config", fixtureConfig(), "--delete-data", "--execute"}, r, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "stdin is not a terminal") {
		t.Errorf("err = %v", err)
	}
}

// A refusal from the configuration alone reaches no host either.
func TestSiteRemoveRefusesBeforeReachingAHost(t *testing.T) {
	noSiteHosts(t)
	for args, want := range map[string]string{
		"home-a":                                 "pinned to it",
		"home-z":                                 "declares no site",
		"home-b --host-gone --delete-data":       "Drop one of them",
		"home-b --execute --host-gone extra-arg": "one site",
	} {
		err := runSiteRemove(append(strings.Fields(args), "--config", fixtureConfig()), strings.NewReader(""), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
}

func TestConfirmSiteWantsTheSitesName(t *testing.T) {
	saved := isTerminal
	isTerminal = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminal = saved })
	for answer, ok := range map[string]bool{"home-b\n": true, "yes\n": false, "": false} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		w.WriteString(answer)
		w.Close()
		err = confirmSite(r, &bytes.Buffer{}, "home-b")
		r.Close()
		if (err == nil) != ok {
			t.Errorf("answer %q: %v", answer, err)
		}
	}
}

func TestChooseHost(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	declared := cfg.Sites["home-b"].Destination().String()
	for _, tc := range []struct{ site, ssh, want, err string }{
		{site: "home-b", want: declared},
		{site: "home-b", ssh: "admin@198.51.100.4", want: "admin@198.51.100.4:22"},
		{site: "never", ssh: "admin@192.0.2.1:2222", want: "admin@192.0.2.1:2222"},
		{site: "never", err: "Name its host with --ssh"},
		{site: "home-b", ssh: "myalias", err: "not user@host"},
	} {
		got, err := chooseHost(cfg, tc.site, tc.ssh)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s %s: err = %v, want %q", tc.site, tc.ssh, err, tc.err)
		case tc.err == "" && (err != nil || got.String() != tc.want):
			t.Errorf("%s %s: got %v, %v; want %s", tc.site, tc.ssh, got, err, tc.want)
		}
	}
}

// Refusals that need no host reach none.
func TestSiteRemoveForceRefusesBeforeReachingAHost(t *testing.T) {
	noSiteHosts(t)
	for args, want := range map[string]string{
		"home-b --ssh admin@192.0.2.1":                                "only --force takes",
		"home-b --force --host-gone":                                  "Drop one of them",
		"never --force":                                               "Name its host with --ssh",
		"home-b --force --ssh myalias":                                "not user@host",
		"home-b --force --execute":                                    "terminal",
		"never --force --ssh admin@192.0.2.1 --execute --delete-data": "terminal",
	} {
		err := runSiteRemove(append(strings.Fields(args), "--config", fixtureConfig()), strings.NewReader("home-b\n"), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
}

// A host holding nothing of this deployment ends the run without a re-run
// hint, dry run or not.
func TestForcedOnAnEmptyHostSaysNothingToDo(t *testing.T) {
	plan := &siteremove.Plan{Site: "home-b", Stages: []*siteremove.Stage{{Number: 3, Name: "clean the host"}}}
	rec := &ui.Recorder{}
	if !forcedNothingToDo(rec, plan, config.Destination{User: "admin", Host: "192.0.2.1", Port: 22}, false) {
		t.Fatal("an empty plan was not ended")
	}
	if out := rec.Lines(); strings.Contains(out, "--execute") || !strings.Contains(out, "holds nothing of this deployment") {
		t.Errorf("printed:\n%s", out)
	}
	rec = &ui.Recorder{}
	forcedNothingToDo(rec, plan, config.Destination{User: "admin", Host: "192.0.2.1", Port: 22}, true)
	if out := rec.Lines(); !strings.Contains(out, "--execute") || !strings.Contains(out, "deployment record") {
		t.Errorf("a dry run with a record left to clean printed:\n%s", out)
	}
	plan.Stages[0].Steps = []siteremove.Step{{Site: "home-b", Verb: "remove"}}
	if forcedNothingToDo(&ui.Recorder{}, plan, config.Destination{}, false) {
		t.Error("a plan with steps was ended")
	}
}

// A forced run on a site no longer declared reports a gateway whose record
// it could not reach, and still finishes: the host is clean.
func TestForcedRunReportsAGatewayItCouldNotReach(t *testing.T) {
	savedSite, savedReg := removeSiteHost, registryHost
	t.Cleanup(func() { removeSiteHost, registryHost = savedSite, savedReg })
	quiet := runningHost{failingHost{match: "\x00"}}
	removeSiteHost = func(string, config.Site, bool) apply.Transport { return quiet }
	registryHost = func(string, config.Site, string, bool) registry.Runner { return &recordFake{down: true} }
	var err error
	out := captureStdout(t, func() {
		err = runSiteRemove([]string{"monitor-a", "--force", "--ssh", "admin@192.0.2.1", "--config", fixtureConfig(), "--secrets", fixtureSecretsPath(), "--execute", "--sudo=false"}, strings.NewReader(""), &bytes.Buffer{})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "vm missed this change to the deployment record") {
		t.Errorf("no warning:\n%s", out)
	}
}

// A host whose only part of the deployment is its kept Caddy says so, and
// names the sites it is kept for, rather than that it holds nothing.
func TestForcedOnAHostWithOnlyAKeptCaddySaysSo(t *testing.T) {
	plan := &siteremove.Plan{Site: "vm", Stages: []*siteremove.Stage{{Number: 3, Name: "clean the host"}}, CaddyKept: []string{"/srv/caddy.d/blog.caddy"}}
	rec := &ui.Recorder{}
	if !forcedNothingToDo(rec, plan, config.Destination{User: "admin", Host: "192.0.2.1", Port: 22}, false) {
		t.Fatal("a plan with nothing to do was not ended")
	}
	out := rec.Lines()
	if !strings.Contains(out, "Caddy") || !strings.Contains(out, "/srv/caddy.d/blog.caddy") || strings.Contains(out, "holds nothing of this deployment.") {
		t.Errorf("printed:\n%s", out)
	}
}

// idHost is an empty running host whose registry holds the fixture
// deployment's monitor and a neighbour's entry.
type idHost struct {
	runningHost
	registry string
}

func (h idHost) ReadFile(path string) (string, bool, error) {
	if path == registry.Path {
		return h.registry, true, nil
	}
	return h.runningHost.ReadFile(path)
}

// Every --id run says DNS was not modified, in one line and the comment to
// search for, since with no configuration there is no provider to ask.
func TestSiteRemoveByIDWarnsThatDNSWasNotModified(t *testing.T) {
	withHost(t, idHost{runningHost{failingHost{match: "\x00"}}, twoDeployments(t)})
	out := plainOutput(t)
	if err := runSiteRemove([]string{"--force", "--ssh", "admin@192.0.2.30", "--id", "f2a9", "--sudo=false"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Log("\n" + got)
	if strings.Count(got, "DNS") != 1 {
		t.Errorf("want the warning once:\n%s", got)
	}
	got = strings.Join(strings.Fields(got), " ")
	for _, want := range []string{
		"WARN DNS records for this deployment, if any exist, were not modified They carry",
		`They carry the comment "paisans-f2a9: created by paisans dns init", and may point at 192.0.2.30`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
}

// A forced removal of a site paisans.yaml no longer declares has no address
// for it, so its DNS stage skips, and no provider is made.
func TestForcedRemovalOfAnUndeclaredSiteMakesNoProvider(t *testing.T) {
	savedSite, savedReg, savedDNS := removeSiteHost, registryHost, dnsProviderFor
	t.Cleanup(func() { removeSiteHost, registryHost, dnsProviderFor = savedSite, savedReg, savedDNS })
	removeSiteHost = func(string, config.Site, bool) apply.Transport { return runningHost{failingHost{match: "\x00"}} }
	registryHost = func(string, config.Site, string, bool) registry.Runner { return &recordFake{} }
	dnsProviderFor = func(string, string) (dns.Provider, error) {
		t.Error("a DNS provider was made")
		return nil, nil
	}
	plainOutput(t)
	err := runSiteRemove([]string{"monitor-a", "--force", "--ssh", "admin@192.0.2.1", "--config", fixtureConfig(), "--secrets", fixtureSecretsPath(), "--sudo=false"}, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
}

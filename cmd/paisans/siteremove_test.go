package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
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
	if err == nil || !strings.Contains(err.Error(), "stdin is not one") {
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
		{site: "never", err: "name its host with --ssh"},
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
		"never --force":                                               "name its host with --ssh",
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
	if !forcedNothingToDo(rec, plan, config.Destination{User: "admin", Host: "192.0.2.1", Port: 22}) {
		t.Fatal("an empty plan was not ended")
	}
	if out := rec.Lines(); strings.Contains(out, "--execute") || !strings.Contains(out, "holds nothing of this deployment") {
		t.Errorf("printed:\n%s", out)
	}
	plan.Stages[0].Steps = []siteremove.Step{{Site: "home-b", Verb: "remove"}}
	if forcedNothingToDo(&ui.Recorder{}, plan, config.Destination{}) {
		t.Error("a plan with steps was ended")
	}
}

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
)

// noSiteHosts fails the test if `site remove` reaches for any host.
func noSiteHosts(t *testing.T) {
	t.Helper()
	saved := removeSiteHost
	removeSiteHost = func(config.Site, bool) apply.Transport {
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

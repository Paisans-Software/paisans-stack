package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/appremove"
	"github.com/paisans-software/paisans-stack/internal/config"
)

// noHosts fails the test if `app remove` reaches for any host.
func noHosts(t *testing.T) {
	t.Helper()
	saved := removeHost
	removeHost = func(string, config.Site, bool) appremove.Host {
		t.Fatal("a host was reached")
		return nil
	}
	t.Cleanup(func() { removeHost = saved })
}

func TestAppRemoveRefusesADeclaredApp(t *testing.T) {
	noHosts(t)
	err := runAppRemove([]string{"talk", "--config", fixtureConfig()}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "still in paisans.yaml") {
		t.Errorf("err = %v", err)
	}
	err = runAppRemove([]string{"--config", fixtureConfig(), "infra"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "infrastructure stack") {
		t.Errorf("err = %v", err)
	}
}

// Deleting member data needs a person at a terminal, and there is no flag
// standing in for one. Refused before any host is read.
func TestDeleteDataNeedsATerminal(t *testing.T) {
	noHosts(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString("old\n")
	w.Close()
	defer r.Close()
	err = runAppRemove([]string{"old", "--config", fixtureConfig(), "--delete-data", "--execute"}, r, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "stdin is not one") {
		t.Errorf("err = %v", err)
	}
}

func TestConfirmNameWantsTheAppsName(t *testing.T) {
	saved := isTerminal
	isTerminal = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminal = saved })
	for answer, ok := range map[string]bool{"docs\n": true, "yes\n": false, "\n": false, "": false} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		w.WriteString(answer)
		w.Close()
		err = confirmName(r, &bytes.Buffer{}, "docs")
		r.Close()
		if (err == nil) != ok {
			t.Errorf("answer %q: %v", answer, err)
		}
	}
	isTerminal = func(*os.File) bool { return false }
	if err := confirmName(os.Stdin, &bytes.Buffer{}, "docs"); err == nil {
		t.Error("a name was accepted from something that is not a terminal")
	}
}

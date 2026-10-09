package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// stateHost answers the instance question with one fixed word and records
// that it was asked.
type stateHost struct {
	state string
	asked *[]string
	name  string
}

func (h stateHost) Describe() string { return h.name }
func (h stateHost) Run(command string) (string, error) {
	*h.asked = append(*h.asked, h.name)
	return h.state + "\n", nil
}
func (h stateHost) RunInput(c, _ string) (string, error)   { return h.Run(c) }
func (h stateHost) ReadFile(string) (string, bool, error)  { return "", false, nil }
func (h stateHost) WriteFile(string, string, uint32) error { return errors.New("no writes") }

// Every test in this package that reaches Pocket ID without --site finds
// home-a active and every other site standing by, unless it says otherwise.
func init() {
	standbyLook = func(tr apply.SSHTransport) apply.Transport {
		var asked []string
		if tr.Host == "home-a.local" {
			return stateHost{"active", &asked, tr.Host}
		}
		return stateHost{"standby", &asked, tr.Host}
	}
}

// lookWith makes each site's instance answer the given state, by ssh host.
func lookWith(t *testing.T, states map[string]string, asked *[]string) {
	t.Helper()
	saved := standbyLook
	standbyLook = func(tr apply.SSHTransport) apply.Transport {
		return stateHost{states[tr.Host], asked, tr.Host}
	}
	t.Cleanup(func() { standbyLook = saved })
}

func standbyCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "internal", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// After an apply that acted on auth on home-a, both apps sites are asked:
// home-a through the apply's own transport, home-b through its section.
func TestApplyChecksOneActivePocketIDAfterActingOnIt(t *testing.T) {
	var asked []string
	plan := &apply.Plan{Site: "home-a", Actions: []apply.Action{{Stack: "auth", Recreate: true}}}
	err := checkStandby(ui.Discard, standbyCfg(t), plan, "home-a", stateHost{"active", &asked, "home-a"}, func(name string) apply.Transport {
		return stateHost{"standby", &asked, name + " by its section"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "home-a,home-b by its section" {
		t.Errorf("asked %v", asked)
	}
}

func TestApplyFailsOnTwoActivePocketIDs(t *testing.T) {
	var asked []string
	plan := &apply.Plan{Site: "home-a", Actions: []apply.Action{{Stack: "auth"}}}
	err := checkStandby(ui.Discard, standbyCfg(t), plan, "home-a", stateHost{"active", &asked, "home-a"}, func(name string) apply.Transport {
		return stateHost{"active", &asked, name}
	})
	if err == nil || !strings.Contains(err.Error(), "2 sites have an active instance") || !strings.Contains(err.Error(), "The apply itself finished") {
		t.Fatalf("err = %v", err)
	}
}

// An apply that did not touch auth asks no site about it.
func TestApplyLeavesPocketIDAloneWhenItDidNotActOnIt(t *testing.T) {
	var asked []string
	plan := &apply.Plan{Site: "home-a", Actions: []apply.Action{{Stack: "talk", Recreate: true}}}
	err := checkStandby(ui.Discard, standbyCfg(t), plan, "home-a", stateHost{"down", &asked, "home-a"}, func(name string) apply.Transport {
		return stateHost{"down", &asked, name}
	})
	if err != nil || len(asked) != 0 {
		t.Fatalf("err = %v, asked %v", err, asked)
	}
}

// The admin commands call Pocket ID where it is active: the first apps site
// may be standing by, with its port closed.
func TestPocketIDAdminGoesToTheActiveSite(t *testing.T) {
	var asked []string
	lookWith(t, map[string]string{"home-a.local": "standby", "home-b.local": "active"}, &asked)
	got, err := pocketIDSite(standbyCfg(t), "auth", "", "oidc client create")
	if err != nil || got != "home-b" {
		t.Fatalf("got %q, %v; want home-b", got, err)
	}
	if strings.Join(asked, ",") != "home-a.local,home-b.local" {
		t.Errorf("asked %v", asked)
	}
}

func TestPocketIDAdminRefusesWithNoActiveSite(t *testing.T) {
	var asked []string
	lookWith(t, map[string]string{"home-a.local": "standby", "home-b.local": "down"}, &asked)
	_, err := pocketIDSite(standbyCfg(t), "auth", "", "oidc client create")
	if err == nil || !strings.Contains(err.Error(), "no site has an active instance, so there is no one site to call") || !strings.Contains(err.Error(), "home-b   down") {
		t.Fatalf("err = %v", err)
	}
}

// --site is the operator's choice and asks nobody.
func TestPocketIDAdminTakesSiteAsGiven(t *testing.T) {
	var asked []string
	lookWith(t, map[string]string{}, &asked)
	got, err := pocketIDSite(standbyCfg(t), "auth", "home-b", "app admin create")
	if err != nil || got != "home-b" || len(asked) != 0 {
		t.Fatalf("got %q, %v, asked %v", got, err, asked)
	}
}

package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
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
	err := checkStandby(standbyCfg(t), plan, "home-a", stateHost{"active", &asked, "home-a"}, func(name string) apply.Transport {
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
	err := checkStandby(standbyCfg(t), plan, "home-a", stateHost{"active", &asked, "home-a"}, func(name string) apply.Transport {
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
	err := checkStandby(standbyCfg(t), plan, "home-a", stateHost{"down", &asked, "home-a"}, func(name string) apply.Transport {
		return stateHost{"down", &asked, name}
	})
	if err != nil || len(asked) != 0 {
		t.Fatalf("err = %v, asked %v", err, asked)
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// titles is the plan as "phase: title" lines.
func titles(steps []convergeStep) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Phase+": "+s.Title)
	}
	return out
}

func fixture(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConvergeFoundsABlankDeploymentWitnessFirst(t *testing.T) {
	got := strings.Join(titles(convergePlan(fixture(t), convergeState{})), "\n")
	want := strings.Join([]string{
		"hosts: host prepare --site home-a",
		"hosts: host prepare --site home-b",
		"hosts: host prepare --site vm",
		"hosts: host prepare --site watch",
		"founding: apply --site vm",
		"founding: apply --site home-a",
		"founding: apply --site home-b",
		"other sites: apply --site watch",
		"storage: storage add",
		"pass two: apply --site home-a",
		"pass two: apply --site home-b",
		"pass two: apply --site vm",
		"pass two: apply --site watch",
		"dns: dns init",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestConvergeJoinsAnUnfoundedMemberOfARunningCluster(t *testing.T) {
	steps := convergePlan(fixture(t), convergeState{Founded: map[string]bool{"home-a": true, "vm": true}})
	got := strings.Join(titles(steps), "\n")
	if !strings.Contains(got, "joining: site add home-b") || strings.Contains(got, "founding:") {
		t.Errorf("got:\n%s", got)
	}
}

func TestConvergeRunsInitFirstWhenItHasWork(t *testing.T) {
	steps := convergePlan(fixture(t), convergeState{NeedsInit: true})
	if len(steps) == 0 || steps[0].Title != "init" {
		t.Errorf("got %v", titles(steps))
	}
}

func TestConvergeAppliesEachSiteOnceBeforePassTwo(t *testing.T) {
	steps := convergePlan(fixture(t), convergeState{})
	seen := map[string]int{}
	for _, s := range steps {
		if s.Phase != "pass two" && len(s.Args) > 0 && s.Args[0] == "apply" {
			seen[s.Title]++
		}
	}
	for title, n := range seen {
		if n != 1 {
			t.Errorf("%s applied %d times before pass two", title, n)
		}
	}
}

func TestConvergeWithoutEtcdAppliesEverySiteInPhaseThree(t *testing.T) {
	cfg := fixture(t)
	cfg.Etcd.Members = nil
	got := strings.Join(titles(convergePlan(cfg, convergeState{})), "\n")
	if strings.Contains(got, "founding:") || strings.Contains(got, "joining:") || !strings.Contains(got, "other sites: apply --site home-a") {
		t.Errorf("got:\n%s", got)
	}
}

func TestConvergeStorageFollowsTheGarageSites(t *testing.T) {
	cfg := fixture(t)
	cfg.Storage.Garage.Sites = []string{"home-a"}
	if got := strings.Join(titles(convergePlan(cfg, convergeState{})), "\n"); !strings.Contains(got, "storage: storage init --site home-a") {
		t.Errorf("one Garage site:\n%s", got)
	}
	cfg.Storage.Garage.Sites = nil
	if got := strings.Join(titles(convergePlan(cfg, convergeState{})), "\n"); strings.Contains(got, "storage:") {
		t.Errorf("no Garage site:\n%s", got)
	}
}

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
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

// fakeConverge replaces the founded probe and the step runner; fail maps a
// step's title to the error it returns.
func fakeConverge(t *testing.T, fail map[string]error) *[]string {
	t.Helper()
	var ran []string
	savedRun, savedFounded := convergeRun, convergeFounded
	convergeRun = func(args []string) error {
		title := strings.Join(args, " ")
		for _, f := range []string{" --config", " --secrets", " --execute", " --sudo"} {
			if i := strings.Index(title, f); i >= 0 {
				title = title[:i]
			}
		}
		ran = append(ran, title)
		return fail[title]
	}
	convergeFounded = func(*config.Config, bool) (map[string]bool, error) { return map[string]bool{}, nil }
	t.Cleanup(func() { convergeRun, convergeFounded = savedRun, savedFounded })
	return &ran
}

func converge(t *testing.T, extra ...string) error {
	t.Helper()
	var err error
	captureStdout(t, func() {
		err = runApply(append([]string{"--config", fixtureConfig(), "--secrets", fixtureSecretsPath(), "--sudo=false"}, extra...))
	})
	return err
}

func TestConvergeRunsEveryStepInOrder(t *testing.T) {
	ran := fakeConverge(t, nil)
	if err := converge(t, "--execute"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*ran, "\n")
	for _, want := range []string{"host prepare --site home-a", "apply --site vm", "storage add", "dns init"} {
		if !strings.Contains(got, want) {
			t.Errorf("did not run %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "apply --site vm") > strings.Index(got, "apply --site home-a") {
		t.Errorf("the witness was not founded first:\n%s", got)
	}
}

func TestConvergeDryRunRunsNothing(t *testing.T) {
	ran := fakeConverge(t, nil)
	if err := converge(t); err != nil {
		t.Fatal(err)
	}
	if len(*ran) != 0 {
		t.Errorf("a dry run ran %v", *ran)
	}
}

func TestTheFoundingStopDoesNotStopTheRun(t *testing.T) {
	ran := fakeConverge(t, map[string]error{"apply --site home-a": fmt.Errorf("home-a: etcd is %w home-b", apply.ErrFoundingWait)})
	err := converge(t, "--execute")
	got := strings.Join(*ran, "\n")
	if !strings.Contains(got, "apply --site home-b") {
		t.Fatalf("the run stopped at the founding stop: %v\n%s", err, got)
	}
}

func TestAFailingStepStopsTheRun(t *testing.T) {
	ran := fakeConverge(t, map[string]error{"apply --site vm": errors.New("vm: boom")})
	err := converge(t, "--execute")
	if err == nil || !strings.Contains(err.Error(), "apply --site vm") || !strings.Contains(err.Error(), "Run paisans apply --execute again to resume") {
		t.Fatalf("err = %v", err)
	}
	if last := (*ran)[len(*ran)-1]; last != "apply --site vm" {
		t.Errorf("ran past the failure: %v", *ran)
	}
}

func TestConvergeRefusesSSH(t *testing.T) {
	fakeConverge(t, nil)
	if err := converge(t, "--ssh", "admin@192.0.2.1"); err == nil || !strings.Contains(err.Error(), "--site") {
		t.Errorf("err = %v", err)
	}
}

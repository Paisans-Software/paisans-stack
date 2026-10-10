package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/dns"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/storageadd"
	"github.com/paisans-software/paisans-stack/internal/validate"
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

func initial(names ...string) render.EtcdInitial {
	var pairs []string
	for i, n := range names {
		pairs = append(pairs, fmt.Sprintf("%s=http://10.44.0.%d:2380", n, i+1))
	}
	return render.EtcdInitial{State: "new", Cluster: strings.Join(pairs, ",")}
}

// A member the founded cluster's initial-cluster does not list joins it.
func TestConvergeJoinsAMemberTheClusterDoesNotList(t *testing.T) {
	steps := convergePlan(fixture(t), convergeState{Initial: map[string]render.EtcdInitial{"home-a": initial("home-a", "vm"), "vm": initial("home-a", "vm")}})
	got := strings.Join(titles(steps), "\n")
	if !strings.Contains(got, "joining: site add home-b") || strings.Contains(got, "founding:") {
		t.Errorf("got:\n%s", got)
	}
}

// A member a founding record lists is still being founded, though another
// member has its record: it is applied, not added.
func TestConvergeResumesAFoundingAfterAFailure(t *testing.T) {
	steps := convergePlan(fixture(t), convergeState{Initial: map[string]render.EtcdInitial{"vm": initial("home-a", "home-b", "vm")}})
	got := strings.Join(titles(steps), "\n")
	if strings.Contains(got, "site add") || !strings.Contains(got, "founding: apply --site home-a") || !strings.Contains(got, "founding: apply --site home-b") {
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
	ran, _ := fakeConvergeArgs(t, fail)
	return ran
}

// fakeConvergeArgs is fakeConverge, and each step's arguments in full.
func fakeConvergeArgs(t *testing.T, fail map[string]error) (*[]string, *[][]string) {
	t.Helper()
	var ran []string
	var full [][]string
	savedRun, savedFounded := convergeRun, convergeFounded
	convergeRun = func(args []string) error {
		full = append(full, args)
		title := strings.Join(args, " ")
		for _, f := range []string{" --config", " --secrets", " --execute", " --sudo"} {
			if i := strings.Index(title, f); i >= 0 {
				title = title[:i]
			}
		}
		ran = append(ran, title)
		return fail[title]
	}
	convergeFounded = func(*config.Config, bool) (map[string]render.EtcdInitial, error) {
		return map[string]render.EtcdInitial{}, nil
	}
	t.Cleanup(func() { convergeRun, convergeFounded = savedRun, savedFounded })
	return &ran, &full
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

// noID is the fixture configuration without its deployment id.
func noID(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "id:") {
			kept = append(kept, line)
		}
	}
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// With no id there is nothing on a host to read yet: the plan is init, and
// the rest once it has run.
func TestConvergeWithoutAnIDPlansInit(t *testing.T) {
	fakeConverge(t, nil)
	path := noID(t)
	var err error
	out := captureStdout(t, func() { err = runApply([]string{"--config", path, "--sudo=false"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "init") || !strings.Contains(out, "once init has run") {
		t.Errorf("printed:\n%s", out)
	}
}

// After init, the state is read again and the rest planned from it.
func TestConvergeReplansAfterInit(t *testing.T) {
	ran := fakeConverge(t, nil)
	saved := convergeRead
	reads := 0
	convergeRead = func(path, secrets string, sudo bool) (*config.Config, convergeState, error) {
		reads++
		cfg, err := config.Load(fixtureConfig())
		if reads == 1 {
			return cfg, convergeState{NeedsInit: true}, err
		}
		return cfg, convergeState{Initial: map[string]render.EtcdInitial{"home-a": initial("home-a", "vm"), "vm": initial("home-a", "vm")}}, err
	}
	t.Cleanup(func() { convergeRead = saved })
	if err := converge(t, "--execute"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*ran, "\n")
	if !strings.HasPrefix(got, "init\n") || strings.Count(got, "init\n") != 1 || !strings.Contains(got, "site add home-b") || strings.Contains(got, "apply --site vm\napply --site home-a\napply --site home-b\napply --site watch\nstorage") {
		t.Errorf("ran:\n%s", got)
	}
}

// The dry run says why each step is there, and names what only the operator
// can do.
func TestConvergeSaysWhyAndWhatIsLeft(t *testing.T) {
	fakeConverge(t, nil)
	out := ""
	captureStdout(t, func() {})
	out = captureStdout(t, func() {
		if err := runApply([]string{"--config", fixtureConfig(), "--secrets", fixtureSecretsPath(), "--sudo=false"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"a witness is founded first", "app admin create"} {
		if !strings.Contains(out, want) {
			t.Errorf("does not say %q:\n%s", want, out)
		}
	}
}

// -v, --keep-images and --min-free reach every step whose command takes
// them: -v every step, the other two each apply.
func TestConvergePassesItsFlagsToEachStep(t *testing.T) {
	_, full := fakeConvergeArgs(t, nil)
	if err := converge(t, "--execute", "-v", "--keep-images", "--min-free", "2G"); err != nil {
		t.Fatal(err)
	}
	for _, args := range *full {
		got := strings.Join(args, " ")
		if !slices.Contains(args, "-v") {
			t.Errorf("no -v: %s", got)
		}
		isApply := args[0] == "apply"
		if slices.Contains(args, "--keep-images") != isApply {
			t.Errorf("--keep-images where apply's flags are not, or missing where they are: %s", got)
		}
		if strings.Contains(got, "--min-free 2G") != isApply {
			t.Errorf("--min-free where apply's flags are not, or missing where they are: %s", got)
		}
	}
	if len(*full) == 0 {
		t.Fatal("ran nothing")
	}
}

// A monitor validate lets into etcd.members is founded with the other
// members, since etcd needs it, and is still applied last in pass two, after
// what it checks.
func TestConvergeFoundsAMonitorMemberAndAppliesItLast(t *testing.T) {
	cfg := fixture(t)
	cfg.Etcd.Members = []string{"home-a", "home-b", "vm", "watch"}
	if result := validate.Check(cfg); result.Refused() {
		t.Fatalf("validate refuses a monitor in etcd.members: %v", result.Refusals())
	}
	got := strings.Join(titles(convergePlan(cfg, convergeState{})), "\n")
	want := strings.Join([]string{
		"hosts: host prepare --site home-a",
		"hosts: host prepare --site home-b",
		"hosts: host prepare --site vm",
		"hosts: host prepare --site watch",
		"founding: apply --site vm",
		"founding: apply --site home-a",
		"founding: apply --site home-b",
		"founding: apply --site watch",
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

// A stop running again cannot get past says what can: a conflicting DNS
// record is changed at the provider by hand, and Garage is waited for. Each
// hint stays one short line.
func TestConvergeStopsWithTheHintThatFits(t *testing.T) {
	conflict := fmt.Errorf("dns: 1 conflicting record(s), listed above: %w", dns.ErrConflict)
	waiting := fmt.Errorf("storage add is waiting at stage 3 (sync): blocks are still moving: %w", storageadd.ErrWaiting)
	for _, tc := range []struct {
		step string
		err  error
		want string
	}{
		{"dns init", conflict, "Change each conflicting record above at your DNS provider, then run paisans apply --execute again."},
		{"storage add", waiting, "Nothing failed: Garage is still moving data. Run paisans apply --execute again later."},
		{"apply --site vm", errors.New("vm: boom"), "Run paisans apply --execute again to resume."},
	} {
		fakeConverge(t, map[string]error{tc.step: tc.err})
		err := converge(t, "--execute")
		if err == nil {
			t.Fatalf("%s: no error", tc.step)
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: the step's error is not wrapped: %v", tc.step, err)
		}
		lines := strings.Split(err.Error(), "\n")
		if hint := lines[len(lines)-1]; hint != tc.want {
			t.Errorf("%s: hint %q, want %q", tc.step, hint, tc.want)
		}
		if hint := lines[len(lines)-1]; len(hint) > 100 {
			t.Errorf("%s: %d characters: %s", tc.step, len(hint), hint)
		}
	}
}

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// samePlan fails t unless steps are want, as "phase: title" lines, exactly
// and in order.
func samePlan(t *testing.T, steps []convergeStep, want ...string) {
	t.Helper()
	sameLines(t, titles(steps), want)
}

func sameLines(t *testing.T, got, want []string) {
	t.Helper()
	if g, w := strings.Join(got, "\n"), strings.Join(want, "\n"); g != w {
		t.Errorf("got:\n%s\nwant:\n%s", g, w)
	}
}

// hostsPhase, storagePhase and the rest are the fixture's plan, phase by
// phase, where a test does not change them.
var (
	hostsPhase = []string{
		"hosts: host prepare --site home-a",
		"hosts: host prepare --site home-b",
		"hosts: host prepare --site vm",
		"hosts: host prepare --site watch",
	}
	foundingPhase = []string{
		"founding: apply --site vm",
		"founding: apply --site home-a",
		"founding: apply --site home-b",
	}
	passTwoAndDNS = []string{
		"pass two: apply --site home-a",
		"pass two: apply --site home-b",
		"pass two: apply --site vm",
		"pass two: apply --site watch",
		"dns: dns init",
	}
)

// blankPlan is the fixture's plan with no member founded.
func blankPlan() []string {
	return slices.Concat(hostsPhase, foundingPhase, []string{"other sites: apply --site watch", "storage: storage add"}, passTwoAndDNS)
}

// commands is plan lines as the commands a run runs: no phase, and no init,
// which runs before the plan is read again.
func commands(lines []string) []string {
	var out []string
	for _, l := range lines {
		_, title, _ := strings.Cut(l, ": ")
		if title != "init" {
			out = append(out, title)
		}
	}
	return out
}

func TestConvergeFoundsABlankDeploymentWitnessFirst(t *testing.T) {
	samePlan(t, convergePlan(fixture(t), convergeState{}), blankPlan()...)
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
	samePlan(t, steps, slices.Concat(hostsPhase, []string{"joining: site add home-b", "other sites: apply --site watch", "storage: storage add"}, passTwoAndDNS)...)
}

// A member a founding record lists is still being founded, though another
// member has its record: it is applied, not added.
func TestConvergeResumesAFoundingAfterAFailure(t *testing.T) {
	steps := convergePlan(fixture(t), convergeState{Initial: map[string]render.EtcdInitial{"vm": initial("home-a", "home-b", "vm")}})
	samePlan(t, steps, slices.Concat(hostsPhase, []string{"founding: apply --site home-a", "founding: apply --site home-b", "other sites: apply --site watch", "storage: storage add"}, passTwoAndDNS)...)
}

func TestConvergeRunsInitFirstWhenItHasWork(t *testing.T) {
	samePlan(t, convergePlan(fixture(t), convergeState{NeedsInit: true}), append([]string{"configuration: init"}, blankPlan()...)...)
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
	samePlan(t, convergePlan(cfg, convergeState{}), slices.Concat(hostsPhase, []string{
		"other sites: apply --site home-a",
		"other sites: apply --site home-b",
		"other sites: apply --site vm",
		"other sites: apply --site watch",
		"storage: storage add",
	}, passTwoAndDNS)...)
}

func TestConvergeStorageFollowsTheGarageSites(t *testing.T) {
	cfg := fixture(t)
	before := slices.Concat(hostsPhase, foundingPhase, []string{"other sites: apply --site watch"})
	cfg.Storage.Garage.Sites = []string{"home-a"}
	samePlan(t, convergePlan(cfg, convergeState{}), slices.Concat(before, []string{"storage: storage init --site home-a"}, passTwoAndDNS)...)
	cfg.Storage.Garage.Sites = nil
	samePlan(t, convergePlan(cfg, convergeState{}), slices.Concat(before, passTwoAndDNS)...)
}

// fakeConverge replaces the founded probe and the step runner; fail maps a
// step's title to the error it returns the first time it runs.
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
		err := fail[title]
		delete(fail, title)
		return err
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
	sameLines(t, *ran, commands(blankPlan()))
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
	if err := converge(t, "--execute"); err != nil {
		t.Fatalf("the run stopped at the founding stop: %v", err)
	}
	sameLines(t, *ran, commands(blankPlan()))
}

func TestAFailingStepStopsTheRun(t *testing.T) {
	ran := fakeConverge(t, map[string]error{"apply --site vm": errors.New("vm: boom")})
	err := converge(t, "--execute")
	if err == nil || !strings.Contains(err.Error(), "apply --site vm") || !strings.Contains(err.Error(), "Run paisans apply --execute again to resume") {
		t.Fatalf("err = %v", err)
	}
	sameLines(t, *ran, commands(slices.Concat(hostsPhase, foundingPhase[:1])))
}

func TestConvergeRefusesSSH(t *testing.T) {
	fakeConverge(t, nil)
	if err := converge(t, "--ssh", "admin@192.0.2.1"); err == nil || !strings.Contains(err.Error(), "--site") {
		t.Errorf("err = %v", err)
	}
}

// A configuration validate refuses is refused before any host is read: a
// typo in etcd.members is reported as one, not as a host that did not answer.
func TestConvergeRefusesBeforeReadingAHost(t *testing.T) {
	ran := fakeConverge(t, nil)
	read := false
	convergeFounded = func(*config.Config, bool) (map[string]render.EtcdInitial, error) {
		read = true
		return nil, errors.New("no host should be read")
	}
	data, err := os.ReadFile(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	body := strings.Replace(string(data), "members: [", "members: [nowhere, ", 1)
	if body == string(data) {
		t.Fatal("the fixture has no etcd.members list to edit")
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		err = runApply([]string{"--config", path, "--secrets", fixtureSecretsPath(), "--sudo=false"})
	})
	if err == nil || !strings.Contains(err.Error(), "was refused") {
		t.Errorf("err = %v, want the configuration refused", err)
	}
	if read {
		t.Error("a host was read for a refused configuration")
	}
	if len(*ran) != 0 {
		t.Errorf("ran %v", *ran)
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
	sameLines(t, *ran, append([]string{"init"}, commands(slices.Concat(hostsPhase, []string{"joining: site add home-b", "other sites: apply --site watch", "storage: storage add"}, passTwoAndDNS))...))
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

// apply takes a site only when given --site, so nothing says it needs one:
// not its usage, and not the commands that compare themselves with it.
func TestNothingSaysApplyTakesOneSite(t *testing.T) {
	entry := usage[strings.Index(usage, "\n  apply      "):]
	entry = entry[:strings.Index(entry[1:], "\n  site ")]
	if !strings.Contains(entry, "Without --site") {
		t.Errorf("apply's usage entry does not say what it does without --site:%s", entry)
	}
	for name, run := range map[string]func([]string) error{"host prepare": runHostPrepare, "storage init": runStorageInit, "prune": runPrune} {
		var err error
		captureStdout(t, func() { err = run([]string{"--config", fixtureConfig()}) })
		if err == nil || !strings.Contains(err.Error(), "the same reason apply --site takes one") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Every step a real plan holds, with the flags the run adds, is accepted by
// its command's own flag parsing: a flag one command lacks would exit the
// run partway. flagsOnly stops each command once its flags parse, before it
// reads a file or reaches a host.
func TestEveryPlannedStepParses(t *testing.T) {
	cfg := fixture(t)
	steps := convergePlan(cfg, convergeState{NeedsInit: true})
	steps = append(steps, convergePlan(cfg, convergeState{Initial: map[string]render.EtcdInitial{"home-a": initial("home-a", "vm"), "vm": initial("home-a", "vm")}})...)
	one := fixture(t)
	one.Storage.Garage.Sites = []string{"home-a"}
	steps = append(steps, convergePlan(one, convergeState{})...)

	commands := map[string]bool{}
	for _, s := range steps {
		commands[strings.Join(s.Args[:min(2, len(s.Args))], " ")] = true
	}
	for _, want := range []string{"init", "host prepare", "apply --site", "site add", "storage init", "storage add", "dns init"} {
		if !commands[want] {
			t.Fatalf("the plans hold no %s step: %v", want, commands)
		}
	}

	saved := flagsOnly
	flagsOnly = true
	t.Cleanup(func() { flagsOnly = saved })
	for _, o := range []convergeOptions{
		{Config: fixtureConfig()},
		{Config: fixtureConfig(), Secrets: fixtureSecretsPath(), Execute: true, Sudo: true, Verbose: true, KeepImages: true, MinFree: "2G"},
	} {
		for _, s := range steps {
			args := convergeFlags(s.Args, o)
			if err := runStep(args); !errors.Is(err, errFlagsOnly) {
				t.Errorf("%s: %v", strings.Join(args, " "), err)
			}
		}
	}
}

// derivedWithNewDataSite is the fixture with cluster.sites and etcd.members
// left out and a site home-c given the data role, so the roles alone make it
// a voter.
func derivedWithNewDataSite(t *testing.T) *config.Config {
	t.Helper()
	data, err := os.ReadFile(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	text := regexp.MustCompile(`(?m)^  (sites|members): \[[^\]]*\]\n`).ReplaceAllString(string(data), "")
	text = strings.Replace(text, "  vm:\n", `  home-c:
    roles: [data]
    address: 10.44.0.5
    endpoint: 198.51.100.5:51820
    ssh:
      host: home-c.local
      user: ubuntu
      public_key: |
        ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
  vm:
`, 1)
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Etcd.MembersDerived || !slices.Contains(cfg.Etcd.Members, "home-c") {
		t.Fatalf("home-c is not a derived member: %v", cfg.Etcd.Members)
	}
	return cfg
}

// A site given the data role on a founded cluster, with etcd.members left
// out, is a derived member that no founding record lists: it joins through
// site add, as it would written into etcd.members by hand.
func TestConvergeJoinsADerivedMember(t *testing.T) {
	cfg := derivedWithNewDataSite(t)
	founded := initial("home-a", "home-b", "vm")
	steps := convergePlan(cfg, convergeState{Initial: map[string]render.EtcdInitial{"home-a": founded, "home-b": founded, "vm": founded}})
	samePlan(t, steps,
		"hosts: host prepare --site home-a",
		"hosts: host prepare --site home-b",
		"hosts: host prepare --site home-c",
		"hosts: host prepare --site vm",
		"hosts: host prepare --site watch",
		"joining: site add home-c",
		"other sites: apply --site watch",
		"storage: storage add",
		"pass two: apply --site home-a",
		"pass two: apply --site home-b",
		"pass two: apply --site home-c",
		"pass two: apply --site vm",
		"pass two: apply --site watch",
		"dns: dns init",
	)
}

// init's line says what it has to do, by name: the subnet, the secrets file,
// or which generated secrets are missing.
func TestConvergeSaysWhatInitIsFor(t *testing.T) {
	fakeConverge(t, nil)
	_, st, err := readConvergeState(fixtureConfig(), filepath.Join(t.TempDir(), "secrets.enc.yaml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !st.NeedsInit || st.InitWhy != "no secrets file yet" {
		t.Errorf("NeedsInit %t, why %q", st.NeedsInit, st.InitWhy)
	}
	steps := convergePlan(fixture(t), convergeState{NeedsInit: true, InitWhy: "no mesh subnet"})
	if steps[0].Why != "no mesh subnet" {
		t.Errorf("init's reason %q", steps[0].Why)
	}
}

// Missing generated secrets are named, the first three and a count of the
// rest, never by value.
func TestInitWhyNamesMissingSecrets(t *testing.T) {
	for _, c := range []struct {
		names []string
		want  string
	}{
		{[]string{"a"}, "generated secret missing: a"},
		{[]string{"a", "b", "c"}, "generated secrets missing: a, b, c"},
		{[]string{"a", "b", "c", "d", "e"}, "generated secrets missing: a, b, c and 2 more"},
	} {
		if got := missingSecrets(c.names); got != c.want {
			t.Errorf("%v: %q, want %q", c.names, got, c.want)
		}
	}
}

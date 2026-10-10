package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/dns"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/storageadd"
	"github.com/paisans-software/paisans-stack/internal/ui"
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

// fakeConverge replaces each etcd record's read and the step runner; fail maps a
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
	savedRun, savedRead, savedCheck := convergeRun, convergeReadInitial, convergeCheck
	// A dry run's checks are each step's own dry run, which reaches a host;
	// here every step is up to date. fakeChecks says otherwise.
	convergeCheck = func(ui.Reporter, []string) (string, []checkReport, error) { return "", nil, nil }
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
	convergeReadInitial = func(*config.Config, string, bool) (render.EtcdInitial, bool, error) {
		return render.EtcdInitial{}, false, nil
	}
	t.Cleanup(func() { convergeRun, convergeReadInitial, convergeCheck = savedRun, savedRead, savedCheck })
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
	convergeReadInitial = func(*config.Config, string, bool) (render.EtcdInitial, bool, error) {
		read = true
		return render.EtcdInitial{}, false, errors.New("no host should be read")
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
	convergeRead = func(path, secrets string) (*config.Config, convergeState, error) {
		reads++
		cfg, err := config.Load(fixtureConfig())
		return cfg, convergeState{NeedsInit: reads == 1}, err
	}
	t.Cleanup(func() { convergeRead = saved })
	founded := initial("home-a", "vm")
	convergeReadInitial = func(_ *config.Config, m string, _ bool) (render.EtcdInitial, bool, error) {
		if reads == 1 {
			t.Errorf("%s's record read before init ran", m)
		}
		return founded, m == "home-a" || m == "vm", nil
	}
	if err := converge(t, "--execute"); err != nil {
		t.Fatal(err)
	}
	sameLines(t, *ran, append([]string{"init"}, commands(slices.Concat(hostsPhase, []string{"joining: site add home-b", "other sites: apply --site watch", "storage: storage add"}, passTwoAndDNS))...))
}

// The dry run names what only the operator can do.
func TestConvergeSaysWhatIsLeft(t *testing.T) {
	fakeConverge(t, nil)
	out := ""
	captureStdout(t, func() {})
	out = captureStdout(t, func() {
		if err := runApply([]string{"--config", fixtureConfig(), "--secrets", fixtureSecretsPath(), "--sudo=false"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"app admin create"} {
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
	_, st, err := readConvergeState(fixtureConfig(), filepath.Join(t.TempDir(), "secrets.enc.yaml"))
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

// A path given without --config is refused with the flag to use, not
// ignored for the default paisans.yaml.
func TestApplyRefusesAStrayArgument(t *testing.T) {
	fakeConverge(t, nil)
	err := runApply([]string{"--sudo=false", "staging/paisans.yaml"})
	if err == nil || !strings.Contains(err.Error(), "--config staging/paisans.yaml") {
		t.Errorf("err = %v", err)
	}
}

// site remove's usage entry names every flag it takes.
func TestSiteRemoveUsageNamesForceAndSSH(t *testing.T) {
	start := strings.Index(usage, "paisans site remove")
	entry := usage[start : start+strings.Index(usage[start:], "\n  paisans storage")]
	for _, flag := range []string{"--force", "--ssh"} {
		if !strings.Contains(entry, flag) {
			t.Errorf("site remove's usage leaves out %s:\n%s", flag, entry)
		}
	}
}

// recordConverge sends the commands' reports to a Recorder for the test.
func recordConverge(t *testing.T) *ui.Recorder {
	t.Helper()
	rec := &ui.Recorder{}
	saved := reporterOverride
	reporterOverride = rec
	t.Cleanup(func() { reporterOverride = saved })
	return rec
}

// Each etcd member's record is read inside a step of its own, so a spinner
// shows while ssh works; the validation findings come first and the plan
// after.
func TestConvergeReadsEachEtcdRecordInAStep(t *testing.T) {
	fakeConverge(t, nil)
	rec := recordConverge(t)
	var read []string
	convergeReadInitial = func(_ *config.Config, m string, _ bool) (render.EtcdInitial, bool, error) {
		last := rec.Events[len(rec.Events)-1]
		if last.Kind != "step" || last.Text != "read "+m+"'s etcd record" {
			t.Errorf("%s read outside its step; last event %+v", m, last)
		}
		read = append(read, m)
		return render.EtcdInitial{}, false, nil
	}
	for _, args := range [][]string{nil, {"--execute"}} {
		rec.Events = nil
		read = nil
		if err := converge(t, args...); err != nil {
			t.Fatal(err)
		}
		sameLines(t, read, fixture(t).Etcd.Members)
		findings, first := rec.Index("warn", ""), rec.Index("step", "etcd record")
		plan := rec.Index("section", "hosts")
		if findings < 0 || !(findings < first && first < plan) {
			t.Errorf("%v: findings at %d, first read at %d, plan at %d:\n%s", args, findings, first, plan, rec.Lines())
		}
		for _, m := range read {
			if !rec.Has("done", "read "+m+"'s etcd record") {
				t.Errorf("%v: %s's read step did not end:\n%s", args, m, rec.Lines())
			}
		}
	}
}

// holdRecorder is a Recorder that also records each hold and its resume, as
// a reporter that draws a spinner would pause it.
type holdRecorder struct{ *ui.Recorder }

func (h holdRecorder) Hold() func() {
	h.Events = append(h.Events, ui.Event{Kind: "hold"})
	return func() { h.Events = append(h.Events, ui.Event{Kind: "resume"}) }
}

// recordHolds is recordConverge, with holds recorded.
func recordHolds(t *testing.T) holdRecorder {
	t.Helper()
	h := holdRecorder{&ui.Recorder{}}
	saved := reporterOverride
	reporterOverride = h
	t.Cleanup(func() { reporterOverride = saved })
	return h
}

// A sudo prompt or an ssh host key question during a read pauses the
// spinner of the step that reads.
func TestAHoldDuringAnEtcdReadPausesItsSpinner(t *testing.T) {
	fakeConverge(t, nil)
	h := recordHolds(t)
	convergeReadInitial = func(_ *config.Config, m string, _ bool) (render.EtcdInitial, bool, error) {
		holdOutput()()
		return render.EtcdInitial{}, false, nil
	}
	if err := converge(t); err != nil {
		t.Fatal(err)
	}
	for _, m := range fixture(t).Etcd.Members {
		i := h.Index("step", "read "+m+"'s etcd record")
		if i < 0 || i+2 >= len(h.Events) || h.Events[i+1].Kind != "hold" || h.Events[i+2].Kind != "resume" {
			t.Errorf("%s's read was not held around its prompt:\n%s", m, h.Lines())
		}
	}
}

// A member that does not answer stops the plan: founding or joining cannot
// be decided. Its step is marked failed.
func TestAnEtcdMemberThatDoesNotAnswerStopsThePlan(t *testing.T) {
	ran := fakeConverge(t, nil)
	rec := recordConverge(t)
	convergeReadInitial = func(_ *config.Config, m string, _ bool) (render.EtcdInitial, bool, error) {
		return render.EtcdInitial{}, false, errors.New("ssh: connect timed out")
	}
	err := converge(t, "--execute")
	if err == nil || !strings.Contains(err.Error(), "connect timed out") {
		t.Fatalf("err = %v", err)
	}
	if !rec.Has("fail", "etcd record") || len(*ran) != 0 {
		t.Errorf("ran %v:\n%s", *ran, rec.Lines())
	}
}

// fakeChecks replaces each step's check with found, by title: a summary, or
// an error. A title found lacks is up to date. It returns the titles checked,
// in order.
func fakeChecks(t *testing.T, found map[string]any) *[]string {
	t.Helper()
	var checked []string
	saved := convergeCheck
	convergeCheck = func(_ ui.Reporter, args []string) (string, []checkReport, error) {
		title := strings.Join(args, " ")
		if i := strings.Index(title, " --config"); i >= 0 {
			title = title[:i]
		}
		if slices.Contains(args, "--execute") {
			t.Errorf("%s checked with --execute", title)
		}
		checked = append(checked, title)
		switch f := found[title].(type) {
		case error:
			return "", nil, f
		case string:
			return f, nil, nil
		}
		return "", nil, nil
	}
	t.Cleanup(func() { convergeCheck = saved })
	return &checked
}

// statuses is each plan step's status as "mark title: result", in order,
// from the events of the dry run's steps after the etcd reads.
func statuses(rec *ui.Recorder) []string {
	var out []string
	for _, e := range rec.Events {
		if e.Kind == "section" && e.Text == "etcd members" {
			out = nil
		}
		mark := map[string]string{"done": "✓", "pending": "○", "waiting": "·", "fail": "✗"}[e.Kind]
		if mark == "" || strings.Contains(e.Text, "etcd record") {
			continue
		}
		line := mark + " " + e.Text
		if e.Extra != "" {
			line += ": " + e.Extra
		}
		out = append(out, line)
	}
	return out
}

// A deployment that is up to date checks every command once, each line ✓,
// and an apply in pass two takes its earlier apply's status.
func TestConvergeDryRunMarksAnUpToDateDeploymentDone(t *testing.T) {
	fakeConverge(t, nil)
	checked := fakeChecks(t, nil)
	rec := recordConverge(t)
	if err := converge(t); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, l := range blankPlan() {
		_, title, _ := strings.Cut(l, ": ")
		want = append(want, "✓ "+title)
	}
	sameLines(t, statuses(rec), want)
	seen := map[string]bool{}
	for _, c := range *checked {
		if seen[c] {
			t.Errorf("%s checked twice", c)
		}
		seen[c] = true
	}
	if rec.Has("item", "") {
		t.Errorf("a dry run listed items:\n%s", rec.Lines())
	}
}

// A pending step says what it would change, and what waits on it says so,
// unchecked. A check that fails is marked with its error and the rest are
// still checked.
func TestConvergeDryRunMarksPendingWaitingAndFailed(t *testing.T) {
	fakeConverge(t, nil)
	checked := fakeChecks(t, map[string]any{
		"host prepare --site home-b": "3 changes",
		"apply --site vm":            "5 changes",
		"host prepare --site watch":  errors.New("ssh: connect to host watch.local: timed out\nmore detail"),
		"dns init":                   "2 records to create",
	})
	rec := recordConverge(t)
	if err := converge(t); err != nil {
		t.Fatal(err)
	}
	sameLines(t, statuses(rec), []string{
		"✓ host prepare --site home-a",
		"○ host prepare --site home-b: 3 changes",
		"✓ host prepare --site vm",
		"✗ host prepare --site watch: ssh: connect to host watch.local: timed out",
		"○ apply --site vm: 5 changes",
		"· apply --site home-a: after apply --site vm",
		"· apply --site home-b: after host prepare --site home-b",
		"· apply --site watch: after host prepare --site watch",
		"· storage add: after host prepare --site home-b",
		"· apply --site home-a: after apply --site home-a in founding",
		"· apply --site home-b: after host prepare --site home-b",
		"· apply --site vm: after apply --site vm in founding",
		"· apply --site watch: after host prepare --site watch",
		"○ dns init: 2 records to create",
	})
	for _, never := range []string{"apply --site home-a", "apply --site home-b", "apply --site watch", "storage add"} {
		if slices.Contains(*checked, never) {
			t.Errorf("%s was checked while it waits", never)
		}
	}
}

// With init to run, every other step waits on it: it writes the subnet and
// the secrets they read.
func TestConvergeDryRunWaitsOnInit(t *testing.T) {
	fakeConverge(t, nil)
	checked := fakeChecks(t, nil)
	rec := recordConverge(t)
	saved := convergeRead
	convergeRead = func(path, secrets string) (*config.Config, convergeState, error) {
		cfg, err := config.Load(fixtureConfig())
		return cfg, convergeState{NeedsInit: true, InitWhy: "no secrets file yet"}, err
	}
	t.Cleanup(func() { convergeRead = saved })
	if err := converge(t); err != nil {
		t.Fatal(err)
	}
	got := statuses(rec)
	if len(got) == 0 || got[0] != "○ init: no secrets file yet" {
		t.Fatalf("init's line: %v", got)
	}
	for _, l := range got[1:] {
		if !strings.HasPrefix(l, "· ") || !strings.HasSuffix(l, ": after init") {
			t.Errorf("does not wait on init: %s", l)
		}
	}
	if len(*checked) != 0 {
		t.Errorf("checked %v", *checked)
	}
}

// A founded cluster's members are applied first in pass two, so that apply
// is checked there, after storage.
func TestConvergeDryRunChecksAFoundedMemberInPassTwo(t *testing.T) {
	ran := fakeConverge(t, nil)
	_ = ran
	founded := initial("home-a", "home-b", "vm")
	convergeReadInitial = func(*config.Config, string, bool) (render.EtcdInitial, bool, error) { return founded, true, nil }
	checked := fakeChecks(t, map[string]any{"storage add": "1 stage"})
	rec := recordConverge(t)
	if err := converge(t); err != nil {
		t.Fatal(err)
	}
	sameLines(t, statuses(rec), []string{
		"✓ host prepare --site home-a",
		"✓ host prepare --site home-b",
		"✓ host prepare --site vm",
		"✓ host prepare --site watch",
		"✓ apply --site watch",
		"○ storage add: 1 stage",
		"· apply --site home-a: after storage add",
		"· apply --site home-b: after storage add",
		"· apply --site vm: after storage add",
		"· apply --site watch: after storage add",
		"✓ dns init",
	})
	if !slices.Contains(*checked, "storage add") {
		t.Errorf("storage add not checked: %v", *checked)
	}
}

// -v says why each step is in the plan, under its line.
func TestConvergeDryRunSaysWhyWhenVerbose(t *testing.T) {
	fakeConverge(t, nil)
	fakeChecks(t, nil)
	rec := recordConverge(t)
	if err := converge(t, "-v"); err != nil {
		t.Fatal(err)
	}
	if i := rec.Index("detail", "a witness is founded first"); i < 0 || rec.Events[i].Extra != "apply --site vm" {
		t.Errorf("no why under the witness's line:\n%s", rec.Lines())
	}
}

// A step is checked through its own dry run, quietly: nothing it reports
// reaches the run's reporter, a hold for a prompt pauses the run's spinner,
// and once it returns the run's reporter is the commands' again.
func TestCheckStepIsQuietAndHoldsTheRunsSpinner(t *testing.T) {
	h := recordHolds(t)
	routeHolds(h)
	run := func(args []string) error {
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		reporter := commonFlags(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		r := reporter()
		r.Section("its own plan")
		r.Item("write 8 files")
		holdOutput()()
		dryRunFound("8 changes", nil)
		r.Result("Nothing changed.")
		return nil
	}
	summary, _, err := checkStep(h, []string{"apply"}, run)
	if err != nil || summary != "8 changes" {
		t.Fatalf("summary %q, err %v", summary, err)
	}
	var kinds []string
	for _, e := range h.Events {
		kinds = append(kinds, e.Kind)
	}
	sameLines(t, kinds, []string{"hold", "resume"})
	if reporterOverride != ui.Reporter(h) {
		t.Errorf("the reporter was not given back: %T", reporterOverride)
	}
	holdOutput()()
	if n := len(h.Events); n != 4 {
		t.Errorf("a hold after the check did not reach the run's reporter:\n%s", h.Lines())
	}
}

// A check fails with the command's error, with what its dry run found
// --execute would stop at, or when the command never said what it would
// change.
func TestCheckStepFails(t *testing.T) {
	recordConverge(t)
	conflict := fmt.Errorf("1 record conflicts with the provider's: %w", dns.ErrConflict)
	for name, run := range map[string]func([]string) error{
		"error":   func([]string) error { return errors.New("ssh: timed out") },
		"stop":    func([]string) error { dryRunFound("", conflict); return nil },
		"silence": func([]string) error { return nil },
	} {
		summary, _, err := checkStep(ui.Discard, []string{"dns", "init"}, run)
		if err == nil || summary != "" {
			t.Errorf("%s: summary %q, err %v", name, summary, err)
		}
	}
	if _, _, err := checkStep(ui.Discard, []string{"dns", "init"}, func([]string) error { dryRunFound("", conflict); return nil }); !errors.Is(err, dns.ErrConflict) {
		t.Errorf("the stop is not the error: %v", err)
	}
}

// Each command's dry run says what it would change: host prepare on an
// empty host, through the fake ssh, has steps to run.
func TestHostPrepareDryRunSaysWhatItWouldChange(t *testing.T) {
	recordConverge(t)
	quietSSH(t)
	o := convergeOptions{Config: fixtureConfig(), Secrets: fixtureSecretsPath()}
	for _, args := range [][]string{{"host", "prepare", "--site", "home-a"}, {"storage", "add"}} {
		summary, _, err := checkStep(ui.Discard, convergeFlags(args, o), runStep)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !regexp.MustCompile(`^[1-9][0-9]* [a-z]+s?$`).MatchString(summary) {
			t.Errorf("%v: summary %q", args, summary)
		}
	}
}

// apply's dry run says what it would change, and a plan --execute would
// refuse, here for an image volume its service does not mount, is the
// check's failure rather than pending work.
func TestApplyDryRunSaysWhatItWouldChangeOrRefuse(t *testing.T) {
	recordConverge(t)
	quietSSH(t)
	o := convergeOptions{Config: fixtureConfig(), Secrets: fixtureSecretsPath()}
	args := []string{"apply", "--site", "watch"}
	summary, _, err := checkStep(ui.Discard, convergeFlags(args, o), runStep)
	if err != nil || !regexp.MustCompile(`^[1-9][0-9]* changes?$`).MatchString(summary) {
		t.Errorf("summary %q, err %v", summary, err)
	}
	unmounted := strings.Replace(quietHost, `echo "volumes $r null"`, `echo "volumes $r {\"/unmounted\":{}}"`, 1)
	if unmounted == quietHost {
		t.Fatal("the fake host no longer answers the volumes probe")
	}
	sshAnswering(t, unmounted)
	summary, _, err = checkStep(ui.Discard, convergeFlags(args, o), runStep)
	if err == nil || summary != "" {
		t.Errorf("a plan --execute refuses was %q, err %v", summary, err)
	}
}

// convergeShaped is converge on the fixture as edit changes it, with every
// etcd member reading as recorded holds it.
func convergeShaped(t *testing.T, edit func(*config.Config), recorded map[string]render.EtcdInitial) error {
	t.Helper()
	saved := convergeRead
	convergeRead = func(path, secrets string) (*config.Config, convergeState, error) {
		cfg, err := config.Load(fixtureConfig())
		if err == nil {
			edit(cfg)
			if result := validate.Check(cfg); result.Refused() {
				t.Fatalf("validate refuses the edited fixture: %v", result.Refusals())
			}
		}
		return cfg, convergeState{}, err
	}
	t.Cleanup(func() { convergeRead = saved })
	convergeReadInitial = func(_ *config.Config, m string, _ bool) (render.EtcdInitial, bool, error) {
		in, ok := recorded[m]
		return in, ok, nil
	}
	return converge(t)
}

// What each step waits on, for each shape of deployment: a member joining
// through site add, one Garage site, no etcd, no Garage, and a monitor in
// etcd.members.
func TestConvergeDryRunWaitsFollowTheDeployment(t *testing.T) {
	founded := initial("home-a", "vm")
	for _, c := range []struct {
		name     string
		edit     func(*config.Config)
		recorded map[string]render.EtcdInitial
		found    map[string]any
		want     []string
	}{
		{
			name:     "joining",
			edit:     func(*config.Config) {},
			recorded: map[string]render.EtcdInitial{"home-a": founded, "vm": founded},
			found:    map[string]any{"site add home-b": "4 stages"},
			want: []string{
				"✓ host prepare --site home-a", "✓ host prepare --site home-b", "✓ host prepare --site vm", "✓ host prepare --site watch",
				"○ site add home-b: 4 stages",
				"✓ apply --site watch",
				"· storage add: after site add home-b",
				"· apply --site home-a: after storage add",
				"· apply --site home-b: after site add home-b",
				"· apply --site vm: after storage add",
				"· apply --site watch: after storage add",
				"✓ dns init",
			},
		},
		{
			name: "one Garage site",
			edit: func(cfg *config.Config) {
				cfg.Storage.Garage.Sites = []string{"home-a"}
				cfg.Storage.Garage.Replication = 1
			},
			found: map[string]any{"apply --site home-b": "2 changes", "apply --site home-a": "3 changes"},
			want: []string{
				"✓ host prepare --site home-a", "✓ host prepare --site home-b", "✓ host prepare --site vm", "✓ host prepare --site watch",
				"✓ apply --site vm",
				"○ apply --site home-a: 3 changes",
				"○ apply --site home-b: 2 changes",
				"✓ apply --site watch",
				"· storage init --site home-a: after apply --site home-a",
				"· apply --site home-a: after apply --site home-a in founding",
				"· apply --site home-b: after apply --site home-b in founding",
				"· apply --site vm: after storage init --site home-a",
				"· apply --site watch: after storage init --site home-a",
				"✓ dns init",
			},
		},
		{
			name:  "no etcd",
			edit:  func(cfg *config.Config) { cfg.Etcd.Members = nil },
			found: map[string]any{"host prepare --site vm": "1 change"},
			want: []string{
				"✓ host prepare --site home-a", "✓ host prepare --site home-b", "○ host prepare --site vm: 1 change", "✓ host prepare --site watch",
				"✓ apply --site home-a",
				"✓ apply --site home-b",
				"· apply --site vm: after host prepare --site vm",
				"✓ apply --site watch",
				"✓ storage add",
				"✓ apply --site home-a",
				"✓ apply --site home-b",
				"· apply --site vm: after host prepare --site vm",
				"✓ apply --site watch",
				"✓ dns init",
			},
		},
		{
			name:  "no Garage",
			edit:  func(cfg *config.Config) { cfg.Storage.Garage.Sites = nil },
			found: map[string]any{"apply --site watch": "1 change"},
			want: []string{
				"✓ host prepare --site home-a", "✓ host prepare --site home-b", "✓ host prepare --site vm", "✓ host prepare --site watch",
				"✓ apply --site vm",
				"✓ apply --site home-a",
				"✓ apply --site home-b",
				"○ apply --site watch: 1 change",
				"✓ apply --site home-a",
				"✓ apply --site home-b",
				"✓ apply --site vm",
				"· apply --site watch: after apply --site watch in other sites",
				"✓ dns init",
			},
		},
		{
			name:  "a monitor in etcd.members",
			edit:  func(cfg *config.Config) { cfg.Etcd.Members = []string{"home-a", "home-b", "vm", "watch"} },
			found: map[string]any{"apply --site vm": "6 changes"},
			want: []string{
				"✓ host prepare --site home-a", "✓ host prepare --site home-b", "✓ host prepare --site vm", "✓ host prepare --site watch",
				"○ apply --site vm: 6 changes",
				"· apply --site home-a: after apply --site vm",
				"· apply --site home-b: after apply --site vm",
				"· apply --site watch: after apply --site vm",
				"· storage add: after apply --site home-a",
				"· apply --site home-a: after apply --site home-a in founding",
				"· apply --site home-b: after apply --site home-b in founding",
				"· apply --site vm: after apply --site vm in founding",
				"· apply --site watch: after apply --site watch in founding",
				"✓ dns init",
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			fakeConverge(t, nil)
			fakeChecks(t, c.found)
			rec := recordConverge(t)
			if err := convergeShaped(t, c.edit, c.recorded); err != nil {
				t.Fatal(err)
			}
			sameLines(t, statuses(rec), c.want)
		})
	}
}

// editedFixture is the fixture configuration with each old replaced by its
// new, written where the secrets fixture is still named by path.
func editedFixture(t *testing.T, pairs ...string) string {
	t.Helper()
	data, err := os.ReadFile(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for i := 0; i < len(pairs); i += 2 {
		edited := strings.Replace(text, pairs[i], pairs[i+1], 1)
		if edited == text {
			t.Fatalf("the fixture has no %q", pairs[i])
		}
		text = edited
	}
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// storage init says what it would change, through its own dry run against
// a Garage behind the fake ssh with no layout, key or bucket yet.
func TestStorageInitDryRunSaysWhatItWouldChange(t *testing.T) {
	recordConverge(t)
	garage := strings.Replace(quietHost, `*"getent passwd"*)`, `*" layout show"*) echo "Current cluster layout version: 0";;
*" node id -q"*) echo "0123456789abcdef0123@10.44.0.1:3901";;
*" key info "*) echo "0 matching keys";;
*" bucket info "*) echo "Bucket not found";;
*"getent passwd"*)`, 1)
	sshAnswering(t, garage)
	one := editedFixture(t, "sites: [home-a, home-b]\n    replication: 2", "sites: [home-a]\n    replication: 1")
	o := convergeOptions{Config: one, Secrets: fixtureSecretsPath()}
	summary, _, err := checkStep(ui.Discard, convergeFlags([]string{"storage", "init", "--site", "home-a"}, o), runStep)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[1-9][0-9]* changes?$`).MatchString(summary) {
		t.Errorf("summary %q", summary)
	}
}

// init, run as a step, points the holds at its own reporter. The etcd reads
// after it hold the run's spinner again.
func TestHoldsReturnToTheRunAfterInit(t *testing.T) {
	fakeConverge(t, nil)
	h := recordHolds(t)
	saved, savedRun := convergeRead, convergeRun
	reads := 0
	convergeRead = func(path, secrets string) (*config.Config, convergeState, error) {
		reads++
		cfg, err := config.Load(fixtureConfig())
		return cfg, convergeState{NeedsInit: reads == 1}, err
	}
	convergeRun = func(args []string) error {
		if args[0] == "init" {
			routeHolds(ui.Discard)
		}
		return nil
	}
	t.Cleanup(func() { convergeRead, convergeRun = saved, savedRun })
	convergeReadInitial = func(*config.Config, string, bool) (render.EtcdInitial, bool, error) {
		holdOutput()()
		return render.EtcdInitial{}, false, nil
	}
	if err := converge(t, "--execute"); err != nil {
		t.Fatal(err)
	}
	if !h.Has("hold", "") {
		t.Errorf("a prompt during an etcd read after init did not hold the run's spinner:\n%s", h.Lines())
	}
}

// Every title the plan and the etcd reads can show is in convergeTitles, so
// the result column is aligned for each of them.
func TestConvergeTitlesCoverEveryLine(t *testing.T) {
	cfg := fixture(t)
	titles := convergeTitles(cfg)
	for _, st := range []convergeState{{}, {NeedsInit: true}, {Initial: map[string]render.EtcdInitial{"vm": initial("vm", "home-a"), "home-a": initial("vm", "home-a")}}} {
		for _, s := range convergePlan(cfg, st) {
			if !slices.Contains(titles, s.Title) {
				t.Errorf("%q is not in convergeTitles", s.Title)
			}
		}
	}
	for _, m := range cfg.Etcd.Members {
		if !slices.Contains(titles, "read "+m+"'s etcd record") {
			t.Errorf("the read of %s is not in convergeTitles", m)
		}
	}
}

// What a step's own dry run reports while it is checked is collected rather
// than dropped: its warnings, notes, refusals and ssh retries.
func TestCheckStepCollectsWhatTheDryRunReports(t *testing.T) {
	t.Cleanup(func() { apply.SetRetryLog(nil) })
	rec := &ui.Recorder{Verbose_: true}
	summary, reports, err := checkStep(rec, []string{"apply", "--site", "home-a"}, func([]string) error {
		inner := reporterOverride
		inner.Warn("held back from this apply", "talk's image is newer than its pin")
		inner.Note("secrets: smtp.password is owed", "paisans secrets set smtp.password < value")
		inner.Refuse("garage is not running", "start it first")
		routeRetries(inner)
		retryOnce(t)
		dryRunFound("3 changes", nil)
		return nil
	})
	if err != nil || summary != "3 changes" {
		t.Fatalf("summary %q, err %v", summary, err)
	}
	want := []checkReport{
		{Text: "! held back from this apply: talk's image is newer than its pin"},
		{Note: true, Text: "secrets: smtp.password is owed", Detail: "paisans secrets set smtp.password < value"},
		{Text: "✗ garage is not running: start it first"},
	}
	if len(reports) != 4 || !reflect.DeepEqual(reports[:3], want) || !strings.Contains(reports[3].Text, "ssh could not connect") {
		t.Errorf("reports:\n%#v", reports)
	}
	if len(rec.Events) != 0 {
		t.Errorf("the check drew on the run's reporter:\n%s", rec.Lines())
	}
}

// A check's reports print under its status line: everything but a note as a
// detail of the line, which only -v shows, and a note after the line at
// every verbosity.
func TestCheckReportsShowUnderTheStep(t *testing.T) {
	fakeConverge(t, nil)
	saved := convergeCheck
	convergeCheck = func(_ ui.Reporter, args []string) (string, []checkReport, error) {
		if strings.Join(args[:3], " ") == "apply --site vm" {
			return "5 changes", []checkReport{{Text: "! held back from this apply"}, {Note: true, Text: "owed", Detail: "paisans secrets set x"}}, nil
		}
		return "", nil, nil
	}
	t.Cleanup(func() { convergeCheck = saved })
	rec := withRecorder(t, true)
	if err := converge(t); err != nil {
		t.Fatal(err)
	}
	pending := slices.IndexFunc(rec.Events, func(e ui.Event) bool { return e.Kind == "pending" && e.Text == "apply --site vm" })
	detail := slices.IndexFunc(rec.Events, func(e ui.Event) bool {
		return e.Kind == "detail" && e.Extra == "apply --site vm" && e.Text == "! held back from this apply"
	})
	note := slices.IndexFunc(rec.Events, func(e ui.Event) bool { return e.Kind == "note" && e.Text == "owed" })
	if pending < 0 || detail < 0 || note < 0 || detail > pending || note < pending {
		t.Errorf("pending %d, detail %d, note %d:\n%s", pending, detail, note, rec.Lines())
	}
}

// A secrets file that exists but does not read (no age key, say) stops the
// run at once, before any host is read: init cannot fix it, since init needs
// the same key.
func TestConvergeStopsOnASecretsFileThatDoesNotRead(t *testing.T) {
	fakeConverge(t, nil)
	read := false
	convergeReadInitial = func(*config.Config, string, bool) (render.EtcdInitial, bool, error) {
		read = true
		return render.EtcdInitial{}, false, nil
	}
	unreadable := t.TempDir() // a directory: present, and not a secrets file
	var err error
	captureStdout(t, func() {
		err = runApply([]string{"--config", fixtureConfig(), "--secrets", unreadable, "--sudo=false"})
	})
	if err == nil || !strings.Contains(err.Error(), unreadable) {
		t.Errorf("err = %v, want it to name %s", err, unreadable)
	}
	if read {
		t.Error("a host was read with secrets that do not read")
	}
}

// A stop ends the run with a Problem naming the step. Its explanation is the
// step's own problem, hint and explanation, and then how to resume, so the
// reason shows without -v.
func TestConvergeStopIsAProblem(t *testing.T) {
	unreachable := &ui.Problem{Hint: "ubuntu@203.0.113.10 cannot be reached over ssh", Explain: "Check that the host is up.", Cause: apply.ErrUnreachable}
	waiting := &storageadd.Waiting{Stage: &storageadd.Stage{Number: 3, Name: "sync"}, Detail: "blocks are still moving"}
	for _, tc := range []struct {
		step    string
		err     error
		hint    string
		explain []string
		code    int
	}{
		{"apply --site vm", unreachable, "apply stopped at apply --site vm", []string{"ubuntu@203.0.113.10 cannot be reached over ssh.", "Check that the host is up.", "Run paisans apply --execute again to resume."}, 1},
		{"apply --site vm", errors.New("vm: boom"), "apply stopped at apply --site vm", []string{"vm: boom.", "Run paisans apply --execute again to resume."}, 1},
		{"storage add", waiting, "apply is waiting at storage add", []string{"storage add is waiting on Garage at stage 3 (sync): blocks are still moving.", "Nothing failed: Garage is still moving data. Run paisans apply --execute again later."}, 75},
	} {
		fakeConverge(t, map[string]error{tc.step: tc.err})
		err := converge(t, "--execute")
		var p *ui.Problem
		if !errors.As(err, &p) {
			t.Fatalf("%s: not a ui.Problem: %v", tc.step, err)
		}
		if p.Hint != tc.hint || p.Explain != strings.Join(tc.explain, "\n") {
			t.Errorf("%s: got %q\n%q", tc.step, p.Hint, p.Explain)
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: the step's error is not wrapped", tc.step)
		}
		var b strings.Builder
		if code := reportError(&b, err, false); code != tc.code {
			t.Errorf("%s: exit %d, want %d", tc.step, code, tc.code)
		}
	}
}

// A dry run's failed step says what to do at every verbosity, not only its
// hint: the explanation is a note after the step's line.
func TestConvergeDryRunShowsAFailedStepsExplanation(t *testing.T) {
	fakeConverge(t, nil)
	fakeChecks(t, map[string]any{
		"host prepare --site watch": &ui.Problem{Hint: "watch's etcd members differ from the running cluster's", Explain: "Run paisans site add for the site being added, then apply."},
	})
	rec := recordConverge(t)
	if err := converge(t); err != nil {
		t.Fatal(err)
	}
	i := rec.Index("note", "host prepare --site watch")
	if i < 0 || rec.Events[i].Extra != "Run paisans site add for the site being added, then apply." {
		t.Errorf("no note with the explanation:\n%s", rec.Lines())
	}
	if f := rec.Index("fail", "host prepare --site watch"); f < 0 || f > i {
		t.Errorf("the note is not after the step's line:\n%s", rec.Lines())
	}
}

// The founding stop's note is its hint and explanation, without a doubled
// stop or a trailing blank.
func TestTheFoundingNoteReadsCleanly(t *testing.T) {
	fakeConverge(t, map[string]error{"apply --site home-a": &ui.Problem{Hint: "home-a is waiting on home-b to start etcd.", Cause: apply.ErrFoundingWait}})
	rec := recordConverge(t)
	if err := converge(t, "--execute"); err != nil {
		t.Fatal(err)
	}
	i := rec.Index("note", "founding stop")
	if i < 0 || rec.Events[i].Extra != "home-a is waiting on home-b to start etcd." {
		t.Errorf("note:\n%s", rec.Lines())
	}
}

// An --ssh that is not a destination is said once, plainly.
func TestSSHFlagProblemSaysItOnce(t *testing.T) {
	err := sshFlagProblem("myalias", errors.New(`"myalias" is not user@host[:port]`))
	var p *ui.Problem
	if !errors.As(err, &p) || p.Hint != "--ssh is not a destination" {
		t.Errorf("got %#v", p)
	}
}

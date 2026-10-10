package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/dns"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/storageadd"
	"github.com/paisans-software/paisans-stack/internal/ui"
	"github.com/paisans-software/paisans-stack/internal/validate"
	"gopkg.in/yaml.v3"
)

// convergeState is what decides the plan of `paisans apply` without --site:
// whether init has work, and each etcd member's infra/etcd-initial record,
// for the members that hold one. See docs/specs/2026-10-09-apply-converge.md.
type convergeState struct {
	NeedsInit bool
	// InitWhy is what init has to do, by name, for the plan to say.
	InitWhy string
	Initial map[string]render.EtcdInitial
}

// clusterNames is the member names an --initial-cluster lists.
func clusterNames(in render.EtcdInitial) []string {
	var out []string
	for _, pair := range strings.Split(in.Cluster, ",") {
		if name, _, ok := strings.Cut(strings.TrimSpace(pair), "="); ok && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// convergeStep is one existing command the plan runs.
type convergeStep struct {
	Phase string
	// Title is what the operator reads, the command as typed less its
	// common flags.
	Title string
	Why   string
	// Args is the command and its own flags; the run adds the common ones,
	// by convergeFlags.
	Args []string
	// Founding is an apply whose founding stop is expected: pass two
	// applies it again.
	Founding bool
}

func step(phase, why string, founding bool, args ...string) convergeStep {
	title := args[0]
	for _, a := range args[1:] {
		title += " " + a
	}
	return convergeStep{Phase: phase, Title: title, Why: why, Args: args, Founding: founding}
}

func applyStep(phase, why, site string, founding bool) convergeStep {
	return step(phase, why, founding, "apply", "--site", site)
}

// convergePlan is every step that takes cfg to a complete, running stack from
// what st read, in the order the deployment needs them.
func convergePlan(cfg *config.Config, st convergeState) []convergeStep {
	var steps []convergeStep
	if st.NeedsInit {
		why := st.InitWhy
		if why == "" {
			why = "the deployment id, the mesh subnet or a generated secret is missing"
		}
		steps = append(steps, step("configuration", why, false, "init"))
	}
	for _, s := range cfg.SiteNames() {
		steps = append(steps, step("hosts", "Docker, WireGuard and the firewall; a prepared host plans nothing", false, "host", "prepare", "--site", s))
	}

	// A member with no record is still being founded when no member has a
	// record, or when a record's --initial-cluster lists it: the founding
	// set is fixed by the first apply that wrote one, and its members are
	// applied, witness first. One no record lists joins the running cluster
	// through site add.
	members := cfg.Etcd.Members
	listed := map[string]bool{}
	for _, in := range st.Initial {
		for _, n := range clusterNames(in) {
			listed[n] = true
		}
	}
	founding := func(m string) bool {
		_, recorded := st.Initial[m]
		return !recorded && (len(st.Initial) == 0 || listed[m])
	}
	for _, m := range members {
		if founding(m) && cfg.Sites[m].Has(config.RoleWitness) {
			steps = append(steps, applyStep("founding", "a witness is founded first: nothing waits on it", m, true))
		}
	}
	for _, m := range members {
		if founding(m) && !cfg.Sites[m].Has(config.RoleWitness) {
			steps = append(steps, applyStep("founding", "founds its etcd member; it may stop until the other members run, and pass two resumes it", m, true))
		}
	}
	for _, m := range members {
		if _, recorded := st.Initial[m]; !recorded && !founding(m) {
			steps = append(steps, step("joining", "the cluster runs and this member has not joined it", false, "site", "add", m))
		}
	}

	// A monitor checks every other site and app, so it is applied after
	// them, in phase three and in pass two. One in etcd.members is founded
	// with the other members, since etcd needs it, and is still last in pass
	// two.
	monitors := cfg.MonitorSites()
	ordered := func(include func(string) bool) []string {
		var first, last []string
		for _, s := range cfg.SiteNames() {
			switch {
			case !include(s):
			case slices.Contains(monitors, s):
				last = append(last, s)
			default:
				first = append(first, s)
			}
		}
		return append(first, last...)
	}
	for _, s := range ordered(func(s string) bool { return !slices.Contains(members, s) }) {
		steps = append(steps, applyStep("other sites", "not an etcd member; monitor sites last", s, false))
	}

	switch garage := cfg.Storage.Garage.Sites; len(garage) {
	case 0:
	case 1:
		steps = append(steps, step("storage", "Garage's layout, each app's bucket and key", false, "storage", "init", "--site", garage[0]))
	default:
		steps = append(steps, step("storage", "Garage's layout across its sites, each app's bucket and key", false, "storage", "add"))
	}

	for _, s := range ordered(func(string) bool { return true }) {
		steps = append(steps, applyStep("pass two", "resumes a founding stop and moves what earlier steps changed; an applied site plans nothing", s, false))
	}
	steps = append(steps, step("dns", "creates the records that are missing; one pointing elsewhere stops it", false, "dns", "init"))
	return steps
}

// convergeRun runs one step: the existing command, in this process, so it
// plans, gates, claims and reports as typed by hand, and the sudo password
// each host asked for is asked once for the whole run. Tests replace it.
var convergeRun func(args []string) error

// The step runner and the check are set here rather than where they are
// declared: each reaches runApply, which reaches it back.
func init() {
	convergeRun = runStep
	convergeCheck = func(r ui.Reporter, args []string) (string, []checkReport, error) {
		return checkStep(r, args, runStep)
	}
}

func runStep(args []string) error {
	switch {
	case args[0] == "init":
		return runInit(args[1:])
	case args[0] == "host" && args[1] == "prepare":
		return runHostPrepare(args[2:])
	case args[0] == "apply":
		return runApply(args[1:])
	case args[0] == "site" && args[1] == "add":
		return runSiteAdd(args[2:])
	case args[0] == "storage" && args[1] == "init":
		return runStorageInit(args[2:])
	case args[0] == "storage" && args[1] == "add":
		return runStorageAdd(args[2:])
	case args[0] == "dns" && args[1] == "init":
		return runDNSInit(args[2:])
	}
	return fmt.Errorf("apply: no command %q", strings.Join(args, " "))
}

// convergeReadInitial reads one etcd member's infra/etcd-initial record
// through its ssh section. Tests replace it.
var convergeReadInitial = func(cfg *config.Config, member string, sudo bool) (render.EtcdInitial, bool, error) {
	return apply.ReadEtcdInitial(siteTransport(member, cfg.Sites[member], "", sudo), cfg.Deployment())
}

// convergeFounded reads which etcd members hold infra/etcd-initial, each in
// a step of its own so a spinner shows while ssh works. A member that does
// not answer is an error: whether the cluster is founded decides between
// founding and joining.
func convergeFounded(r ui.Reporter, cfg *config.Config, sudo bool) (map[string]render.EtcdInitial, error) {
	records := map[string]render.EtcdInitial{}
	if len(cfg.Etcd.Members) > 0 {
		r.Section("etcd members")
	}
	for _, m := range cfg.Etcd.Members {
		s := r.Step("read " + m + "'s etcd record")
		in, recorded, err := convergeReadInitial(cfg, m, sudo)
		if err != nil {
			s.Fail(err)
			return nil, fmt.Errorf("apply: reading whether %s's etcd member has been founded: %w. Every etcd member is read before anything runs", m, err)
		}
		if recorded {
			records[m] = in
			s.Done("founded")
		} else {
			s.Done("not founded yet")
		}
	}
	return records, nil
}

// convergeOptions is what apply without --site hands on to its steps: the
// flags every command takes, and apply's own that are not about one site.
type convergeOptions struct {
	Config, Secrets string
	Execute, Sudo   bool
	// Verbose is -v, which every command takes.
	Verbose bool
	// KeepImages and MinFree are apply's, so each apply step takes them.
	KeepImages bool
	MinFree    string
}

// convergeFlags is args with the flags o hands on, each to the commands that
// take it: --config and -v to every one, --secrets to all but host prepare,
// --execute to all but init, --sudo to all but dns, and --min-free and
// --keep-images to apply alone.
func convergeFlags(args []string, o convergeOptions) []string {
	out := append([]string(nil), args...)
	out = append(out, "--config", o.Config)
	takesSecrets := !(args[0] == "host")
	takesExecute := args[0] != "init"
	takesSudo := !(args[0] == "dns")
	if takesSecrets && o.Secrets != "" {
		out = append(out, "--secrets", o.Secrets)
	}
	if takesExecute && o.Execute {
		out = append(out, "--execute")
	}
	if takesSudo {
		out = append(out, fmt.Sprintf("--sudo=%t", o.Sudo))
	}
	if o.Verbose {
		out = append(out, "-v")
	}
	if args[0] == "apply" {
		if o.MinFree != "" {
			out = append(out, "--min-free", o.MinFree)
		}
		if o.KeepImages {
			out = append(out, "--keep-images")
		}
	}
	return out
}

// convergeRead reads what decides the plan on this machine. Tests replace
// it.
var convergeRead = readConvergeState

// readConvergeState reads what decides the plan without reaching a host:
// whether init has work (no id, no mesh subnet, no secrets file, or a
// generated secret missing). With no id there is no deployment to read on a
// host yet, and the configuration is nil. Each etcd member's record is read
// by convergeFounded, once the configuration is validated.
func readConvergeState(configPath, secretsPath string) (*config.Config, convergeState, error) {
	var st convergeState
	cfg, err := config.Load(configPath)
	if err != nil {
		if !missingID(configPath) {
			return nil, st, err
		}
		st.NeedsInit = true
		return nil, st, nil
	}
	var why []string
	if cfg.Mesh.Subnet == "" {
		why = append(why, "no mesh subnet")
	}
	if secretsPath == "" {
		secretsPath = filepath.Join(filepath.Dir(configPath), "secrets.enc.yaml")
	}
	if secrets, err := config.LoadSecrets(secretsPath); errors.Is(err, fs.ErrNotExist) {
		why = append(why, "no secrets file yet")
	} else if err != nil {
		// A file that is there and does not read (no age key, most often)
		// is not init's to fix: init needs the same key. So nothing is
		// planned and no host is read.
		return nil, st, fmt.Errorf("apply: %w", err)
	} else if filled, err := secretsgen.Fill(cfg, secrets); err != nil {
		return nil, st, fmt.Errorf("apply: generating the secrets %s lacks: %w", secretsPath, err)
	} else if filled.Changed() {
		why = append(why, missingSecrets(filled.Generated))
	}
	if len(why) > 0 {
		st.NeedsInit = true
		st.InitWhy = strings.Join(why, "; ")
	}
	return cfg, st, nil
}

// missingSecrets names the generated secrets init would create: the first
// three, and how many more. Names only; a value never reaches the terminal.
func missingSecrets(names []string) string {
	if len(names) == 1 {
		return "generated secret missing: " + names[0]
	}
	shown := names
	if len(names) > 3 {
		shown = names[:3]
	}
	out := "generated secrets missing: " + strings.Join(shown, ", ")
	if len(names) > 3 {
		out += fmt.Sprintf(" and %d more", len(names)-3)
	}
	return out
}

// missingID reports whether the file at path declares no deployment id.
func missingID(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc struct {
		ID string `yaml:"id"`
	}
	return yaml.Unmarshal(data, &doc) == nil && doc.ID == ""
}

// runConverge is `paisans apply` without --site: every step that takes
// paisans.yaml to a complete, running stack, run in the order the deployment
// needs them and stopped at the first failure. Run again, it reads live state
// again and carries on. See docs/specs/2026-10-09-apply-converge.md.
func runConverge(r ui.Reporter, o convergeOptions) error {
	configPath, secretsPath, execute, sudo := o.Config, o.Secrets, o.Execute, o.Sudo
	cfg, st, err := convergeRead(configPath, secretsPath)
	if err != nil {
		return err
	}
	if st.NeedsInit && execute {
		// init writes the id, the subnet and the secrets the rest is
		// planned from, so the state is read again once it has run.
		if err := convergeRun(convergeFlags([]string{"init"}, o)); err != nil {
			return convergeStop("apply stopped at init", err, "Run paisans apply --execute again to resume.")
		}
		// init reported through its own reporter, and pointed the retries
		// and holds at it; the reads below draw on r.
		routeRetries(r)
		routeHolds(r)
		if cfg, st, err = convergeRead(configPath, secretsPath); err != nil {
			return err
		}
		st.NeedsInit = false
	}
	if cfg == nil {
		r.Section("configuration")
		r.Step("init").End(ui.Pending, "the deployment id, the mesh subnet and every generated secret")
		r.Result("Nothing changed. %s has no deployment id yet, so the rest is planned once init has run: re-run with --execute, or run paisans init.", configPath)
		return nil
	}
	// A configuration validate refuses reaches no host: a typo in
	// etcd.members would otherwise read as a host that did not answer.
	result := validate.Check(cfg)
	reportFindings(r, configPath, result)
	if result.Refused() {
		return refused(configPath, len(result.Refusals()), "")
	}
	ui.Align(r, convergeTitles(cfg)...)
	defer ui.Align(r)
	if st.Initial, err = convergeFounded(r, cfg, sudo); err != nil {
		return err
	}
	steps := convergePlan(cfg, st)
	if !execute {
		convergeStatus(r, cfg, steps, o)
		convergeLeft(r, cfg, secretsPath, configPath)
		r.Result("Nothing changed. Re-run with --execute to apply. Each step's own dry run, Eg: paisans apply --site <name>, shows its detail.")
		return nil
	}
	phase := ""
	for _, s := range steps {
		if s.Phase != phase {
			phase = s.Phase
			r.Section(phase)
		}
		r.Item(s.Title + ": " + s.Why)
	}
	for _, s := range steps {
		if s.Title == "init" {
			continue
		}
		err := convergeRun(convergeFlags(s.Args, o))
		switch {
		case err == nil:
		case s.Founding && errors.Is(err, apply.ErrFoundingWait):
			r.Note(s.Title+" stopped at the founding stop; pass two applies it again", sentences(err))
		case errors.Is(err, dns.ErrConflict):
			return convergeStop("apply stopped at "+s.Title, err, "Change each conflicting record above at your DNS provider, then run paisans apply --execute again.")
		case errors.Is(err, storageadd.ErrWaiting):
			// Not a failure, and main exits 75 for it as storage add does.
			return convergeStop("apply is waiting at "+s.Title, err, "Nothing failed: Garage is still moving data. Run paisans apply --execute again later.")
		default:
			return convergeStop("apply stopped at "+s.Title, err, "Run paisans apply --execute again to resume.")
		}
	}
	convergeLeft(r, cfg, secretsPath, configPath)
	r.Result("%s is converged: every step ran.", configPath)
	return nil
}

// convergeStop is the error a run stops with at a step: the step named in
// the hint, and the step's own problem, its hint and explanation, ahead of
// next, how to go on. The step's error is the cause, so -v shows it whole and
// errors.Is still finds what it wraps.
// sentences is err's hint as a sentence, then its explanation.
func sentences(err error) string {
	hint, explain := ui.Describe(err)
	lines := []string{strings.TrimSuffix(hint, ".") + "."}
	if explain != "" {
		lines = append(lines, explain)
	}
	return strings.Join(lines, "\n")
}

func convergeStop(hint string, err error, next string) error {
	inner, explain := ui.Describe(err)
	// A wait's own advice is storage add's, which a run of apply replaces
	// with next: only what it waits on is kept.
	var w *storageadd.Waiting
	if errors.As(err, &w) {
		inner, explain = w.Hint()+": "+w.Detail, ""
	}
	lines := []string{strings.TrimSuffix(inner, ".") + "."}
	if explain != "" {
		lines = append(lines, explain)
	}
	return &ui.Problem{Hint: hint, Explain: strings.Join(append(lines, next), "\n"), Cause: err}
}

// convergeStatus is the dry run's plan: each step under its phase, marked
// with its status. A step is checked by its own dry run once every step it
// waits on is up to date, and is otherwise marked as waiting on the first
// that is not; until that one runs, its dry run would describe a host about
// to change. A check that fails is marked with its error, and the rest are
// still checked. Each command is checked once.
func convergeStatus(r ui.Reporter, cfg *config.Config, steps []convergeStep, o convergeOptions) {
	type status struct {
		mark   ui.Mark
		result string
	}
	// done is the status of each command, by title, as its latest step was
	// marked.
	done := map[string]status{}
	phase := ""
	for i, s := range steps {
		if s.Phase != phase {
			phase = s.Phase
			r.Section(phase)
		}
		line := r.Step(s.Title)
		line.Detail("%s", s.Why)
		var notes []checkReport
		var st status
		after := ""
		for _, w := range convergeWaits(cfg, steps, i) {
			if done[w].mark != ui.OK {
				after = w
				break
			}
		}
		prior, checked := done[s.Title]
		switch {
		case s.Title == "init":
			st = status{ui.Pending, s.Why}
		case after != "":
			st = status{ui.Waiting, "after " + after}
			// An apply in pass two waits on the same command in an
			// earlier phase, which its line names.
			if after == s.Title {
				j := slices.IndexFunc(steps[:i], func(e convergeStep) bool { return e.Title == after })
				st.result += " in " + steps[j].Phase
			}
		case checked:
			st = prior
		default:
			summary, reports, err := convergeCheck(r, convergeFlags(s.Args, o))
			for _, c := range reports {
				if c.Note {
					notes = append(notes, c)
				} else {
					line.Detail("%s", c.Text)
				}
			}
			switch {
			case err != nil:
				// The hint is the line's result; what to do follows it at
				// every verbosity, as a failure always says.
				hint, explain := ui.Describe(err)
				st = status{ui.Failed, hint}
				if explain != "" {
					notes = append(notes, checkReport{Note: true, Text: "before " + s.Title + " can run", Detail: explain})
				}
			case summary == "":
				st = status{ui.OK, ""}
			default:
				st = status{ui.Pending, summary}
			}
		}
		done[s.Title] = st
		line.End(st.mark, st.result)
		for _, n := range notes {
			r.Note(n.Text, n.Detail)
		}
		notes = nil
	}
}

// convergeTitles is every title the etcd reads and the plan can show, for
// the title column: the plan of a blank deployment, and site add for each
// member, since which members found and which join is read after the reads
// have been drawn.
func convergeTitles(cfg *config.Config) []string {
	var titles []string
	for _, m := range cfg.Etcd.Members {
		titles = append(titles, "read "+m+"'s etcd record", "site add "+m)
	}
	for _, s := range convergePlan(cfg, convergeState{NeedsInit: true}) {
		titles = append(titles, s.Title)
	}
	return titles
}

// convergeWaits is the titles of the earlier steps that steps[i] waits on,
// in the order they ran: init, for every step once it has work; the site's
// host prepare, for every step on a site; each founding witness's apply, for
// a founding member that is not one, whose gate refuses it until the witness
// runs; the site's earlier apply or site add and the storage step, for an
// apply in pass two; and the host prepare and first apply of each Garage
// site, for storage, since Garage has to be running to be asked.
func convergeWaits(cfg *config.Config, steps []convergeStep, i int) []string {
	var waits []string
	if steps[0].Title == "init" && i > 0 {
		waits = append(waits, "init")
	}
	s := steps[i]
	earlier := steps[:i]
	// first is the title of the first earlier apply or site add of site.
	first := func(site string) []string {
		for _, e := range earlier {
			if e.Phase != "pass two" && stepSite(e) == site && e.Args[0] != "host" {
				return []string{e.Title}
			}
		}
		return nil
	}
	prepare := func(site string) string { return "host prepare --site " + site }
	switch {
	case s.Args[0] == "apply" || s.Args[0] == "site":
		site := stepSite(s)
		waits = append(waits, prepare(site))
		if s.Phase == "founding" && !cfg.Sites[site].Has(config.RoleWitness) {
			for _, e := range earlier {
				if e.Phase == "founding" && cfg.Sites[stepSite(e)].Has(config.RoleWitness) {
					waits = append(waits, e.Title)
				}
			}
		}
		if s.Phase == "pass two" {
			waits = append(waits, first(site)...)
			for _, e := range earlier {
				if e.Phase == "storage" {
					waits = append(waits, e.Title)
				}
			}
		}
	case s.Args[0] == "storage":
		for _, g := range cfg.Storage.Garage.Sites {
			waits = append(waits, prepare(g))
			waits = append(waits, first(g)...)
		}
	}
	ran := func(title string) int {
		return slices.IndexFunc(earlier, func(e convergeStep) bool { return e.Title == title })
	}
	slices.SortStableFunc(waits, func(a, b string) int { return ran(a) - ran(b) })
	return waits
}

// stepSite is the site a step is about, or empty for one about none. Each
// step about a site names it last.
func stepSite(s convergeStep) string {
	switch strings.Join(s.Args[:min(2, len(s.Args))], " ") {
	case "apply --site", "site add", "host prepare", "storage init":
		return s.Args[len(s.Args)-1]
	}
	return ""
}

// dryRunFound is told by the dry run of each command apply without --site
// runs as a step what it would change: a summary such as "3 changes", empty
// when there is nothing to do, and stop when it found something --execute
// would stop at. checkStep sets it; otherwise it does nothing.
var dryRunFound = func(summary string, stop error) {}

// count is n of unit, Eg: "3 changes", or empty for none, as dryRunFound
// takes it.
func count(n int, unit string) string {
	if n == 0 {
		return ""
	}
	return plural(n, unit)
}

// convergeCheck is one step's status in a dry run: what its own dry run
// would change, empty for nothing. Tests replace it.
var convergeCheck func(r ui.Reporter, args []string) (string, []checkReport, error)

// checkReport is one thing a step's own dry run reported while it was
// checked. A note is shown after the step's line at every verbosity, since
// it is something left for the operator; the rest is a detail of the line,
// shown with -v.
type checkReport struct {
	Note   bool
	Text   string
	Detail string
}

// checkReporter is what a step's own dry run reports through while it is
// checked. Its plan is not shown, since the step's line says what it found;
// its warnings, notes, refusals and ssh retries are kept for that line, and a
// hold for a prompt reaches r, which draws the line's spinner.
type checkReporter struct {
	ui.Reporter
	r       ui.Reporter
	reports *[]checkReport
}

func (c checkReporter) Hold() (resume func()) { return ui.Hold(c.r) }

func (c checkReporter) Warn(hint, detail string) {
	*c.reports = append(*c.reports, checkReport{Text: joinDetail("! "+hint, detail)})
}

func (c checkReporter) Note(hint, detail string) {
	*c.reports = append(*c.reports, checkReport{Note: true, Text: hint, Detail: detail})
}

func (c checkReporter) Refuse(hint, explanation string) {
	*c.reports = append(*c.reports, checkReport{Text: joinDetail("✗ "+hint, explanation)})
}

// RetryLog is where the check's ssh retries go: kept, as a detail of the
// step's line.
func (c checkReporter) RetryLog() io.Writer { return retryCollector{c.reports} }

type retryCollector struct{ reports *[]checkReport }

func (w retryCollector) Write(p []byte) (int, error) {
	*w.reports = append(*w.reports, checkReport{Text: strings.TrimRight(string(p), "\n")})
	return len(p), nil
}

func joinDetail(hint, detail string) string {
	if detail == "" {
		return hint
	}
	return hint + ": " + detail
}

// checkStep runs args, a step's command without --execute, through run, in
// this process and quietly, and returns what its dry run told dryRunFound.
// Its reports, retries and holds are routed back to r once it returns.
func checkStep(r ui.Reporter, args []string, run func([]string) error) (string, []checkReport, error) {
	found := false
	var summary string
	var stop error
	var reports []checkReport
	savedFound, savedOverride := dryRunFound, reporterOverride
	dryRunFound = func(s string, err error) { found, summary, stop = true, s, err }
	reporterOverride = checkReporter{ui.Discard, r, &reports}
	defer func() {
		dryRunFound, reporterOverride = savedFound, savedOverride
		routeRetries(r)
		routeHolds(r)
	}()
	if err := run(args); err != nil {
		return "", reports, err
	}
	if stop != nil {
		return "", reports, stop
	}
	if !found {
		name := args[0]
		if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
			name += " " + args[1]
		}
		return "", reports, fmt.Errorf("%s ended without saying what it would change", name)
	}
	return summary, reports, nil
}

// convergeLeft names what only the operator can do: each credential the
// toolkit cannot generate, and the first Pocket ID admin.
func convergeLeft(r ui.Reporter, cfg *config.Config, secretsPath, configPath string) {
	if secretsPath == "" {
		secretsPath = filepath.Join(filepath.Dir(configPath), "secrets.enc.yaml")
	}
	if secrets, err := config.LoadSecrets(secretsPath); err == nil {
		for _, key := range secretsgen.OwedNames(cfg, secrets) {
			r.Note("secrets: "+key+" is owed and only you can provide it", "paisans secrets set "+key+" < value")
		}
	}
	for _, app := range cfg.AppNames() {
		if cfg.Apps[app].Kind == config.KindPocketID {
			r.Note("Pocket ID "+app+": create its first admin, if it has none yet", "paisans app admin create --app "+app+" --username <name> --email <address>")
		}
	}
}

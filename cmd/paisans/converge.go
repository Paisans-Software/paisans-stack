package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/ui"
	"github.com/paisans-software/paisans-stack/internal/validate"
	"gopkg.in/yaml.v3"
)

// convergeState is what decides the plan of `paisans apply` without --site:
// whether init has work, and each etcd member's infra/etcd-initial record,
// for the members that hold one. See docs/specs/2026-10-09-apply-converge.md.
type convergeState struct {
	NeedsInit bool
	Initial   map[string]render.EtcdInitial
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
	// Args is the command and its own flags; the run adds --config,
	// --secrets, --execute and --sudo.
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
		steps = append(steps, step("configuration", "the deployment id, the mesh subnet or a generated secret is missing", false, "init"))
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

// The step runner is set here rather than where it is declared: it reaches
// runApply, which reaches it back.
func init() { convergeRun = runStep }

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

// convergeFounded reads which etcd members hold infra/etcd-initial, each
// through its ssh section. A member that does not answer is an error: whether
// the cluster is founded decides between founding and joining. Tests replace
// it.
var convergeFounded = func(cfg *config.Config, sudo bool) (map[string]render.EtcdInitial, error) {
	records := map[string]render.EtcdInitial{}
	for _, m := range cfg.Etcd.Members {
		in, recorded, err := apply.ReadEtcdInitial(siteTransport(m, cfg.Sites[m], "", sudo), cfg.Deployment())
		if err != nil {
			return nil, fmt.Errorf("apply: reading whether %s's etcd member has been founded: %w. Every etcd member is read before anything runs", m, err)
		}
		if recorded {
			records[m] = in
		}
	}
	return records, nil
}

// convergeFlags is the common flags each command takes, of --config,
// --secrets, --execute and --sudo.
func convergeFlags(args []string, configPath, secretsPath string, execute, sudo bool) []string {
	out := append([]string(nil), args...)
	out = append(out, "--config", configPath)
	takesSecrets := !(args[0] == "host")
	takesExecute := args[0] != "init"
	takesSudo := !(args[0] == "dns")
	if takesSecrets && secretsPath != "" {
		out = append(out, "--secrets", secretsPath)
	}
	if takesExecute && execute {
		out = append(out, "--execute")
	}
	if takesSudo {
		out = append(out, fmt.Sprintf("--sudo=%t", sudo))
	}
	return out
}

// convergeRead reads what decides the plan. Tests replace it.
var convergeRead = readConvergeState

// readConvergeState reads what decides the plan: whether init has work (no
// id, no mesh subnet, no secrets file, or a generated secret missing), and,
// once there is an id, each etcd member's record. With no id there is no
// deployment to read on a host yet, and the configuration is nil.
func readConvergeState(configPath, secretsPath string, sudo bool) (*config.Config, convergeState, error) {
	var st convergeState
	cfg, err := config.Load(configPath)
	if err != nil {
		if !missingID(configPath) {
			return nil, st, err
		}
		st.NeedsInit = true
		return nil, st, nil
	}
	if cfg.Mesh.Subnet == "" {
		st.NeedsInit = true
	}
	if secretsPath == "" {
		secretsPath = filepath.Join(filepath.Dir(configPath), "secrets.enc.yaml")
	}
	if secrets, err := config.LoadSecrets(secretsPath); err != nil {
		st.NeedsInit = true
	} else if filled, err := secretsgen.Fill(cfg, secrets); err != nil || filled.Changed() {
		st.NeedsInit = true
	}
	if st.Initial, err = convergeFounded(cfg, sudo); err != nil {
		return nil, st, err
	}
	return cfg, st, nil
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
func runConverge(r ui.Reporter, configPath, secretsPath string, execute, sudo bool) error {
	cfg, st, err := convergeRead(configPath, secretsPath, sudo)
	if err != nil {
		return err
	}
	if st.NeedsInit && execute {
		// init writes the id, the subnet and the secrets the rest is
		// planned from, so the state is read again once it has run.
		if err := convergeRun(convergeFlags([]string{"init"}, configPath, secretsPath, true, sudo)); err != nil {
			return fmt.Errorf("apply: stopped at init: %w\nRun paisans apply --execute again to resume.", err)
		}
		if cfg, st, err = convergeRead(configPath, secretsPath, sudo); err != nil {
			return err
		}
		st.NeedsInit = false
	}
	if cfg == nil {
		r.Section("configuration")
		r.Item("init: the deployment id, the mesh subnet and every generated secret")
		r.Result("Nothing changed. %s has no deployment id yet, so the rest is planned once init has run: re-run with --execute, or run paisans init.", configPath)
		return nil
	}
	result := validate.Check(cfg)
	reportFindings(r, configPath, result)
	if result.Refused() {
		return fmt.Errorf("%s was refused: %d problem(s) above", configPath, len(result.Refusals()))
	}
	steps := convergePlan(cfg, st)
	phase := ""
	for _, s := range steps {
		if s.Phase != phase {
			phase = s.Phase
			r.Section(phase)
		}
		r.Item(s.Title + ": " + s.Why)
	}
	if !execute {
		convergeLeft(r, cfg, secretsPath, configPath)
		r.Result("Nothing changed. Re-run with --execute to apply. Each step's own dry run, Eg: paisans apply --site <name>, shows its detail.")
		return nil
	}
	for _, s := range steps {
		if s.Title == "init" {
			continue
		}
		err := convergeRun(convergeFlags(s.Args, configPath, secretsPath, true, sudo))
		switch {
		case err == nil:
		case s.Founding && errors.Is(err, apply.ErrFoundingWait):
			r.Note(s.Title+" stopped at the founding stop; pass two applies it again", err.Error())
		default:
			return fmt.Errorf("apply: stopped at %s: %w\nRun paisans apply --execute again to resume.", s.Title, err)
		}
	}
	convergeLeft(r, cfg, secretsPath, configPath)
	r.Result("%s is converged: every step ran.", configPath)
	return nil
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

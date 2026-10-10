package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// deploymentRecord is the newest of the gateways' deployment records, and,
// for each gateway that could not give one, why: deployrecord.ErrNoRecord
// for one that answered without one.
func deploymentRecord(cfg *config.Config, hosts func(string) registry.Runner) (deployrecord.Record, map[string]error) {
	newest, _, missing := deployrecord.Gather(gatewayHosts(cfg, hosts), cfg.Deployment())
	return newest, missing
}

// gatewayHosts is every gateway paisans.yaml declares, reached through hosts.
func gatewayHosts(cfg *config.Config, hosts func(string) registry.Runner) map[string]registry.Runner {
	out := map[string]registry.Runner{}
	for _, gw := range cfg.GatewaySites() {
		out[gw] = hosts(gw)
	}
	return out
}

// recordForWarnings is the record init and apply warn against. A gateway
// with no record yet is silent; one that cannot be read is a warning. With
// no record at all the yaml alone is used.
func recordForWarnings(r ui.Reporter, cfg *config.Config, hosts func(string) registry.Runner) *deployrecord.Record {
	rec, missing := deploymentRecord(cfg, hosts)
	read := len(cfg.GatewaySites()) - len(missing)
	for _, gw := range sortedKeys(missing) {
		if !errors.Is(missing[gw], deployrecord.ErrNoRecord) {
			r.Warn("could not read the deployment record on "+gw, missing[gw].Error())
		}
	}
	if read == 0 {
		return nil
	}
	return &rec
}

// warnSecrets warns once per orphaned secret and once per name the record
// lists that paisans.yaml no longer declares. Never a refusal.
func warnSecrets(r ui.Reporter, cfg *config.Config, secrets *config.Secrets, deployed *deployrecord.Record) {
	for _, o := range secretsgen.Dropped(cfg, deployed) {
		r.Warn(o.Key+" "+o.Why, o.Leaves+".")
	}
	for _, o := range secretsgen.Orphans(cfg, secrets, deployed) {
		r.Warn("secrets: "+o.Key+" "+o.Why, "`paisans secrets prune` removes it.")
	}
}

func sortedKeys(m map[string]error) []string { return slices.Sorted(maps.Keys(m)) }

// confirmWord says what is about to happen and asks for word at the
// terminal, refusing anything else. There is no flag to answer it.
func confirmWord(stdin io.Reader, stdout io.Writer, word, what string) error {
	if !stdinIsTerminal(stdin) {
		return fmt.Errorf("it asks for the word %s at a terminal, and stdin is not one. Run it from an interactive shell. Nothing was changed", word)
	}
	fmt.Fprintf(stdout, "\n%s Type %s to go on: ", what, word)
	answer, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && answer == "" {
		return fmt.Errorf("no answer read. Nothing was changed")
	}
	if strings.TrimSpace(answer) != word {
		return fmt.Errorf("%q is not %s. Nothing was changed", strings.TrimSpace(answer), word)
	}
	return nil
}

// recordApplied adds what paisans.yaml declares to the deployment record on
// a gateway, after its apply: apply only ever adds, so a yaml edited by
// mistake cannot take a running site's protection away.
func recordApplied(r ui.Reporter, cfg *config.Config, site string, t registry.Runner) error {
	if !cfg.Sites[site].Has(config.RoleGateway) {
		return nil
	}
	s := r.Step("record the deployment on " + site)
	changed, err := deployrecord.Add(t, cfg.Deployment(), deployrecord.FromConfig(cfg))
	if errors.Is(err, deployrecord.ErrMalformed) {
		s.Fail(err)
		return fmt.Errorf("%w. The apply itself finished. What the record held cannot be read, so it is not rewritten: delete it on %s with `sudo rm %s`, then apply %s again, which writes it from paisans.yaml", err, site, deployrecord.Path(cfg.Deployment()), site)
	}
	if err != nil {
		s.Fail(err)
		return fmt.Errorf("%w. The apply itself finished; run it again to record it", err)
	}
	s.Done(map[bool]string{true: "updated", false: "up to date"}[changed])
	return nil
}

// forgetInRecords takes names out of the deployment record on every gateway
// paisans.yaml declares. Without execute it lists them, and reports whether
// any record still lists one. The removal they follow is done whether or not this
// reaches every gateway, so a gateway it cannot reach is a warning: its
// record keeps the secrets, which a later run of the same removal frees.
func forgetInRecords(r ui.Reporter, cfg *config.Config, names deployrecord.Record, what string, execute, sudo bool) bool {
	d := cfg.Deployment()
	left, sectioned := false, false
	section := func() {
		if !sectioned {
			r.Section("deployment record")
			sectioned = true
		}
	}
	for _, gw := range cfg.GatewaySites() {
		t := registryHost(gw, cfg.Sites[gw], "", sudo)
		rec, found, err := deployrecord.Read(t, d)
		if err != nil {
			section()
			r.Warn("the deployment record on "+gw+" still lists "+what, err.Error())
			continue
		}
		if !found || !listsAny(rec, names) {
			continue
		}
		section()
		title := "forget " + what + " in the deployment record on " + gw
		if !execute {
			r.Item(title)
			left = true
			continue
		}
		s := r.Step(title)
		if _, err := deployrecord.Forget(t, d, names); err != nil {
			s.Fail(err)
			r.Warn("the deployment record on "+gw+" still lists "+what, err.Error()+". Run the same command again to take it out")
			continue
		}
		s.Done("")
	}
	return left
}

func listsAny(rec, names deployrecord.Record) bool {
	for _, kind := range []string{"sites", "apps", "pocket_id_groups"} {
		for _, n := range names.List(kind) {
			if rec.Lists(kind, n) {
				return true
			}
		}
	}
	return false
}

// appRecordNames is what app remove takes out of a record: the app, and each
// group the record lists that no Pocket ID app paisans.yaml declares names.
func appRecordNames(cfg *config.Config, app string, deployed deployrecord.Record) deployrecord.Record {
	declared := deployrecord.FromConfig(cfg)
	names := deployrecord.Record{Apps: []string{app}}
	for _, g := range deployed.PocketIDGroups {
		if !declared.Lists("pocket_id_groups", g) {
			names.PocketIDGroups = append(names.PocketIDGroups, g)
		}
	}
	return names
}

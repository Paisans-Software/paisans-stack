package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

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
// every gateway, after a gateway's apply: apply only ever adds, so a yaml
// edited by mistake cannot take a running site's protection away. A gateway
// it cannot reach is warned about and caught up by the next change.
func recordApplied(r ui.Reporter, cfg *config.Config, site string, hosts func(string) registry.Runner) error {
	if !cfg.Sites[site].Has(config.RoleGateway) {
		return nil
	}
	s := r.Step("record the deployment")
	res, err := deployrecord.Update(gatewayHosts(cfg, hosts), cfg.Deployment(), deployrecord.Adding(deployrecord.FromConfig(cfg)), time.Now())
	if errors.Is(err, deployrecord.ErrMalformed) {
		s.Fail(err)
		return fmt.Errorf("%w. The apply itself finished. What the record held cannot be read, so it is not rewritten: delete it on that gateway with `sudo rm %s`, then apply %s again, which writes it from paisans.yaml", err, deployrecord.Path(cfg.Deployment()), site)
	}
	if err != nil {
		s.Fail(err)
		return fmt.Errorf("%w. The apply itself finished; run it again to record it", err)
	}
	s.Done(recordResult(res))
	reportMissed(r, res)
	return nil
}

func recordResult(res deployrecord.Result) string {
	switch {
	case res.Changed:
		return "updated"
	case len(res.Wrote) > 0:
		return "brought " + strings.Join(res.Wrote, ", ") + " up to date"
	}
	return "up to date"
}

// reportMissed warns once per gateway a change to the deployment record did
// not reach. It is never a failure: the next change catches it up.
func reportMissed(r ui.Reporter, res deployrecord.Result) {
	for _, gw := range sortedKeys(res.Missed) {
		r.Warn(gw+" missed this change to the deployment record", res.Missed[gw].Error()+". It is brought up to date the next time a command that writes the record reaches it")
	}
}

// forgetInRecords takes names out of the deployment record on every gateway
// paisans.yaml declares. Without execute it lists them, and reports whether
// the newest record still lists one. A gateway it cannot reach is warned
// about, never a failure: the removal it follows is done, and the next change
// to the record catches that gateway up.
func forgetInRecords(r ui.Reporter, cfg *config.Config, names deployrecord.Record, what string, execute, sudo bool) bool {
	hosts := gatewayHosts(cfg, func(gw string) registry.Runner { return registryHost(gw, cfg.Sites[gw], "", sudo) })
	if len(hosts) == 0 {
		return false
	}
	d := cfg.Deployment()
	title := "forget " + what + " in the deployment record"
	if !execute {
		newest, found, missing := deployrecord.Gather(hosts, d)
		listed := found > 0 && listsAny(newest, names)
		unread := []string{}
		for _, gw := range sortedKeys(missing) {
			if !errors.Is(missing[gw], deployrecord.ErrNoRecord) {
				unread = append(unread, gw)
			}
		}
		if listed || len(unread) > 0 {
			r.Section("deployment record")
		}
		for _, gw := range unread {
			r.Warn("could not read the deployment record on "+gw, missing[gw].Error())
		}
		if listed {
			r.Item(title)
		}
		return listed
	}
	res, err := deployrecord.Update(hosts, d, deployrecord.Forgetting(names), time.Now())
	if err == nil && !res.Changed && len(res.Wrote) == 0 && len(res.Missed) == 0 {
		return false
	}
	r.Section("deployment record")
	s := r.Step(title)
	if err != nil {
		s.Fail(err)
		r.Warn("the deployment record still lists "+what, err.Error())
		return false
	}
	s.Done(recordResult(res))
	reportMissed(r, res)
	return false
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

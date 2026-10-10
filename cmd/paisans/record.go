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

// errNoRecord is a gateway that answered with no deployment record.
var errNoRecord = errors.New("it has no deployment record, which its next apply writes")

// deploymentRecord is the union of every gateway's deployment record, and,
// for each gateway that could not give one, why.
func deploymentRecord(cfg *config.Config, hosts func(string) registry.Runner) (deployrecord.Record, map[string]error) {
	missing := map[string]error{}
	var records []deployrecord.Record
	for _, gw := range cfg.GatewaySites() {
		rec, found, err := deployrecord.Read(hosts(gw), cfg.Deployment())
		switch {
		case err != nil:
			missing[gw] = err
		case !found:
			missing[gw] = errNoRecord
		default:
			records = append(records, rec)
		}
	}
	return deployrecord.Union(records...), missing
}

// recordForWarnings is the record init and apply warn against. A gateway
// with no record yet is silent; one that cannot be read is a warning. With
// no record at all the yaml alone is used.
func recordForWarnings(r ui.Reporter, cfg *config.Config, hosts func(string) registry.Runner) *deployrecord.Record {
	rec, missing := deploymentRecord(cfg, hosts)
	read := len(cfg.GatewaySites()) - len(missing)
	for _, gw := range sortedKeys(missing) {
		if !errors.Is(missing[gw], errNoRecord) {
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

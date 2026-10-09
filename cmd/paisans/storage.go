package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/storageadd"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// runStorageAdd joins every Garage site the configuration lists into one
// cluster, stage by stage. It is modelled on runSiteAdd: it reaches every
// site through its own ssh section, prints the plan, and changes hosts only
// with --execute.
//
// It takes no --site: a layout at replication 3 cannot grow one node at a
// time, and another replication factor is a whole-cluster stop, so the unit
// of work is the cluster. docs/specs/2026-10-07-multisite-garage.md.
func runStorageAdd(args []string) error {
	fs := flag.NewFlagSet("storage add", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets (default: secrets.enc.yaml beside the config)")
	execute := fs.Bool("execute", false, "actually run the stages, stopping at the first gate that fails or waits")
	changeReplication := fs.Bool("change-replication", false, "allow Garage's reset for another replication factor: every node stopped, its stored layout set aside, the cluster laid out again. Media is unavailable for about a minute")
	wait := fs.Duration("wait", 0, "how long a stage waiting on Garage polls before the run exits, Eg: 2h. By default it reads once and exits with status 75")
	stopTest := fs.Bool("stop-test", false, "in the smoke stage, stop Garage on the last listed site, prove reads and an upload survive, and start it again. Refused unless uploads survive one node down: replication 3 on three sites, or consistency dangerous")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since /srv and /etc are not the deploy user's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	_ = r
	if fs.NArg() > 0 {
		return fmt.Errorf("storage add: takes no site; it joins every site in storage.garage.sites. %q is extra", fs.Arg(0))
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	report(os.Stderr, *configPath, result)
	if result.Refused() {
		return fmt.Errorf("%s was refused: %d problem(s) above", *configPath, len(result.Refusals()))
	}
	if *secretsPath == "" {
		*secretsPath = filepath.Join(filepath.Dir(*configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(*secretsPath)
	if err != nil {
		return err
	}
	if !secrets.Encrypted {
		fmt.Fprintf(os.Stderr, "paisans: %s is not encrypted. That is accepted for fixtures and examples; a real deployment keeps its secrets under sops.\n", *secretsPath)
	}
	if err := secretsgen.CheckGarageKeys(cfg, secrets); err != nil {
		return err
	}

	transports := map[string]apply.Transport{}
	for _, name := range cfg.SiteNames() {
		transports[name] = siteTransport(name, cfg.Sites[name], "", *sudo)
	}
	// storage add applies files on every Garage site, on the gateway and,
	// last, on every monitor site, so each is host checked before any
	// changes.
	sites := union(cfg.Storage.Garage.Sites, cfg.GatewaySites(), cfg.MonitorSites())
	shared, err := gateSites(os.Stdout, cfg, sites, func(site string) hostcheck.Transport { return transports[site] })
	if err != nil {
		return err
	}
	// Every Garage site, the gateway, which takes the media routes, and the
	// monitor, which is reseeded.
	if err := claimSites(cfg, *execute, *sudo, sites...); err != nil {
		return err
	}
	plan, err := storageadd.Build(cfg, secrets, transports, storageadd.Options{
		ChangeReplication: *changeReplication,
		Wait:              *wait,
		StopTest:          *stopTest,
		SharedSites:       shared,
	})
	if err != nil {
		return err
	}
	plan.Print(os.Stdout)

	if !*execute {
		fmt.Fprintf(os.Stdout, "\nNothing was changed. Re-run with --execute to run these stages; each stops at its gate if it does not pass, and a stage waiting on Garage exits for a later run to resume.\n")
		return nil
	}
	plan.Progress = os.Stdout
	fmt.Fprintln(os.Stdout)
	if err := storageadd.Execute(plan); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\nstorage add: every gate passed\n")
	return nil
}

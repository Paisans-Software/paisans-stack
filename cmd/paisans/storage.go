package main

import (
	"flag"
	"fmt"
	"github.com/paisans-software/paisans-stack/internal/ui"
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
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	r := reporter()
	if fs.NArg() > 0 {
		return fmt.Errorf("storage add: takes no site; it joins every site in storage.garage.sites. %q is extra", fs.Arg(0))
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	reportFindings(r, *configPath, result)
	if result.Refused() {
		return refused(*configPath, len(result.Refusals()), "")
	}
	if *secretsPath == "" {
		*secretsPath = filepath.Join(filepath.Dir(*configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(*secretsPath)
	if err != nil {
		return err
	}
	if !secrets.Encrypted {
		warnUnencrypted(r, *secretsPath)
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
	shared, err := gateSites(r, cfg, sites, func(site string) hostcheck.Transport { return transports[site] })
	if err != nil {
		return err
	}
	// Every Garage site, the gateway, which takes the media routes, and the
	// monitor, which is reseeded.
	if err := claimSites(r, cfg, *execute, *sudo, sites...); err != nil {
		return err
	}
	plan, err := ui.Get(r, "read every Garage site", func() (*storageadd.Plan, error) {
		return storageadd.Build(cfg, secrets, transports, storageadd.Options{
			ChangeReplication: *changeReplication,
			Wait:              *wait,
			StopTest:          *stopTest,
			SharedSites:       shared,
		})
	})
	if err != nil {
		return err
	}
	if !*execute || r.Verbose() {
		plan.Show(r)
	}

	if !*execute {
		dryRunFound(count(plan.PendingStages(), "stage"), nil)
		reportRemains(r, plan.Notes)
		r.Result("Nothing changed. Re-run with --execute to apply.")
		return nil
	}
	plan.Report = r
	if err := storageadd.Execute(plan); err != nil {
		return err
	}
	reportRemains(r, plan.Notes)
	r.Result("storage add: every gate passed.")
	return nil
}

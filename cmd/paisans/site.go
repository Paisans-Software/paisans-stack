package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/siteadd"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// runSiteAdd joins a new data site to the running cluster, stage by stage.
//
// It takes no --ssh: it reaches every site, and an override for one would
// leave the rest to their sections anyway, so every site is reached through
// its own ssh section.
func runSiteAdd(args []string) error {
	fs := flag.NewFlagSet("site add", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets (default: secrets.enc.yaml beside the config)")
	execute := fs.Bool("execute", false, "actually run the stages, stopping at the first gate that fails")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since /srv and /etc are not the deploy user's")
	// The site may come before or after the flags. flag stops at the first
	// argument that is not one, so a leading name is taken off first.
	var site string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		site, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	_ = r
	if site == "" && fs.NArg() > 0 {
		site = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return fmt.Errorf("site add: one site at a time; %q is extra", fs.Arg(0))
	}
	if site == "" {
		return fmt.Errorf("site add: name the site to add, Eg: paisans site add home-b")
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
	if err := requireACMEToken(cfg, secrets, cfg.SiteNames()); err != nil {
		return err
	}

	transports := map[string]apply.Transport{}
	for _, name := range cfg.SiteNames() {
		transports[name] = siteTransport(name, cfg.Sites[name], "", *sudo)
	}
	// The site being added is the one whose host the join changes from
	// nothing; every other site already runs the deployment, and each is
	// checked when it is applied.
	if _, ok := cfg.Sites[site]; !ok {
		return fmt.Errorf("site add: %s declares no site %q. Declared sites are %s", *configPath, site, strings.Join(cfg.SiteNames(), ", "))
	}
	host, err := hostGate(os.Stdout, cfg, site, transports[site])
	if err != nil {
		return err
	}
	// The monitor's stack is applied last, with the new site in its seed,
	// so each monitor site is host checked as apply would check it.
	shared, err := gateSites(os.Stdout, cfg, cfg.MonitorSites(), func(site string) hostcheck.Transport { return transports[site] })
	if err != nil {
		return err
	}
	// Site add writes to every site: the mesh, HAProxy and etcd change on
	// each, not only on the new one.
	if err := claimSites(cfg, *execute, *sudo, cfg.SiteNames()...); err != nil {
		return err
	}
	plan, err := siteadd.Build(cfg, secrets, site, transports)
	if err != nil {
		return err
	}
	plan.KeepImages = host.Shared()
	plan.SharedSites = shared
	plan.Print(os.Stdout)

	if !*execute {
		fmt.Fprintf(os.Stdout, "\nNothing was changed. Re-run with --execute to run these stages; each stops at its gate if it does not pass.\n")
		return nil
	}
	plan.Progress = os.Stdout
	fmt.Fprintln(os.Stdout)
	if err := siteadd.Execute(plan); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\n%s joined: every gate passed\n", site)
	return nil
}

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// removeSiteHost is how `site remove` reaches a site. Tests replace it.
var removeSiteHost = func(name string, site config.Site, sudo bool) apply.Transport {
	return siteTransport(name, site, "", sudo)
}

// runSiteRemove takes one site out of the running deployment, stage by stage:
// its data moved off, its cluster memberships removed, its host cleaned and
// its entry taken out of paisans.yaml. See internal/siteremove and README's
// *`site remove` takes a site out*.
//
// Like every command that changes a host, it plans from what it reads and
// changes nothing without --execute. --delete-data asks for the site's name
// at a terminal, for the reason app remove does.
func runSiteRemove(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("site remove", flag.ContinueOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets (default: secrets.enc.yaml beside the config)")
	execute := fs.Bool("execute", false, "actually run the stages, stopping at the first gate that fails")
	hostGone := fs.Bool("host-gone", false, "the site's host is never coming back: run every stage but cleaning it, and reach it not at all")
	deleteData := fs.Bool("delete-data", false, "also delete this deployment's data on the host (its directory and named volumes); asks for the site's name at a terminal")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since /srv, /etc and Docker are root's")
	var site string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		site, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	if site == "" && fs.NArg() > 0 {
		site = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("site remove takes one site: paisans site remove <site> [--execute] [--host-gone] [--delete-data]. Got extra argument(s): %s", strings.Join(fs.Args(), " "))
	}
	if site == "" {
		return fmt.Errorf("site remove: name the site, Eg: paisans site remove home-b")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	reportFindings(r, *configPath, result)
	if result.Refused() {
		return fmt.Errorf("%s was refused: %d problem(s) above", *configPath, len(result.Refusals()))
	}
	// Refused before any host is read, so an unattended run with
	// --delete-data stops with nothing asked of anything.
	if *execute && *deleteData && !stdinIsTerminal(stdin) {
		return fmt.Errorf("site remove: --delete-data deletes member data, which nothing brings back, so it asks for the site's name at a terminal and stdin is not one. Run it from an interactive shell. Nothing was changed")
	}
	opts := siteremove.Options{HostGone: *hostGone, DeleteData: *deleteData, ConfigPath: *configPath}
	if err := siteremove.Refusal(cfg, site, opts); err != nil {
		return err
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
	var claim []string
	for _, name := range cfg.SiteNames() {
		if name == site && *hostGone {
			continue
		}
		transports[name] = removeSiteHost(name, cfg.Sites[name], *sudo)
		claim = append(claim, name)
	}
	// The monitor's stack is applied last, with the site out of its seed,
	// so each monitor site is host checked as apply would check it.
	if _, err := gateSites(r, cfg, cfg.MonitorSites(), func(site string) hostcheck.Transport { return transports[site] }); err != nil {
		return err
	}
	plan, err := siteremove.Build(cfg, secrets, site, transports, opts)
	if err != nil {
		return err
	}
	plan.Print(stdout)
	// Every site it reaches is written to: the cluster and the mesh change
	// on each remaining one, and the leaving host is cleaned.
	if err := claimSites(r, cfg, *execute, *sudo, claim...); err != nil {
		return err
	}
	if !*execute {
		printRemains(stdout, plan.Remains())
		fmt.Fprintf(stdout, "\nNothing was changed. Re-run with --execute to run these stages; each stops at its gate if it does not pass.\n")
		return nil
	}
	if *deleteData {
		if err := confirmSite(stdin, stdout, site); err != nil {
			return err
		}
	}
	plan.Progress = stdout
	fmt.Fprintln(stdout)
	if err := siteremove.Execute(plan); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\n%s is removed: every gate passed, and %s no longer declares it.\n", site, *configPath)
	printRemains(stdout, plan.Remains())
	return nil
}

// confirmSite asks for the site's name at the terminal and refuses anything
// else. There is no flag to answer it.
func confirmSite(stdin io.Reader, stdout io.Writer, site string) error {
	if !stdinIsTerminal(stdin) {
		return fmt.Errorf("site remove: --delete-data asks for the site's name at a terminal, and stdin is not one. Nothing was changed")
	}
	fmt.Fprintf(stdout, "\nThis deletes this deployment's data on %s above, for good. Type %s to go on: ", site, site)
	answer, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && answer == "" {
		return fmt.Errorf("site remove: no answer read. Nothing was changed")
	}
	if strings.TrimSpace(answer) != site {
		return fmt.Errorf("site remove: %q is not %s. Nothing was changed", strings.TrimSpace(answer), site)
	}
	return nil
}

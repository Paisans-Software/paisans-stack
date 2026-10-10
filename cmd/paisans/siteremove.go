package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
	"github.com/paisans-software/paisans-stack/internal/ui"
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
	force := fs.Bool("force", false, "clean this deployment off one host and nothing else: no cluster stage or refusal, and neither paisans.yaml nor the secrets file edited")
	sshFlag := fs.String("ssh", "", "with --force: the host to clean, user@host[:port], any host; only what carries this deployment's id or token is removed")
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
		return fmt.Errorf("site remove takes one site: paisans site remove <site> [--execute] [--host-gone] [--delete-data] [--force [--ssh user@host[:port]]]. Got extra argument(s): %s", strings.Join(fs.Args(), " "))
	}
	if site == "" {
		return fmt.Errorf("site remove: name the site, Eg: paisans site remove home-b")
	}
	if *sshFlag != "" && !*force {
		return fmt.Errorf("site remove: --ssh names the host to clean, which only --force takes. A full removal reaches the site through its ssh section")
	}
	if *force {
		return runSiteRemoveForced(r, site, forcedArgs{config: *configPath, secrets: *secretsPath, ssh: *sshFlag, execute: *execute, hostGone: *hostGone, deleteData: *deleteData, sudo: *sudo}, stdin, stdout)
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
		warnUnencrypted(r, *secretsPath)
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
	if !*execute || r.Verbose() {
		plan.Show(r)
	}
	// Every site it reaches is written to: the cluster and the mesh change
	// on each remaining one, and the leaving host is cleaned.
	if err := claimSites(r, cfg, *execute, *sudo, claim...); err != nil {
		return err
	}
	if !*execute {
		reportRemains(r, plan.Remains())
		r.Result("Nothing changed. Re-run with --execute to apply.")
		return nil
	}
	if *deleteData {
		if err := confirmSite(stdin, stdout, site); err != nil {
			return err
		}
	}
	plan.Report = r
	if err := siteremove.Execute(plan); err != nil {
		return err
	}
	reportRemains(r, plan.Remains())
	r.Result("%s is removed: every gate passed, and %s no longer declares it.", site, *configPath)
	return nil
}

type forcedArgs struct {
	config, secrets, ssh                string
	execute, hostGone, deleteData, sudo bool
}

// runSiteRemoveForced is `site remove --force`: one host cleaned of this
// deployment, nothing else read or changed. See siteremove.BuildForced.
//
// The host is never claimed in its registry, as other commands that write to
// a host do: cleaning it removes this deployment's entry there.
func runSiteRemoveForced(r ui.Reporter, site string, a forcedArgs, stdin io.Reader, stdout io.Writer) error {
	if a.hostGone {
		return fmt.Errorf("site remove %s: --force cleans one host, and --host-gone reaches none. Drop one of them", site)
	}
	cfg, err := config.Load(a.config)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	reportFindings(r, a.config, result)
	if result.Refused() {
		return fmt.Errorf("%s was refused: %d problem(s) above", a.config, len(result.Refusals()))
	}
	dest, err := chooseHost(cfg, site, a.ssh)
	if err != nil {
		return err
	}
	declared, isDeclared := cfg.Sites[site]
	own := isDeclared && declared.Destination() == dest
	// Refused before any host is read, so an unattended run stops with
	// nothing asked of anything.
	if a.execute && (own || a.deleteData) && !stdinIsTerminal(stdin) {
		why := "--delete-data deletes member data, which nothing brings back"
		if own {
			why = "it cleans the host " + site + " runs on, out from under its cluster"
		}
		return fmt.Errorf("site remove %s --force: %s, so it asks for the site's name at a terminal and stdin is not one. Run it from an interactive shell. Nothing was changed", site, why)
	}
	if a.secrets == "" {
		a.secrets = filepath.Join(filepath.Dir(a.config), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(a.secrets)
	if err != nil {
		return err
	}
	if !secrets.Encrypted {
		warnUnencrypted(r, a.secrets)
	}
	// The declared entry's public keys, if any, still say which key the
	// operator logs in with; only where is dest.
	reach := declared
	reach.SSH.User, reach.SSH.Host, reach.SSH.Port = dest.User, dest.Host, dest.Port
	t := removeSiteHost(site, reach, a.sudo)
	opts := siteremove.Options{DeleteData: a.deleteData, ConfigPath: a.config}
	plan, err := siteremove.BuildForced(cfg, secrets, site, dest, t, opts)
	if err != nil {
		return err
	}
	if forcedNothingToDo(r, plan, dest) {
		return nil
	}
	if !a.execute || r.Verbose() {
		plan.Show(r)
	}
	if !a.execute {
		reportRemains(r, plan.Remains())
		r.Result("Nothing changed. Re-run with --execute to apply.")
		return nil
	}
	var what []string
	if plan.Current {
		what = append(what, fmt.Sprintf("This cleans %s, the host %s runs on, out from under its cluster.", dest, site))
	}
	if a.deleteData {
		what = append(what, fmt.Sprintf("This deletes this deployment's data on %s above, for good.", dest))
	}
	if len(what) > 0 {
		if err := confirmSiteFor(stdin, stdout, site, strings.Join(what, " ")); err != nil {
			return err
		}
	}
	plan.Report = r
	if err := siteremove.Execute(plan); err != nil {
		return err
	}
	reportRemains(r, plan.Remains())
	r.Result("%s is cleaned of this deployment. %s and the secrets file are unchanged.", dest, a.config)
	return nil
}

// forcedNothingToDo ends a forced run on a host that holds nothing of this
// deployment, dry run or not: there is nothing to run again, and what the
// host's owner keeps there is not this command's to list.
func forcedNothingToDo(r ui.Reporter, plan *siteremove.Plan, dest config.Destination) bool {
	if plan.Pending() {
		return false
	}
	r.Result("%s holds nothing of this deployment. Nothing to do.", dest)
	return true
}

// chooseHost is the host --force cleans: --ssh when given, any host, else the
// declared site's own. Any host is safe to name, since only what carries this
// deployment's id or token is removed.
func chooseHost(cfg *config.Config, site, ssh string) (config.Destination, error) {
	if ssh != "" {
		d, err := config.ParseDestination(ssh)
		if err != nil {
			return config.Destination{}, fmt.Errorf("site remove %s: --ssh: %w", site, err)
		}
		return d, nil
	}
	if s, ok := cfg.Sites[site]; ok {
		return s.Destination(), nil
	}
	return config.Destination{}, fmt.Errorf("site remove %s: it is not declared, so name its host with --ssh user@host[:port]", site)
}

// confirmSite asks for the site's name before --delete-data deletes its data.
func confirmSite(stdin io.Reader, stdout io.Writer, site string) error {
	return confirmSiteFor(stdin, stdout, site, fmt.Sprintf("This deletes this deployment's data on %s above, for good.", site))
}

// confirmSiteFor says what is about to happen, asks for the site's name at
// the terminal, and refuses anything else. There is no flag to answer it.
func confirmSiteFor(stdin io.Reader, stdout io.Writer, site, what string) error {
	if err := confirmWord(stdin, stdout, site, what); err != nil {
		return fmt.Errorf("site remove: %w", err)
	}
	return nil
}

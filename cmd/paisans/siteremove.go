package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
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
	idFlag := fs.String("id", "", "with --force and --ssh, and no paisans.yaml: the deployment to clean off the host, by its full id or its token, as `paisans host deployments` lists it")
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
		return &ui.Problem{Hint: "site remove takes one site", Explain: "Got extra argument(s): " + strings.Join(fs.Args(), " ") + ". Usage: paisans site remove <site> [--execute] [--host-gone] [--delete-data] [--force [--ssh user@host[:port]]]"}
	}
	if *idFlag != "" {
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		return runSiteRemoveByID(r, site, byIDArgs{id: *idFlag, ssh: *sshFlag, force: *force, config: set["config"], secrets: set["secrets"], execute: *execute, hostGone: *hostGone, deleteData: *deleteData, sudo: *sudo}, stdin, stdout)
	}
	if site == "" {
		return &ui.Problem{Hint: "site remove needs a site", Explain: "Name the site to remove, Eg: paisans site remove home-b"}
	}
	if *sshFlag != "" && !*force {
		return &ui.Problem{Hint: "--ssh is only for --force", Explain: "--ssh names the host to clean, which only --force takes. A full removal reaches the site through its ssh section."}
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
		return refused(*configPath, len(result.Refusals()), "")
	}
	// Refused before any host is read, so an unattended run with
	// --delete-data stops with nothing asked of anything.
	if *execute && *deleteData && !stdinIsTerminal(stdin) {
		return noTerminal("--delete-data deletes member data, which nothing brings back")
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
		return forceAndHostGone()
	}
	cfg, err := config.Load(a.config)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	reportFindings(r, a.config, result)
	if result.Refused() {
		return refused(a.config, len(result.Refusals()), "")
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
		return noTerminal(strings.ToUpper(why[:1]) + why[1:])
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
	// A site no longer declared leaves every gateway's deployment record,
	// whatever its host held, so secrets prune may remove its secrets.
	forget := func(execute bool) bool {
		return !isDeclared && forgetInRecords(r, cfg, deployrecord.Record{Sites: []string{site}}, site, execute, a.sudo)
	}
	recordLeft := false
	if !plan.Pending() {
		recordLeft = forget(a.execute)
	}
	if forcedNothingToDo(r, plan, dest, recordLeft) {
		return nil
	}
	if !a.execute || r.Verbose() {
		plan.Show(r)
	}
	if !a.execute {
		forget(false)
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
	forget(true)
	reportRemains(r, plan.Remains())
	r.Result("%s is cleaned of this deployment. %s and the secrets file are unchanged.", dest, a.config)
	return nil
}

type byIDArgs struct {
	id, ssh                                                     string
	force, config, secrets, execute, hostGone, deleteData, sudo bool
}

// tokenShape is a token as --id takes it: deployment.TokenLength lowercase
// hex digits.
var tokenShape = regexp.MustCompile(fmt.Sprintf("^[0-9a-f]{%d}$", deployment.TokenLength))

// runSiteRemoveByID is `site remove --force --ssh <dest> --id <id or token>`:
// one host cleaned of the deployment its registry names, with no
// paisans.yaml and no secrets file. See siteremove.BuildForcedByID and
// docs/specs/2026-10-10-remove-without-config.md.
//
// Nothing says whether the deployment still runs elsewhere, or whether
// someone still holds its paisans.yaml, so --execute always asks for the
// site's name at a terminal.
func runSiteRemoveByID(r ui.Reporter, site string, a byIDArgs, stdin io.Reader, stdout io.Writer) error {
	switch {
	case !a.force:
		return &ui.Problem{Hint: "--id needs --force", Explain: "--id names a deployment for --force to clean off one host, so add --force."}
	case site != "":
		return &ui.Problem{Hint: "--id takes no site", Explain: "The host's registry entry names the site, so drop " + site + "."}
	case a.config:
		return &ui.Problem{Hint: "--id takes no --config", Explain: "--id reads no paisans.yaml, and a configuration names its own id. Drop --config, or --id."}
	case a.secrets:
		return &ui.Problem{Hint: "--id takes no --secrets", Explain: "--id reads no secrets file. Drop --secrets."}
	case a.hostGone:
		return forceAndHostGone()
	case a.ssh == "":
		return &ui.Problem{Hint: "--id needs --ssh", Explain: "With no configuration there is no ssh section to reach the host through, so name it with --ssh user@host[:port]."}
	case !deployment.ValidID(a.id) && !tokenShape.MatchString(a.id):
		return &ui.Problem{Hint: fmt.Sprintf("--id %q is not a deployment id or token", a.id), Explain: fmt.Sprintf("Give the full id or its %d hex digit token, as paisans host deployments lists them, Eg: --id f2a9", deployment.TokenLength)}
	}
	dest, err := config.ParseDestination(a.ssh)
	if err != nil {
		return sshFlagProblem(a.ssh, err)
	}
	// Refused before any host is read, so an unattended run stops with
	// nothing asked of anything.
	if a.execute && !stdinIsTerminal(stdin) {
		return noTerminal("Nothing says whether this deployment still runs")
	}
	t := reachDestination(dest, a.sudo)
	plan, err := siteremove.BuildForcedByID(dest, t, a.id, siteremove.Options{DeleteData: a.deleteData})
	if err != nil {
		return err
	}
	if forcedNothingToDo(r, plan, dest, false) {
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
	if err := confirmSiteFor(stdin, stdout, plan.Site, plan.Confirmation(dest)); err != nil {
		return err
	}
	plan.Report = r
	if err := siteremove.Execute(plan); err != nil {
		return err
	}
	reportRemains(r, plan.Remains())
	r.Result("%s is cleaned of deployment %s. No paisans.yaml or secrets file was read.", dest, plan.DeploymentID())
	return nil
}

// forcedNothingToDo ends a forced run on a host that holds nothing of this
// deployment, dry run or not: there is nothing to run again, and what the
// host's owner keeps there is not this command's to list.
func forcedNothingToDo(r ui.Reporter, plan *siteremove.Plan, dest config.Destination, recordLeft bool) bool {
	if plan.Pending() {
		return false
	}
	if len(plan.CaddyKept) > 0 {
		r.Result("%s holds nothing of this deployment but its Caddy, kept because it serves %s. Run this again once they have moved, and it goes.", dest, strings.Join(plan.CaddyKept, ", "))
		return true
	}
	if recordLeft {
		r.Result("%s holds nothing of this deployment. Re-run with --execute to take it out of the deployment record.", dest)
		return true
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
			return config.Destination{}, sshFlagProblem(ssh, err)
		}
		return d, nil
	}
	if s, ok := cfg.Sites[site]; ok {
		return s.Destination(), nil
	}
	return config.Destination{}, &ui.Problem{Hint: site + " is not declared", Explain: "Name its host with --ssh user@host[:port]."}
}

// noTerminal refuses a removal that must ask for the site's name, for why,
// when stdin is not a terminal to ask at.
func noTerminal(why string) error {
	return &ui.Problem{
		Hint:    "the site's name must be typed, and stdin is not a terminal",
		Explain: why + ", so it asks for the site's name at a terminal. Run it from an interactive shell. Nothing was changed.",
	}
}

func forceAndHostGone() error {
	return &ui.Problem{Hint: "--force and --host-gone cannot be used together", Explain: "--force cleans one host, and --host-gone reaches none. Drop one of them."}
}

// sshFlagProblem is the Problem for an --ssh value that is not a
// destination.
func sshFlagProblem(value string, err error) error {
	return &ui.Problem{Hint: fmt.Sprintf("--ssh %q is not a destination", value), Explain: err.Error() + ".", Cause: err}
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

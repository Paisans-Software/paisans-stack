package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/appremove"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// removeHost is how `app remove` reaches a site. Tests replace it.
var removeHost = func(name string, site config.Site, sudo bool) appremove.Host {
	return siteTransport(name, site, "", sudo)
}

// removeClients is Pocket ID's API on site, for `app remove`. It goes
// through clientAPI, the same path `oidc client create` takes.
var removeClients = func(cfg *config.Config, site, key string) appremove.Clients {
	return clientAPI(cfg, site, "", key)
}

// runAppRemove takes an app that has left paisans.yaml off every host. See
// internal/appremove for what it touches and how each thing is proven to be
// the app's, and README's *`app remove` takes an app off its hosts*.
//
// Like every command that changes a host, it plans from what it reads and
// changes nothing without --execute. Deleting member data takes
// --delete-data and the app's name typed at a terminal, because nothing
// brings it back: a flag can sit in a script or a shell history and be run
// again by somebody who did not mean it, and a typed name cannot.
func runAppRemove(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("app remove", flag.ContinueOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets file (default: secrets.enc.yaml beside the config)")
	execute := fs.Bool("execute", false, "actually stop, remove and delete")
	deleteData := fs.Bool("delete-data", false, "also delete the app's database, Garage bucket and keys, named volumes and data directories; asks for the app's name at a terminal")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since /srv and Docker are root's")
	// The app comes first, as `app remove talk --execute`, or after the flags.
	app := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		app, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	if app == "" && fs.NArg() > 0 {
		app = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("app remove takes one app: paisans app remove <app> [--execute] [--delete-data]. Got extra argument(s): %s", strings.Join(fs.Args(), " "))
	}
	if app == "" {
		return fmt.Errorf("app remove: name the app: paisans app remove <app> [--execute] [--delete-data]")
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
	if err := appremove.Refusal(cfg, app); err != nil {
		return err
	}
	// Refused before any host is read, so an unattended run with
	// --delete-data stops with nothing asked of anything.
	if *execute && *deleteData && !stdinIsTerminal(stdin) {
		return fmt.Errorf("app remove: --delete-data deletes member data, which nothing brings back, so it asks for the app's name at a terminal and stdin is not one. Run it from an interactive shell. Nothing was changed")
	}
	if *secretsPath == "" {
		*secretsPath = filepath.Join(filepath.Dir(*configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(*secretsPath)
	if err != nil {
		return err
	}

	hosts := map[string]appremove.Host{}
	in := appremove.Input{DeleteData: *deleteData}
	for _, site := range cfg.SiteNames() {
		hosts[site] = removeHost(site, cfg.Sites[site], *sudo)
		st, err := appremove.ProbeSite(cfg, app, site, hosts[site])
		if err != nil {
			return fmt.Errorf("app remove: %w. Every site is read before anything changes, so nothing was changed", err)
		}
		in.Sites = append(in.Sites, st)
	}

	var clients appremove.Clients
	if idp := pocketIDApp(cfg); idp != "" {
		where, err := pocketIDSite(cfg, idp, "", "app remove")
		if err != nil {
			return err
		}
		key, _ := secrets.Apps[idp]["static_api_key"].(string)
		if key == "" {
			return fmt.Errorf("app remove: secrets apps.%s.static_api_key is empty, so Pocket ID cannot be asked for %s's client. Nothing was changed", idp, app)
		}
		clients = removeClients(cfg, where, key)
		if in.Client, err = appremove.ProbeClient(clients, app, idp, where, secrets.OIDCClients[app].ClientID); err != nil {
			return fmt.Errorf("app remove: %w. Nothing was changed", err)
		}
	}
	if *deleteData {
		if in.Database, err = appremove.ProbeDatabase(cfg, app, hosts); err != nil {
			return fmt.Errorf("app remove: %w. Nothing was changed", err)
		}
		if len(cfg.Storage.Garage.Sites) > 0 {
			if in.Storage, err = appremove.ProbeStorage(cfg, secrets, app, hosts[cfg.Storage.Garage.Sites[0]]); err != nil {
				return fmt.Errorf("app remove: %w. Nothing was changed", err)
			}
		}
	}

	plan, err := appremove.Build(cfg, app, in)
	if err != nil {
		return err
	}
	// The monitor is reseeded last, from the render without the app, so
	// its checks of the app go with it. Each monitor site is host checked
	// as apply would check it.
	if len(cfg.MonitorSites()) > 0 {
		transports := map[string]apply.Transport{}
		for _, site := range cfg.MonitorSites() {
			transports[site] = hosts[site]
		}
		if _, err := gateSites(r, cfg, cfg.MonitorSites(), func(site string) hostcheck.Transport { return transports[site] }); err != nil {
			return err
		}
		rendered, err := render.Build(cfg, secrets)
		if err != nil {
			return err
		}
		if err := plan.PlanMonitors(cfg, rendered, acme.Module(cfg.ACME.Provider), transports); err != nil {
			return err
		}
	}
	if plan.Empty() {
		where := "on any site or at Pocket ID"
		if *deleteData {
			where = "on any site, at Pocket ID, in Postgres or in Garage"
		}
		fmt.Fprintf(stdout, "Nothing of %s was found %s. Nothing to do.\n", app, where)
		printRemains(stdout, appremove.Remains(plan, secrets, nil))
		return nil
	}
	plan.Print(stdout)

	touched := map[string]registry.Runner{}
	for _, s := range plan.Sites {
		if !s.Empty() {
			touched[s.Site] = registryHost(s.Site, cfg.Sites[s.Site], "", *sudo)
		}
	}
	if plan.Client.Delete != nil {
		touched[plan.Client.Site] = registryHost(plan.Client.Site, cfg.Sites[plan.Client.Site], "", *sudo)
	}
	if db := plan.Database; db != nil && (db.DropDatabase || db.DropRole) {
		touched[db.Leader] = registryHost(db.Leader, cfg.Sites[db.Leader], "", *sudo)
	}
	if st := plan.Storage; st != nil && (len(st.Buckets) > 0 || len(st.Keys) > 0) {
		touched[st.Anchor] = registryHost(st.Anchor, cfg.Sites[st.Anchor], "", *sudo)
	}
	for _, m := range plan.Monitors {
		if m.Pending() {
			touched[m.Site] = registryHost(m.Site, cfg.Sites[m.Site], "", *sudo)
		}
	}
	if err := claimHosts(r, cfg, *execute, touched); err != nil {
		return err
	}

	if !*execute {
		printRemains(stdout, appremove.Remains(plan, secrets, nil))
		fmt.Fprintf(stdout, "\nNothing was changed. Re-run with --execute to apply this.\n")
		return nil
	}
	if *deleteData {
		if err := confirmName(stdin, stdout, app); err != nil {
			return err
		}
	}
	fmt.Fprintln(stdout)
	runner := &appremove.Executor{Hosts: hosts, Clients: clients, Progress: stdout}
	if err := runner.Execute(plan); err != nil {
		var me *appremove.MonitorError
		if errors.As(err, &me) {
			return fmt.Errorf("app remove: %w", err)
		}
		return fmt.Errorf("app remove: %w\nEvery step is safe to repeat: run the same command again to resume", err)
	}
	fmt.Fprintf(stdout, "\n%s is removed.\n", app)
	printRemains(stdout, appremove.Remains(plan, secrets, runner.Kept))
	return nil
}

// confirmName asks for the app's name at the terminal and refuses anything
// else. There is no flag to answer it.
func confirmName(stdin io.Reader, stdout io.Writer, app string) error {
	if !stdinIsTerminal(stdin) {
		return fmt.Errorf("app remove: --delete-data asks for the app's name at a terminal, and stdin is not one. Nothing was changed")
	}
	fmt.Fprintf(stdout, "\nThis deletes %s's member data above, for good. Type %s to go on: ", app, app)
	answer, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && answer == "" {
		return fmt.Errorf("app remove: no answer read. Nothing was changed")
	}
	if strings.TrimSpace(answer) != app {
		return fmt.Errorf("app remove: %q is not %s. Nothing was changed", strings.TrimSpace(answer), app)
	}
	return nil
}

func printRemains(w io.Writer, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "\nLeft for you:\n")
	for _, l := range lines {
		fmt.Fprintf(w, "  %s\n", l)
	}
}

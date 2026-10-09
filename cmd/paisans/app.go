package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/appadmin"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// maxStdin bounds what `app admin create` reads to tell whether anything was
// piped in. It takes nothing on stdin, so any amount is a wrong pipe.
const maxStdin = 4 << 10

// adminTransport is how `app admin create` reaches a host. Tests replace it.
var adminTransport = func(t apply.SSHTransport) appadmin.Transport { return t }

// runApp dispatches `paisans app <subcommand>`.
func runApp(args []string) error {
	if len(args) >= 1 && args[0] == "remove" {
		return runAppRemove(args[1:], os.Stdin, os.Stdout)
	}
	if len(args) < 2 || args[0] != "admin" || args[1] != "create" {
		return fmt.Errorf("app takes two subcommands: paisans app admin create --app <pocket-id app> --username <u> --email <e>, and paisans app remove <app>")
	}
	return runAppAdminCreate(args[2:], os.Stdin)
}

// runAppAdminCreate makes sure one user exists and is an administrator of one
// app.
//
// It exists because an app with registrations closed has no other way to get
// its first admin, and making one for Pocket ID by hand means a browser on a
// setup page that is open to whoever reaches it first. It is modelled on
// `storage init`: probe read only, print the plan, change nothing without
// --execute.
//
// Pocket ID takes no password. Anything piped in is refused rather than
// dropped silently, which would leave the operator believing it was set, and
// it is refused before the host is touched, so a dry run refuses it as an
// execute would. The account is reached through a one-time login link
// instead, printed once on --execute.
//
// The user is also put in every group an app signing in through Pocket ID
// reads as its admin group, creating a group that does not exist yet, so one
// command makes an administrator of Pocket ID and of every such app. An Mbin
// app is refused by name: its administrators come only through single sign
// on, so the refusal points at this command run against Pocket ID.
func runAppAdminCreate(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("app admin create", flag.ContinueOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets file, read for pocket-id's API key (default: secrets.enc.yaml beside the config)")
	appName := fs.String("app", "", "the app, by the name it has in the configuration")
	username := fs.String("username", "", "the administrator's username")
	email := fs.String("email", "", "the administrator's email, required to create the account and used only then")
	firstName := fs.String("first-name", "", "the first name, used only if the account is created (default: the username)")
	lastName := fs.String("last-name", "", "the last name, used only if the account is created")
	loginLink := fs.Bool("login-link", false, "issue a fresh one-time login link for an account that already exists")
	site := fs.String("site", "", "the site whose copy of the app to call (default: the pinned site, or the site whose instance is active)")
	destination := fs.String("ssh", "", "ssh destination, used verbatim in place of the site's ssh section (its user, host, port and keys are then ignored)")
	execute := fs.Bool("execute", false, "actually create, verify and grant")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	_ = r
	if fs.NArg() > 0 {
		return fmt.Errorf("app admin create takes flags only. Got extra argument(s): %s", strings.Join(fs.Args(), " "))
	}
	if *appName == "" || *username == "" {
		return fmt.Errorf("app admin create: --app and --username are required")
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
	app, ok := cfg.Apps[*appName]
	if !ok {
		return fmt.Errorf("app admin create: %s declares no app %q. Declared apps are %s", *configPath, *appName, strings.Join(cfg.AppNames(), ", "))
	}
	// Refuse a kind with no Creator before resolving a site or reaching a host.
	if app.Kind == config.KindMbin {
		return mbinAdminRefusal(cfg, *appName, *username)
	}
	if _, err := appadmin.For(app.Kind); err != nil {
		return fmt.Errorf("app admin create: %w", err)
	}
	if err := refuseStdin(stdin, app.Kind); err != nil {
		return err
	}

	where, err := pocketIDSite(cfg, *appName, *site, "app admin create")
	if err != nil {
		return err
	}
	// The calls need no root and a dry run reaches the host without sudo,
	// so only a run that will change something claims it, through sudo,
	// since the registry is root's.
	if *execute {
		if err := claimHosts(cfg, true, map[string]registry.Runner{where: registryHost(where, cfg.Sites[where], *destination, true)}); err != nil {
			return err
		}
	}
	key, err := pocketIDKey(cfg, *configPath, *secretsPath, *appName)
	if err != nil {
		return fmt.Errorf("app admin create: %w", err)
	}
	req := appadmin.Request{App: *appName, Username: *username, Email: *email,
		FirstName: *firstName, LastName: *lastName, LoginLink: *loginLink, AdminGroups: adminGroups(cfg),
		APIKey: key, APIBase: pocketIDBase(cfg, where), PublicURL: "https://" + app.Hostname}

	// No sudo: the calls are curl to Pocket ID's API, which needs no root.
	transport := adminTransport(siteTransport(where, cfg.Sites[where], *destination, false))
	plan, err := appadmin.Build(app.Kind, transport, req)
	if err != nil {
		return fmt.Errorf("app admin create: %w", err)
	}
	fmt.Fprintf(os.Stdout, "%s on %s (%s)\n", *appName, where, app.Kind)
	for _, line := range plan.Lines() {
		fmt.Fprintf(os.Stdout, "  %s\n", line)
	}

	if len(plan.Actions) == 0 {
		return nil
	}
	if !*execute {
		fmt.Fprintf(os.Stdout, "\nNothing was changed. Re-run with --execute to apply this.\n")
		return nil
	}
	outcome, err := appadmin.Execute(plan, transport)
	if err != nil {
		return fmt.Errorf("app admin create: %w", err)
	}
	fmt.Fprintf(os.Stdout, "\n%s is an administrator of %s\n", *username, *appName)
	if outcome.LoginLink != "" {
		// The one place the link is ever written. It is a credential, so it
		// goes to this terminal once and is kept nowhere: not in the secrets
		// file, not in a log, not in an error.
		fmt.Fprintf(os.Stdout, "\nOne-time login link for %s. It signs in as %s once, within %s, so treat it as a password: open it yourself and register a passkey. It is printed here and nowhere else. If it expires, re-run with --login-link --execute.\n\n  %s\n",
			*username, *username, outcome.ExpiresIn, outcome.LoginLink)
	}
	return nil
}

// mbinAdminRefusal is the answer for an Mbin app. Its administrators come
// only through single sign on: the user is made at Pocket ID, and the same
// command puts them in every group an app reads as its administrators. The
// Pocket ID app is named when the configuration has exactly one.
func mbinAdminRefusal(cfg *config.Config, appName, username string) error {
	var pids []string
	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Kind == config.KindPocketID {
			pids = append(pids, name)
		}
	}
	pid := "<pocket-id app>"
	if len(pids) == 1 {
		pid = pids[0]
	}
	return fmt.Errorf("app admin create: %s is mbin, whose administrators come only through single sign on. Run `paisans app admin create --app %s --username %s --email <e>`: it makes the user at Pocket ID and puts them in every app's admin group, %s's included. Nothing was changed",
		appName, pid, username, appName)
}

// adminGroups is every admin group an app that signs in through Pocket ID
// reads, sorted and without duplicates: the groups an administrator made by
// `app admin create` is put in.
func adminGroups(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		spec, ok := kinds.OIDCClient(app.Kind, app.Hostname)
		if !ok {
			continue
		}
		if group, _ := spec.Groups(app); group != "" && !seen[group] {
			seen[group] = true
			out = append(out, group)
		}
	}
	sort.Strings(out)
	return out
}

// refuseStdin refuses anything piped in, since there is nothing to read: a
// kind here takes no password. A terminal on stdin is left unread, so an
// interactive run never waits.
func refuseStdin(stdin io.Reader, kind config.Kind) error {
	if stdinIsTerminal(stdin) {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxStdin+1))
	if err != nil {
		return fmt.Errorf("app admin create: reading stdin: %w", err)
	}
	if strings.TrimSpace(string(data)) != "" {
		return fmt.Errorf("app admin create: %s users sign in with passkeys, so there is no password to set and nothing to read on stdin, and something was piped in. Nothing was changed; run it without the pipe", kind)
	}
	return nil
}

// pocketIDKey reads the static API key for a Pocket ID app from the secrets
// file. It is held only in memory and sent only on curl's stdin.
func pocketIDKey(cfg *config.Config, configPath, secretsPath, app string) (string, error) {
	if secretsPath == "" {
		secretsPath = filepath.Join(filepath.Dir(configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(secretsPath)
	if err != nil {
		return "", err
	}
	key, _ := secrets.Apps[app]["static_api_key"].(string)
	if key == "" {
		return "", fmt.Errorf("secrets apps.%s.static_api_key is empty. Run `paisans init` to generate it and `paisans apply` to render it into Pocket ID, then re-run", app)
	}
	return key, nil
}

// pocketIDBase is where Pocket ID answers from the host it runs on: the port
// it publishes on that site's mesh address, and nowhere else.
func pocketIDBase(cfg *config.Config, site string) string {
	return fmt.Sprintf("http://%s:%d", cfg.Sites[site].Address, render.AppPort(config.KindPocketID))
}

// adminSite picks the site to run an app's commands on. A pinned app has one.
// A clustered app runs on every apps site against one shared database, so any
// of them will do, and the first in sorted order is chosen so that the same
// command always reaches the same host. --site overrides it, but only to a
// site the app actually runs on.
func adminSite(cfg *config.Config, appName, override, command string) (string, error) {
	app := cfg.Apps[appName]
	var candidates []string
	switch app.Placement.Mode {
	case config.PlacementPinned:
		candidates = []string{app.Placement.Site}
	case config.PlacementCluster:
		candidates = cfg.AppsSites()
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("%s: %s runs on no site", command, appName)
	}
	if override == "" {
		return candidates[0], nil
	}
	if !slices.Contains(candidates, override) {
		return "", fmt.Errorf("%s: %s does not run on %s. It runs on %s", command, appName, override, strings.Join(candidates, ", "))
	}
	return override, nil
}

// standbyLook is how the admin commands ask a site what its Pocket ID is
// doing. Tests replace it.
var standbyLook = func(t apply.SSHTransport) apply.Transport { return t }

// pocketIDSite is adminSite for a Pocket ID that runs on more than one site:
// with no --site, the site whose instance is active, since the others stand
// by with their port closed and an API call there would be refused. It asks
// without sudo, as the API calls themselves run: the active site is found by
// /healthz on its mesh address, which needs no root, and a standby merely
// reads as down when docker is not the deploy user's. An explicit --site is
// used as given, through adminSite's own check.
func pocketIDSite(cfg *config.Config, appName, override, command string) (string, error) {
	if override != "" || !slices.Contains(apply.StandbyApps(cfg), appName) {
		return adminSite(cfg, appName, override, command)
	}
	site, list, err := activeInstance(cfg, appName, "", "")
	if err != nil {
		return "", fmt.Errorf("%s: %w, so there is no one site to call:\n%sName one with --site once exactly one is active", command, err, apply.DescribeInstances(list))
	}
	return site, nil
}

// activeInstance is the site whose instance of a Pocket ID app running on
// several sites is active, with what every site answered, or why there is no
// one such site. site, when named, is reached through destination, as the
// command running on it was told with --ssh.
func activeInstance(cfg *config.Config, appName, site, destination string) (string, []apply.Instance, error) {
	transports := map[string]apply.Transport{}
	for _, name := range cfg.AppsSites() {
		override := ""
		if name == site {
			override = destination
		}
		transports[name] = standbyLook(siteTransport(name, cfg.Sites[name], override, false))
	}
	list := apply.LookAtInstances(cfg, appName, transports)
	if _, err := apply.OneActive(cfg.Deployment(), appName, list); err != nil {
		return "", list, err
	}
	for _, in := range list {
		if in.State == apply.Active {
			return in.Site, list, nil
		}
	}
	return "", list, fmt.Errorf("no active instance of %s", appName)
}

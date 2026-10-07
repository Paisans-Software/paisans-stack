package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/appadmin"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// maxPassword bounds what `app admin create` reads. No password is near it;
// anything that is was a wrong pipe.
const maxPassword = 4 << 10

// adminTransport is how `app admin create` reaches a host. Tests replace it.
var adminTransport = func(t apply.SSHTransport) appadmin.Transport { return t }

// runApp dispatches `paisans app <subcommand>`.
func runApp(args []string) error {
	if len(args) < 2 || args[0] != "admin" || args[1] != "create" {
		return fmt.Errorf("app takes one subcommand, admin create: paisans app admin create --app <name> --username <u> --email <e> < password (no password for pocket-id)")
	}
	return runAppAdminCreate(args[2:], os.Stdin)
}

// runAppAdminCreate makes sure one user exists and is an administrator of one
// app.
//
// It exists because an app with registrations closed has no other way to get
// its first admin, and making one by hand means a shell on the server and a
// password typed into a command line, or for Pocket ID a browser on a setup
// page. It is modelled on `storage init`: probe read only, print the plan,
// change nothing without --execute.
//
// For a password kind the password is read from stdin for the reasons
// `secrets set` gives, and it is read before the host is touched, so a dry run
// refuses a bad pipe as an execute would. A passwordless kind (pocket-id)
// takes no password and refuses one that is piped in, rather than dropping it
// silently and leaving the operator to believe it was set; its account is
// reached through a one-time login link instead, printed once on --execute.
func runAppAdminCreate(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("app admin create", flag.ContinueOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets file, read for pocket-id's API key (default: secrets.enc.yaml beside the config)")
	appName := fs.String("app", "", "the app, by the name it has in the configuration")
	username := fs.String("username", "", "the administrator's username")
	email := fs.String("email", "", "the administrator's email, used only if the account is created")
	firstName := fs.String("first-name", "", "pocket-id only: the first name, used only if the account is created (default: the username)")
	lastName := fs.String("last-name", "", "pocket-id only: the last name, used only if the account is created")
	loginLink := fs.Bool("login-link", false, "pocket-id only: issue a fresh one-time login link for an account that already exists")
	site := fs.String("site", "", "the site whose copy of the app to run the commands in (default: the pinned site, the first apps site, or for pocket-id the site whose instance is active)")
	destination := fs.String("ssh", "", "ssh destination, used verbatim in place of the site's ssh section (its user, host, port and keys are then ignored)")
	resetPassword := fs.Bool("reset-password", false, "replace the password of an account that already exists")
	execute := fs.Bool("execute", false, "actually create, verify and grant")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since docker is root's (pocket-id never uses it: curl needs no root)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("app admin create: the password is read from stdin and never from an argument, which shell history and `ps` would both keep. Got extra argument(s)")
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
	// Refuse an unimplemented kind before resolving a site or reaching a host.
	if _, err := appadmin.For(app.Kind); err != nil {
		return fmt.Errorf("app admin create: %w", err)
	}

	req := appadmin.Request{App: *appName, Username: *username, Email: *email, ResetPassword: *resetPassword,
		FirstName: *firstName, LastName: *lastName, LoginLink: *loginLink}
	passwordless := appadmin.Passwordless(app.Kind)
	if passwordless {
		if *resetPassword {
			return fmt.Errorf("app admin create: %s users sign in with passkeys and have no password to reset. Use --login-link to give an existing account a way back in", app.Kind)
		}
		if err := refusePipedPassword(stdin, app.Kind); err != nil {
			return err
		}
	} else {
		if *loginLink || *firstName != "" || *lastName != "" {
			return fmt.Errorf("app admin create: --login-link, --first-name and --last-name are for a passwordless kind, and %s is not one", app.Kind)
		}
		if *email == "" {
			return fmt.Errorf("app admin create: --email is required for %s", app.Kind)
		}
		req.Password, err = readPassword(stdin)
		if err != nil {
			return err
		}
	}

	where, err := adminSite(cfg, *appName, *site)
	if app.Kind == config.KindPocketID {
		where, err = pocketIDSite(cfg, *appName, *site, "app admin create")
	}
	if err != nil {
		return err
	}

	if app.Kind == config.KindPocketID {
		key, err := pocketIDKey(cfg, *configPath, *secretsPath, *appName)
		if err != nil {
			return fmt.Errorf("app admin create: %w", err)
		}
		req.APIKey = key
		req.APIBase = pocketIDBase(cfg, where)
		req.PublicURL = "https://" + app.Hostname
		*sudo = false
	}

	transport := adminTransport(siteTransport(cfg.Sites[where], *destination, *sudo))
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

// refusePipedPassword refuses a password piped to a passwordless kind. A
// terminal on stdin is left unread, so an interactive run never waits.
func refusePipedPassword(stdin io.Reader, kind config.Kind) error {
	if file, ok := stdin.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return nil
		}
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxPassword+1))
	if err != nil {
		return fmt.Errorf("app admin create: reading stdin: %w", err)
	}
	if strings.TrimSpace(string(data)) != "" {
		return fmt.Errorf("app admin create: %s users sign in with passkeys, so there is no password to set, and something was piped in. Nothing was changed; run it without the pipe", kind)
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
func adminSite(cfg *config.Config, appName, override string) (string, error) {
	app := cfg.Apps[appName]
	var candidates []string
	switch app.Placement.Mode {
	case config.PlacementPinned:
		candidates = []string{app.Placement.Site}
	case config.PlacementCluster:
		candidates = cfg.AppsSites()
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("app admin create: %s runs on no site", appName)
	}
	if override == "" {
		return candidates[0], nil
	}
	if !slices.Contains(candidates, override) {
		return "", fmt.Errorf("app admin create: %s does not run on %s. It runs on %s", appName, override, strings.Join(candidates, ", "))
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
		return adminSite(cfg, appName, override)
	}
	transports := map[string]apply.Transport{}
	for _, name := range cfg.AppsSites() {
		transports[name] = standbyLook(siteTransport(cfg.Sites[name], "", false))
	}
	list := apply.LookAtInstances(cfg, appName, transports)
	if _, err := apply.OneActive(appName, list); err != nil {
		return "", fmt.Errorf("%s: %w, so there is no one site to call:\n%sName one with --site once exactly one is active", command, err, apply.DescribeInstances(list))
	}
	for _, in := range list {
		if in.State == apply.Active {
			return in.Site, nil
		}
	}
	return "", fmt.Errorf("%s: no active instance of %s", command, appName)
}

// readPassword reads the password from stdin, refusing a terminal and an
// empty value, and stripping one trailing newline, the same rules `secrets
// set` applies to a credential. Nothing it returns as an error carries the
// value.
func readPassword(stdin io.Reader) (string, error) {
	if file, ok := stdin.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return "", fmt.Errorf("app admin create: stdin is a terminal, so the password would be typed where it can be seen and kept in scrollback. Pipe it in, as in `security find-generic-password -s <item> -w | paisans app admin create ...`")
		}
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxPassword+1))
	if err != nil {
		return "", fmt.Errorf("app admin create: reading stdin: %w", err)
	}
	if len(data) > maxPassword {
		return "", fmt.Errorf("app admin create: more than %d bytes on stdin, which is not a password", maxPassword)
	}
	value := string(data)
	if strings.HasSuffix(value, "\r\n") {
		value = strings.TrimSuffix(value, "\r\n")
	} else {
		value = strings.TrimSuffix(value, "\n")
	}
	if value == "" {
		return "", fmt.Errorf("app admin create: nothing on stdin. Pipe the password in")
	}
	return value, nil
}

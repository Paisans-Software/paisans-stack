package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/appadmin"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// maxPassword bounds what `app admin create` reads. No password is near it;
// anything that is was a wrong pipe.
const maxPassword = 4 << 10

// adminTransport is how `app admin create` reaches a host. Tests replace it.
var adminTransport = func(destination string, sudo bool) appadmin.Transport {
	return apply.SSHTransport{Destination: destination, Sudo: sudo}
}

// runApp dispatches `paisans app <subcommand>`.
func runApp(args []string) error {
	if len(args) < 2 || args[0] != "admin" || args[1] != "create" {
		return fmt.Errorf("app takes one subcommand, admin create: paisans app admin create --app <name> --username <u> --email <e> < password")
	}
	return runAppAdminCreate(args[2:], os.Stdin)
}

// runAppAdminCreate makes sure one user exists, is verified, and is an
// administrator of one app.
//
// It exists because an app with registrations closed has no other way to get
// its first admin, and making one by hand means a shell on the server and a
// password typed into a command line. It is modelled on `storage init`: probe
// read only, print the plan, change nothing without --execute.
//
// The password is read from stdin for the reasons `secrets set` gives, and it
// is read before the host is touched, so a dry run refuses a bad pipe as an
// execute would.
func runAppAdminCreate(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("app admin create", flag.ContinueOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	appName := fs.String("app", "", "the app, by the name it has in the configuration")
	username := fs.String("username", "", "the administrator's username")
	email := fs.String("email", "", "the administrator's email, used only if the account is created")
	site := fs.String("site", "", "the site whose copy of the app to run the commands in (default: the pinned site, or the first apps site)")
	destination := fs.String("ssh", "", "ssh destination (default: the site's declared ssh address)")
	resetPassword := fs.Bool("reset-password", false, "replace the password of an account that already exists")
	execute := fs.Bool("execute", false, "actually create, verify and grant")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since docker is root's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("app admin create: the password is read from stdin and never from an argument, which shell history and `ps` would both keep. Got extra argument(s)")
	}
	if *appName == "" || *username == "" || *email == "" {
		return fmt.Errorf("app admin create: --app, --username and --email are all required")
	}

	password, err := readPassword(stdin)
	if err != nil {
		return err
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
	where, err := adminSite(cfg, *appName, *site)
	if err != nil {
		return err
	}
	if *destination == "" {
		*destination = cfg.Sites[where].SSH
	}
	if *destination == "" {
		return fmt.Errorf("app admin create: site %s has no ssh address and none was given with --ssh", where)
	}

	transport := adminTransport(*destination, *sudo)
	req := appadmin.Request{App: *appName, Username: *username, Email: *email, Password: password, ResetPassword: *resetPassword}
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
	if err := appadmin.Execute(plan, transport); err != nil {
		return fmt.Errorf("app admin create: %w", err)
	}
	fmt.Fprintf(os.Stdout, "\n%s is an administrator of %s\n", *username, *appName)
	return nil
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

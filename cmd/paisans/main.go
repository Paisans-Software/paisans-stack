// Command paisans renders, checks and applies a paisans deployment
// declaration.
//
// validate, init and render touch nothing outside the working directory.
// `host prepare`, `apply` and `storage init` reach a machine: each reads it to
// show what it would do, and changes nothing unless told to with --execute.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/dns"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/hostprep"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

const usage = `paisans renders and checks a community stack declaration.

Usage:
  paisans validate [--config paisans.yaml]
  paisans init     [--config paisans.yaml] [--secrets secrets.enc.yaml]
  paisans render   [--config paisans.yaml] [--secrets secrets.enc.yaml] --out ./out
  paisans host prepare --site <name> [--config paisans.yaml] [--ssh <destination>]
               [--execute]
  paisans apply    --site <name> [--config paisans.yaml] [--secrets secrets.enc.yaml]
                   [--ssh <destination>] [--overwrite <path>]... [--execute]
  paisans storage init --site <name> [--config paisans.yaml] [--secrets secrets.enc.yaml]
               [--ssh <destination>] [--execute]
  paisans dns init [--config paisans.yaml] [--secrets secrets.enc.yaml] [--execute]
  paisans secrets set <dotted.key> [--config paisans.yaml] [--secrets secrets.enc.yaml] < value

Commands:
  validate   Load the configuration and report every problem found.
  init       Generate the secrets this configuration needs, filling in only
             what is missing, and say what is still owed from elsewhere.
  render     Validate, then write per site artifacts to a local directory.
  host       Take a blank host to the state apply assumes: Docker, the
             WireGuard tools, a firewall, and a watchdog on a data site.
             Installs only what is missing. Writes nothing without --execute.
  apply      Compare one site's rendered artifacts with what is on that host
             and show what would change. Writes nothing without --execute.
  storage    Provision object storage on a site: the cluster layout, each
             app's key, and its bucket. Creates only what is missing.
             Writes nothing without --execute.
  dns        Create the public DNS records the configuration implies, at the
             provider named by acme.provider. Creates only what is missing,
             never updates or deletes, and refuses if any record conflicts.
             Writes nothing without --execute.
  secrets    set: read one value from stdin and write it into the encrypted
             secrets file, printing only its name. For credentials issued
             elsewhere, so they never touch a terminal or an editor.

host prepare, apply and storage init are the only commands that reach a host.
Each reads it to plan, and changes it only with --execute. dns init reaches no
host, only the DNS provider's API, and changes it only with --execute.
Everything else writes files locally and stops.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "validate":
		err = runValidate(os.Args[2:])
	case "init":
		err = runInit(os.Args[2:])
	case "render":
		err = runRender(os.Args[2:])
	case "apply":
		err = runApply(os.Args[2:])
	case "host":
		if len(os.Args) < 3 || os.Args[2] != "prepare" {
			fmt.Fprintf(os.Stderr, "paisans: host takes one subcommand, prepare\n\n%s", usage)
			os.Exit(2)
		}
		err = runHostPrepare(os.Args[3:])
	case "secrets":
		err = runSecrets(os.Args[2:])
	case "storage":
		if len(os.Args) < 3 || os.Args[2] != "init" {
			fmt.Fprintf(os.Stderr, "paisans: storage takes one subcommand, init\n\n%s", usage)
			os.Exit(2)
		}
		err = runStorageInit(os.Args[3:])
	case "dns":
		if len(os.Args) < 3 || os.Args[2] != "init" {
			fmt.Fprintf(os.Stderr, "paisans: dns takes one subcommand, init\n\n%s", usage)
			os.Exit(2)
		}
		err = runDNSInit(os.Args[3:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "paisans: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "paisans: %v\n", err)
		os.Exit(1)
	}
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	report(os.Stdout, *configPath, result)
	if result.Refused() {
		return fmt.Errorf("%s cannot be rendered: %d refusal(s) above", *configPath, len(result.Refusals()))
	}
	return nil
}

// runInit generates the secrets a configuration needs.
//
// It is safe to re-run, and re-running is how a site or an app added later
// gets its secrets: nothing already set is replaced, because regenerating a
// WireGuard key breaks every peer that trusted the old one and regenerating a
// database password locks an application out of a role that still holds the
// old one.
//
// It touches no host. Standing a deployment up is `apply`, and that is a
// separate decision from having credentials to stand it up with.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets file (default: secrets.enc.yaml beside the config)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	report(os.Stderr, *configPath, result)
	if result.Refused() {
		return fmt.Errorf("%s was refused: %d problem(s) above. Secrets are not generated for a configuration that cannot be deployed", *configPath, len(result.Refusals()))
	}

	if *secretsPath == "" {
		*secretsPath = filepath.Join(filepath.Dir(*configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(*secretsPath)
	switch {
	case os.IsNotExist(errors.Unwrap(err)), os.IsNotExist(err):
		secrets = &config.Secrets{Version: 1}
	case err != nil:
		return err
	}

	filled, err := secretsgen.Fill(cfg, secrets)
	if err != nil {
		return err
	}

	recipients, err := config.Recipients(filepath.Dir(*secretsPath))
	if err != nil {
		return err
	}

	if !filled.Changed() {
		fmt.Fprintf(os.Stdout, "%s already has every generated secret (%d). Nothing written.\n",
			*secretsPath, len(filled.Kept))
		reportOwed(filled)
		return nil
	}

	if err := config.WriteSecrets(*secretsPath, secrets, recipients); err != nil {
		return err
	}

	// Names, never values. A secret printed to a terminal is in a scrollback
	// buffer, and often in a multiplexer's log as well.
	fmt.Fprintf(os.Stdout, "%s: generated %d secret(s), kept %d.\n", *secretsPath, len(filled.Generated), len(filled.Kept))
	for _, name := range filled.Generated {
		fmt.Fprintf(os.Stdout, "  + %s\n", name)
	}
	if len(recipients) == 0 {
		fmt.Fprintf(os.Stderr, "\npaisans: %s was written in PLAINTEXT, because no %s beside it names an age recipient.\nA real deployment encrypts this file. Add one and re-encrypt before committing anything.\n",
			*secretsPath, config.SOPSConfigName)
	} else {
		fmt.Fprintf(os.Stdout, "\nEncrypted to %d age recipient(s) from %s.\n", len(recipients), config.SOPSConfigName)
	}
	reportOwed(filled)
	return nil
}

// reportOwed prints what the toolkit will not invent. Leaving these silent
// would let an operator believe an install is finished when sign in and
// certificates are both still missing.
func reportOwed(filled secretsgen.Result) {
	if len(filled.Owed) == 0 {
		return
	}
	fmt.Fprintf(os.Stdout, "\nStill owed, and not generated here:\n")
	for _, owed := range filled.Owed {
		fmt.Fprintf(os.Stdout, "  ? %s\n      %s\n", owed.Name, owed.Why)
	}
}

func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the sops encrypted secrets (default: secrets.enc.yaml beside the config)")
	out := fs.String("out", "", "directory to write artifacts into")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("render: --out is required. Artifacts are written to a local directory and pushed by a later step")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	report(os.Stderr, *configPath, result)
	if result.Refused() {
		return fmt.Errorf("%s cannot be rendered: %d refusal(s) above", *configPath, len(result.Refusals()))
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
	// render does not call secretsgen.Fill, so a key hand edited into the file
	// after the last `init` is never looked at unless this is checked here too.
	if err := secretsgen.CheckGarageKeys(cfg, secrets); err != nil {
		return err
	}
	if err := requireACMEToken(cfg, secrets, cfg.SiteNames()); err != nil {
		return err
	}

	plan, err := render.Build(cfg, secrets)
	if err != nil {
		return err
	}
	if err := render.Write(plan, *out); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "rendered %d files to %s\n", len(plan.Files), *out)
	return nil
}

// runApply compares one site against what is rendered for it, and changes
// nothing unless told to.
//
// A dry run by default is not politeness. This is the only command that
// reaches a machine, the machine it reaches is running a community, and the
// difference between "show me" and "do it" should be a flag an operator typed
// rather than a habit they formed.
func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets (default: secrets.enc.yaml beside the config)")
	site := fs.String("site", "", "the site to apply, by the name it has in the configuration")
	destination := fs.String("ssh", "", "ssh destination (default: the site's declared ssh address)")
	execute := fs.Bool("execute", false, "actually write files and restart services")
	var overwrite pathList
	fs.Var(&overwrite, "overwrite", "replace this conflicting file although it differs from the last apply's record (repeatable)")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since /srv and /etc are not the deploy user's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *site == "" {
		return fmt.Errorf("apply: --site is required. A site at a time is deliberate: a staged change that half succeeds across three machines is worse than one that failed on one")
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
	declared, ok := cfg.Sites[*site]
	if !ok {
		return fmt.Errorf("apply: %s declares no site %q. Declared sites are %s", *configPath, *site, strings.Join(cfg.SiteNames(), ", "))
	}
	if *destination == "" {
		*destination = declared.SSH
	}
	if *destination == "" {
		return fmt.Errorf("apply: site %s has no ssh address and none was given with --ssh", *site)
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
	// apply does not call secretsgen.Fill either, and this is the path that
	// actually reaches a host: a malformed key has to stop here, not just
	// print a confusing failure partway through provisioning on the machine.
	if err := secretsgen.CheckGarageKeys(cfg, secrets); err != nil {
		return err
	}
	if err := requireACMEToken(cfg, secrets, []string{*site}); err != nil {
		return err
	}

	rendered, err := render.Build(cfg, secrets)
	if err != nil {
		return err
	}

	transport := apply.SSHTransport{Destination: *destination, Sudo: *sudo}
	plan, err := apply.Build(*site, rendered, acme.Module(cfg.ACME.Provider), transport, overwrite...)
	if err != nil {
		return err
	}
	databases, err := apply.Databases(cfg, secrets, *site)
	if err != nil {
		return err
	}
	plan.WithDatabases(databases)
	plan.Progress = os.Stdout
	printPlan(plan)

	if !*execute {
		if len(plan.Writes()) == 0 && len(plan.Actions) == 0 && plan.WireGuard == apply.WireGuardNone {
			return nil
		}
		fmt.Fprintf(os.Stdout, "\nNothing was changed. Re-run with --execute to apply this.\n")
		return nil
	}
	if err := apply.Execute(plan, transport); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\napplied %d file(s) to %s\n", len(plan.Writes()), plan.Transport)
	return nil
}

// runStorageInit provisions Garage object storage on one site: the cluster
// layout, each app's S3 key, and its bucket. It is modelled on runApply,
// right down to the dry run by default, because it is the other command that
// reaches a host.
//
// It must run after the infrastructure stack is up, since Garage has to be
// reachable to be asked what it already has.
func runStorageInit(args []string) error {
	fs := flag.NewFlagSet("storage init", flag.ExitOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets (default: secrets.enc.yaml beside the config)")
	site := fs.String("site", "", "the site to provision, by the name it has in the configuration")
	destination := fs.String("ssh", "", "ssh destination (default: the site's declared ssh address)")
	execute := fs.Bool("execute", false, "actually create what is missing")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since /srv and /etc are not the deploy user's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *site == "" {
		return fmt.Errorf("storage init: --site is required. A site at a time is deliberate, the same reason apply takes one")
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
	declared, ok := cfg.Sites[*site]
	if !ok {
		return fmt.Errorf("storage init: %s declares no site %q. Declared sites are %s", *configPath, *site, strings.Join(cfg.SiteNames(), ", "))
	}
	if *destination == "" {
		*destination = declared.SSH
	}
	if *destination == "" {
		return fmt.Errorf("storage init: site %s has no ssh address and none was given with --ssh", *site)
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
	// A malformed key has to stop here, before it reaches `garage key import`
	// partway through provisioning: earlier keys in the same run would
	// already be imported and cannot be imported again.
	if err := secretsgen.CheckGarageKeys(cfg, secrets); err != nil {
		return err
	}

	transport := apply.SSHTransport{Destination: *destination, Sudo: *sudo}
	plan, err := garage.Build(*site, cfg, secrets, transport)
	if err != nil {
		return err
	}
	printGaragePlan(plan)

	if !*execute {
		if len(plan.Steps) == 0 {
			return nil
		}
		fmt.Fprintf(os.Stdout, "\nNothing was changed. Re-run with --execute to apply this.\n")
		return nil
	}
	if err := garage.Execute(plan, transport); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\nprovisioned %d step(s) on %s\n", len(plan.Steps), *site)
	return nil
}

// runHostPrepare takes one site's host to the state apply assumes. It is
// modelled on runStorageInit: probe read only, print, and change the host only
// with --execute. It needs no secrets; nothing it installs is a credential.
func runHostPrepare(args []string) error {
	fs := flag.NewFlagSet("host prepare", flag.ExitOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	site := fs.String("site", "", "the site to prepare, by the name it has in the configuration")
	destination := fs.String("ssh", "", "ssh destination (default: the site's declared ssh address)")
	execute := fs.Bool("execute", false, "actually install and configure what is missing")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since packages, the firewall and kernel modules are root's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *site == "" {
		return fmt.Errorf("host prepare: --site is required. A site at a time is deliberate, the same reason apply takes one")
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
	declared, ok := cfg.Sites[*site]
	if !ok {
		return fmt.Errorf("host prepare: %s declares no site %q. Declared sites are %s", *configPath, *site, strings.Join(cfg.SiteNames(), ", "))
	}
	if *destination == "" {
		*destination = declared.SSH
	}
	if *destination == "" {
		return fmt.Errorf("host prepare: site %s has no ssh address and none was given with --ssh", *site)
	}

	transport := apply.SSHTransport{Destination: *destination, Sudo: *sudo}
	plan, err := hostprep.Build(*site, cfg, transport)
	if err != nil {
		return err
	}
	plan.Print(os.Stdout)

	if !*execute {
		if len(plan.Steps) == 0 {
			return nil
		}
		fmt.Fprintf(os.Stdout, "\nNothing was changed. Re-run with --execute to apply this.\n")
		return nil
	}
	if err := hostprep.Execute(plan, transport); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\nprepared %s: %d step(s) on %s\n", *site, len(plan.Steps), *destination)
	return nil
}

func printGaragePlan(plan *garage.Plan) {
	fmt.Fprintf(os.Stdout, "%s\n", plan.Site)
	for _, step := range plan.Steps {
		fmt.Fprintf(os.Stdout, "  %-9s %s\n", "create", step.Describe)
	}
	for _, present := range plan.Present {
		fmt.Fprintf(os.Stdout, "  %-9s %s\n", "present", present)
	}
}

func printPlan(plan *apply.Plan) {
	fmt.Fprintf(os.Stdout, "%s (%s)\n", plan.Site, plan.Transport)
	var unchanged int
	for _, change := range plan.Changes {
		if change.Kind == apply.Unchanged {
			unchanged++
			continue
		}
		kind := change.Kind.String()
		if change.Overwritten {
			kind = "overwrite"
		}
		fmt.Fprintf(os.Stdout, "  %-9s %s\n", kind, change.Path)
	}
	if unchanged > 0 {
		fmt.Fprintf(os.Stdout, "  %-9s %d file(s)\n", "unchanged", unchanged)
	}
	if plan.WireGuard != apply.WireGuardNone {
		fmt.Fprintf(os.Stdout, "  %-9s %s\n", "mesh", plan.WireGuard.Describe())
	}
	bootstrapped := plan.Bootstrap == nil
	for _, action := range plan.Actions {
		if action.Stack != "infra" && !bootstrapped {
			printBootstrap(plan.Bootstrap)
			bootstrapped = true
		}
		verb := "restart"
		if action.Recreate {
			verb = "recreate"
		}
		fmt.Fprintf(os.Stdout, "  %-9s %s\n      %s\n", verb, action.Stack, action.Reason)
	}
	if !bootstrapped {
		printBootstrap(plan.Bootstrap)
	}
	if plan.GatewayChanging && plan.ACMEModule != "" {
		fmt.Fprintf(os.Stdout, "  %-9s the gateway's Caddy carries %s, before anything moves\n", "check", plan.ACMEModule)
	}
	if plan.GatewayReload {
		fmt.Fprintf(os.Stdout, "  %-9s the gateway, after its assembled configuration validates\n", "reload")
	} else if plan.GatewayChanging {
		fmt.Fprintf(os.Stdout, "  %-9s the assembled gateway configuration, before the gateway is replaced\n", "validate")
	}
	if conflicts := plan.Conflicts(); len(conflicts) > 0 {
		fmt.Fprintf(os.Stdout, "\n%d file(s) were edited on the host. Nothing will be applied until that is resolved.\n", len(conflicts))
	}
}

func report(w *os.File, path string, result validate.Result) {
	if len(result.Findings) == 0 {
		fmt.Fprintf(w, "%s: no problems found\n", path)
		return
	}
	for _, finding := range result.Findings {
		fmt.Fprintf(w, "%s\n\n", finding)
	}
	fmt.Fprintf(w, "%s: %d refusal(s), %d warning(s)\n",
		path, len(result.Refusals()), len(result.Warnings()))
}

// runDNSInit creates the public DNS records a deployment needs, at the
// provider acme.provider names, using the token that already answers ACME
// challenges. It is modelled on runStorageInit: a dry run by default, and
// only what is missing is created.
//
// It reaches no host. The workstation talks to the provider's API and to
// nothing else, so it takes no --site and no --ssh.
func runDNSInit(args []string) error {
	fs := flag.NewFlagSet("dns init", flag.ExitOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets (default: secrets.enc.yaml beside the config)")
	execute := fs.Bool("execute", false, "actually create the missing records")
	if err := fs.Parse(args); err != nil {
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
	// Worked out before the secrets are opened or the provider is contacted,
	// so a configuration that cannot name its records is refused offline.
	wants, err := dns.Desired(cfg)
	if err != nil {
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
	provider, err := dns.For(cfg.ACME.Provider, secrets.External["acme_dns_token"])
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	plan, err := dns.Build(ctx, provider, wants)
	if err != nil {
		return err
	}
	plan.Write(os.Stdout)

	if !*execute {
		if len(plan.Creates()) == 0 {
			return nil
		}
		fmt.Fprintf(os.Stdout, "\nNothing was changed. Re-run with --execute to create these.\n")
		return nil
	}
	if err := dns.Execute(ctx, provider, plan); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\ncreated %d record(s) at %s, each read back\n", len(plan.Creates()), plan.Provider)
	return nil
}

// printBootstrap shows the database work where it happens: after the
// infrastructure stack and before any app stack.
func printBootstrap(b *apply.Bootstrap) {
	fmt.Fprintf(os.Stdout, "  %-9s for a Patroni primary at %s, up to 3 minutes; a replica leaves the rest to the leader's site\n", "wait", b.Patroni)
	for _, db := range b.Databases {
		fmt.Fprintf(os.Stdout, "  %-9s database %s: role %s with its password, database owned by it, creating only what is missing\n", "bootstrap", db.App, db.Role)
	}
}

// pathList collects a repeatable flag. --overwrite takes one path each time it
// is given, so that every file replaced against its record was named on its
// own: a pattern or a blanket switch would let one decision cover files the
// operator never looked at.
type pathList []string

func (l *pathList) String() string { return strings.Join(*l, ",") }

func (l *pathList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

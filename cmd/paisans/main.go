// Command paisans renders, checks and applies a paisans deployment
// declaration.
//
// Three of its four commands touch nothing outside the working directory.
// `apply` is the exception and is the only code path here that reaches a
// machine: it shows what it would do and changes nothing unless it is told to
// with --execute.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

const usage = `paisans renders and checks a community stack declaration.

Usage:
  paisans validate [--config paisans.yaml]
  paisans init     [--config paisans.yaml] [--secrets secrets.enc.yaml]
  paisans render   [--config paisans.yaml] [--secrets secrets.enc.yaml] --out ./out
  paisans apply    --site <name> [--config paisans.yaml] [--secrets secrets.enc.yaml]
                   [--ssh <destination>] [--execute]

Commands:
  validate   Load the configuration and report every problem found.
  init       Generate the secrets this configuration needs, filling in only
             what is missing, and say what is still owed from elsewhere.
  render     Validate, then write per site artifacts to a local directory.
  apply      Compare one site's rendered artifacts with what is on that host
             and show what would change. Writes nothing without --execute.

Only apply reaches a host, and only with --execute. Everything else writes
files locally and stops.
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

	rendered, err := render.Build(cfg, secrets)
	if err != nil {
		return err
	}

	transport := apply.SSHTransport{Destination: *destination, Sudo: *sudo}
	plan, err := apply.Build(*site, rendered, acme.Module(cfg.ACME.Provider), transport)
	if err != nil {
		return err
	}
	printPlan(plan)

	if !*execute {
		if len(plan.Writes()) == 0 && len(plan.Actions) == 0 {
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

func printPlan(plan *apply.Plan) {
	fmt.Fprintf(os.Stdout, "%s (%s)\n", plan.Site, plan.Transport)
	var unchanged int
	for _, change := range plan.Changes {
		if change.Kind == apply.Unchanged {
			unchanged++
			continue
		}
		fmt.Fprintf(os.Stdout, "  %-9s %s\n", change.Kind, change.Path)
	}
	if unchanged > 0 {
		fmt.Fprintf(os.Stdout, "  %-9s %d file(s)\n", "unchanged", unchanged)
	}
	for _, action := range plan.Actions {
		verb := "restart"
		if action.Recreate {
			verb = "recreate"
		}
		fmt.Fprintf(os.Stdout, "  %-9s %s\n      %s\n", verb, action.Stack, action.Reason)
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

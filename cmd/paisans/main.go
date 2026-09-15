// Command paisans renders and checks a paisans deployment declaration.
//
// This build does three things and touches nothing outside the working
// directory: it validates a configuration, generates the secrets that
// configuration needs, and renders artifacts to a local directory. There is no
// SSH, no Docker and no network path in it at all.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/josephquigley/paisans-stack/internal/config"
	"github.com/josephquigley/paisans-stack/internal/render"
	"github.com/josephquigley/paisans-stack/internal/secretsgen"
	"github.com/josephquigley/paisans-stack/internal/validate"
)

const usage = `paisans renders and checks a community stack declaration.

Usage:
  paisans validate [--config paisans.yaml]
  paisans init     [--config paisans.yaml] [--secrets secrets.enc.yaml]
  paisans render   [--config paisans.yaml] [--secrets secrets.enc.yaml] --out ./out

Commands:
  validate   Load the configuration and report every problem found.
  init       Generate the secrets this configuration needs, filling in only
             what is missing, and say what is still owed from elsewhere.
  render     Validate, then write per site artifacts to a local directory.

Nothing here reaches a host. Rendering writes files and stops.
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

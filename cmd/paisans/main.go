// Command paisans renders and checks a paisans deployment declaration.
//
// This build does two things and touches nothing outside the working
// directory: it validates a configuration, and it renders one to a local
// directory. There is no SSH, no Docker and no network path in it at all.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/josephquigley/paisans-stack/internal/config"
	"github.com/josephquigley/paisans-stack/internal/render"
	"github.com/josephquigley/paisans-stack/internal/validate"
)

const usage = `paisans renders and checks a community stack declaration.

Usage:
  paisans validate [--config paisans.yaml]
  paisans render   [--config paisans.yaml] [--secrets secrets.enc.yaml] --out ./out

Commands:
  validate   Load the configuration and report every problem found.
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

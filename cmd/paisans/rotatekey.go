package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/rotatekey"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/ui"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// runStorageRotateKey replaces one app's Garage S3 key, stage by stage:
// internal/rotatekey. It is modelled on runStorageAdd: it reaches the first
// Garage site and every site running the app through their own ssh sections,
// prints the plan, and changes anything only with --execute.
func runStorageRotateKey(args []string) error {
	fs := flag.NewFlagSet("storage rotate-key", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets (default: secrets.enc.yaml beside the config)")
	appName := fs.String("app", "", "the app whose S3 key to replace, by the name it has in the configuration")
	execute := fs.Bool("execute", false, "actually run the stages, stopping at the first gate that fails")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since /srv and docker are root's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	if fs.NArg() > 0 {
		return fmt.Errorf("storage rotate-key: name the app with --app. %q is extra", fs.Arg(0))
	}
	if *appName == "" {
		return fmt.Errorf("storage rotate-key: --app is required. A key belongs to one app, and a rotation replaces one at a time")
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
	if err := secretsgen.CheckGarageKeys(cfg, secrets); err != nil {
		return err
	}
	// The rotation writes the secrets twice, and an encrypted file with no
	// recipient beside it would be written back in plaintext: refused before
	// anything reaches a host, the same as `secrets set`.
	recipients, err := config.Recipients(filepath.Dir(*secretsPath))
	if err != nil {
		return err
	}
	if secrets.Encrypted && len(recipients) == 0 {
		return fmt.Errorf("storage rotate-key: %s is encrypted, but no %s beside it names a recipient, so writing the new key into it would leave it in plaintext. Nothing was changed", *secretsPath, config.SOPSConfigName)
	}

	transports := map[string]apply.Transport{}
	for _, name := range cfg.SiteNames() {
		transports[name] = siteTransport(name, cfg.Sites[name], "", *sudo)
	}
	// The switch is an apply of the app on every site it runs on, so each
	// of those is host checked first, as apply checks it.
	sites := append([]string(nil), render.AppSites(cfg)[*appName]...)
	sort.Strings(sites)
	shared, err := gateSites(r, cfg, sites, func(site string) hostcheck.Transport { return transports[site] })
	if err != nil {
		return err
	}
	// Garage's first site, where the key is imported and deleted, and every
	// site the app runs on, where the switch applies it.
	if len(cfg.Storage.Garage.Sites) > 0 {
		if err := claimSites(r, cfg, *execute, *sudo, union(cfg.Storage.Garage.Sites[:1], render.AppSites(cfg)[*appName])...); err != nil {
			return err
		}
	}
	plan, err := rotatekey.Build(rotatekey.Options{
		App:        *appName,
		Config:     cfg,
		Secrets:    secrets,
		Transports: transports,
		Switch:     applySwitch{cfg: cfg, app: *appName, transports: transports, shared: shared},
		Save: func(s *config.Secrets) error {
			return config.WriteSecrets(*secretsPath, s, recipients)
		},
	})
	if err != nil {
		return err
	}
	plan.Print(os.Stdout)

	if !*execute {
		fmt.Fprintf(os.Stdout, "\nNothing was changed. Re-run with --execute to run these stages; each stops at its gate if it does not pass, and the old key is deleted only after the app is switched and the new key proven.\n")
		return nil
	}
	plan.Progress = os.Stdout
	fmt.Fprintln(os.Stdout)
	if err := rotatekey.Execute(plan); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\nstorage rotate-key: %s now uses %s, and %s is deleted from Garage\n", *appName, plan.NewKeyID, plan.OldKeyID)
	return nil
}

// applySwitch is rotatekey.Switch as `paisans apply --site <site> --only
// <app>`: the same plan, the same refusal on a host edit, the same health
// gate.
type applySwitch struct {
	cfg        *config.Config
	app        string
	transports map[string]apply.Transport
	// shared is the host check's finding per site: a shared site keeps its
	// images, as apply does there.
	shared map[string]bool
}

func (a applySwitch) plan(site string, secrets *config.Secrets) (*apply.Plan, error) {
	options := []apply.Option{apply.Only(a.app)}
	if a.shared[site] {
		options = append(options, apply.KeepImages())
	}
	plan, err := planSiteApply(a.cfg, secrets, site, a.transports[site], options...)
	if err != nil {
		return nil, err
	}
	if c := plan.Conflicts(); len(c) > 0 {
		return nil, fmt.Errorf("%s's %s differs from what the last apply recorded, so somebody edited it on the host. Nothing was changed on %s. Restore it, or run `paisans apply --site %s --only %s --overwrite %s`, then run rotate-key again", site, c[0].Path, site, site, a.app, c[0].Path)
	}
	return plan, nil
}

func (a applySwitch) Pending(site string, secrets *config.Secrets) ([]string, error) {
	plan, err := a.plan(site, secrets)
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, c := range plan.Writes() {
		pending = append(pending, c.Kind.String()+" "+c.Path)
	}
	for _, action := range plan.Actions {
		verb := "restart"
		if action.Recreate || action.Force {
			verb = "recreate"
		}
		pending = append(pending, verb+" "+action.Stack)
	}
	return pending, nil
}

func (a applySwitch) Apply(site string, secrets *config.Secrets) error {
	plan, err := a.plan(site, secrets)
	if err != nil {
		return err
	}
	plan.Report = ui.NewPlain(os.Stdout, false)
	return apply.Execute(plan, a.transports[site])
}

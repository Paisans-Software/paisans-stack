package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/oidcclient"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// oidcTransport is how `oidc client create` reaches the identity provider's
// host. Tests replace it. It never uses sudo: curl needs no root.
var oidcTransport = func(t apply.SSHTransport) pocketid.Transport { return t }

// runOIDC dispatches `paisans oidc <subcommand>`.
func runOIDC(args []string) error {
	if len(args) < 2 || args[0] != "client" || args[1] != "create" {
		return fmt.Errorf("oidc takes one subcommand, client create: paisans oidc client create --app <name> [--execute]")
	}
	return runOIDCClientCreate(args[2:])
}

// runOIDCClientCreate creates an app's client at the deployment's Pocket ID
// and records its credentials in the secrets file.
//
// Every step is a Pocket ID mutation a human approves, so the command prints
// each one with what it sends and changes nothing without --execute. The
// client secret is generated here, written into the secrets file, and only
// then sent; it is never printed. `paisans apply` then renders it into the
// app like any other secret.
func runOIDCClientCreate(args []string) error {
	fs := flag.NewFlagSet("oidc client create", flag.ContinueOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets file (default: secrets.enc.yaml beside the config)")
	appName := fs.String("app", "", "the app the client is for, by the name it has in the configuration")
	rotate := fs.Bool("rotate-secret", false, "add a new secret even when the recorded one is live; the old one stays valid until deleted")
	site := fs.String("site", "", "the site whose Pocket ID to call (default: its pinned site, or the apps site whose instance is active)")
	destination := fs.String("ssh", "", "ssh destination, used verbatim in place of the site's ssh section (its user, host, port and keys are then ignored)")
	execute := fs.Bool("execute", false, "actually create and record")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("oidc client create: unexpected argument(s) %s", strings.Join(fs.Args(), " "))
	}
	if *appName == "" {
		return fmt.Errorf("oidc client create: --app is required")
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
		return fmt.Errorf("oidc client create: %s declares no app %q. Declared apps are %s", *configPath, *appName, strings.Join(cfg.AppNames(), ", "))
	}
	desired, ok := clientDesired(*appName, app, *rotate)
	if !ok {
		return fmt.Errorf("oidc client create: this toolkit does not know what a %s client looks like yet. Implemented kinds: mbin, uptime", app.Kind)
	}
	idp := pocketIDApp(cfg)
	if idp == "" {
		return fmt.Errorf("oidc client create: %s declares no pocket-id app to create the client in", *configPath)
	}
	where, err := pocketIDSite(cfg, idp, *site, "oidc client create")
	if err != nil {
		return err
	}
	// The calls need no root and a dry run reaches the host without sudo,
	// so only a run that will change something claims it, through sudo,
	// since the registry is root's.
	if *execute {
		if err := claimHosts(cfg, true, map[string]registry.Runner{where: registryHost(cfg.Sites[where], *destination, true)}); err != nil {
			return err
		}
	}

	if *secretsPath == "" {
		*secretsPath = filepath.Join(filepath.Dir(*configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(*secretsPath)
	if err != nil {
		return err
	}
	recipients, err := config.Recipients(filepath.Dir(*secretsPath))
	if err != nil {
		return err
	}
	// Refused before anything is planned, because the secret is written
	// before Pocket ID is sent it and a run that cannot write must not start.
	if secrets.Encrypted && len(recipients) == 0 {
		return fmt.Errorf("oidc client create: %s is encrypted, but no %s beside it names a recipient, so the client secret could not be written back encrypted. Nothing was changed", *secretsPath, config.SOPSConfigName)
	}
	key, _ := secrets.Apps[idp]["static_api_key"].(string)
	if key == "" {
		return fmt.Errorf("oidc client create: secrets apps.%s.static_api_key is empty. Run `paisans init` to generate it and `paisans apply` to render it into Pocket ID, then re-run", idp)
	}

	api := clientAPI(cfg, where, *destination, key)
	plan, err := planClient(api, desired, recordedClient(secrets, *appName))
	if err != nil {
		return fmt.Errorf("oidc client create: %w", err)
	}
	printClientPlan(*appName, idp, where, plan)
	if len(plan.Steps) == 0 {
		return nil
	}
	if !*execute {
		fmt.Fprintf(os.Stdout, "\nNothing was changed. Re-run with --execute to apply this.\n")
		return nil
	}

	rec := &secretsRecorder{app: *appName, path: *secretsPath, secrets: secrets, recipients: recipients}
	if err := oidcclient.Execute(plan, api, rec, secretsgen.ClientSecret); err != nil {
		return fmt.Errorf("oidc client create: %w", err)
	}
	if rec.wrote {
		fmt.Fprintf(os.Stdout, "\nrecorded oidc_clients.%s.client_id and oidc_clients.%s.client_secret\n", *appName, *appName)
		if len(recipients) == 0 {
			fmt.Fprintf(os.Stderr, "paisans: %s is PLAINTEXT, because no %s beside it names an age recipient.\n", *secretsPath, config.SOPSConfigName)
		}
	}
	fmt.Fprintf(os.Stdout, "\n%s's client is in place. `paisans apply --site <site> --execute` on each site running %s renders it.\n", *appName, *appName)
	return nil
}

// secretsRecorder writes a client's credentials into the secrets file,
// re-encrypted to the recipients beside it, as `secrets set` does.
type secretsRecorder struct {
	app        string
	path       string
	secrets    *config.Secrets
	recipients []string
	wrote      bool
}

func (r *secretsRecorder) Record(clientID, clientSecret string) error {
	// Written with no recipient, an encrypted file would come back as
	// plaintext. Callers refuse this before planning; this is the floor.
	if r.secrets.Encrypted && len(r.recipients) == 0 {
		return fmt.Errorf("%s is encrypted, but no %s beside it names a recipient, so the client's credentials could not be written back encrypted", r.path, config.SOPSConfigName)
	}
	if err := r.secrets.Set("oidc_clients."+r.app+".client_id", clientID); err != nil {
		return err
	}
	if err := r.secrets.Set("oidc_clients."+r.app+".client_secret", clientSecret); err != nil {
		return err
	}
	if err := config.WriteSecrets(r.path, r.secrets, r.recipients); err != nil {
		return err
	}
	r.wrote = true
	return nil
}

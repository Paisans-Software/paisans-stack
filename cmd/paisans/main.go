// Command paisans renders, checks and applies a paisans deployment
// declaration.
//
// validate and render touch nothing outside the working directory. init
// reads every site to choose a mesh subnet, and writes only local files.
// `host prepare`, `apply` and `storage init` reach a machine: each reads it to
// show what it would do, and changes nothing unless told to with --execute.
// `doctor` reaches every site to report what is stuck and changes nothing.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/dns"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/hostprep"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
	"github.com/paisans-software/paisans-stack/internal/storageadd"
	"github.com/paisans-software/paisans-stack/internal/ui"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

const usage = `paisans renders and checks a community stack declaration.

Usage:
  paisans validate [--config paisans.yaml]
  paisans init     [--config paisans.yaml] [--secrets secrets.enc.yaml] [--sudo=false]
  paisans render   [--config paisans.yaml] [--secrets secrets.enc.yaml] --out ./out
  paisans host prepare --site <name> [--config paisans.yaml] [--ssh <destination>]
               [--execute]
  paisans apply    --site <name> [--config paisans.yaml] [--secrets secrets.enc.yaml]
                   [--ssh <destination>] [--overwrite <path>]... [--recreate <stack>]...
                   [--min-free <size>] [--keep-images] [--execute]
  paisans site add <site> [--config paisans.yaml] [--secrets secrets.enc.yaml]
               [--execute]
  paisans site remove <site> [--config paisans.yaml] [--secrets secrets.enc.yaml]
               [--host-gone] [--delete-data] [--execute]
  paisans storage init --site <name> [--config paisans.yaml] [--secrets secrets.enc.yaml]
               [--ssh <destination>] [--execute]
  paisans storage add [--config paisans.yaml] [--secrets secrets.enc.yaml]
               [--change-replication] [--wait <duration>] [--stop-test] [--execute]
  paisans storage rotate-key --app <name> [--config paisans.yaml]
               [--secrets secrets.enc.yaml] [--execute]
  paisans prune    --site <name> [--config paisans.yaml] [--ssh <destination>] [--execute]
  paisans preflight --site <new site> [--config paisans.yaml]
  paisans failover test [--config paisans.yaml] [--execute]
  paisans doctor   [--config paisans.yaml] [--site <name>]... [--sudo]
  paisans dns init [--config paisans.yaml] [--secrets secrets.enc.yaml] [--execute]
  paisans dns prune [--config paisans.yaml] [--secrets secrets.enc.yaml] [--name <fqdn>]... [--execute]
  paisans secrets set <dotted.key> [--config paisans.yaml] [--secrets secrets.enc.yaml] < value
  paisans secrets prune [--config paisans.yaml] [--secrets secrets.enc.yaml] [--execute]
  paisans app admin create --app <pocket-id app> --username <u> --email <e>
               [--first-name <f>] [--last-name <l>] [--login-link]
               [--config paisans.yaml] [--secrets secrets.enc.yaml]
               [--site <name>] [--ssh <destination>] [--execute]
  paisans app remove <app> [--config paisans.yaml] [--secrets secrets.enc.yaml]
               [--delete-data] [--execute]
  paisans oidc client create --app <name> [--rotate-secret]
               [--config paisans.yaml] [--secrets secrets.enc.yaml]
               [--site <name>] [--ssh <destination>] [--execute]
  paisans ingress show  --app <name> [--config paisans.yaml]
  paisans ingress check --app <name> [--config paisans.yaml]

Commands:
  validate   Load the configuration and report every problem found.
  init       Give the configuration an id if it has none; until the
             deployment is on any site, reach every site and keep
             mesh.subnet only if it overlaps nothing there, else roll a
             random /24 that overlaps nothing and move each site's address
             into it; then generate the secrets it needs, filling in only
             what is missing, and say what is still owed from elsewhere.
  render     Validate, then write per site artifacts to a local directory.
  host       Take a blank host to the state apply assumes: Docker, the
             WireGuard tools, a firewall, and a watchdog on a data site.
             Installs only what is missing. Writes nothing without --execute.
  apply      Compare one site's rendered artifacts with what is on that host
             and show what would change. Writes nothing without --execute.
  site       add: join a new data site to the running cluster in seven gated
             stages: preflight, mesh, etcd (learners, then promoted), the
             Patroni replica, synchronous mode, HAProxy, and each existing
             replica's patroni.env, one replica restart at a time. Reads
             every site and plans only what differs, so a re-run resumes.
             Writes nothing without --execute.
             remove: take a site out of the running deployment in four
             gated stages: its data out (the Patroni leader switched to
             the Sync Standby, its Garage node out of the layout and the
             objects moved), out of the cluster (its etcd member, and the
             mesh, HAProxy and gateway routes on every remaining site, and
             each replica's patroni.env, one at a time; one data site out
             of two and a witness takes the witness out of etcd too),
             its host cleaned of everything provably this deployment's
             (a gateway's Caddy handed over to the host's owner when
             their sites rely on it), and its entry out of paisans.yaml.
             Refuses the only gateway, data or apps site, a site an app
             is pinned to, and an unhealthy cluster. --host-gone skips
             the host; --delete-data deletes its data after the site's
             name is typed at a terminal. Writes nothing without
             --execute.
  storage    init: provision object storage on a site: each app's key and
             bucket, and the layout when it is the only Garage site.
             add: join every site in storage.garage.sites into one Garage
             cluster, in gated stages: connect, layout, sync, provision,
             media routes, a smoke test; and, with --change-replication,
             Garage's reset for another replication factor. Exits at a
             stage waiting on Garage (status 75) and resumes on the next
             run. Both create only what is missing, and write nothing
             without --execute.
             rotate-key: replace one app's S3 key, in gated stages: the
             new pair into the secrets (the old one kept beside it), the
             new key imported and granted, the app applied alone on every
             site running it, a probe written, read and deleted with the
             new key, and only then the old key deleted from Garage and
             the secrets. Resumes from the secrets file. Writes nothing
             without --execute.
  prune      List one site's dangling Docker volumes, with size and top level
             entries, and say which carry this deployment's label.
             Removes those with --execute; every other volume is kept.
  preflight  The read only checks site add runs first, for the site being
             added and every site already running. Changes nothing.
  failover   test: switch the Patroni primary to another data site and
             back, checking the cluster and every app before and after
             each switch. Interrupts writes briefly, twice. Changes
             nothing without --execute.
  doctor     Reach every site, or the ones --site names, and report what is
             stuck: unreachable sites, etcd quorum and version, the Patroni
             leader and why a replica will not promote, containers that are
             down, Pocket ID's active instance, and clocks. Prints how to
             recover. Reaches hosts, changes nothing; exits 1 on any FAIL.
  dns        init: create the public DNS records the configuration implies,
             at the provider named by acme.provider. Creates only what is
             missing, never updates or deletes, and refuses if any record
             conflicts.
             prune: delete the A and AAAA records dns init created that the
             configuration no longer implies: only one carrying init's
             comment, at a name under community.domain or one the
             configuration names, pointing at a site's public address, and
             no longer wanted. Lists every other record init marked, with why
             it is kept, and lists the zone again to confirm each delete.
             Both write nothing without --execute.
  secrets    set: read one value from stdin and write it into the encrypted
             secrets file, printing only its name. For credentials issued
             elsewhere, so they never touch a terminal or an editor.
  app        admin create: make sure a user exists, is verified and is an
             administrator of Pocket ID, and is in every admin group the
             apps signing in through it read. There is no password: a
             created account gets a one-time login link, printed once, to
             register a passkey with. Writes nothing without --execute.
             remove: take an app that has left paisans.yaml off every
             host, once a whole apply on each site has marked its files
             left over: its containers and networks, its rendered files
             (one edited on the host is kept), its stack directory when
             nothing else is in it, and its Pocket ID client when the
             secrets file records that client's ID. --delete-data also
             deletes its database and role, its Garage bucket and keys,
             its named volumes and its data directories, after the app's
             name is typed at a terminal. Leaves its secrets and DNS
             records, and says so. Writes nothing without --execute.
  oidc       client create: create an app's client at the deployment's
             Pocket ID, with the groups the app reads, and record its ID and
             secret in the secrets file. The secret is never printed.
             Writes nothing without --execute. Mbin only, so far.
  ingress    show: for an app pinned to a monitor site, print what the web
             server in front of it must do (terminate TLS for its hostname,
             pass Host, set X-Forwarded-For and X-Forwarded-Proto), filled
             in for Caddy, nginx and Apache. From paisans.yaml alone.
             check: from this machine, check that the hostname resolves to
             the monitor, that /healthz answers with a valid certificate,
             that sign in redirects with an https callback, and that the
             published port is closed from outside. Reads only; exits 1 on
             any FAIL. With the toolkit's own Caddy in front (ingress mode
             paisans), only the first two apply.

host prepare, apply, prune, site add, site remove, storage init, storage
add, storage rotate-key, app admin create, app remove, oidc client create,
preflight, failover test, doctor and init are the only commands that reach a
host.
Each reads it to plan, and changes it only with --execute; preflight, doctor and
init have no --execute and never change it. With --execute, a command first
claims each host it writes to in /var/lib/paisans/registry.json, and refuses if
another deployment there holds this one's token, WireGuard interface or listen
port, or a mesh subnet overlapping this one's. apply and host prepare also
refuse when anything else on the host overlaps the mesh subnet. dns init and dns
prune reach no host, only the DNS provider's API, and change it only with
--execute. ingress check reaches no host over ssh and changes nothing: it
looks at a monitor's public hostname as any visitor could.
Everything else writes files locally and stops.

Every command takes -v or --verbose. By default each step is one line; with it,
the reasons, values, request bodies and command output behind each step show
too. A failure always prints in full, with or without it.
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
	case "site":
		switch {
		case len(os.Args) >= 3 && os.Args[2] == "add":
			err = runSiteAdd(os.Args[3:])
		case len(os.Args) >= 3 && os.Args[2] == "remove":
			err = runSiteRemove(os.Args[3:], os.Stdin, os.Stdout)
		default:
			fmt.Fprintf(os.Stderr, "paisans: site takes one subcommand, add or remove\n\n%s", usage)
			os.Exit(2)
		}
	case "secrets":
		err = runSecrets(os.Args[2:])
	case "app":
		err = runApp(os.Args[2:])
	case "oidc":
		err = runOIDC(os.Args[2:])
	case "ingress":
		err = runIngress(os.Args[2:])
	case "storage":
		switch {
		case len(os.Args) >= 3 && os.Args[2] == "init":
			err = runStorageInit(os.Args[3:])
		case len(os.Args) >= 3 && os.Args[2] == "add":
			err = runStorageAdd(os.Args[3:])
		case len(os.Args) >= 3 && os.Args[2] == "rotate-key":
			err = runStorageRotateKey(os.Args[3:])
		default:
			fmt.Fprintf(os.Stderr, "paisans: storage takes one subcommand, init, add or rotate-key\n\n%s", usage)
			os.Exit(2)
		}
	case "prune":
		err = runPrune(os.Args[2:])
	case "preflight":
		err = runPreflight(os.Args[2:])
	case "failover":
		err = runFailover(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "dns":
		switch {
		case len(os.Args) >= 3 && os.Args[2] == "init":
			err = runDNSInit(os.Args[3:])
		case len(os.Args) >= 3 && os.Args[2] == "prune":
			err = runDNSPrune(os.Args[3:])
		default:
			fmt.Fprintf(os.Stderr, "paisans: dns takes one subcommand, init or prune\n\n%s", usage)
			os.Exit(2)
		}
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "paisans: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "paisans: %v\n", err)
		// Not a failure: a gate is waiting on Garage, and the next run
		// resumes there. EX_TEMPFAIL (sysexits.h), "try again later", so a
		// script can tell it from one.
		if errors.Is(err, storageadd.ErrWaiting) {
			os.Exit(75)
		}
		os.Exit(1)
	}
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	reportFindings(r, *configPath, result)
	if len(result.Findings) == 0 {
		r.Result("%s: no problems found", *configPath)
	}
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
// It changes no host. It reads every site, to settle the mesh subnet before
// anything is deployed (see settleMesh), and that is all. Standing a
// deployment up is `apply`, and that is a separate decision from having
// credentials to stand it up with.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets file (default: secrets.enc.yaml beside the config)")
	sudo := fs.Bool("sudo", true, "read each site through sudo, since the host registry is root's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	// The id comes first: everything a host holds is named from it, and a
	// declaration without one cannot even be loaded. One that exists is never
	// replaced.
	id, added, err := config.EnsureID(*configPath)
	if err != nil {
		return err
	}
	if added {
		s := r.Step("write deployment id")
		s.Detail("%s: wrote deployment id %s. It never changes; every name and path this deployment holds on a host is derived from it.", *configPath, id)
		s.Done("")
	}
	cfg, err := config.LoadForInit(*configPath)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	reportFindings(r, *configPath, result)
	if result.Refused() {
		return fmt.Errorf("%s was refused: %d problem(s) above. Secrets are not generated for a configuration that cannot be deployed", *configPath, len(result.Refusals()))
	}
	// The mesh subnet next, while nothing is deployed: it is the one value
	// that has to be checked against every host before the first apply,
	// and cannot change after it.
	wrote, err := settleMesh(r, cfg, *configPath, initHosts(cfg, *sudo), meshRandom)
	if err != nil {
		return err
	}
	if wrote {
		if cfg, err = config.Load(*configPath); err != nil {
			return err
		}
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
	warnOrphans(r, cfg, secrets)

	filled, err := secretsgen.Fill(cfg, secrets)
	if err != nil {
		return err
	}

	recipients, err := config.Recipients(filepath.Dir(*secretsPath))
	if err != nil {
		return err
	}

	if !filled.Changed() {
		r.Detail("%s already has every generated secret (%d)", *secretsPath, len(filled.Kept))
		reportOwed(r, filled)
		r.Result("Every generated secret is already present. Nothing written.")
		return nil
	}

	// Names, never values. A secret printed to a terminal is in a scrollback
	// buffer, and often in a multiplexer's log as well.
	write := r.Step("write secrets")
	write.Detail("%s", *secretsPath)
	for _, name := range filled.Generated {
		write.Detail("+ %s", name)
	}
	if err := config.WriteSecrets(*secretsPath, secrets, recipients); err != nil {
		write.Fail(err)
		return err
	}
	write.Done(fmt.Sprintf("%d generated, %d kept", len(filled.Generated), len(filled.Kept)))
	if len(recipients) == 0 {
		warnPlaintext(r, *secretsPath)
	} else {
		r.Detail("Encrypted to %d age recipient(s) from %s.", len(recipients), config.SOPSConfigName)
	}
	reportOwed(r, filled)
	r.Result("Generated %s, kept %d.", plural(len(filled.Generated), "secret"), len(filled.Kept))
	return nil
}

// reportOwed warns of what the toolkit will not invent. Leaving these silent
// would let an operator believe an install is finished when sign in and
// certificates are both still missing.
func reportOwed(r ui.Reporter, filled secretsgen.Result) {
	for _, owed := range filled.Owed {
		r.Warn(owed.Name+" needs a decision", owed.Why)
	}
}

func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the sops encrypted secrets (default: secrets.enc.yaml beside the config)")
	out := fs.String("out", "", "directory to write artifacts into")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	if *out == "" {
		return fmt.Errorf("render: --out is required. Artifacts are written to a local directory and pushed by a later step")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	reportFindings(r, *configPath, result)
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
		warnUnencrypted(r, *secretsPath)
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
	r.Result("Rendered %s to %s.", plural(len(plan.Files), "file"), *out)
	return nil
}

// runApply compares one site against what is rendered for it, and changes
// nothing unless told to.
//
// A dry run by default is not politeness. This is the only command that
// reaches a machine, the machine it reaches is running a community, and the
// difference between "show me" and "do it" should be a flag an operator typed
// rather than a habit they formed.
//
// An app that signs in through Pocket ID has its client ensured first, by
// the identity step in clients.go: declaring the app is the approval for
// its client, so `--execute` creates and records it like any other secret.
func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets (default: secrets.enc.yaml beside the config)")
	site := fs.String("site", "", "the site to apply, by the name it has in the configuration")
	destination := fs.String("ssh", "", "ssh destination, used verbatim in place of the site's ssh section (its user, host, port and keys are then ignored)")
	execute := fs.Bool("execute", false, "actually write files and restart services")
	var overwrite pathList
	fs.Var(&overwrite, "overwrite", "replace this conflicting file although it differs from the last apply's record (repeatable)")
	var recreate pathList
	var only pathList
	fs.Var(&only, "only", "apply only this stack's files and actions, and leave the rest of the site as it is (repeatable)")
	fs.Var(&recreate, "recreate", "replace every container of this stack with `up -d --force-recreate`, even if nothing changed (repeatable)")
	minFree := fs.String("min-free", "3G", "free space Docker's data root must have before a stack pulls an image, Eg: 2G")
	keepImages := fs.Bool("keep-images", false, "leave the images this apply supersedes on the host, Eg: to keep one to roll back to")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since /srv and /etc are not the deploy user's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	needFree, err := apply.ParseSize(*minFree)
	if err != nil {
		return fmt.Errorf("apply: --min-free: %w", err)
	}
	if *site == "" {
		return fmt.Errorf("apply: --site is required. A site at a time is deliberate: a staged change that half succeeds across three machines is worse than one that failed on one")
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
	declared, ok := cfg.Sites[*site]
	if !ok {
		return fmt.Errorf("apply: %s declares no site %q. Declared sites are %s", *configPath, *site, strings.Join(cfg.SiteNames(), ", "))
	}

	if *secretsPath == "" {
		*secretsPath = filepath.Join(filepath.Dir(*configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(*secretsPath)
	if err != nil {
		return err
	}
	if !secrets.Encrypted {
		warnUnencrypted(r, *secretsPath)
	}
	warnOrphans(r, cfg, secrets)
	// apply does not call secretsgen.Fill either, and this is the path that
	// actually reaches a host: a malformed key has to stop here, not just
	// print a confusing failure partway through provisioning on the machine.
	if err := secretsgen.CheckGarageKeys(cfg, secrets); err != nil {
		return err
	}
	if err := requireACMEToken(cfg, secrets, []string{*site}); err != nil {
		return err
	}

	transport := siteTransport(*site, declared, *destination, *sudo)
	r.Section(fmt.Sprintf("%s (%s)", *site, transport.Describe()))
	done := r.Step("check mesh subnet")
	err = checkMeshLive(cfg, *site, transport)
	if err != nil {
		done.Fail(err)
	} else {
		// Only once it is true: a check that could not run found nothing.
		done.Detail("nothing on %s overlaps the mesh subnet", *site)
		done.Done("")
	}
	if err != nil {
		return err
	}
	host, err := hostGate(r, cfg, *site, transport)
	if err != nil {
		return err
	}
	if err := claimHosts(r, cfg, *execute, map[string]registry.Runner{*site: transport}); err != nil {
		return err
	}

	// The identity step: every app this site starts that signs in through
	// Pocket ID gets its client ensured before it renders. Planned here,
	// read only, so the dry run shows it and an app it must hold back is
	// left out of the plan below.
	clients, err := newClientStep(r, cfg, *site, *destination, *secretsPath, secrets, only)
	if err != nil {
		return err
	}
	options := []apply.Option{apply.Overwrite(overwrite...), apply.Recreate(recreate...), apply.MinFree(needFree), apply.Only(only...)}
	// On a shared host an image the toolkit renders (caddy, postgres) may
	// be what a foreign project runs from, so none is removed.
	if *keepImages || host.Shared() {
		options = append(options, apply.KeepImages())
	}
	// planFor plans the site holding back the named app stacks, as a later
	// pass after the done ones. See executeWithClients. The reporter goes to
	// Build, so planning announces what it reads from the host as Execute
	// announces what it does there. Only the first plan reports its reads: a
	// later pass reads the same host again, and saying so twice is noise.
	// Every plan reports its own Execute.
	reads := r
	planFor := func(hold []string, done []*apply.Plan) (*apply.Plan, error) {
		p, err := planSiteApply(cfg, secrets, *site, transport, slices.Concat(options, []apply.Option{apply.Except(hold...), apply.After(done...), apply.Report(reads)})...)
		reads = ui.Discard
		if err != nil {
			return nil, err
		}
		p.Report = r
		return p, nil
	}
	plan, err := planWithClients(clients, func(hold []string) (*apply.Plan, error) { return planFor(hold, nil) }, *execute)
	if err != nil {
		return err
	}

	// A site running etcd, or configured to, is checked against the live
	// membership. Only this site is asked unless it is a configured member,
	// so applying a site with nothing to do with etcd reaches no other host.
	transports := map[string]apply.Transport{*site: transport}
	if contains(cfg.Etcd.Members, *site) {
		for _, name := range cfg.Etcd.Members {
			if name != *site {
				transports[name] = siteTransport(name, cfg.Sites[name], "", *sudo)
			}
		}
	}

	// A member being founded is the one case where the order sites are
	// applied in matters: the witness first, and no wait for a primary while
	// another founding member has not started. See apply.WitnessFirstRefusal.
	founding := false
	var running map[string]bool
	if contains(cfg.Etcd.Members, *site) {
		_, recorded, err := apply.ReadEtcdInitial(transport, cfg.Deployment())
		if err != nil {
			return err
		}
		founding = !recorded
	}
	if founding {
		if running, err = apply.EtcdRunning(cfg, transports); err != nil {
			return err
		}
		if plan.Bootstrap != nil {
			plan.Bootstrap.EtcdUnstarted = apply.FoundingUnstarted(cfg, *site, running)
		}
	}
	// A dry run is the plan. --execute reports progress instead, and shows
	// the plan first only with --verbose, since its steps say the same.
	// Either way the plan's notes show, and a later pass's new ones too.
	noted := map[string]bool{}
	presentPlan(r, plan, *execute, noted)
	if err := printLeftovers(r, cfg, *site, host.Inventory, plan); err != nil {
		return err
	}

	if err := apply.EtcdGates(cfg, plan, transports, founding, running); err != nil {
		return err
	}

	if !*execute {
		err := clients.result()
		if len(plan.Writes()) == 0 && len(plan.Actions) == 0 && plan.WireGuard == apply.WireGuardNone && (clients == nil || clients.steps == 0) {
			r.Result("%s is up to date. Nothing to apply.", *site)
			return err
		}
		r.Result("Nothing changed. Re-run with --execute to apply.")
		return err
	}

	plans := []*apply.Plan{plan}
	if clients == nil {
		if err := apply.Execute(plan, transport); err != nil {
			return err
		}
	} else {
		pass := sitePass{
			plan: func(hold []string, done []*apply.Plan) (*apply.Plan, error) {
				p, err := planFor(hold, done)
				if err != nil {
					return nil, err
				}
				if plan.Bootstrap != nil && p.Bootstrap != nil {
					p.Bootstrap.EtcdUnstarted = plan.Bootstrap.EtcdUnstarted
				}
				return p, nil
			},
			execute: func(p *apply.Plan) error { return apply.Execute(p, transport) },
			notes:   func(p *apply.Plan) { reportNotes(r, p, noted) },
		}
		if plans, err = executeWithClients(clients, pass); err != nil {
			return err
		}
	}
	// The check after the apply looks at what every pass acted on.
	acted := &apply.Plan{}
	written := 0
	for _, p := range plans {
		acted.Actions = append(acted.Actions, p.Actions...)
		written += len(p.Writes())
	}
	if err := checkStandby(r, cfg, acted, *site, transport, func(name string) apply.Transport {
		return siteTransport(name, cfg.Sites[name], "", *sudo)
	}); err != nil {
		return err
	}
	err = clients.result()
	r.Result("Applied %s to %s.", plural(written, "file"), *site)
	return err
}

// checkStandby is the deployment level gate after an apply that acted on a
// Pocket ID stack running on more than one site: every stack passed its own
// gate, active or standing by, and this asks every site that exactly one is
// active. The applied site is reached as the apply reached it, the others
// through their ssh sections. See apply.CheckOneActive.
func checkStandby(r ui.Reporter, cfg *config.Config, plan *apply.Plan, site string, transport apply.Transport, other func(string) apply.Transport) error {
	acted := map[string]bool{}
	for _, action := range plan.Actions {
		acted[action.Stack] = true
	}
	sites := render.AppSites(cfg)
	for _, app := range apply.StandbyApps(cfg) {
		if !acted[app] {
			continue
		}
		transports := map[string]apply.Transport{}
		for _, name := range sites[app] {
			if name == site {
				transports[name] = transport
			} else {
				transports[name] = other(name)
			}
		}
		// The sites' states are the check's detail on success. On failure
		// the table is the evidence the error points at ("each site
		// above"), so it is shown whatever the verbosity.
		s := r.Step("check one pocket-id " + app + " is active")
		var seen bytes.Buffer
		if err := apply.CheckOneActive(cfg, app, transports, &seen); err != nil {
			s.Fail(err)
			r.Refuse("pocket-id "+app+" instances", seen.String())
			return fmt.Errorf("%w. The apply itself finished; this is the check after it", err)
		}
		s.Detail("%s", strings.TrimRight(seen.String(), "\n"))
		s.Done("")
	}
	return nil
}

// planSiteApply is everything apply decides for one site, without doing any
// of it: the etcd flags the site's member was born with, the render, the plan
// and the databases it bootstraps. `storage rotate-key` switches an app with
// exactly this, scoped by apply.Only, so a rotation's switch is the apply an
// operator would have typed rather than a second path to the same host.
func planSiteApply(cfg *config.Config, secrets *config.Secrets, site string, transport apply.Transport, options ...apply.Option) (*apply.Plan, error) {
	// An etcd member keeps the flags it was born with, read from its host,
	// so that etcd.members growing never changes a running member's compose
	// file. See render.EtcdInitialPath.
	var renderOptions []render.Option
	if contains(cfg.Etcd.Members, site) {
		initial, found, err := apply.ReadEtcdInitial(transport, cfg.Deployment())
		if err != nil {
			return nil, err
		}
		if found {
			renderOptions = append(renderOptions, render.WithEtcdInitial(site, initial))
		}
	}
	rendered, err := render.Build(cfg, secrets, renderOptions...)
	if err != nil {
		return nil, err
	}
	options = append(options, apply.DatabaseApps(apply.ClusterDatabaseApps(cfg)...))
	plan, err := apply.Build(site, rendered, acme.Module(cfg.ACME.Provider), transport, options...)
	if err != nil {
		return nil, err
	}
	databases, err := apply.Databases(cfg, secrets, site)
	if err != nil {
		return nil, err
	}
	plan.WithDatabases(databases)
	return plan, nil
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
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets (default: secrets.enc.yaml beside the config)")
	site := fs.String("site", "", "the site to provision, by the name it has in the configuration")
	destination := fs.String("ssh", "", "ssh destination, used verbatim in place of the site's ssh section (its user, host, port and keys are then ignored)")
	execute := fs.Bool("execute", false, "actually create what is missing")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since /srv and /etc are not the deploy user's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	if *site == "" {
		return fmt.Errorf("storage init: --site is required. A site at a time is deliberate, the same reason apply takes one")
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
	declared, ok := cfg.Sites[*site]
	if !ok {
		return fmt.Errorf("storage init: %s declares no site %q. Declared sites are %s", *configPath, *site, strings.Join(cfg.SiteNames(), ", "))
	}

	if *secretsPath == "" {
		*secretsPath = filepath.Join(filepath.Dir(*configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(*secretsPath)
	if err != nil {
		return err
	}
	if !secrets.Encrypted {
		warnUnencrypted(r, *secretsPath)
	}
	// A malformed key has to stop here, before it reaches `garage key import`
	// partway through provisioning: earlier keys in the same run would
	// already be imported and cannot be imported again.
	if err := secretsgen.CheckGarageKeys(cfg, secrets); err != nil {
		return err
	}

	transport := siteTransport(*site, declared, *destination, *sudo)
	r.Section(fmt.Sprintf("%s (%s)", *site, transport.Describe()))
	if err := claimHosts(r, cfg, *execute, map[string]registry.Runner{*site: transport}); err != nil {
		return err
	}
	plan, err := garage.Build(*site, cfg, secrets, transport)
	if err != nil {
		return err
	}
	// A dry run is the plan. --execute reports progress instead, and shows
	// the plan first only with --verbose, since its steps say the same.
	if !*execute || r.Verbose() {
		plan.Show(r)
	}

	if !*execute {
		if len(plan.Steps) == 0 {
			r.Result("%s is provisioned. Nothing to do.", *site)
			return nil
		}
		r.Result("Nothing changed. Re-run with --execute to apply.")
		return nil
	}
	if err := garage.Report(plan, transport, r); err != nil {
		return err
	}
	r.Result("Provisioned %s on %s.", plural(len(plan.Steps), "step"), *site)
	return nil
}

// runHostPrepare takes one site's host to the state apply assumes. It is
// modelled on runStorageInit: probe read only, print, and change the host only
// with --execute. It needs no secrets; nothing it installs is a credential.
func runHostPrepare(args []string) error {
	fs := flag.NewFlagSet("host prepare", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	site := fs.String("site", "", "the site to prepare, by the name it has in the configuration")
	destination := fs.String("ssh", "", "ssh destination, used verbatim in place of the site's ssh section (its user, host, port and keys are then ignored)")
	execute := fs.Bool("execute", false, "actually install and configure what is missing")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since packages, the firewall and kernel modules are root's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	if *site == "" {
		return fmt.Errorf("host prepare: --site is required. A site at a time is deliberate, the same reason apply takes one")
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
	declared, ok := cfg.Sites[*site]
	if !ok {
		return fmt.Errorf("host prepare: %s declares no site %q. Declared sites are %s", *configPath, *site, strings.Join(cfg.SiteNames(), ", "))
	}

	transport := siteTransport(*site, declared, *destination, *sudo)
	r.Section(fmt.Sprintf("%s (%s)", *site, transport.Describe()))
	done := r.Step("check mesh subnet")
	err = checkMeshLive(cfg, *site, transport)
	if err != nil {
		done.Fail(err)
		return err
	}
	// Only once it is true: a check that could not run found nothing.
	done.Detail("nothing on %s overlaps the mesh subnet", *site)
	done.Done("")
	host, err := hostGate(r, cfg, *site, transport)
	if err != nil {
		return err
	}
	if err := claimHosts(r, cfg, *execute, map[string]registry.Runner{*site: transport}); err != nil {
		return err
	}
	var options []hostprep.Option
	if host.Shared() {
		options = append(options, hostprep.Shared())
	}
	plan, err := hostprep.Build(*site, cfg, transport, options...)
	if err != nil {
		return err
	}
	// A dry run is the plan. --execute reports progress instead, and shows
	// the plan first only with --verbose, since its steps say the same.
	if !*execute || r.Verbose() {
		plan.Show(r)
	} else {
		plan.Warn(r)
	}

	if !*execute {
		if len(plan.Steps) == 0 {
			r.Result("%s is prepared. Nothing to do.", *site)
			return nil
		}
		r.Result("Nothing changed. Re-run with --execute to apply.")
		return nil
	}
	if err := hostprep.Execute(plan, transport, r); err != nil {
		return err
	}
	r.Result("Prepared %s.", *site)
	return nil
}

// runDNSInit creates the public DNS records a deployment needs, at the
// provider acme.provider names, using the token that already answers ACME
// challenges. It is modelled on runStorageInit: a dry run by default, and
// only what is missing is created.
//
// It reaches no host. The workstation talks to the provider's API and to
// nothing else, so it takes no --site and no --ssh.
func runDNSInit(args []string) error {
	r, cfg, wants, provider, execute, err := dnsSetup("dns init", "actually create the missing records", args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	plan, err := dns.Build(ctx, provider, cfg.Deployment(), wants)
	if err != nil {
		return err
	}
	// A dry run is the plan. --execute reports progress instead, and shows
	// the plan first only with --verbose, since its steps say the same.
	if !execute || r.Verbose() || len(plan.Conflicts()) > 0 {
		plan.Show(r)
	}

	if !execute {
		if n := len(plan.Conflicts()); n > 0 {
			r.Result("%s conflict with the provider's. Resolve %s before --execute, which creates nothing while one stands.", plural(n, "record"), map[bool]string{true: "it", false: "them"}[n == 1])
			return nil
		}
		if len(plan.Creates()) == 0 {
			r.Result("Every record is present. Nothing to create.")
			return nil
		}
		r.Result("Nothing changed. Re-run with --execute to apply.")
		return nil
	}
	if err := dns.Execute(ctx, provider, plan, r); err != nil {
		return err
	}
	r.Result("Created %s at %s.", plural(len(plan.Creates()), "record"), plan.Provider)
	return nil
}

// runDNSPrune deletes the address records dns init created that the
// configuration no longer implies, under the rules internal/dns/prune.go
// states. A dry run by default, like dns init, and it reaches no host.
func runDNSPrune(args []string) error {
	var vouched pathList
	r, cfg, wants, provider, execute, err := dnsSetup("dns prune", "actually delete the records listed as remove", args, func(fs *flag.FlagSet) {
		fs.Var(&vouched, "name", "vouch that this exact name, outside community.domain and no longer configured, was this deployment's (repeatable)")
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	plan, err := dns.BuildPrune(ctx, provider, cfg, wants, vouched...)
	if err != nil {
		return err
	}
	if !execute || r.Verbose() {
		plan.Show(r)
	}

	if !execute {
		if len(plan.Removes()) == 0 {
			r.Result("No record to delete.")
			return nil
		}
		r.Result("Nothing changed. Re-run with --execute to apply.")
		return nil
	}
	if err := dns.ExecutePrune(ctx, provider, plan, r); err != nil {
		return err
	}
	r.Result("Deleted %s at %s.", plural(len(plan.Removes()), "record"), plan.Provider)
	return nil
}

// dnsSetup is what both dns subcommands do before contacting the provider:
// validate, derive the wanted records, then open the secrets. The records
// are worked out before the secrets are opened or the provider is contacted,
// so a configuration that cannot name its records is refused offline.
func dnsSetup(name, executeHelp string, args []string, extra ...func(*flag.FlagSet)) (ui.Reporter, *config.Config, []dns.Want, dns.Provider, bool, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	reporter := commonFlags(fs)
	for _, add := range extra {
		add(fs)
	}
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets (default: secrets.enc.yaml beside the config)")
	execute := fs.Bool("execute", false, executeHelp)
	if err := fs.Parse(args); err != nil {
		return nil, nil, nil, nil, false, err
	}
	r := reporter()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return nil, nil, nil, nil, false, err
	}
	result := validate.Check(cfg)
	reportFindings(r, *configPath, result)
	if result.Refused() {
		return nil, nil, nil, nil, false, fmt.Errorf("%s was refused: %d problem(s) above", *configPath, len(result.Refusals()))
	}
	wants, err := dns.Desired(cfg)
	if err != nil {
		return nil, nil, nil, nil, false, err
	}

	if *secretsPath == "" {
		*secretsPath = filepath.Join(filepath.Dir(*configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(*secretsPath)
	if err != nil {
		return nil, nil, nil, nil, false, err
	}
	if !secrets.Encrypted {
		warnUnencrypted(r, *secretsPath)
	}
	provider, err := dns.For(cfg.ACME.Provider, secrets.External["acme_dns_token"])
	if err != nil {
		return nil, nil, nil, nil, false, err
	}
	return r, cfg, wants, provider, *execute, nil
}

// warnUnencrypted is the warning every command that opens the secrets gives
// for a file that is not under sops. The file is accepted, since fixtures and
// examples are plain, so it is a warning and not a refusal.
func warnUnencrypted(r ui.Reporter, path string) {
	r.Warn(path+" is not encrypted", "That is accepted for fixtures and examples; a real deployment keeps its secrets under sops.")
}

// pathList collects a repeatable flag. --overwrite takes one path each time it
// is given, so that every file replaced against its record was named on its
// own: a pattern or a blanket switch would let one decision cover files the
// operator never looked at. --recreate takes one stack each time, for the same
// reason.
type pathList []string

func (l *pathList) String() string { return strings.Join(*l, ",") }

func (l *pathList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// siteTransport is how a command reaches a site: its ssh section, or the
// --ssh override verbatim. The override replaces the whole section rather
// than one part of it, so what is used is always either everything the file
// says or exactly what the operator typed, never a blend of the two.
//
// name is the site's name in paisans.yaml. The transport carries it so that
// a sudo password prompt names the site it is for; it plays no part in
// reaching the host.
func siteTransport(name string, site config.Site, override string, sudo bool) apply.SSHTransport {
	var t apply.SSHTransport
	if override != "" {
		t = apply.SSHTransport{Site: name, Destination: override, Sudo: sudo}
	} else {
		// validate has already refused a bad key, so problems are empty here.
		keys, _ := site.SSH.Keys()
		lines := make([]string, len(keys))
		for i, k := range keys {
			lines[i] = k.Line
		}
		t = apply.SSHTransport{Site: name, User: site.SSH.User, Host: site.SSHHost(), Port: site.SSH.PortOrDefault(), PublicKeys: lines, Sudo: sudo}
	}
	if sudo {
		t.Auth = sudoAuthFor(t.Describe())
	}
	return t
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

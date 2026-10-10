package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/ui"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// runPrune lists a site's dangling volumes and, with --execute, removes the
// ones a paisans container left behind. It is modelled on runHostPrepare: one
// site, a dry run by default, and the plan reported before anything happens.
//
// It exists for what accumulated before apply refused undeclared volumes:
// a real apps site had 18 anonymous volumes under Mbin's /app/var/, about
// 830 MB. `docker volume prune` was rejected because it removes every
// unused volume whoever made it, with no list first, and on Docker 23 and
// later only anonymous ones unless given --all, which is two different
// surprises depending on the engine (Docker Engine 23.0 release notes).
func runPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	site := fs.String("site", "", "the site to prune, by the name it has in the configuration")
	destination := fs.String("ssh", "", "ssh destination, used verbatim in place of the site's ssh section (its user, host, port and keys are then ignored)")
	execute := fs.Bool("execute", false, "actually remove the volumes marked remove")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since Docker's volume directories are root's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	if *site == "" {
		return fmt.Errorf("prune: --site is required. A site at a time is deliberate, the same reason apply --site takes one")
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
		return fmt.Errorf("prune: %s declares no site %q. Declared sites are %s", *configPath, *site, strings.Join(cfg.SiteNames(), ", "))
	}

	transport := siteTransport(*site, declared, *destination, *sudo)
	r.Section(fmt.Sprintf("%s (%s)", *site, transport.Describe()))
	if _, err := hostGate(r, cfg, *site, transport); err != nil {
		return err
	}
	if err := claimHosts(r, cfg, *execute, map[string]registry.Runner{*site: transport}); err != nil {
		return err
	}
	plan, err := apply.BuildVolumePrune(cfg.Deployment(), *site, transport)
	if err != nil {
		return err
	}
	// A dry run is the plan. --execute reports the removal alone, and shows
	// the plan first only with --verbose.
	if !*execute || r.Verbose() {
		showVolumePrune(r, plan)
	}
	removals := plan.Removals()
	if !*execute {
		if len(removals) == 0 {
			r.Result("No dangling volume to remove.")
			return nil
		}
		r.Result("Nothing changed. Re-run with --execute to apply.")
		return nil
	}
	if len(removals) == 0 {
		r.Result("No dangling volume to remove.")
		return nil
	}
	s := r.Step("remove volumes")
	s.Detail("from %s", plan.Transport)
	if err := apply.ExecuteVolumePrune(plan, transport); err != nil {
		s.Fail(err)
		return err
	}
	s.Done(plural(len(removals), "volume"))
	r.Result("Removed %s from %s.", plural(len(removals), "volume"), *site)
	return nil
}

// showVolumePrune reports the plan: each volume it would remove as an item,
// and every verdict, kept or removed, with its size, contents and reason as
// details, since the reason is what an operator reads before agreeing.
func showVolumePrune(r ui.Reporter, plan *apply.VolumePrune) {
	r.Detail("%s", apply.PruneHeader)
	if len(plan.Volumes) == 0 {
		r.Detail("no dangling volumes")
		return
	}
	var total int64
	for _, v := range plan.Volumes {
		verb := "keep"
		if v.Remove {
			verb = "remove"
			total += v.Size
			// Only an anonymous volume's name is shortened, since it is 64
			// hex characters whose front tells one from another. A named
			// volume's name is what an operator recognises, and this list
			// is the consent to delete its data, so it is shown whole.
			name := v.Name
			if apply.IsAnonymousVolume(name) {
				name = name[:12]
			}
			r.Item("remove volume " + name)
		}
		entries := "empty"
		if len(v.Entries) > 0 {
			entries = strings.Join(v.Entries, " ")
		}
		r.Detail("%-9s %s  %s  [%s]\n    %s", verb, v.Name, apply.FormatSize(v.Size), entries, v.Reason)
	}
	r.Detail("%-9s %d of %d dangling volume(s), %s", "total", len(plan.Removals()), len(plan.Volumes), apply.FormatSize(total))
}

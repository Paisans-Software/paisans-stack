package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// runPrune lists a site's dangling volumes and, with --execute, removes the
// ones a paisans container left behind. It is modelled on runHostPrepare: one
// site, a dry run by default, and the plan printed before anything happens.
//
// It exists for what accumulated before apply refused undeclared volumes:
// a real apps site had 18 anonymous volumes under Mbin's /app/var/, about
// 830 MB. `docker volume prune` was rejected because it removes every
// unused volume whoever made it, with no list first, and on Docker 23 and
// later only anonymous ones unless given --all, which is two different
// surprises depending on the engine (Docker Engine 23.0 release notes).
func runPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	site := fs.String("site", "", "the site to prune, by the name it has in the configuration")
	destination := fs.String("ssh", "", "ssh destination, used verbatim in place of the site's ssh section (its user, host, port and keys are then ignored)")
	execute := fs.Bool("execute", false, "actually remove the volumes marked remove")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo, since Docker's volume directories are root's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *site == "" {
		return fmt.Errorf("prune: --site is required. A site at a time is deliberate, the same reason apply takes one")
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
		return fmt.Errorf("prune: %s declares no site %q. Declared sites are %s", *configPath, *site, strings.Join(cfg.SiteNames(), ", "))
	}

	transport := siteTransport(declared, *destination, *sudo)
	plan, err := apply.BuildVolumePrune(*site, transport)
	if err != nil {
		return err
	}
	printVolumePrune(os.Stdout, plan)
	removals := plan.Removals()
	if !*execute {
		if len(removals) > 0 {
			fmt.Fprintf(os.Stdout, "\nNothing was changed. Re-run with --execute to remove %d volume(s).\n", len(removals))
		}
		return nil
	}
	if err := apply.ExecuteVolumePrune(plan, transport); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\nremoved %d volume(s) from %s\n", len(removals), plan.Transport)
	return nil
}

func printVolumePrune(w io.Writer, plan *apply.VolumePrune) {
	fmt.Fprintf(w, "%s (%s)\n", plan.Site, plan.Transport)
	fmt.Fprintf(w, "  %s\n", apply.PruneHeader)
	if len(plan.Volumes) == 0 {
		fmt.Fprintf(w, "  no dangling volumes\n")
		return
	}
	var total int64
	for _, v := range plan.Volumes {
		verb := "keep"
		if v.Remove {
			verb = "remove"
			total += v.Size
		}
		entries := "empty"
		if len(v.Entries) > 0 {
			entries = strings.Join(v.Entries, " ")
		}
		fmt.Fprintf(w, "  %-9s %s  %s  [%s]\n      %s\n", verb, v.Name, apply.FormatSize(v.Size), entries, v.Reason)
	}
	fmt.Fprintf(w, "  %-9s %d of %d dangling volume(s), %s\n", "total", len(plan.Removals()), len(plan.Volumes), apply.FormatSize(total))
}

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/preflight"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// runPreflight is stage 1 of `site add` on its own: every check, printed,
// and nothing changed anywhere. It exists so an operator can fix a host and
// look again without reading a site add plan each time.
//
// It takes no --ssh: it reaches every site, and one override cannot describe
// several destinations.
func runPreflight(args []string) error {
	fs := flag.NewFlagSet("preflight", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	site := fs.String("site", "", "the site being added, by the name it has in the configuration")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	_ = r
	if *site == "" {
		return fmt.Errorf("preflight: --site is required: the site being added")
	}
	cfg, err := loadChecked(*configPath)
	if err != nil {
		return err
	}
	if _, ok := cfg.Sites[*site]; !ok {
		return fmt.Errorf("preflight: %s declares no site %q. Declared sites are %s", *configPath, *site, strings.Join(cfg.SiteNames(), ", "))
	}
	report, err := preflight.Run(cfg, *site, allSiteTransports(cfg, *sudo))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "preflight for adding %s\n", *site)
	report.Print(os.Stdout)
	if report.Refused() {
		return fmt.Errorf("preflight refused adding %s: fix what is marked REFUSED and run it again", *site)
	}
	return nil
}

// loadChecked loads the configuration and refuses one validate refuses.
func loadChecked(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	result := validate.Check(cfg)
	report(os.Stderr, path, result)
	if result.Refused() {
		return nil, fmt.Errorf("%s was refused: %d problem(s) above", path, len(result.Refusals()))
	}
	return cfg, nil
}

// allSiteTransports reaches every declared site through its ssh section.
func allSiteTransports(cfg *config.Config, sudo bool) map[string]apply.Transport {
	out := map[string]apply.Transport{}
	for _, name := range cfg.SiteNames() {
		out[name] = siteTransport(name, cfg.Sites[name], "", sudo)
	}
	return out
}

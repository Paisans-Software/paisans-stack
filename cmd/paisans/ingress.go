package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/ingress"
)

// ingressProbes is how `ingress check` reaches DNS and the web server. Tests
// replace it.
var ingressProbes = ingress.DefaultProbes

// runIngress dispatches `paisans ingress show` and `paisans ingress check`.
// Neither reaches a host over ssh nor changes anything: show reads
// paisans.yaml alone, and check looks at the monitor from this machine as a
// visitor would.
func runIngress(args []string) error {
	if len(args) < 1 || (args[0] != "show" && args[0] != "check") {
		return fmt.Errorf("ingress takes one subcommand, show or check: paisans ingress show --app <name>")
	}
	sub := args[0]
	fs := flag.NewFlagSet("ingress "+sub, flag.ContinueOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	appName := fs.String("app", "", "the app pinned to a monitor site, by the name it has in the configuration")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	r := reporter()
	if fs.NArg() > 0 {
		return fmt.Errorf("ingress %s: unexpected argument(s) %s", sub, strings.Join(fs.Args(), " "))
	}
	if *appName == "" {
		return fmt.Errorf("ingress %s: --app is required", sub)
	}
	cfg, err := loadChecked(r, *configPath)
	if err != nil {
		return err
	}
	target, err := ingress.For(cfg, *appName)
	if err != nil {
		return err
	}
	if sub == "show" {
		// The sheet is the command's product, to be read and copied into a
		// web server's configuration, so it goes to stdout whole and not
		// through the reporter, which hides its details by default.
		ingress.Show(os.Stdout, target)
		return nil
	}
	r.Section(fmt.Sprintf("ingress %s at %s", target.App, target.Hostname))
	results := ingress.Check(context.Background(), target, ingressProbes())
	if ingress.Report(r, results) {
		return fmt.Errorf("ingress check for %s: fix what is marked FAIL and run it again", target.App)
	}
	r.Result("%s passed.", plural(len(results), "check"))
	return nil
}

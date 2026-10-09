package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/paisans-software/paisans-stack/internal/failover"
)

func runFailover(args []string) error {
	if len(args) < 1 || args[0] != "test" {
		return fmt.Errorf("failover takes one subcommand, test")
	}
	return runFailoverTest(args[1:])
}

// runFailoverTest moves the primary to another data site and back. It is a
// dry run by default like every command that reaches a host, and more
// deliberately so than most: each switch interrupts writes for everyone.
func runFailoverTest(args []string) error {
	fs := flag.NewFlagSet("failover test", flag.ExitOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	execute := fs.Bool("execute", false, "actually switch the primary over and back")
	sudo := fs.Bool("sudo", true, "run remote commands through sudo")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	cfg, err := loadChecked(r, *configPath)
	if err != nil {
		return err
	}
	// The switchover runs on the data sites, and the restart after it on
	// the apps sites.
	if err := claimSites(cfg, *execute, *sudo, union(cfg.Cluster.Sites, cfg.AppsSites())...); err != nil {
		return err
	}
	return failover.Run(cfg, failover.Options{
		Transports: allSiteTransports(cfg, *sudo),
		Out:        os.Stdout,
		Execute:    *execute,
	})
}

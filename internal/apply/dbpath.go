package apply

import (
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// ClusterDatabaseApps returns, sorted, the apps that reach the cluster's
// database through each apps site's HAProxy: cluster placement and a kind that
// uses Postgres, the same test Databases makes. A pinned app has its own
// Postgres and is not among them.
//
// These are the apps a database path change breaks. A real `site add`
// restarted HAProxy and Mbin answered 500 until it was recreated by hand: its
// FrankenPHP workers keep their connections open and never reconnect, and its
// healthcheck does not touch the database, so the container stayed healthy.
// Whatever in the toolkit restarts HAProxy or moves the primary therefore
// restarts these apps after it, and says so in its plan.
func ClusterDatabaseApps(cfg *config.Config) []string {
	var out []string
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		if app.Placement.Mode == config.PlacementCluster && kinds.UsesPostgres(app.Kind) {
			out = append(out, name)
		}
	}
	return out
}

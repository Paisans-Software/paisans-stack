package render

import "github.com/paisans-software/paisans-stack/internal/config"

// The values below are what the rendered files say, for a command that reads
// a host and must compare against the same numbers. They go through the
// planner rather than repeating its defaults, so a changed default cannot
// leave preflight checking a number nothing renders.

// HeartbeatMS is the etcd heartbeat interval the rendered compose file sets.
func HeartbeatMS(cfg *config.Config) int { return (&planner{cfg: cfg}).heartbeatMS() }

// ElectionTimeoutMS is the etcd election timeout the rendered compose file
// sets.
func ElectionTimeoutMS(cfg *config.Config) int { return (&planner{cfg: cfg}).electionTimeoutMS() }

// ClusterPort is where a site's HAProxy listens for clustered applications.
func ClusterPort(cfg *config.Config) int { return (&planner{cfg: cfg}).clusterPort() }

// PostgresPort is Patroni's Postgres port on a data site.
const PostgresPort = postgresPort

// BgMonPort is where Spilo's bg_mon listens on a data site.
const BgMonPort = bgMonPort

// RunsHAProxy reports whether a site runs HAProxy: it holds the apps role and
// at least one app is clustered, the same condition placeApps sets
// NeedsProxy on.
func RunsHAProxy(cfg *config.Config, site string) bool {
	if !cfg.Sites[site].Has(config.RoleApps) {
		return false
	}
	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Placement.Mode == config.PlacementCluster {
			return true
		}
	}
	return false
}

// AppSites returns where each app runs, keyed by app name: its pinned site,
// or every apps site for a clustered app. It is placeApps's answer without
// rendering anything, for a command that only needs to know which stacks to
// look at.
func AppSites(cfg *config.Config) map[string][]string {
	out := map[string][]string{}
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		switch app.Placement.Mode {
		case config.PlacementPinned:
			out[name] = []string{app.Placement.Site}
		case config.PlacementCluster:
			out[name] = cfg.AppsSites()
		}
	}
	return out
}

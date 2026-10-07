package apply

import (
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

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

// databasePathReason is what the plan says for an app restarted because the
// infrastructure under it moved.
const databasePathReason = "its database path changed (HAProxy/Patroni moved), and an app's open database connections do not survive that"

// restartDatabaseApps adds a restart for every database app this site runs
// when the plan moves HAProxy or Patroni here. An app that already has its own
// action keeps it, since a restart or a recreate gives it fresh connections
// either way, and is not acted on twice. The restarts come after the
// infrastructure stack's action and health gate and after the database
// bootstrap, because Execute runs actions in order and the bootstrap before
// the first app.
//
// --only applies to what the operator changes, not to this. An `apply --only
// infra` that restarts HAProxy and leaves Mbin on dead connections is the
// incident this exists for; an --only that excludes infra moves nothing under
// the apps and triggers nothing.
func (p *Plan) restartDatabaseApps(apps []string, rendered map[string]bool) {
	if !p.databasePathMoves() {
		return
	}
	acted := map[string]bool{}
	for i, a := range p.Actions {
		acted[a.Stack] = true
		for _, app := range apps {
			if a.Stack == app {
				p.Actions[i].Reason += "; " + databasePathReason
			}
		}
	}
	for _, app := range apps {
		if rendered[app] && !acted[app] {
			p.Actions = append(p.Actions, Action{Stack: app, Reason: databasePathReason})
		}
	}
	rest := p.Actions[1:]
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Stack < rest[j].Stack })
}

// databasePathMoves reports whether the infrastructure stack's action restarts
// or replaces HAProxy or Patroni on this site, or changes what either reads.
// Either way every app's connection to the database through this site's
// HAProxy is closed. An action that touches only etcd, Garage or Caddy does
// not: a restart is narrowed to the services whose files changed, and a
// recreate replaces only the services whose configuration Compose sees
// changed, so a compose change outside the haproxy and patroni services
// leaves both running.
func (p *Plan) databasePathMoves() bool {
	if len(p.Actions) == 0 || p.Actions[0].Stack != infraStack {
		return false
	}
	infra := p.Actions[0]
	runs := map[string]bool{}
	for _, c := range p.Changes {
		switch c.Path {
		case "/" + haproxyConfig:
			runs["haproxy"] = true
		case "/" + patroniEnv:
			runs["patroni"] = true
		}
	}
	if !runs["haproxy"] && !runs["patroni"] {
		return false
	}
	for _, c := range p.Changes {
		if c.Kind != Create && c.Kind != Update {
			continue
		}
		switch strings.TrimPrefix(c.Path, remoteRoot) {
		case haproxyConfig, patroniEnv:
			return true
		case gatewayCompose:
			if servicesDiffer(c.before, c.content, "haproxy", "patroni") {
				return true
			}
		}
	}
	switch {
	case infra.Force:
		return true
	case infra.Recreate:
		return false
	case len(infra.Services) == 0:
		return true
	}
	for _, s := range infra.Services {
		if s == "haproxy" || s == "patroni" {
			return true
		}
	}
	return false
}

// The two infrastructure files that are the database path, relative to a
// site's root.
const (
	haproxyConfig = "srv/infra/haproxy/haproxy.cfg"
	patroniEnv    = "srv/infra/patroni.env"
)

// servicesDiffer reports whether any of the named services is defined
// differently in two versions of a compose file, which is what makes `up -d`
// recreate it. A file that does not parse counts as different: the cost of a
// wrong yes is one restart per app, and of a wrong no an app on dead
// connections.
func servicesDiffer(before, after string, services ...string) bool {
	var a, b struct {
		Services map[string]any `yaml:"services"`
	}
	if yaml.Unmarshal([]byte(before), &a) != nil || yaml.Unmarshal([]byte(after), &b) != nil {
		return true
	}
	for _, name := range services {
		if !reflect.DeepEqual(a.Services[name], b.Services[name]) {
			return true
		}
	}
	return false
}

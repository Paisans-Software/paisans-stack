package apply

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// Database is one clustered app's role and database, owned by that role.
type Database struct {
	App  string
	Role string
	Name string
	// password is never printed and never placed on a command line.
	password string
}

// Bootstrap is the per app database work a site with a Patroni member does,
// after the infrastructure stack and before any app stack.
type Bootstrap struct {
	// Patroni is this site's Patroni REST API, host:port, on the mesh.
	Patroni   string
	Databases []Database
}

// identifier is what this package will put into SQL as a role or database
// name. It is narrower than Postgres allows on purpose: a name that needs
// quoting to be safe is refused rather than quoted, so the SQL an operator
// reads in a log is the SQL that ran.
var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// reservedRoles are the roles the rendered Patroni defines for itself
// (patroni.env.tmpl). An app with one of these names would have its password
// set on the cluster's own superuser, admin or replication role.
var reservedRoles = map[string]bool{"postgres": true, "admin": true, "standby": true}

// Databases returns what site must create, or nil when it creates nothing:
// the site holds no Patroni member, or no clustered app uses Postgres.
//
// Only a site in cluster.sites runs Patroni, so only such a site can reach a
// primary through its own container. The work is idempotent and runs on
// whichever of them is the leader when it is applied; a replica says so and
// leaves it to the leader's site.
func Databases(cfg *config.Config, secrets *config.Secrets, site string) (*Bootstrap, error) {
	inCluster := false
	for _, name := range cfg.Cluster.Sites {
		if name == site {
			inCluster = true
		}
	}
	if !inCluster {
		return nil, nil
	}
	out := &Bootstrap{Patroni: fmt.Sprintf("%s:%d", cfg.Sites[site].Address, render.PatroniAPIPort)}
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		if app.Placement.Mode != config.PlacementCluster || !kinds.UsesPostgres(app.Kind) {
			continue
		}
		id := render.DBIdentifier(name)
		if !identifier.MatchString(id) || len(id) > 63 {
			return nil, fmt.Errorf("apps.%s: its database role would be named %q, and a role is created only from lowercase letters, digits and underscores, starting with a letter, at most 63 characters. Rename the app", name, id)
		}
		if reservedRoles[id] || strings.HasPrefix(id, "pg_") {
			return nil, fmt.Errorf("apps.%s: its database role would be %q, which is a role the cluster itself uses or reserves. Setting this app's password on it would hand the app that role. Rename the app", name, id)
		}
		password, _ := secrets.Apps[name]["database_password"].(string)
		if password == "" {
			return nil, fmt.Errorf("secrets apps.%s.database_password: required to create the app's database role. Run `paisans init` to generate it", name)
		}
		if strings.ContainsRune(password, 0) {
			return nil, fmt.Errorf("secrets apps.%s.database_password: contains a NUL byte, which Postgres cannot store", name)
		}
		out.Databases = append(out.Databases, Database{App: name, Role: id, Name: id, password: password})
	}
	if len(out.Databases) == 0 {
		return nil, nil
	}
	return out, nil
}

// WithDatabases attaches the bootstrap to a plan that acts on any stack. An
// apply with nothing to act on stays one that runs nothing, which is what
// makes re-running apply a habit rather than a risk. A rotated password
// changes the app's rendered environment, so it always arrives with an action.
func (p *Plan) WithDatabases(b *Bootstrap) {
	if b == nil || len(p.Actions) == 0 {
		return
	}
	p.Bootstrap = b
}

// How long to wait for a primary, and how often to ask. The first start of a
// fresh Spilo runs initdb before Patroni takes the leader key, which on a
// small host is tens of seconds; three minutes leaves room for a slow disk
// without leaving an operator watching a frozen terminal.
var (
	primaryWait = 180 * time.Second
	primaryPoll = 3 * time.Second
	sleep       = time.Sleep
)

// patroniCompose is how every Patroni command reaches the container: the
// infrastructure stack's compose file, which is where apply put it.
const patroniCompose = "docker compose -f /srv/infra/compose.yaml exec -T patroni"

// errReplica means this site's Patroni is not the leader, and names who is.
type errReplica struct{ leader string }

func (e errReplica) Error() string { return "replica of " + e.leader }

// waitForPrimary polls Patroni's /cluster until a member holds the leader key
// and is running, then reports whether that member is this site.
//
// /cluster rather than /primary because it answers two questions at once.
// /primary says only "not me" with a 503, which is the same answer a node
// still running initdb gives; /cluster names the leader, so a replica can be
// told apart from a cluster that has none yet (Patroni v4.1.0,
// docs/rest_api.rst, "GET /cluster"). curl runs inside the Spilo container,
// which installs it (zalando/spilo, postgres-appliance/build_scripts/
// prepare.sh), so the host needs nothing beyond Docker.
func waitForPrimary(plan *Plan, t Transport) error {
	command := fmt.Sprintf("%s curl -s --max-time 5 http://%s/cluster", patroniCompose, plan.Bootstrap.Patroni)
	attempts := int(primaryWait / primaryPoll)
	if attempts < 1 {
		attempts = 1
	}
	var last string
	for i := 0; i < attempts; i++ {
		out, err := t.Run(command)
		if err == nil {
			leader := patroniLeader(out)
			switch {
			case leader == plan.Site:
				return nil
			case leader != "":
				return errReplica{leader: leader}
			}
			last = strings.TrimSpace(out)
		} else {
			last = err.Error()
		}
		if i < attempts-1 {
			sleep(primaryPoll)
		}
	}
	return fmt.Errorf(
		"%s: no Patroni primary after %s, so no app database was created and no app stack was started. Last answer from %s:\n%s\nRead `docker compose -f /srv/infra/compose.yaml logs patroni` on the host, then apply again: it resumes here",
		plan.Site, primaryWait, plan.Bootstrap.Patroni, last)
}

// patroniLeader returns the running leader's name from a /cluster document,
// empty when there is none yet or the document is not one.
func patroniLeader(document string) string {
	var cluster struct {
		Members []struct {
			Name  string `json:"name"`
			Role  string `json:"role"`
			State string `json:"state"`
		} `json:"members"`
	}
	if err := json.Unmarshal([]byte(document), &cluster); err != nil {
		return ""
	}
	for _, member := range cluster.Members {
		if member.Role == "leader" && member.State == "running" {
			return member.Name
		}
	}
	return ""
}

// psql is how the SQL reaches Postgres: over the container's own unix socket,
// as the superuser the rendered patroni.env names. Spilo's pg_hba trusts
// every local socket connection (zalando/spilo, postgres-appliance/scripts/
// configure_spilo.py, `local all all trust`), so no password is needed here
// and none is passed. -X skips any psqlrc, and ON_ERROR_STOP makes the first
// failure the exit status rather than a line in the output.
const psql = patroniCompose + " psql -X -q -U postgres -d postgres -v ON_ERROR_STOP=1"

// bootstrapSQL is the whole script for one site, sent on stdin.
//
// Each step is idempotent, so a re-run converges rather than failing:
//
//   - the role is created only when missing
//   - its password is set every time, so rotating apps.<name>.database_password
//     and applying is the whole rotation
//   - the database is created only when missing, owned by the role
//
// CREATE DATABASE cannot run inside a transaction block, and so not inside a
// DO block either; `\gexec` runs a generated statement as its own top level
// command, which is the documented way to make it conditional.
//
// Statement logging is switched off for this session first. Spilo ships
// log_statement = 'ddl' (configure_spilo.py), and CREATE ROLE and ALTER ROLE
// are DDL, so without this every password would be written to the server log
// in plaintext. log_min_error_statement covers the same text on a failure.
func bootstrapSQL(b *Bootstrap) string {
	var sql strings.Builder
	sql.WriteString("SET log_statement = 'none';\n")
	sql.WriteString("SET log_min_duration_statement = -1;\n")
	sql.WriteString("SET log_min_error_statement = 'panic';\n")
	for _, db := range b.Databases {
		role, name := quoteIdent(db.Role), quoteLiteral(db.Role)
		fmt.Fprintf(&sql, "-- %s\n", db.App)
		fmt.Fprintf(&sql, "SELECT 'CREATE ROLE %s LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %s)\\gexec\n", role, name)
		fmt.Fprintf(&sql, "ALTER ROLE %s WITH LOGIN PASSWORD %s;\n", role, quoteLiteral(db.password))
		fmt.Fprintf(&sql, "SELECT 'CREATE DATABASE %s OWNER %s' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = %s)\\gexec\n",
			quoteIdent(db.Name), role, quoteLiteral(db.Name))
	}
	return sql.String()
}

// quoteIdent and quoteLiteral are belt and braces: identifiers are already
// restricted to [a-z0-9_], but a password is any string, and the doubling
// rule is the whole of SQL literal quoting with standard_conforming_strings
// on, which is the default since PostgreSQL 9.1.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// runBootstrap waits for a primary and creates what is missing. A replica
// skips, saying which site does the work; anything else that goes wrong stops
// the apply before any app stack is started.
func runBootstrap(plan *Plan, t Transport) error {
	if err := waitForPrimary(plan, t); err != nil {
		if replica, ok := err.(errReplica); ok {
			plan.say("  %-9s skipped: Patroni here is a replica, and %s holds the leader. Apply %s to create app roles and databases\n",
				"databases", replica.leader, replica.leader)
			return nil
		}
		return err
	}
	if out, err := t.RunInput(psql, bootstrapSQL(plan.Bootstrap)); err != nil {
		// psql quotes the failing line back for a syntax error, and that line
		// can be the one carrying a password.
		for _, db := range plan.Bootstrap.Databases {
			out = strings.ReplaceAll(out, quoteLiteral(db.password), "'[redacted]'")
			out = strings.ReplaceAll(out, db.password, "[redacted]")
		}
		return fmt.Errorf("%s: creating app roles and databases failed, so no app stack was started:\n%s", plan.Site, out)
	}
	for _, db := range plan.Bootstrap.Databases {
		plan.say("  %-9s role and database %s for %s\n", "ensured", db.Name, db.App)
	}
	return nil
}

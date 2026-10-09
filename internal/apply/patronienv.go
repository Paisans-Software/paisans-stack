package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// A data site's patroni.env lists the etcd hosts (ETCD3_HOSTS), so it changes
// whenever etcd's membership does: `site add` and `site remove` both change
// it on every data site that stays. What follows is the one way both bring it
// up to date.
//
// Patroni reads that list only to make first contact with etcd. With
// use_proxies off, which is the default and what Spilo renders from
// ETCD3_HOSTS, it then asks etcd for the cluster's members and talks to those,
// refreshing the list as it goes (Patroni v4.1.0, patroni/dcs/etcd.py,
// AbstractEtcdClientWithFailover._load_machines_cache and
// _refresh_machines_cache; etcd3.py, Etcd3Client._get_members). An out of
// date list is therefore harmless while Patroni runs, and the file matters
// only at its next start.
//
// So a replica's file is applied at once: recreating a replica's Patroni is
// a replica restart, which HAProxy, routing only to the primary, does not
// notice. The leader's is left for its next restart, since recreating the
// leader's Patroni is a failover.

// PatroniEnv is a data site's patroni.env, in rendered form.
func PatroniEnv(d deployment.Deployment) string { return d.RelPath(infraStack, "patroni.env") }

// LeaderPatroniEnvNote is what a command that leaves the leader's
// patroni.env out of date says about it.
func LeaderPatroniEnvNote(site string) string {
	return fmt.Sprintf("%s leads: `paisans apply --site %s` when failover is acceptable. Its patroni.env is left with the etcd hosts it started with, since recreating its Patroni now would be a failover. It is picked up at the leader's next restart, which that apply makes, Eg: after switching the leader to another site. Until then nothing depends on it: Patroni reads that list only to reach etcd at start, and from then on asks etcd for its members (Patroni v4.1.0, patroni/dcs/etcd.py, _refresh_machines_cache)", site, site)
}

// ReplicaEnv is one replica site's patroni.env brought up to date: the file
// written by a scoped apply when it differs, then the site's Patroni
// recreated when the running container has other etcd hosts.
type ReplicaEnv struct {
	Site string
	// Running is ETCD3_HOSTS as the site's running Patroni has it, and Want
	// as the render has it.
	Running, Want string
	// API is the address and port the site's Patroni serves its REST API on,
	// from the rendered restapi listen address.
	API string

	plan     *Plan
	rendered *render.Plan
}

// ReplicaEnvGate is how ReplicaEnv.Execute waits for the replica to stream
// again, from a site whose Patroni stays up.
type ReplicaEnvGate struct {
	// At reaches the site where patronictl runs; it is not the replica's
	// own, whose Patroni is the one restarting.
	At    Transport
	Wait  time.Duration
	Poll  time.Duration
	Sleep func(time.Duration)
	// Sync asks for a Sync Standby among the members too, when the cluster
	// keeps one.
	Sync bool
}

// runningEtcdHosts reads ETCD3_HOSTS from the running Patroni container.
// Compose passes an env_file into the container's environment at start, so
// this is what the running Patroni was started with.
func runningEtcdHosts(d deployment.Deployment) string {
	return d.ComposeCmd(infraStack) + " exec -T patroni printenv ETCD3_HOSTS"
}

// RecreatePatroni recreates the site's Patroni alone: --no-deps leaves etcd
// and every other service of the stack running, and --force-recreate makes
// the container start again with the file now on the host, which a run that
// wrote the file and stopped before recreating needs too.
func RecreatePatroni(d deployment.Deployment) string {
	return d.ComposeCmd(infraStack) + " up -d --no-deps --force-recreate patroni"
}

func patronictlList(d deployment.Deployment) string {
	return d.ComposeCmd(infraStack) + " exec -T patroni patronictl -c /home/postgres/postgres.yml list -f json"
}

// restAPIListen is the restapi listen address in a rendered patroni.env's
// SPILO_CONFIGURATION (patroni.env.tmpl): the site's mesh address and
// render.PatroniAPIPort, never loopback.
var restAPIListen = regexp.MustCompile(`restapi: \{listen: '([^']+)'`)

// ownStatus asks the site's own Patroni process, from inside its container,
// for its state through its REST API. GET /patroni answers with the node's
// status, including `state` and, on a replica, `replication_state`
// (Patroni v4.1.0, patroni/api.py, do_GET_patroni and
// get_postgresql_status, where state is postgresql.state, "running" once
// Postgres is up (patroni/postgresql/misc.py, PostgresqlState.RUNNING), and
// replication_state is "streaming" when pg_stat_wal_receiver says so
// (patroni/postgresql/__init__.py, replication_state_from_parameters)).
// It is python3's urllib, since Patroni itself runs on python3 in Spilo and
// nothing assures curl is in the image. It prints the two values on one line.
func ownStatus(d deployment.Deployment, api string) string {
	script := fmt.Sprintf(`import json, urllib.request; s = json.load(urllib.request.urlopen("http://%s/patroni", timeout=5)); print(s.get("state", ""), s.get("replication_state", ""))`, api)
	return d.ComposeCmd(infraStack) + " exec -T patroni python3 -c " + shellQuote(script)
}

// envValue is one variable's value in a rendered env file, empty when absent.
func envValue(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
	}
	return ""
}

// PlanReplicaEnv decides what site's patroni.env needs, reading the host and
// changing nothing. It returns nil when the file on the host and the running
// Patroni both have the rendered etcd hosts already, so a run that stopped
// part way plans only what is left.
func PlanReplicaEnv(site string, rendered *render.Plan, t Transport) (*ReplicaEnv, error) {
	d := rendered.Deployment
	rel := PatroniEnv(d)
	content, found := "", false
	for _, f := range rendered.Files {
		if f.Path == site+"/"+rel {
			content, found = f.Content, true
		}
	}
	if !found {
		return nil, nil
	}
	sp, err := Build(site, rendered, "", t, Scope(rel))
	if err != nil {
		return nil, err
	}
	if c := sp.Conflicts(); len(c) > 0 {
		return nil, fmt.Errorf("%s's %s differs from what the last apply recorded, so somebody edited it on the host. Nothing was changed. Restore it, or copy what is wanted into the configuration, and run again", site, c[0].Path)
	}
	out, err := t.Run(runningEtcdHosts(d))
	if err != nil {
		if errors.Is(err, ErrUnreachable) {
			return nil, fmt.Errorf("%s: reading the etcd hosts its Patroni runs with: %w", site, err)
		}
		return nil, fmt.Errorf("%s: reading the etcd hosts its Patroni runs with: %w: %s", site, err, strings.TrimSpace(out))
	}
	listen := restAPIListen.FindStringSubmatch(envValue(content, "SPILO_CONFIGURATION"))
	if listen == nil {
		return nil, fmt.Errorf("%s: the rendered %s names no restapi listen address, so its Patroni could not be asked whether it streams", site, rel)
	}
	r := &ReplicaEnv{Site: site, Running: strings.TrimSpace(out), Want: envValue(content, "ETCD3_HOSTS"), API: listen[1], plan: sp, rendered: rendered}
	if len(sp.Writes()) == 0 && r.Running == r.Want {
		return nil, nil
	}
	return r, nil
}

// ReplicaEnvStep is one thing ReplicaEnv.Execute does, for a plan.
type ReplicaEnvStep struct{ Verb, Text string }

// Steps is what Execute does, in order, for a plan.
func (r *ReplicaEnv) Steps(d deployment.Deployment) []ReplicaEnvStep {
	var out []ReplicaEnvStep
	for _, w := range r.plan.Writes() {
		out = append(out, ReplicaEnvStep{w.Kind.String(), w.Path})
	}
	return append(out,
		ReplicaEnvStep{"recreate", fmt.Sprintf("Patroni, a replica, so it starts with ETCD3_HOSTS=%s (it runs with %s): a replica restart, which HAProxy, routing only to the primary, does not notice: %s", r.Want, r.Running, RecreatePatroni(d))},
		ReplicaEnvStep{"check", "its own Patroni answers running and streaming through its REST API, before the next replica is touched"})
}

// Execute writes the file, recreates the replica's Patroni, and waits for it
// to stream again. It first asks whether the site still is a replica, and
// touches nothing when it leads by now: a recreate there is a failover.
func (r *ReplicaEnv) Execute(d deployment.Deployment, t Transport, g ReplicaEnvGate) error {
	members, err := listMembers(d, g.At)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.Member == r.Site && m.Role == "Leader" {
			return fmt.Errorf("%s leads now, so its Patroni was not recreated, which would be a failover. Run again: the plan leaves the leader's patroni.env for its next restart", r.Site)
		}
	}
	// Built again, not run as planned: a scoped apply keeps the manifest it
	// read for every file outside its scope, and an earlier stage may have
	// written this site's manifest since.
	sp, err := Build(r.Site, r.rendered, "", t, Scope(PatroniEnv(d)))
	if err != nil {
		return err
	}
	if c := sp.Conflicts(); len(c) > 0 {
		return fmt.Errorf("%s's %s differs from what the last apply recorded, so somebody edited it on the host. Its Patroni was not recreated. Restore it, or copy what is wanted into the configuration, and run again", r.Site, c[0].Path)
	}
	if len(sp.Writes()) > 0 {
		if err := Execute(sp, t); err != nil {
			return err
		}
	}
	if out, err := t.Run(RecreatePatroni(d)); err != nil {
		return fmt.Errorf("%s: recreating Patroni: %w: %s", r.Site, err, lastLines(out, 5))
	}
	n := 1
	if g.Poll > 0 && g.Wait/g.Poll > 0 {
		n = int(g.Wait / g.Poll)
	}
	var last error
	for i := 0; i < n; i++ {
		if last = r.streaming(d, t, g); last == nil {
			return nil
		}
		if errors.Is(last, ErrUnreachable) {
			return last
		}
		if i < n-1 && g.Sleep != nil {
			g.Sleep(g.Poll)
		}
	}
	return fmt.Errorf("%s does not stream again after its Patroni was recreated, within %s: %w. Read `%s logs patroni` there, and run again", r.Site, g.Wait, last, d.ComposeCmd(infraStack))
}

// streaming passes once the recreated container runs with the new etcd
// hosts, which proves it is the new one, and that container's own Patroni
// answers that Postgres is running and streaming. `patronictl list` alone is
// not enough: it reads the member key in etcd, which the old process wrote
// and which can still say streaming until its TTL runs out. The list is
// still read for the Sync Standby, which only the leader decides.
func (r *ReplicaEnv) streaming(d deployment.Deployment, t Transport, g ReplicaEnvGate) error {
	out, err := t.Run(runningEtcdHosts(d))
	if err != nil {
		if errors.Is(err, ErrUnreachable) {
			return err
		}
		return fmt.Errorf("its Patroni does not answer yet: %s", lastLines(out, 2))
	}
	if got := strings.TrimSpace(out); got != r.Want {
		return fmt.Errorf("its Patroni runs with ETCD3_HOSTS=%s", got)
	}
	out, err = t.Run(ownStatus(d, r.API))
	if err != nil {
		if errors.Is(err, ErrUnreachable) {
			return err
		}
		return fmt.Errorf("its Patroni's REST API on %s does not answer yet: %s", r.API, lastLines(out, 2))
	}
	if fields := strings.Fields(out); len(fields) != 2 || fields[0] != "running" || fields[1] != "streaming" {
		return fmt.Errorf("its Patroni answers %q, not running and streaming", strings.TrimSpace(out))
	}
	if !g.Sync {
		return nil
	}
	members, err := listMembers(d, g.At)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.Role == "Sync Standby" {
			return nil
		}
	}
	return fmt.Errorf("no member is a Sync Standby")
}

// listedMember is the part of a `patronictl list -f json` row this reads
// (Patroni v4.1.0, patroni/ctl.py, output_members).
type listedMember struct {
	Member string `json:"Member"`
	Role   string `json:"Role"`
	State  string `json:"State"`
}

func listMembers(d deployment.Deployment, at Transport) ([]listedMember, error) {
	out, err := at.Run(patronictlList(d))
	if err != nil {
		return nil, fmt.Errorf("%s: patronictl list: %w: %s", at.Describe(), err, lastLines(out, 3))
	}
	var list []listedMember
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &list); err != nil {
		return nil, fmt.Errorf("%s: patronictl list is not JSON: %w", at.Describe(), err)
	}
	return list, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

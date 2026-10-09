package doctor

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// EtcdHealthCommand asks every configured member for its health, from inside
// the etcd container on one member's host.
//
// The endpoints are etcd.members' mesh addresses, named one by one, rather
// than `--cluster`. --cluster first lists the members through the cluster
// (etcd v3.5.16, etcdctl/ctlv3/command/ep_command.go, endpointsFromCluster),
// and that is exactly the call that fails when quorum is gone, which is when
// this check matters most. Each named endpoint is asked on its own, so a
// member that is down reads as unhealthy beside the ones that answered.
func EtcdHealthCommand(cfg *config.Config) string {
	var endpoints []string
	for _, name := range cfg.Etcd.Members {
		endpoints = append(endpoints, fmt.Sprintf("http://%s:%d", cfg.Sites[name].Address, render.EtcdClientPort))
	}
	return cfg.Deployment().ComposeCmd("infra") + " exec -T etcd etcdctl --endpoints=" + strings.Join(endpoints, ",") + " endpoint health -w json"
}

// noCurl is what EtcdVersionCommand prints on a host without curl.
const noCurl = "__PAISANS_NO_CURL__"

// EtcdVersionCommand reads the member's /version on the host's loopback,
// which the rendered etcd listens on (infra-compose.yaml.tmpl,
// --listen-client-urls). It needs curl on the host, which the toolkit does
// not install; without it the check is skipped and says so.
const EtcdVersionCommand = "if command -v curl >/dev/null 2>&1; then curl -s --max-time 5 http://127.0.0.1:2379/version; else echo " + noCurl + "; fi"

// EtcdProbe is what one member's host said.
type EtcdProbe struct {
	// Site is the member that answered, empty when none did.
	Site string
	// Health is EtcdHealthCommand's output. etcdctl prints the JSON and then
	// exits non zero when any endpoint is unhealthy, so a failed command with
	// JSON in it is still an answer.
	Health string
	// Version is EtcdVersionCommand's output, and VersionErr its failure.
	Version    string
	VersionErr string
	// Tried is each member asked before Site, with why it did not answer.
	Tried []string
}

// EtcdHealth is one entry of `etcdctl endpoint health -w json`. The keys are
// etcdctl's epHealth (etcd v3.5.16, etcdctl/ctlv3/command/ep_command.go).
type EtcdHealth struct {
	Endpoint string `json:"endpoint"`
	Health   bool   `json:"health"`
	Took     string `json:"took"`
	Error    string `json:"error"`
}

// ParseEtcdHealth finds the JSON list in etcdctl's output, which may be
// followed by "Error: unhealthy cluster".
func ParseEtcdHealth(out string) ([]EtcdHealth, bool) {
	start := strings.Index(out, "[")
	end := strings.LastIndex(out, "]")
	if start < 0 || end < start {
		return nil, false
	}
	var list []EtcdHealth
	if err := json.Unmarshal([]byte(out[start:end+1]), &list); err != nil {
		return nil, false
	}
	return list, true
}

// EtcdVersion is etcd's /version document.
type EtcdVersion struct {
	Server  string `json:"etcdserver"`
	Cluster string `json:"etcdcluster"`
}

// Etcd reports quorum and the cluster version.
func Etcd(cfg *config.Config, in Input) []Finding {
	members := cfg.Etcd.Members
	if len(members) == 0 {
		return nil
	}
	quorum := len(members)/2 + 1
	p := in.Etcd
	if p.Site == "" {
		more := append([]string{}, p.Tried...)
		more = append(more, fmt.Sprintf("etcd.members is %s, and quorum needs %d. Patroni keeps its leader key in etcd, so without an answer here nothing about the database can be trusted either.", strings.Join(members, ", "), quorum))
		return []Finding{{Section: SectionEtcd, Level: Fail, Line: "no etcd member reached, so quorum could not be read", More: more}}
	}

	var out []Finding
	list, _ := ParseEtcdHealth(p.Health)
	healthy := 0
	var down []string
	var detail []string
	for _, name := range members {
		h, found := healthFor(cfg, name, list)
		switch {
		case found && h.Health:
			healthy++
		case found:
			down = append(down, name)
			detail = append(detail, fmt.Sprintf("%s: %s", name, firstLine(h.Error)))
		default:
			down = append(down, name)
			detail = append(detail, name+": not in etcdctl's answer")
		}
	}
	line := fmt.Sprintf("quorum: %d of %d members healthy (needs %d), asked from %s", healthy, len(members), quorum, p.Site)
	switch {
	case healthy >= quorum && len(down) == 0:
		out = append(out, Finding{Section: SectionEtcd, Level: OK, Line: line})
	case healthy >= quorum:
		out = append(out, Finding{Section: SectionEtcd, Level: Warn, Line: line,
			More: append(detail, fmt.Sprintf("Quorum holds, and one more loss ends it. Start %s.", strings.Join(down, ", ")))})
	default:
		more := append(detail,
			fmt.Sprintf("Start the members that are not healthy: %s. A member whose host is up but whose etcd is not comes back with `paisans apply --site <site> --execute`.", strings.Join(down, ", ")),
			"Without quorum Patroni cannot renew its leader key, so the primary demotes itself and every write stops until quorum returns (Patroni v4.1.0, patroni/ha.py, _handle_dcs_error: \"demoting self because DCS is not accessible\").")
		out = append(out, Finding{Section: SectionEtcd, Level: Fail, Line: strings.Replace(line, "quorum:", "no quorum:", 1), More: more})
	}
	return append(out, etcdVersion(p, down)...)
}

// healthFor matches a member to its endpoint by the mesh address.
func healthFor(cfg *config.Config, site string, list []EtcdHealth) (EtcdHealth, bool) {
	want := cfg.Sites[site].Address
	for _, h := range list {
		u, err := url.Parse(h.Endpoint)
		if err == nil && u.Hostname() == want {
			return h, true
		}
	}
	return EtcdHealth{}, false
}

// etcdVersion reports a cluster version that never settled.
//
// A new etcd cluster decides its version only from every member's version:
// decideClusterVersion returns nothing while any member's is unknown (etcd
// v3.5.16, server/etcdserver/cluster_util.go), and until it has a decision
// the leader sets the minimum, 3.0.0 (server/etcdserver/server.go,
// monitorVersions; api/version/version.go, MinClusterVersion). Patroni reads
// that number from /version, and below 3.3 it switches to the /v3alpha
// gateway (Patroni v4.1.0, patroni/dcs/etcd3.py), which etcd 3.5 does not
// serve (etcd v3.5.16, server/embed/serve.go, serves /v3/ and rewrites only
// /v3beta/ to it), so it waits on etcd forever and no primary appears. The fix is to
// start the founding members that never ran.
func etcdVersion(p EtcdProbe, down []string) []Finding {
	switch {
	case p.VersionErr != "":
		return []Finding{{Section: SectionEtcd, Level: Warn, Line: fmt.Sprintf("version: could not read /version on %s", p.Site), More: []string{firstLine(p.VersionErr)}}}
	case strings.TrimSpace(p.Version) == noCurl:
		return []Finding{{Section: SectionEtcd, Level: Skip, Line: fmt.Sprintf("version: curl is not installed on %s", p.Site), More: []string{"so the cluster version was not read"}}}
	}
	var v EtcdVersion
	if err := json.Unmarshal([]byte(strings.TrimSpace(p.Version)), &v); err != nil || v.Server == "" {
		return []Finding{{Section: SectionEtcd, Level: Warn, Line: fmt.Sprintf("version: unreadable answer from %s: %q", p.Site, firstLine(p.Version))}}
	}
	line := fmt.Sprintf("version: server %s, cluster %s", v.Server, v.Cluster)
	unsettled := v.Cluster == "3.0.0" || v.Cluster == "not_decided"
	if !unsettled || !strings.HasPrefix(v.Server, "3.5.") {
		return []Finding{{Section: SectionEtcd, Level: OK, Line: line}}
	}
	advice := "Apply each founding member whose etcd has not run yet"
	if len(down) > 0 {
		advice = fmt.Sprintf("Apply the members that are not running: %s, Eg: `paisans apply --site %s --execute`", strings.Join(down, ", "), down[0])
	}
	return []Finding{{Section: SectionEtcd, Level: Fail, Line: line + ": the cluster version never settled",
		More: []string{
			"A new etcd cluster settles its version only once every founding member has run (etcd v3.5.16, server/etcdserver/cluster_util.go, decideClusterVersion); until then it reports 3.0.0.",
			"Patroni reads that and falls back to etcd's /v3alpha API (Patroni v4.1.0, patroni/dcs/etcd3.py), which etcd 3.5 does not serve, so it waits on etcd forever and no primary appears.",
			advice + ". The version settles within seconds of the last one starting.",
		}}}
}

// SyncCommand reads Patroni's /sync key. --consistency=s is a serializable
// read, answered from the local member's own copy, so it works without
// quorum, which is when it is needed; the value may be a moment stale, and
// /sync changes only when the leader changes who it waits for.
func SyncCommand(d deployment.Deployment) string {
	return apply.Etcdctl(d, "get /service/"+render.PatroniScope+"/sync --print-value-only --consistency=s")
}

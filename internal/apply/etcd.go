package apply

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// ReadEtcdInitial returns the flags this host's etcd member was first started
// with, for render.WithEtcdInitial. found is false on a host with no etcd
// member yet, which renders as a founder.
//
// The record is read first. A host deployed before the record existed has
// none, and its compose file's own flags are the answer instead: they are
// what the member runs with, so repeating them changes nothing. Falling back
// to etcd.members there would be wrong the moment the configuration names
// more members than the cluster was born with, and a compose file that
// changed only in inert flags would still make apply recreate the member.
func ReadEtcdInitial(t Transport, d deployment.Deployment) (render.EtcdInitial, bool, error) {
	content, found, err := t.ReadFile(remoteRoot + render.EtcdInitialPath(d))
	if err != nil {
		return render.EtcdInitial{}, false, err
	}
	if found {
		in, ok := render.ParseEtcdInitial(content)
		if !ok {
			return render.EtcdInitial{}, false, fmt.Errorf("%s%s holds no initial-cluster-state and initial-cluster lines. It records how this etcd member was first started; restore it from the member's compose file, or delete it to fall back to that file", remoteRoot, render.EtcdInitialPath(d))
		}
		return in, true, nil
	}
	compose, found, err := t.ReadFile(d.Compose(infraStack))
	if err != nil || !found {
		return render.EtcdInitial{}, false, err
	}
	in, ok := render.ParseEtcdFlags(compose)
	return in, ok, nil
}

// EtcdMember is one entry of `etcdctl member list -w json`. The keys are
// etcd's etcdserverpb.Member as etcdctl's JSON printer marshals it (etcd
// v3.5.16, etcdctl/ctlv3/command/printer_json.go): the ID is a decimal
// number there, and every other command takes it in hex.
type EtcdMember struct {
	ID         uint64   `json:"ID"`
	Name       string   `json:"name"`
	PeerURLs   []string `json:"peerURLs"`
	ClientURLs []string `json:"clientURLs"`
	IsLearner  bool     `json:"isLearner"`
}

// HexID is the member's ID as `etcdctl member promote` and `member remove`
// parse it (strconv.ParseUint(args[0], 16, 64), etcd v3.5.16,
// etcdctl/ctlv3/command/member_command.go).
func (m EtcdMember) HexID() string { return strconv.FormatUint(m.ID, 16) }

// Started reports whether the member has run. A member added and never
// started has no name and no client URLs yet: it publishes both when it
// first joins.
func (m EtcdMember) Started() bool { return m.Name != "" }

// Etcdctl is how every etcd command reaches the cluster: through the etcd
// container on a member's own host, against its loopback listener, which is
// rendered for exactly this (infra-compose.yaml.tmpl, --listen-client-urls).
// The image ships etcdctl beside etcd, so the host needs nothing.
func Etcdctl(d deployment.Deployment, args string) string {
	return d.ComposeCmd(infraStack) + " exec -T etcd etcdctl --endpoints=http://127.0.0.1:2379 " + args
}

// noEtcd is what the membership probe prints on a host with no etcd running,
// so that "not here" is an answer rather than a failed command.
const noEtcd = "__PAISANS_NO_ETCD__"

// ReadEtcdMembers asks this host's etcd for the cluster's membership. running
// is false when no etcd container runs here, which is an answer, not an error.
func ReadEtcdMembers(t Transport, d deployment.Deployment) (members []EtcdMember, running bool, err error) {
	command := fmt.Sprintf(
		"if %s ps --status running --quiet etcd 2>/dev/null | grep -q .; then %s; else echo %s; fi",
		d.ComposeCmd(infraStack), Etcdctl(d, "member list -w json"), noEtcd)
	out, err := t.Run(command)
	if err != nil {
		return nil, false, fmt.Errorf("asking etcd for its members: %w", err)
	}
	if strings.TrimSpace(out) == noEtcd {
		return nil, false, nil
	}
	members, err = ParseEtcdMembers(out)
	if err != nil {
		return nil, false, err
	}
	return members, true, nil
}

// ParseEtcdMembers reads `etcdctl member list -w json`.
func ParseEtcdMembers(out string) ([]EtcdMember, error) {
	var list struct {
		Members []EtcdMember `json:"members"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &list); err != nil {
		return nil, fmt.Errorf("etcd's member list is not JSON: %w\n%s", err, out)
	}
	return list.Members, nil
}

// EtcdMemberSite names the configured site a member runs on, matched by its
// peer URL, which is the one fact every member has from the moment it is
// added. A member matching no site is named as etcd names it, or by its ID
// when it has never started.
func EtcdMemberSite(cfg *config.Config, m EtcdMember) string {
	for _, name := range cfg.SiteNames() {
		want := render.EtcdPeerURL(cfg.Sites[name].Address)
		for _, url := range m.PeerURLs {
			if url == want {
				return name
			}
		}
	}
	if m.Name != "" {
		return m.Name
	}
	return "member " + m.HexID()
}

// ProbeEtcdMembers finds the live membership from the first of these sites
// whose etcd answers, trying site first. found is false when no etcd runs on
// any of them, which is a deployment not started yet.
//
// founding is whether site's own etcd member is being founded, from
// ReadEtcdInitial. Only then is a member that is running but has no leader
// passed over rather than reported as an error. `member list` is
// linearizable in etcd v3.5.16 and waits for a leader, and a founding member
// has none until a second founding member starts: the witness, applied
// first, is alone and leaderless until the first data site's etcd joins it.
// A founding cluster's membership is fixed by --initial-cluster from the
// configuration, so there is nothing for EtcdRefusal to compare yet. The
// member counts as leaderless only when EtcdLeaderless says so; a failed
// `member list` alone is never read as one.
func ProbeEtcdMembers(cfg *config.Config, site string, transports map[string]Transport, founding bool) (members []EtcdMember, found bool, err error) {
	order := []string{site}
	for _, name := range cfg.Etcd.Members {
		if name != site {
			order = append(order, name)
		}
	}
	for _, name := range order {
		t, ok := transports[name]
		if !ok {
			continue
		}
		members, running, err := ReadEtcdMembers(t, cfg.Deployment())
		if err != nil {
			if founding {
				if leaderless, lerr := EtcdLeaderless(t, cfg.Deployment()); lerr == nil && leaderless {
					continue
				}
			}
			return nil, false, fmt.Errorf("%s: %w", name, err)
		}
		if running {
			return members, true, nil
		}
	}
	return nil, false, nil
}

// EtcdLeaderless reports whether this host's etcd member answers and knows
// of no leader. It asks `etcdctl endpoint status`, the Maintenance Status
// RPC, which the member answers from its own state without a leader, unlike
// `member list`. A leader of 0 is raft's "none", and etcdctl's JSON printer
// leaves the field out when it is 0. An error means the member did not
// answer, which says nothing about its leader.
func EtcdLeaderless(t Transport, d deployment.Deployment) (bool, error) {
	out, err := t.Run(Etcdctl(d, "endpoint status -w json"))
	if err != nil {
		return false, fmt.Errorf("asking etcd for its status: %w", err)
	}
	// The output is combined with stderr, where etcdctl logs warnings, so
	// the JSON is read from the line it starts on.
	text := strings.TrimSpace(out)
	if i := strings.Index(text, "\n["); i >= 0 && !strings.HasPrefix(text, "[") {
		text = text[i+1:]
	}
	var status []struct {
		Endpoint string `json:"Endpoint"`
		Status   struct {
			Leader uint64 `json:"leader"`
		} `json:"Status"`
	}
	if err := json.Unmarshal([]byte(text), &status); err != nil {
		return false, fmt.Errorf("etcd's endpoint status is not JSON: %w\n%s", err, out)
	}
	if len(status) != 1 {
		return false, fmt.Errorf("etcd's endpoint status names %d endpoints, want 1:\n%s", len(status), out)
	}
	return status[0].Status.Leader == 0, nil
}

// EtcdContainerRunning reports whether this host's etcd container runs. It
// asks Docker, not etcd, so it answers the same for a member with no leader.
func EtcdContainerRunning(t Transport, d deployment.Deployment) (bool, error) {
	command := fmt.Sprintf(
		"if %s ps --status running --quiet etcd 2>/dev/null | grep -q .; then echo running; else echo %s; fi",
		d.ComposeCmd(infraStack), noEtcd)
	out, err := t.Run(command)
	if err != nil {
		return false, fmt.Errorf("asking docker whether etcd runs: %w", err)
	}
	return strings.TrimSpace(out) != noEtcd, nil
}

// EtcdRunning asks every configured etcd member with a transport whether its
// etcd container runs, keyed by site. It is asked only when a member is being
// founded, so an ordinary apply reaches no more hosts than it did. It asks
// Docker rather than etcd, because a founding member alone has no leader and
// etcd would not answer for it.
func EtcdRunning(cfg *config.Config, transports map[string]Transport) (map[string]bool, error) {
	running := map[string]bool{}
	for _, name := range cfg.Etcd.Members {
		t, ok := transports[name]
		if !ok {
			continue
		}
		up, err := EtcdContainerRunning(t, cfg.Deployment())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		running[name] = up
	}
	return running, nil
}

// EtcdGates runs apply's two etcd refusals in order: EtcdRefusal against the
// live membership, then WitnessFirstRefusal. founding and running are as
// WitnessFirstRefusal takes them.
func EtcdGates(cfg *config.Config, plan *Plan, transports map[string]Transport, founding bool, running map[string]bool) error {
	members, found, err := ProbeEtcdMembers(cfg, plan.Site, transports, founding)
	if err != nil {
		return err
	}
	if found {
		if err := EtcdRefusal(cfg, plan, members); err != nil {
			return err
		}
	}
	return WitnessFirstRefusal(cfg, plan, founding, running)
}

// FoundingUnstarted lists the other members of etcd.members whose etcd does
// not run, in configuration order. A founding apply stops after its
// infrastructure stack rather than wait for a primary that cannot appear
// until each of them is applied; see WitnessFirstRefusal for why.
func FoundingUnstarted(cfg *config.Config, site string, running map[string]bool) []string {
	var unstarted []string
	for _, name := range cfg.Etcd.Members {
		if name != site && !running[name] {
			unstarted = append(unstarted, name)
		}
	}
	return unstarted
}

// EtcdRefusal is apply's refusal to touch a site's infrastructure stack while
// the live etcd membership differs from etcd.members.
//
// Without it an operator who edited the configuration and ran apply first
// would start an etcd the running cluster has not admitted: rendered as a
// founder of the configured set, it would try to form a second cluster, or,
// on a member already running, recreate it for flags it ignores. Growing the
// membership is site add's job, one learner at a time. A plan that touches
// no file and no action in the infrastructure stack is let through, so an app
// change is never held hostage to a half grown cluster.
func EtcdRefusal(cfg *config.Config, plan *Plan, members []EtcdMember) error {
	if !touchesInfra(plan) {
		return nil
	}
	var live []string
	differs := false
	for _, m := range members {
		name := EtcdMemberSite(cfg, m)
		if m.IsLearner {
			name += " (learner)"
			differs = true
		}
		live = append(live, name)
	}
	sort.Strings(live)
	want := append([]string(nil), cfg.Etcd.Members...)
	sort.Strings(want)
	if !differs && len(live) != len(want) {
		differs = true
	}
	for i := 0; !differs && i < len(want); i++ {
		if live[i] != want[i] {
			differs = true
		}
	}
	if !differs {
		return nil
	}
	return fmt.Errorf(
		"%s: the running etcd cluster's members are %s, and etcd.members says %s, so this site's infrastructure stack was not touched. Applying it now would start an etcd the cluster has not admitted. Growing the cluster is `paisans site add <site>`, which adds each member as a learner and promotes it once it has caught up; run it for the site being added, then apply",
		plan.Site, strings.Join(live, ", "), strings.Join(want, ", "))
}

// touchesInfra reports whether a plan writes a file in, or acts on, the
// infrastructure stack.
func touchesInfra(plan *Plan) bool {
	for _, c := range plan.Changes {
		if c.Stack == infraStack && (c.Kind == Create || c.Kind == Update) {
			return true
		}
	}
	for _, a := range plan.Actions {
		if a.Stack == infraStack {
			return true
		}
	}
	return false
}

// WitnessesFirst names the witness sites a founding apply waits on: the
// members of etcd.members with the witness role, other than site itself.
// Empty when site is a witness, is not an etcd member, or the deployment
// has no witness in etcd.
func WitnessesFirst(cfg *config.Config, site string) []string {
	if !contains(cfg.Etcd.Members, site) || cfg.Sites[site].Has(config.RoleWitness) {
		return nil
	}
	var witnesses []string
	for _, name := range cfg.Etcd.Members {
		if name != site && cfg.Sites[name].Has(config.RoleWitness) {
			witnesses = append(witnesses, name)
		}
	}
	return witnesses
}

// WitnessFirstRefusal is apply's refusal to found an etcd member on a site
// that is not a witness while a witness's etcd is not running yet.
//
// A new etcd cluster holds its cluster version at 3.0 until every founding
// member answers: the leader decides the version only from a full set of
// member versions (etcd v3.5.16, server/etcdserver/cluster_util.go,
// decideClusterVersion returns nil when any member's version is unknown).
// Patroni reads that version from /version and, finding 3.0, falls back to
// the v3alpha gateway (Patroni v4.1.0, patroni/dcs/etcd3.py), which etcd 3.5
// does not serve, so it logs "waiting on
// etcd" and never takes the leader key. Every data site's apply then times
// out waiting for a primary, with nothing on screen that points at the
// witness. Observed on the first three-site deployment: the two data sites
// applied first both timed out, and the cluster formed seconds after the
// witness was applied.
//
// The witness goes first because it is the one founding member nothing else
// depends on: it runs no Patroni and waits on nothing, so its apply finishes
// on its own. The data sites follow, and each one's etcd completes the set
// for the next. Applying the witness from inside a data site's apply was
// rejected: apply is a site at a time on purpose, and an apply that reaches
// into another site's stack is the half success across machines that rule
// exists to prevent.
//
// founding is whether this site's etcd member has never been started, from
// ReadEtcdInitial. A member that has run once is past bootstrap, and a
// witness that is down later must never hold up an apply of a data site:
// that is the outage the witness exists to ride out. running says which of
// the witnesses has an etcd container running now.
func WitnessFirstRefusal(cfg *config.Config, plan *Plan, founding bool, running map[string]bool) error {
	if !founding || !touchesInfra(plan) {
		return nil
	}
	var waiting []string
	for _, name := range WitnessesFirst(cfg, plan.Site) {
		if !running[name] {
			waiting = append(waiting, name)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	var commands []string
	for _, name := range waiting {
		commands = append(commands, "`paisans apply --site "+name+"`")
	}
	return fmt.Errorf(
		"%s: this site's etcd member would be founded while the witness %s runs no etcd, so this site's infrastructure stack was not touched. A new etcd cluster settles its version only once every founding member answers, and until then Patroni cannot take the leader key on any data site. Apply the witness first with %s, then apply this site again",
		plan.Site, strings.Join(waiting, ", "), strings.Join(commands, ", then "))
}

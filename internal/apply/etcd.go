package apply

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
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
func ReadEtcdInitial(t Transport) (render.EtcdInitial, bool, error) {
	content, found, err := t.ReadFile(remoteRoot + render.EtcdInitialPath)
	if err != nil {
		return render.EtcdInitial{}, false, err
	}
	if found {
		in, ok := render.ParseEtcdInitial(content)
		if !ok {
			return render.EtcdInitial{}, false, fmt.Errorf("%s%s holds no initial-cluster-state and initial-cluster lines. It records how this etcd member was first started; restore it from the member's compose file, or delete it to fall back to that file", remoteRoot, render.EtcdInitialPath)
		}
		return in, true, nil
	}
	compose, found, err := t.ReadFile(remoteRoot + gatewayCompose)
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
func Etcdctl(args string) string {
	return "docker compose -f /srv/infra/compose.yaml exec -T etcd etcdctl --endpoints=http://127.0.0.1:2379 " + args
}

// noEtcd is what the membership probe prints on a host with no etcd running,
// so that "not here" is an answer rather than a failed command.
const noEtcd = "__PAISANS_NO_ETCD__"

// ReadEtcdMembers asks this host's etcd for the cluster's membership. running
// is false when no etcd container runs here, which is an answer, not an error.
func ReadEtcdMembers(t Transport) (members []EtcdMember, running bool, err error) {
	command := fmt.Sprintf(
		"if docker compose -f /srv/infra/compose.yaml ps --status running --quiet etcd 2>/dev/null | grep -q .; then %s; else echo %s; fi",
		Etcdctl("member list -w json"), noEtcd)
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
func ProbeEtcdMembers(cfg *config.Config, site string, transports map[string]Transport) (members []EtcdMember, found bool, err error) {
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
		members, running, err := ReadEtcdMembers(t)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", name, err)
		}
		if running {
			return members, true, nil
		}
	}
	return nil, false, nil
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
	touches := false
	for _, c := range plan.Changes {
		if c.Stack == infraStack && (c.Kind == Create || c.Kind == Update) {
			touches = true
		}
	}
	for _, a := range plan.Actions {
		if a.Stack == infraStack {
			touches = true
		}
	}
	if !touches {
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

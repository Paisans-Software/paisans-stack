package siteadd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// startEtcd starts only the etcd service of a site's infrastructure stack.
// `up -d` with a service name acts on that service and nothing else, so a
// gateway's Caddy and a data site's Patroni are not touched: Patroni waits
// for stage 4, and the gateway has its own gates in apply.
const startEtcd = "docker compose -f /srv/infra/compose.yaml up -d etcd"

// probeEtcd finds a site whose etcd is a running voter, and the membership as
// it reports it. Live members the configuration does not name are refused:
// removing one is a separate command with its own gates.
func (p *Plan) probeEtcd() ([]apply.EtcdMember, error) {
	order := []string{}
	for _, name := range p.cfg.Etcd.Members {
		if name != p.Site {
			order = append(order, name)
		}
	}
	order = append(order, p.Site)
	for _, name := range order {
		members, running, err := apply.ReadEtcdMembers(p.transports[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if !running {
			continue
		}
		voter := false
		for _, m := range members {
			if apply.EtcdMemberSite(p.cfg, m) == name && !m.IsLearner {
				voter = true
			}
		}
		if !voter {
			continue
		}
		for _, m := range members {
			if site := apply.EtcdMemberSite(p.cfg, m); !contains(p.cfg.Etcd.Members, site) {
				return nil, fmt.Errorf("site add %s: etcd has a member %s that etcd.members does not list. Removing a member is not part of site add; list it, or remove it by hand with `etcdctl member remove %s`", p.Site, site, m.HexID())
			}
		}
		p.control = name
		return members, nil
	}
	return nil, fmt.Errorf("site add %s: no etcd answers as a voter on any of %s. site add grows a running cluster; a deployment not started yet is started with `paisans apply`", p.Site, strings.Join(order, ", "))
}

// joiners are the configured members that are not yet voters: existing sites
// gaining the role first, so the witness joins before the new data site, then
// the new site. One at a time, because etcd admits one learner at a time
// (maxLearners = 1, etcd v3.5.16, server/etcdserver/api/membership/
// cluster.go).
func (p *Plan) joiners(live []apply.EtcdMember) []string {
	voter := map[string]bool{}
	for _, m := range live {
		if !m.IsLearner {
			voter[apply.EtcdMemberSite(p.cfg, m)] = true
		}
	}
	var out []string
	for _, name := range sortedCopy(p.cfg.Etcd.Members) {
		if !voter[name] && name != p.Site {
			out = append(out, name)
		}
	}
	if !voter[p.Site] {
		out = append(out, p.Site)
	}
	return out
}

// buildEtcd is stage 3: each joiner added as a learner, started with the
// membership it joins, and promoted once it has caught up.
func (p *Plan) buildEtcd(live []apply.EtcdMember) *Stage {
	st := &Stage{
		Number: 3,
		Name:   "etcd",
		Gate:   fmt.Sprintf("`etcdctl endpoint health --cluster` on %s reports every member healthy, and the voters are exactly %s", p.control, strings.Join(sortedCopy(p.cfg.Etcd.Members), ", ")),
	}
	predicted := map[string]string{}
	present := map[string]bool{}
	for _, m := range live {
		site := apply.EtcdMemberSite(p.cfg, m)
		predicted[site] = peerURL(m)
		present[site] = true
	}
	joiners := p.joiners(live)
	for _, j := range joiners {
		url := render.EtcdPeerURL(p.cfg.Sites[j].Address)
		if !present[j] {
			st.Steps = append(st.Steps, Step{Site: j, Verb: "add", Text: fmt.Sprintf("an etcd learner %s with peer URL %s, through %s", j, url, p.control)})
		}
		predicted[j] = url
		initial, ok := p.initial[j]
		if !ok || initial.State != render.EtcdStateExisting {
			initial = render.EtcdInitial{State: render.EtcdStateExisting, Cluster: clusterString(predicted)}
		}
		st.Steps = append(st.Steps,
			Step{Site: j, Verb: "write", Text: fmt.Sprintf("/srv/infra/compose.yaml and /%s: --initial-cluster-state=%s --initial-cluster=%s", render.EtcdInitialPath, initial.State, initial.Cluster)},
			Step{Site: j, Verb: "start", Text: "etcd alone: " + startEtcd},
			Step{Site: j, Verb: "promote", Text: fmt.Sprintf("the learner once it has caught up; etcd refuses until then, so it is retried up to %d times", promoteAttempts)},
		)
	}
	st.run = func() error {
		for _, j := range joiners {
			if err := p.joinEtcd(j); err != nil {
				return err
			}
		}
		return nil
	}
	st.gate = p.etcdGate
	return st
}

func peerURL(m apply.EtcdMember) string {
	if len(m.PeerURLs) > 0 {
		return m.PeerURLs[0]
	}
	return ""
}

// clusterString is an --initial-cluster value, sorted by name the way the
// render sorts a founding one.
func clusterString(members map[string]string) string {
	var names []string
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		parts = append(parts, name+"="+members[name])
	}
	return strings.Join(parts, ",")
}

func (p *Plan) members() ([]apply.EtcdMember, error) {
	out, err := p.transports[p.control].Run(apply.Etcdctl("member list -w json"))
	if err != nil {
		return nil, fmt.Errorf("%s: etcd's member list: %w", p.control, err)
	}
	return apply.ParseEtcdMembers(out)
}

func (p *Plan) find(members []apply.EtcdMember, site string) *apply.EtcdMember {
	for i := range members {
		if apply.EtcdMemberSite(p.cfg, members[i]) == site {
			return &members[i]
		}
	}
	return nil
}

// joinEtcd takes one joiner from wherever it is to a voter. Every step reads
// the cluster first, so a run that stopped half way picks up where it was.
func (p *Plan) joinEtcd(j string) error {
	control := p.transports[p.control]
	t := p.transports[j]
	members, err := p.members()
	if err != nil {
		return err
	}
	m := p.find(members, j)
	if m != nil && !m.IsLearner {
		return nil
	}
	if m == nil {
		url := render.EtcdPeerURL(p.cfg.Sites[j].Address)
		p.say("  %-9s %s as an etcd learner\n", "add", j)
		if out, err := control.Run(apply.Etcdctl(fmt.Sprintf("member add %s --peer-urls=%s --learner", j, url))); err != nil {
			return fmt.Errorf("%s: adding %s as a learner: %s", p.control, j, lastLines(out, 3))
		}
		if members, err = p.members(); err != nil {
			return err
		}
		if m = p.find(members, j); m == nil {
			return fmt.Errorf("%s: %s was added, and is not in the member list", p.control, j)
		}
	}

	// What the member is born with: what its host already records, when that
	// is a join, or the membership as it now stands, which etcd requires to
	// match exactly.
	initial, found, err := apply.ReadEtcdInitial(t)
	if err != nil {
		return fmt.Errorf("%s: %w", j, err)
	}
	if !found || initial.State != render.EtcdStateExisting {
		current := map[string]string{}
		for _, member := range members {
			current[apply.EtcdMemberSite(p.cfg, member)] = peerURL(member)
		}
		initial = render.EtcdInitial{State: render.EtcdStateExisting, Cluster: clusterString(current)}
	}
	p.initial[j] = initial

	rendered, err := p.render()
	if err != nil {
		return err
	}
	paths := []string{"srv/infra/compose.yaml", render.EtcdInitialPath}
	// compose parses every service's env_file when it loads the project, so
	// a data site's patroni.env has to exist before its etcd can start.
	if renders(rendered, j, "srv/infra/patroni.env") {
		paths = append(paths, "srv/infra/patroni.env")
	}
	sp, err := apply.Build(j, rendered, "", t, apply.Scope(paths...))
	if err != nil {
		return err
	}
	if c := sp.Conflicts(); len(c) > 0 {
		return fmt.Errorf("%s: %s differs from what the last apply recorded, so somebody edited it on the host. The learner was added and not started; restore the file and run site add again", j, c[0].Path)
	}
	if err := apply.Execute(sp, t); err != nil {
		return err
	}
	p.say("  %-9s %s's etcd\n", "start", j)
	if out, err := t.Run(startEtcd); err != nil {
		return fmt.Errorf("%s: starting etcd: %s", j, lastLines(out, 5))
	}

	n := attempts(learnerWait, learnerPoll)
	for i := 0; ; i++ {
		if members, err = p.members(); err != nil {
			return err
		}
		if m = p.find(members, j); m != nil && m.Started() {
			break
		}
		if i >= n-1 {
			return fmt.Errorf("%s's etcd has not joined after %s: it is a learner that never started. Read `docker compose -f /srv/infra/compose.yaml logs etcd` on %s", j, learnerWait, j)
		}
		sleep(learnerPoll)
	}

	delay := promoteFirstDelay
	var last string
	for i := 0; i < promoteAttempts; i++ {
		out, err := control.Run(apply.Etcdctl("member promote " + m.HexID()))
		if err == nil {
			p.say("  %-9s %s to a voter\n", "promoted", j)
			return nil
		}
		last = lastLines(out, 3)
		if i < promoteAttempts-1 {
			sleep(delay)
			delay *= 2
			if delay > promoteMaxDelay {
				delay = promoteMaxDelay
			}
		}
	}
	return fmt.Errorf("%s: etcd would not promote %s after %d attempts, so it is still a learner and the cluster still has its quorum. Last answer: %s", p.control, j, promoteAttempts, last)
}

// endpointHealth is one entry of `etcdctl endpoint health -w json` (etcd
// v3.5.16, etcdctl/ctlv3/command/ep_command.go, epHealth).
type endpointHealth struct {
	Endpoint string `json:"endpoint"`
	Health   bool   `json:"health"`
	Error    string `json:"error"`
}

func (p *Plan) etcdGate() error {
	want := sortedCopy(p.cfg.Etcd.Members)
	var problems []string
	n := attempts(etcdWait, etcdPoll)
	for i := 0; i < n; i++ {
		problems = nil
		out, err := p.transports[p.control].Run(apply.Etcdctl("endpoint health --cluster -w json"))
		var health []endpointHealth
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &health); jerr != nil {
			problems = append(problems, "endpoint health: "+lastLines(out, 3))
		} else {
			for _, h := range health {
				if !h.Health {
					problems = append(problems, fmt.Sprintf("%s is unhealthy: %s", h.Endpoint, h.Error))
				}
			}
			if err != nil && len(problems) == 0 {
				problems = append(problems, "endpoint health: "+err.Error())
			}
		}
		members, err := p.members()
		if err != nil {
			problems = append(problems, err.Error())
		} else {
			var voters []string
			for _, m := range members {
				site := apply.EtcdMemberSite(p.cfg, m)
				if m.IsLearner {
					problems = append(problems, site+" is still a learner")
					continue
				}
				voters = append(voters, site)
			}
			sort.Strings(voters)
			if strings.Join(voters, ",") != strings.Join(want, ",") {
				problems = append(problems, fmt.Sprintf("the voters are %s, and etcd.members is %s", strings.Join(voters, ", "), strings.Join(want, ", ")))
			}
		}
		if len(problems) == 0 {
			return nil
		}
		if i < n-1 {
			sleep(etcdPoll)
		}
	}
	return fmt.Errorf("etcd is not whole after %s:\n  %s", etcdWait, strings.Join(problems, "\n  "))
}

func sortedCopy(list []string) []string {
	out := append([]string(nil), list...)
	sort.Strings(out)
	return out
}

package siteremove

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/failover"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/storageadd"
)

// etcdState is what the removal read of etcd.
type etcdState struct {
	// control is a remaining site whose etcd is a running voter, where every
	// etcdctl command runs. Empty when the deployment has no etcd.
	control string
	// member is the site's own member, nil once it has none.
	member *apply.EtcdMember
	// witness is the leaving witness's member, nil once it has none or when
	// no witness leaves (see EndState).
	witness *apply.EtcdMember
	// witnessRunning is whether an etcd container of this deployment still
	// runs on the leaving witness's host.
	witnessRunning bool
}

// endpointHealth is one entry of `etcdctl endpoint health -w json` (etcd
// v3.5.16, etcdctl/ctlv3/command/ep_command.go, epHealth).
type endpointHealth struct {
	Endpoint string `json:"endpoint"`
	Health   bool   `json:"health"`
	Error    string `json:"error"`
}

// probeEtcd finds a remaining voter to run etcdctl on, and refuses a removal
// that would leave the cluster without a quorum of healthy members or that
// starts while any other member is unhealthy: a membership change on an
// unhealthy cluster is how a cluster that was limping stops.
func (p *Plan) probeEtcd() error {
	if len(p.cfg.Etcd.Members) == 0 {
		return nil
	}
	var members []apply.EtcdMember
	for _, name := range p.end.Etcd.Members {
		list, running, err := apply.ReadEtcdMembers(p.transports[name], p.dep())
		if err != nil {
			return fmt.Errorf("site remove %s: %s: %w", p.Site, name, err)
		}
		if !running {
			continue
		}
		for _, m := range list {
			if apply.EtcdMemberSite(p.cfg, m) == name && !m.IsLearner {
				p.etcd.control, members = name, list
			}
		}
		if p.etcd.control != "" {
			break
		}
	}
	if p.etcd.control == "" {
		return fmt.Errorf("site remove %s: no etcd answers as a voter on any of %s, so the cluster cannot be changed. Run `paisans doctor` to see why", p.Site, strings.Join(p.end.Etcd.Members, ", "))
	}
	var problems []string
	for i, m := range members {
		site := apply.EtcdMemberSite(p.cfg, m)
		switch {
		case site == p.Site:
			p.etcd.member = &members[i]
		case site == p.witness && p.witness != "":
			p.etcd.witness = &members[i]
		}
		switch {
		case site == p.Site:
		case !contains(p.cfg.Etcd.Members, site):
			problems = append(problems, fmt.Sprintf("etcd has a member %s that etcd.members does not list", site))
		case m.IsLearner:
			problems = append(problems, fmt.Sprintf("%s is still a learner", site))
		}
	}
	healthy, err := p.etcdHealth(members)
	if err != nil {
		return err
	}
	after, up := 0, 0
	for _, m := range members {
		site := apply.EtcdMemberSite(p.cfg, m)
		if site == p.Site || m.IsLearner {
			continue
		}
		after++
		if healthy[site] {
			up++
		} else {
			problems = append(problems, fmt.Sprintf("%s's etcd is unhealthy", site))
		}
	}
	if after > 0 && up*2 <= after {
		problems = append(problems, fmt.Sprintf("without %s, %d of %d remaining members are healthy, which is no quorum", p.Site, up, after))
	}
	if p.witness != "" && up < after {
		problems = append(problems, fmt.Sprintf("%s leaves etcd after %s, going from two voters to one, which is safe only while both are healthy: the removal is a change two voters must both agree to", p.witness, p.Site))
	}
	if p.witness != "" {
		out, err := p.transports[p.witness].Run(p.witnessEtcdIDs())
		if err != nil {
			return fmt.Errorf("site remove %s: %s: looking for its etcd container: %w: %s", p.Site, p.witness, err, lastLines(out, 3))
		}
		p.etcd.witnessRunning = strings.TrimSpace(out) != ""
	}
	if len(problems) > 0 {
		return fmt.Errorf("site remove %s: etcd is not healthy enough to change its membership:\n  %s\nFix the members named (`paisans doctor` says how) and run again. Nothing was changed", p.Site, strings.Join(problems, "\n  "))
	}
	return nil
}

// etcdHealth asks the control site for every member's health, by site.
// etcdctl exits non zero when any endpoint is unhealthy and still prints
// every endpoint, so the answer is read whatever the exit.
func (p *Plan) etcdHealth(members []apply.EtcdMember) (map[string]bool, error) {
	out, err := p.transports[p.etcd.control].Run(apply.Etcdctl(p.dep(), "endpoint health --cluster -w json"))
	var health []endpointHealth
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &health); jerr != nil {
		if err != nil {
			return nil, fmt.Errorf("site remove %s: %s: etcd's endpoint health: %v: %s", p.Site, p.etcd.control, err, lastLines(out, 3))
		}
		return nil, fmt.Errorf("site remove %s: %s: etcd's endpoint health is not JSON: %s", p.Site, p.etcd.control, lastLines(out, 3))
	}
	byURL := map[string]string{}
	for _, m := range members {
		for _, u := range m.ClientURLs {
			byURL[u] = apply.EtcdMemberSite(p.cfg, m)
		}
	}
	out2 := map[string]bool{}
	for _, h := range health {
		if site, ok := byURL[h.Endpoint]; ok {
			out2[site] = h.Health
		}
	}
	return out2, nil
}

// member is one row of `patronictl list -f json`: Role is "Leader", "Sync
// Standby" or "Replica", and State "running" for the leader and "streaming"
// for a replica that replicates (Patroni v4.1.0, patroni/ctl.py,
// output_members).
type member map[string]any

func (m member) str(key string) string {
	s, _ := m[key].(string)
	return s
}

func (m member) name() string  { return m.str("Member") }
func (m member) role() string  { return m.str("Role") }
func (m member) state() string { return m.str("State") }

// patroniState is what the removal read of Patroni.
type patroniState struct {
	// at is a remaining cluster site whose Patroni answers, where every
	// patronictl command runs.
	at      string
	members []member
	leader  string
	// candidate is the healthy Sync Standby the leadership goes to when the
	// site holds it.
	candidate string
	// listed is whether the site is still a member.
	listed bool
	// sync is the live synchronous_mode.
	sync bool
}

const noPatroni = "__PAISANS_NO_PATRONI__"

// patronictl runs inside a site's Patroni container, as site add's does.
func (p *Plan) patronictl(args string) string {
	return p.dep().ComposeCmd("infra") + " exec -T patroni patronictl -c /home/postgres/postgres.yml " + args
}

func (p *Plan) patroniList() ([]member, error) {
	out, err := p.transports[p.patroni.at].Run(p.patronictl("list -f json"))
	if err != nil {
		return nil, fmt.Errorf("%s: patronictl list: %w: %s", p.patroni.at, err, lastLines(out, 3))
	}
	var list []member
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &list); err != nil {
		return nil, fmt.Errorf("%s: patronictl list is not JSON: %w", p.patroni.at, err)
	}
	return list, nil
}

// probePatroni reads the cluster from a remaining data site, and refuses a
// removal that starts while any other member is unhealthy, or that would
// move the leadership with no synchronous standby to take it losslessly.
func (p *Plan) probePatroni() error {
	if len(p.cfg.Cluster.Sites) == 0 {
		return nil
	}
	for _, name := range p.end.Cluster.Sites {
		command := fmt.Sprintf("if %s ps --status running --quiet patroni 2>/dev/null | grep -q .; then %s; else echo %s; fi",
			p.dep().ComposeCmd("infra"), p.patronictl("list -f json"), noPatroni)
		out, err := p.transports[name].Run(command)
		if err != nil {
			return fmt.Errorf("site remove %s: %s: asking Patroni for its members: %w: %s", p.Site, name, err, lastLines(out, 3))
		}
		if strings.TrimSpace(out) == noPatroni {
			continue
		}
		var list []member
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &list); err != nil {
			return fmt.Errorf("site remove %s: %s: patronictl list is not JSON: %w", p.Site, name, err)
		}
		p.patroni.at, p.patroni.members = name, list
		break
	}
	if p.patroni.at == "" {
		return fmt.Errorf("site remove %s: no Patroni runs on any of %s, so the cluster cannot be changed. Run `paisans doctor` to see why", p.Site, strings.Join(p.end.Cluster.Sites, ", "))
	}
	var problems []string
	seen := map[string]bool{}
	for _, m := range p.patroni.members {
		seen[m.name()] = true
		switch {
		case m.name() == p.Site:
			p.patroni.listed = true
			if m.role() == "Leader" {
				p.patroni.leader = m.name()
			}
		case m.role() == "Leader":
			p.patroni.leader = m.name()
			if m.state() != "running" {
				problems = append(problems, fmt.Sprintf("the leader %s is %s", m.name(), m.state()))
			}
		case m.state() != "streaming":
			problems = append(problems, fmt.Sprintf("%s is %s, not streaming", m.name(), m.state()))
		}
	}
	for _, name := range p.end.Cluster.Sites {
		if !seen[name] {
			problems = append(problems, name+" is not a cluster member")
		}
	}
	if p.patroni.leader == "" {
		problems = append(problems, "no member holds the leader key")
	}
	if len(problems) > 0 {
		return fmt.Errorf("site remove %s: the Patroni cluster is not healthy enough to change:\n  %s\nFix the members named (`paisans doctor` says how) and run again. Nothing was changed", p.Site, strings.Join(problems, "\n  "))
	}
	if p.patroni.leader == p.Site {
		for _, m := range p.patroni.members {
			if m.role() == "Sync Standby" && m.state() == "streaming" && contains(p.end.Cluster.Sites, m.name()) {
				p.patroni.candidate = m.name()
				break
			}
		}
		if p.patroni.candidate == "" {
			return fmt.Errorf("site remove %s: it holds the Patroni leader, and no member is a streaming Sync Standby, which is the only one that takes over without losing writes the leader has acknowledged. Set cluster.synchronous: true and apply, or switch the leader to another site yourself, then run again. Nothing was changed", p.Site)
		}
	}
	out, err := p.transports[p.patroni.at].Run(p.patronictl("show-config"))
	if err != nil {
		return fmt.Errorf("site remove %s: %s: patronictl show-config: %w: %s", p.Site, p.patroni.at, err, lastLines(out, 3))
	}
	var live map[string]any
	if err := yaml.Unmarshal([]byte(out), &live); err != nil {
		return fmt.Errorf("site remove %s: %s: patronictl show-config is not YAML: %w", p.Site, p.patroni.at, err)
	}
	p.patroni.sync = fmt.Sprint(live["synchronous_mode"]) == "true"
	return nil
}

// garageState is what the removal read of Garage.
type garageState struct {
	// anchor is the first remaining Garage site, where the layout is
	// changed.
	anchor string
	// node is the short ID of the site's row in the layout, empty once it
	// has none, and version the layout's version.
	node    string
	version int
}

func gcmd(p *Plan, args string) string { return garage.Command(p.dep()) + " " + args }

// probeGarage finds the site's node in the layout, by the zone `storage
// add` names after a site, and refuses when a node that stays is not
// healthy: the data the site holds is copied to those nodes.
func (p *Plan) probeGarage() error {
	if !contains(p.cfg.Storage.Garage.Sites, p.Site) {
		return nil
	}
	p.garage.anchor = p.end.Storage.Garage.Sites[0]
	t := p.transports[p.garage.anchor]
	out, err := t.Run(gcmd(p, "layout show"))
	if err != nil {
		return fmt.Errorf("site remove %s: %s: `garage layout show`: %w: %s", p.Site, p.garage.anchor, err, lastLines(out, 3))
	}
	version, zones, err := storageadd.LayoutZones(out)
	if err != nil {
		return fmt.Errorf("site remove %s: %s: %w", p.Site, p.garage.anchor, err)
	}
	p.garage.version = version
	for id, zone := range zones {
		if zone == p.Site {
			p.garage.node = id
		}
	}
	status, err := t.Run(gcmd(p, "status"))
	if err != nil {
		return fmt.Errorf("site remove %s: %s: `garage status`: %w: %s", p.Site, p.garage.anchor, err, lastLines(status, 3))
	}
	healthy, _ := storageadd.HealthyNodes(status)
	var sick []string
	for id, zone := range zones {
		if zone != p.Site && !healthy[id] {
			sick = append(sick, fmt.Sprintf("%s (%s)", zone, id))
		}
	}
	sort.Strings(sick)
	if len(sick) > 0 {
		return fmt.Errorf("site remove %s: Garage node(s) %s are not healthy, and the objects on %s are copied to the nodes that stay. Fix them and run again. Nothing was changed", p.Site, strings.Join(sick, ", "), p.Site)
	}
	return nil
}

// memberKey is the site's member key in etcd: Patroni keeps its state under
// /service/<scope>/ (see render.PatroniScope) and a member's key under
// members/<name>.
func (p *Plan) memberKey() string {
	return "/service/" + render.PatroniScope + "/members/" + p.Site
}

// stopService stops one service of the site's infrastructure stack, and is
// a no-op once its compose file is gone.
func (p *Plan) stopService(service string) string {
	compose := p.dep().Compose("infra")
	return fmt.Sprintf("if [ -f %s ]; then %s stop %s; fi", quote(compose), p.dep().ComposeCmd("infra"), service)
}

func (p *Plan) stopPatroni() string { return p.stopService("patroni") }

// endWantsSync is whether the end state keeps a synchronous standby: it asks
// for one and has a replica to be it.
func (p *Plan) endWantsSync() bool {
	return p.end.Cluster.Synchronous && len(p.end.Cluster.Sites) >= 2
}

// buildData is stage 1: the database and the objects out of the site.
func (p *Plan) buildData() *Stage {
	st := &Stage{Number: 1, Name: "data out of the site"}
	var gates []string
	inCluster := contains(p.cfg.Cluster.Sites, p.Site)
	if inCluster {
		if p.patroni.leader == p.Site {
			st.Steps = append(st.Steps, Step{Site: p.patroni.candidate, Verb: "switch", Text: fmt.Sprintf("the Patroni leader from %s to the Sync Standby %s, losing no acknowledged write: %s", p.Site, p.patroni.candidate, failover.SwitchoverCommand(p.dep(), p.Site, p.patroni.candidate))})
		}
		if p.patroni.sync && len(p.end.Cluster.Sites) < 2 {
			st.Steps = append(st.Steps, Step{Site: p.patroni.at, Verb: "set", Text: "synchronous_mode=false in Patroni's dynamic configuration: one data site remains, and a leader that waits for a standby that is leaving stops taking writes"})
		}
		if p.patroni.listed {
			if !p.HostGone {
				st.Steps = append(st.Steps, Step{Site: p.Site, Verb: "stop", Text: "Patroni: " + p.stopPatroni()})
			}
			st.Steps = append(st.Steps, Step{Site: p.etcd.control, Verb: "delete", Text: fmt.Sprintf("%s's member key, %s, so Patroni stops listing it", p.Site, p.memberKey())})
		}
		gate := fmt.Sprintf("`patronictl list` on %s shows another site leading and no %s", p.patroni.at, p.Site)
		if p.endWantsSync() {
			gate += ", with a Sync Standby"
		}
		gates = append(gates, gate)
	}
	if p.garage.anchor != "" {
		if p.garage.node != "" {
			st.Steps = append(st.Steps,
				Step{Site: p.garage.anchor, Verb: "remove", Text: fmt.Sprintf("%s's node %s from Garage's layout: %s", p.Site, p.garage.node, gcmd(p, "layout remove "+p.garage.node))},
				Step{Site: p.garage.anchor, Verb: "apply", Text: fmt.Sprintf("the layout: %s; Garage copies the node's partitions to the nodes that stay", gcmd(p, fmt.Sprintf("layout apply --version %d", p.garage.version+1)))},
			)
		}
		gates = append(gates, fmt.Sprintf("no layout row for %s, and on every remaining Garage site one live layout version and an empty resync queue with no errors, %d reads in a row, within %s", p.Site, garageSamples, garageWait))
	}
	if len(gates) == 0 {
		st.Gate = fmt.Sprintf("none: %s holds no database and no objects", p.Site)
		return st
	}
	st.Gate = strings.Join(gates, "; ")
	st.run = p.runData
	st.gate = func() error {
		if inCluster {
			if err := p.patroniGate(); err != nil {
				return err
			}
		}
		if p.garage.anchor != "" {
			return p.garageGate()
		}
		return nil
	}
	return st
}

func (p *Plan) runData() error {
	at := p.transports[p.patroni.at]
	if p.patroni.leader == p.Site {
		p.say("  %-9s the leader to %s\n", "switch", p.patroni.candidate)
		t := p.transports[p.patroni.candidate]
		if out, err := t.Run(failover.SwitchoverCommand(p.dep(), p.Site, p.patroni.candidate)); err != nil {
			return fmt.Errorf("%s: switchover: %w: %s", p.patroni.candidate, err, lastLines(out, 3))
		}
		err := poll(switchWait, switchPoll, func() error {
			list, err := p.patroniList()
			if err != nil {
				return err
			}
			for _, m := range list {
				if m.role() == "Leader" && m.state() == "running" {
					if m.name() == p.patroni.candidate {
						return nil
					}
					return fmt.Errorf("%s leads", m.name())
				}
			}
			return fmt.Errorf("no member leads")
		})
		if err != nil {
			return fmt.Errorf("the switchover to %s did not complete within %s: %w. Read `patronictl list` on %s", p.patroni.candidate, switchWait, err, p.patroni.at)
		}
	}
	if p.patroni.sync && len(p.end.Cluster.Sites) < 2 && contains(p.cfg.Cluster.Sites, p.Site) {
		p.say("  %-9s synchronous_mode off\n", "set")
		if out, err := at.Run(p.patronictl("edit-config --force -q -s synchronous_mode=false -s synchronous_mode_strict=false")); err != nil {
			return fmt.Errorf("%s: patronictl edit-config: %w: %s", p.patroni.at, err, lastLines(out, 3))
		}
	}
	if p.patroni.listed {
		if !p.HostGone {
			p.say("  %-9s Patroni on %s\n", "stop", p.Site)
			if out, err := p.transports[p.Site].Run(p.stopPatroni()); err != nil {
				return fmt.Errorf("%s: stopping Patroni: %w: %s", p.Site, err, lastLines(out, 3))
			}
		}
		if out, err := p.transports[p.etcd.control].Run(apply.Etcdctl(p.dep(), "del "+p.memberKey())); err != nil {
			return fmt.Errorf("%s: deleting %s: %w: %s", p.etcd.control, p.memberKey(), err, lastLines(out, 3))
		}
	}
	if p.garage.node != "" {
		t := p.transports[p.garage.anchor]
		p.say("  %-9s %s's Garage node from the layout\n", "remove", p.Site)
		if out, err := t.Run(gcmd(p, "layout remove "+p.garage.node)); err != nil {
			return fmt.Errorf("%s: `garage layout remove`: %w: %s", p.garage.anchor, err, lastLines(out, 3))
		}
		if out, err := t.Run(gcmd(p, fmt.Sprintf("layout apply --version %d", p.garage.version+1))); err != nil {
			return fmt.Errorf("%s: `garage layout apply`: %w: %s", p.garage.anchor, err, lastLines(out, 3))
		}
	}
	return nil
}

func (p *Plan) patroniGate() error {
	err := poll(patroniWait, patroniPoll, func() error {
		list, err := p.patroniList()
		if err != nil {
			return err
		}
		leader, sync := "", false
		for _, m := range list {
			if m.name() == p.Site {
				return fmt.Errorf("%s is still listed, %s", p.Site, m.state())
			}
			if m.role() == "Leader" && m.state() == "running" {
				leader = m.name()
			}
			if m.role() == "Sync Standby" {
				sync = true
			}
		}
		if leader == "" {
			return fmt.Errorf("no member leads")
		}
		if p.endWantsSync() && !sync {
			return fmt.Errorf("no member is a Sync Standby")
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("Patroni after %s: %w", patroniWait, err)
	}
	return nil
}

// garageGate waits, within garageWait, for the layout to have no row for the
// site and every remaining node to have finished moving data: one live
// layout version, then an empty resync queue with no errors on every node,
// garageSamples reads in a row. The queue is read only once the layout is
// stable, as storage add reads it.
func (p *Plan) garageGate() error {
	sites := p.end.Storage.Garage.Sites
	err := poll(garageWait, garagePoll, func() error {
		out, err := p.transports[p.garage.anchor].Run(gcmd(p, "layout show"))
		if err != nil {
			return fmt.Errorf("%s: `garage layout show`: %w: %s", p.garage.anchor, err, lastLines(out, 3))
		}
		_, zones, err := storageadd.LayoutZones(out)
		if err != nil {
			return err
		}
		for id, zone := range zones {
			if zone == p.Site {
				return fmt.Errorf("the layout still has %s's node %s", p.Site, id)
			}
		}
		for _, site := range sites {
			out, err := p.transports[site].Run(gcmd(p, "layout history"))
			if err != nil {
				return fmt.Errorf("%s: `garage layout history`: %w: %s", site, err, lastLines(out, 3))
			}
			if !strings.Contains(out, storageadd.StableLayout) {
				return fmt.Errorf("%s reports more than one live layout version, so metadata is still moving", site)
			}
		}
		for i := 0; i < garageSamples; i++ {
			if i > 0 {
				sleep(garageGap)
			}
			for _, site := range sites {
				out, err := p.transports[site].Run(gcmd(p, "stats"))
				if err != nil {
					return fmt.Errorf("%s: `garage stats`: %w: %s", site, err, lastLines(out, 3))
				}
				queue, errs, err := storageadd.ResyncQueue(out)
				if err != nil {
					return fmt.Errorf("%s: %w", site, err)
				}
				if queue > 0 || errs > 0 {
					return fmt.Errorf("%s's resync queue is %d with %d error(s), so blocks are still moving", site, queue, errs)
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("Garage has not settled after %s: %w. Garage carries on copying; run site remove again to resume at this gate", garageWait, err)
	}
	return nil
}

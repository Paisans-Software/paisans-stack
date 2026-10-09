package siteadd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// patronictl runs inside a site's Patroni container, against the
// configuration file Spilo writes for it (zalando/spilo, postgres-appliance/
// scripts/configure_spilo.py, which writes postgres.yml under PGHOME,
// /home/postgres). The host needs nothing beyond Docker.
func (p *Plan) patronictl(args string) string {
	return p.dep().ComposeCmd("infra") + " exec -T patroni patronictl -c /home/postgres/postgres.yml " + args
}

// noPatroni is what the list probe prints where no Patroni runs.
const noPatroni = "__PAISANS_NO_PATRONI__"

// startInfra starts whatever of the new site's infrastructure stack is not
// running, which after stage 3 is Patroni alone: etcd is running with an
// unchanged definition, and `up -d` leaves such a container be.
func (p *Plan) startInfra() string { return p.dep().ComposeCmd("infra") + " up -d" }

// patroniMember is one row of `patronictl list -f json`. The keys are the
// column titles (Patroni v4.1.0, patroni/ctl.py, output_members): Role is
// "Leader", "Sync Standby" or "Replica", and State for a replica that is
// replicating is "streaming". Lag is in MB, a number, or an empty string when
// unknown.
type patroniMember map[string]any

func (m patroniMember) str(key string) string {
	s, _ := m[key].(string)
	return s
}

func (m patroniMember) name() string  { return m.str("Member") }
func (m patroniMember) role() string  { return m.str("Role") }
func (m patroniMember) state() string { return m.str("State") }

// lag is the member's replay lag in MB. Patroni 4 splits the old "Lag in MB"
// column into receive and replay lag; replay is what a read on the replica
// sees, so it is preferred, and the older column is still read.
func (m patroniMember) lag() (float64, bool) {
	for _, key := range []string{"Replay Lag", "Lag in MB", "Receive Lag"} {
		if v, ok := m[key].(float64); ok {
			return v, true
		}
	}
	return 0, false
}

func parsePatroniList(out string) ([]patroniMember, error) {
	var list []patroniMember
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &list); err != nil {
		return nil, fmt.Errorf("patronictl list is not JSON: %w\n%s", err, out)
	}
	return list, nil
}

func (p *Plan) patroniList() ([]patroniMember, error) {
	out, err := p.transports[p.patroni].Run(p.patronictl("list -f json"))
	if err != nil {
		return nil, fmt.Errorf("%s: patronictl list: %s", p.patroni, lastLines(out, 3))
	}
	return parsePatroniList(out)
}

// probePatroni finds an existing cluster site whose Patroni answers, and the
// members it reports.
func (p *Plan) probePatroni() ([]patroniMember, error) {
	for _, name := range p.cfg.Cluster.Sites {
		if name == p.Site {
			continue
		}
		command := fmt.Sprintf(
			"if %s ps --status running --quiet patroni 2>/dev/null | grep -q .; then %s; else echo %s; fi",
			p.dep().ComposeCmd("infra"), p.patronictl("list -f json"), noPatroni)
		out, err := p.transports[name].Run(command)
		if err != nil {
			return nil, fmt.Errorf("%s: asking Patroni for its members: %s", name, lastLines(out, 3))
		}
		if strings.TrimSpace(out) == noPatroni {
			continue
		}
		members, err := parsePatroniList(out)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		p.patroni = name
		return members, nil
	}
	return nil, fmt.Errorf("site add %s: no Patroni runs on any existing site of cluster.sites, so there is no leader to clone. Apply the existing data site first", p.Site)
}

func findMember(members []patroniMember, name string) patroniMember {
	for _, m := range members {
		if m.name() == name {
			return m
		}
	}
	return nil
}

// buildReplica is stage 4: the new site's Patroni, which Spilo bootstraps as
// a replica of the leader with pg_basebackup.
func (p *Plan) buildReplica(members []patroniMember) *Stage {
	st := &Stage{
		Number: 4,
		Name:   "replica",
		Short:  p.Site + " streams",
		Gate: fmt.Sprintf("`patronictl list` on %s shows %s streaming, with its replay lag falling, or zero, across %d samples %s apart",
			p.patroni, p.Site, lagSamples, lagInterval),
	}
	if m := findMember(members, p.Site); m != nil && m.state() == "streaming" {
		st.gate = p.replicaGate
		return st
	}
	st.Steps = append(st.Steps,
		Step{Site: p.Site, Verb: "apply", Title: "apply missing files on " + p.Site, Text: "any file of its own still missing, as `paisans apply --site " + p.Site + "` would"},
		Step{Site: p.Site, Verb: "start", Title: "start Patroni on " + p.Site, Text: "Patroni, which clones the leader with pg_basebackup: " + p.startInfra()},
	)
	st.run = func() error {
		if _, ok := p.initial[p.Site]; !ok {
			return fmt.Errorf("%s has no record of how its etcd member was started, so its infrastructure cannot be rendered without risking a founder's flags. Stage 3 writes it; run site add again", p.Site)
		}
		rendered, err := p.render()
		if err != nil {
			return err
		}
		t := p.transports[p.Site]
		var opts []apply.Option
		if p.KeepImages {
			opts = append(opts, apply.KeepImages())
		}
		whole, err := apply.Build(p.Site, rendered, p.acmeModule(), t, opts...)
		if err != nil {
			return err
		}
		// The whole apply reports its own steps, so no step of this stage is
		// open while it runs: two cannot be at once.
		p.idle()
		whole.Report = p.reporter()
		if err := apply.Execute(whole, t); err != nil {
			return err
		}
		p.work("start Patroni on "+p.Site).Detail("%s", p.startInfra())
		if out, err := t.Run(p.startInfra()); err != nil {
			return fmt.Errorf("%s: starting Patroni: %s", p.Site, lastLines(out, 5))
		}
		return nil
	}
	st.gate = p.replicaGate
	return st
}

func (p *Plan) replicaGate() error {
	var last string
	n := attempts(streamWait, streamPoll)
	streaming := false
	for i := 0; i < n; i++ {
		members, err := p.patroniList()
		if err != nil {
			last = err.Error()
		} else if m := findMember(members, p.Site); m == nil {
			last = p.Site + " is not a member yet"
		} else if m.state() != "streaming" {
			last = fmt.Sprintf("%s is %s", p.Site, m.state())
		} else {
			streaming = true
			break
		}
		if i < n-1 {
			sleep(streamPoll)
		}
	}
	if !streaming {
		return fmt.Errorf("%s is not streaming after %s: %s. A large database takes a while to clone, and Patroni carries on meanwhile; read `%s logs patroni` on %s, and run site add again", p.Site, streamWait, last, p.dep().ComposeCmd("infra"), p.Site)
	}

	var samples []float64
	for i := 0; i < lagSamples; i++ {
		if i > 0 {
			sleep(lagInterval)
		}
		members, err := p.patroniList()
		if err != nil {
			return err
		}
		m := findMember(members, p.Site)
		if m == nil {
			return fmt.Errorf("%s left the member list while its lag was sampled", p.Site)
		}
		lag, ok := m.lag()
		if !ok {
			return fmt.Errorf("%s's lag is unknown in `patronictl list`", p.Site)
		}
		samples = append(samples, lag)
	}
	if lagConverging(samples) {
		return nil
	}
	return fmt.Errorf("%s streams, but its replay lag is not falling: %v MB, %s apart", p.Site, samples, lagInterval)
}

// lagConverging passes a lag that ends at zero, or that never rises and ends
// lower than it began.
func lagConverging(samples []float64) bool {
	if len(samples) == 0 {
		return false
	}
	last := samples[len(samples)-1]
	if last == 0 {
		return true
	}
	for i := 1; i < len(samples); i++ {
		if samples[i] > samples[i-1] {
			return false
		}
	}
	return last < samples[0]
}

// editSynchronous sets both keys in Patroni's dynamic configuration, in etcd.
// `-s` sets one key, parsing its value as YAML, and may be repeated; --force
// applies without asking; -q skips printing the diff (Patroni v4.1.0,
// patroni/ctl.py, edit_config). Spilo's own bootstrap.dcs values only seed a
// new cluster, so a cluster already running needs this.
func (p *Plan) editSynchronous(strict bool) string {
	return p.patronictl("edit-config --force -q -s synchronous_mode=true -s synchronous_mode_strict=" + strconv.FormatBool(strict))
}

// buildClusterConfig is stage 5: synchronous mode, when the configuration
// asks for it and the live configuration differs.
func (p *Plan) buildClusterConfig() (*Stage, error) {
	st := &Stage{Number: 5, Name: "cluster configuration", Short: "synchronous standby"}
	if !p.cfg.Cluster.Synchronous {
		st.Short = "synchronous mode is off"
		st.Gate = "none: cluster.synchronous is false, and site add does not turn synchronous mode off"
		return st, nil
	}
	st.Gate = fmt.Sprintf("`patronictl list` on %s shows a Sync Standby", p.patroni)
	out, err := p.transports[p.patroni].Run(p.patronictl("show-config"))
	if err != nil {
		return nil, fmt.Errorf("%s: patronictl show-config: %s", p.patroni, lastLines(out, 3))
	}
	var live map[string]any
	if err := yaml.Unmarshal([]byte(out), &live); err != nil {
		return nil, fmt.Errorf("%s: patronictl show-config is not YAML: %w", p.patroni, err)
	}
	wantStrict := strconv.FormatBool(p.cfg.Cluster.SynchronousStrict)
	if fmt.Sprint(live["synchronous_mode"]) != "true" || boolString(live["synchronous_mode_strict"]) != wantStrict {
		st.Steps = append(st.Steps, Step{Site: p.patroni, Verb: "set", Title: "set synchronous mode", Text: fmt.Sprintf("synchronous_mode=true and synchronous_mode_strict=%s in Patroni's dynamic configuration (live: %s and %s)", wantStrict, boolString(live["synchronous_mode"]), boolString(live["synchronous_mode_strict"]))})
	}
	st.run = func() error {
		p.work("set synchronous mode")
		if out, err := p.transports[p.patroni].Run(p.editSynchronous(p.cfg.Cluster.SynchronousStrict)); err != nil {
			return fmt.Errorf("%s: patronictl edit-config: %s", p.patroni, lastLines(out, 3))
		}
		return nil
	}
	st.gate = func() error {
		var roles []string
		n := attempts(syncWait, syncPoll)
		for i := 0; i < n; i++ {
			members, err := p.patroniList()
			if err != nil {
				return err
			}
			roles = nil
			for _, m := range members {
				if m.role() == "Sync Standby" {
					return nil
				}
				roles = append(roles, m.name()+" "+m.role())
			}
			if i < n-1 {
				sleep(syncPoll)
			}
		}
		return fmt.Errorf("no Sync Standby after %s; the members are %s", syncWait, strings.Join(roles, ", "))
	}
	return st, nil
}

// boolString reads an absent key as false, which is Patroni's default for
// both synchronous keys.
func boolString(v any) string {
	if v == nil {
		return "false"
	}
	return fmt.Sprint(v)
}

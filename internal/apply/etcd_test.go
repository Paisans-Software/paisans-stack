package apply_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// A host applied before the etcd record existed gains it on its next apply,
// and that write alone acts on no stack: no container reads the record, and
// restarting etcd and Patroni for it would be an outage for nothing.
func TestTheEtcdRecordAloneActsOnNoStack(t *testing.T) {
	host := newHost()
	rendered := plan(t)
	first, err := apply.Build("home-a", rendered, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}
	// As an older toolkit left it: no record, and none in the manifest.
	delete(host.files, "/"+"srv/paisans/f2a9/infra/etcd-initial")
	var m render.Manifest
	if err := json.Unmarshal([]byte(host.files["/srv/paisans/f2a9/.paisans-manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	var kept []render.ManifestFile
	for _, f := range m.Files {
		if f.Path != "srv/paisans/f2a9/infra/etcd-initial" {
			kept = append(kept, f)
		}
	}
	m.Files = kept
	data, _ := json.Marshal(m)
	host.files["/srv/paisans/f2a9/.paisans-manifest.json"] = string(data)

	second, err := apply.Build("home-a", rendered, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Writes()) != 1 || second.Writes()[0].Path != "/"+"srv/paisans/f2a9/infra/etcd-initial" {
		t.Fatalf("want only the record written, got %v", second.Writes())
	}
	if len(second.Actions) != 0 {
		t.Errorf("writing the record acts on %v", second.Actions)
	}
}

// The record wins, and a host without one keeps the flags its compose file
// already runs with.
func TestReadEtcdInitial(t *testing.T) {
	host := newHost()
	if _, found, err := apply.ReadEtcdInitial(host, apply.Fixture); err != nil || found {
		t.Fatalf("an empty host reported a record: %v %v", found, err)
	}

	host.files["/srv/paisans/f2a9/infra/compose.yaml"] = "services:\n  etcd:\n    command:\n      - --initial-cluster=home-a=http://10.44.0.1:2380\n      - --initial-cluster-state=new\n"
	in, found, err := apply.ReadEtcdInitial(host, apply.Fixture)
	if err != nil || !found || in.State != "new" || in.Cluster != "home-a=http://10.44.0.1:2380" {
		t.Fatalf("compose fallback: %+v %v %v", in, found, err)
	}

	host.files["/"+"srv/paisans/f2a9/infra/etcd-initial"] = render.FormatEtcdInitial(render.EtcdInitial{State: "existing", Cluster: "a=http://x:2380,vm=http://y:2380"})
	in, found, err = apply.ReadEtcdInitial(host, apply.Fixture)
	if err != nil || !found || in.State != "existing" || !strings.Contains(in.Cluster, "vm=") {
		t.Fatalf("record: %+v %v %v", in, found, err)
	}

	host.files["/"+"srv/paisans/f2a9/infra/etcd-initial"] = "garbage\n"
	if _, _, err := apply.ReadEtcdInitial(host, apply.Fixture); err == nil {
		t.Error("an unreadable record was accepted")
	}
}

const memberListOne = `{"header":{"cluster_id":1,"member_id":2,"raft_term":3},"members":[{"ID":12345678901234567890,"name":"home-a","peerURLs":["http://10.44.0.1:2380"],"clientURLs":["http://10.44.0.1:2379"]}]}`

func fixtureConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestParseEtcdMembers(t *testing.T) {
	members, err := apply.ParseEtcdMembers(memberListOne + "\n")
	if err != nil || len(members) != 1 {
		t.Fatalf("%v %v", members, err)
	}
	if members[0].HexID() != "ab54a98ceb1f0ad2" {
		t.Errorf("the ID lost precision or is not hex: %s", members[0].HexID())
	}
	unstarted := apply.EtcdMember{ID: 1, PeerURLs: []string{"http://10.44.0.3:2380"}, IsLearner: true}
	if got := apply.EtcdMemberSite(fixtureConfig(t), unstarted); got != "vm" {
		t.Errorf("an unstarted member is named %q, want vm by its peer URL", got)
	}
}

// apply on a site of a cluster whose live membership differs from
// etcd.members refuses to touch the infrastructure stack, and says to use
// site add; the same apply against a matching membership proceeds.
func TestApplyRefusesAHalfGrownEtcd(t *testing.T) {
	cfg := fixtureConfig(t) // etcd.members: home-a, home-b, vm
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	members, _ := apply.ParseEtcdMembers(memberListOne)
	err = apply.EtcdRefusal(cfg, p, members)
	if err == nil || !strings.Contains(err.Error(), "paisans site add") {
		t.Fatalf("a half grown cluster was not refused: %v", err)
	}

	all := append(members,
		apply.EtcdMember{ID: 2, Name: "home-b", PeerURLs: []string{"http://10.44.0.2:2380"}},
		apply.EtcdMember{ID: 3, Name: "vm", PeerURLs: []string{"http://10.44.0.3:2380"}})
	if err := apply.EtcdRefusal(cfg, p, all); err != nil {
		t.Errorf("a matching membership was refused: %v", err)
	}
	all[2].IsLearner = true
	if err := apply.EtcdRefusal(cfg, p, all); err == nil {
		t.Error("a learner still catching up was not refused")
	}
}

// A data site founding its etcd member waits for the witness: until the
// witness runs, the new cluster cannot settle its version and no primary
// appears. A member that has run before is never held up by a witness that
// is down, which is the outage a witness exists to ride out, and the witness
// itself waits on nobody.
func TestTheWitnessGoesFirst(t *testing.T) {
	cfg := fixtureConfig(t) // home-a, home-b data; vm the witness
	p, err := apply.Build("home-a", plan(t), acmeModule(t), newHost())
	if err != nil {
		t.Fatal(err)
	}
	err = apply.WitnessFirstRefusal(cfg, p, true, map[string]bool{"home-b": true})
	if err == nil || !strings.Contains(err.Error(), "`paisans apply --site vm`") {
		t.Fatalf("a data site was founded before the witness: %v", err)
	}
	if err := apply.WitnessFirstRefusal(cfg, p, true, map[string]bool{"vm": true}); err != nil {
		t.Errorf("a running witness did not let the data site through: %v", err)
	}
	if err := apply.WitnessFirstRefusal(cfg, p, false, nil); err != nil {
		t.Errorf("a member past bootstrap was held up by a witness that is down: %v", err)
	}
	if got := apply.WitnessesFirst(cfg, "vm"); len(got) != 0 {
		t.Errorf("the witness waits on %v", got)
	}
	if got := apply.WitnessesFirst(cfg, "home-b"); len(got) != 1 || got[0] != "vm" {
		t.Errorf("home-b waits on %v, want vm", got)
	}
}

// The founding stop names every other member not running, in the order the
// configuration lists them, and not the site being applied.
func TestFoundingUnstarted(t *testing.T) {
	cfg := fixtureConfig(t)
	got := apply.FoundingUnstarted(cfg, "home-a", map[string]bool{"vm": true})
	if len(got) != 1 || got[0] != "home-b" {
		t.Errorf("got %v, want [home-b]", got)
	}
	if got := apply.FoundingUnstarted(cfg, "home-b", map[string]bool{"home-a": true, "vm": true}); len(got) != 0 {
		t.Errorf("the last founder still waits on %v", got)
	}
}

// The probe asks the site first and then the other configured members, and
// a host with no etcd running is an answer rather than an error.
func TestProbeEtcdMembers(t *testing.T) {
	cfg := fixtureConfig(t)
	quiet := &scriptHost{fakeHost: newHost(), answer: "__PAISANS_NO_ETCD__\n"}
	live := &scriptHost{fakeHost: newHost(), answer: memberListOne}
	members, found, err := apply.ProbeEtcdMembers(cfg, "vm", map[string]apply.Transport{"vm": quiet, "home-a": live}, false)
	if err != nil || !found || len(members) != 1 {
		t.Fatalf("%v %v %v", members, found, err)
	}
	_, found, err = apply.ProbeEtcdMembers(cfg, "vm", map[string]apply.Transport{"vm": quiet}, false)
	if err != nil || found {
		t.Errorf("no etcd anywhere reported a membership: %v %v", found, err)
	}
}

// etcdHost is a host whose etcd container is running or not, and whose etcd
// either has a leader or is a founding member alone, the way etcd v3.5.16
// answers each: `member list` times out without a leader, and `endpoint
// status` answers with leader 0. statusFails makes `endpoint status` fail
// too, as a member that answers nothing does.
type etcdHost struct {
	*fakeHost
	up, leaderless, statusFails bool
}

const deadline = `{"level":"warn","msg":"retrying of unary invoker failed","error":"rpc error: code = DeadlineExceeded desc = context deadline exceeded"}
Error: context deadline exceeded`

func (h *etcdHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	switch {
	case strings.Contains(command, "member list -w json"):
		if !h.up {
			return "__PAISANS_NO_ETCD__\n", nil
		}
		if h.leaderless {
			return deadline, fmt.Errorf("exit status 1")
		}
		return memberListOne, nil
	case strings.Contains(command, "endpoint status -w json"):
		if !h.up || h.statusFails {
			return deadline, fmt.Errorf("exit status 1")
		}
		leader := `,"leader":12345678901234567890`
		if h.leaderless {
			leader = ""
		}
		return `{"level":"warn","msg":"a line etcdctl logs first"}
[{"Endpoint":"http://127.0.0.1:2379","Status":{"header":{"cluster_id":1,"member_id":2},"version":"3.5.16"` + leader + `,"raftTerm":2}}]`, nil
	case strings.Contains(command, "ps --status running --quiet etcd"):
		if h.up {
			return "running\n", nil
		}
		return "__PAISANS_NO_ETCD__\n", nil
	}
	return h.fakeHost.Run(command)
}

// The first data site of a new deployment is applied after the witness,
// whose etcd runs alone with no leader. It is not refused: the witness counts
// as running without asking etcd, the membership probe passes over a
// leaderless founding member, and the plan carries the founding stop naming
// the data site still to apply.
func TestAFoundingDataSiteGetsPastALeaderlessWitness(t *testing.T) {
	cfg := fixtureConfig(t)
	p, err := apply.Build("home-a", plan(t), acmeModule(t), newHost())
	if err != nil {
		t.Fatal(err)
	}
	witness := &etcdHost{fakeHost: newHost(), up: true, leaderless: true}
	transports := map[string]apply.Transport{
		"home-a": &etcdHost{fakeHost: newHost()},
		"home-b": &etcdHost{fakeHost: newHost()},
		"vm":     witness,
	}
	running, err := apply.EtcdRunning(cfg, transports)
	if err != nil {
		t.Fatalf("asking which members run: %v", err)
	}
	for _, c := range witness.commands {
		if strings.Contains(c, "etcdctl") {
			t.Errorf("asking whether etcd runs called etcdctl: %s", c)
		}
	}
	if !running["vm"] || running["home-b"] {
		t.Fatalf("running: %v", running)
	}
	if got := apply.FoundingUnstarted(cfg, "home-a", running); len(got) != 1 || got[0] != "home-b" {
		t.Errorf("the founding stop names %v, want [home-b]", got)
	}
	if err := apply.EtcdGates(cfg, p, transports, true, running); err != nil {
		t.Errorf("a founding data site was refused behind a leaderless witness: %v", err)
	}
}

// Outside founding, a member with no leader is a cluster in trouble, not one
// being born, and apply stops on it as it always has.
func TestALeaderlessEtcdStopsAnApplyPastFounding(t *testing.T) {
	cfg := fixtureConfig(t)
	p, err := apply.Build("home-a", plan(t), acmeModule(t), newHost())
	if err != nil {
		t.Fatal(err)
	}
	transports := map[string]apply.Transport{
		"home-a": &etcdHost{fakeHost: newHost(), up: true, leaderless: true},
		"home-b": &etcdHost{fakeHost: newHost(), up: true, leaderless: true},
		"vm":     &etcdHost{fakeHost: newHost(), up: true, leaderless: true},
	}
	err = apply.EtcdGates(cfg, p, transports, false, nil)
	if err == nil || !strings.Contains(err.Error(), "asking etcd for its members") {
		t.Errorf("a leaderless etcd past founding was let through: %v", err)
	}
}

// A member whose `member list` failed is passed over only when `endpoint
// status` confirms it has no leader. One that answers neither is an error,
// founding or not: a failed probe says nothing about the host.
func TestAnUnansweredEtcdIsNeverReadAsLeaderless(t *testing.T) {
	cfg := fixtureConfig(t)
	transports := map[string]apply.Transport{
		"home-a": &etcdHost{fakeHost: newHost()},
		"vm":     &etcdHost{fakeHost: newHost(), up: true, leaderless: true, statusFails: true},
	}
	_, _, err := apply.ProbeEtcdMembers(cfg, "home-a", transports, true)
	if err == nil || !strings.HasPrefix(err.Error(), "vm: ") {
		t.Errorf("an etcd answering nothing was passed over: %v", err)
	}
	if _, err := apply.EtcdLeaderless(transports["vm"], apply.Fixture); err == nil {
		t.Error("a failed endpoint status reported a leader state")
	}
	led := &etcdHost{fakeHost: newHost(), up: true}
	if leaderless, err := apply.EtcdLeaderless(led, apply.Fixture); err != nil || leaderless {
		t.Errorf("a member with a leader: leaderless %v, %v", leaderless, err)
	}
}

// scriptHost answers the membership probe with a fixed output.
type scriptHost struct {
	*fakeHost
	answer string
}

func (h *scriptHost) Run(command string) (string, error) {
	if strings.Contains(command, "member list -w json") {
		return h.answer, nil
	}
	return h.fakeHost.Run(command)
}

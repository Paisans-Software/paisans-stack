package siteremove_test

import (
	"encoding/json"
	"github.com/paisans-software/paisans-stack/internal/ui"
	"os"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// twoAndAWitness is home-a and home-b holding the data, vm the gateway and
// the witness: three etcd voters, two of them data sites. home-c only stores
// objects, so Garage keeps two nodes when home-b goes.
var twoAndAWitness = []func(string) string{
	replace("  home-c:\n    roles: [data]", "  home-c:\n    roles: [storage]"),
	replace("sites: [home-a, home-b, home-c]\n  port", "sites: [home-a, home-b]\n  port"),
	replace("members: [home-a, home-b, home-c, vm]", "members: [home-a, home-b, vm]"),
}

func witnessWorld(t *testing.T, edits ...func(string) string) *world {
	t.Helper()
	w := newWorld(t, append(append([]func(string) string(nil), twoAndAWitness...), edits...)...)
	t.Cleanup(siteremove.SetFast())
	t.Cleanup(siteremove.SetInspect(w.inspect))
	return w
}

// index is where the first command containing sub ran in the world's log, -1
// when none did.
func (w *world) index(sub string) int {
	for i, c := range w.log {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// Taking one data site out of two data sites and a witness takes the witness
// out of etcd too, after the data site's member, and leaves one voter.
func TestTwoDataSitesAndAWitnessShrinkToOneVoter(t *testing.T) {
	w := witnessWorld(t)
	p := w.mustBuild("home-b", siteremove.Options{})
	for _, want := range []struct {
		stage            int
		site, verb, text string
	}{
		{1, "home-a", "set", "synchronous_mode=false"},
		{2, "home-a", "remove", "home-b's etcd member 2222"},
		{2, "home-a", "check", "home-a and vm, both healthy"},
		{2, "home-a", "remove", "the witness vm's etcd member 3333"},
		{2, "vm", "stop", "label=com.docker.compose.service=etcd"},
		{2, "vm", "update", "infra/compose.yaml"},
		{4, "vm", "remove", "vm from etcd.members and witness from sites.vm.roles"},
	} {
		if !hasStep(p, want.stage, want.site, want.verb, want.text) {
			t.Errorf("stage %d has no %s %s step with %q:\n%s", want.stage, want.verb, want.site, want.text, printed(p))
		}
	}
	rec := &ui.Recorder{}
	p.Report = rec
	if err := siteremove.Execute(p); err != nil {
		t.Fatalf("%v\n%s", err, rec.Lines())
	}

	if len(w.etcd) != 1 || w.etcd[0].Name != "home-a" {
		t.Errorf("etcd after: %v", w.etcd)
	}
	if first, second := w.index("member remove 2222"), w.index("member remove 3333"); first < 0 || second < first {
		t.Errorf("the members were removed in the order %d, %d", first, second)
	}
	vm := w.hosts["vm"]
	if vm.etcdContainer {
		t.Error("the witness's etcd still runs")
	}
	if strings.Contains(vm.files[root+"/infra/compose.yaml"], "image: gcr.io/etcd-development/etcd") {
		t.Error("the witness's compose file still declares etcd")
	}
	if !strings.Contains(vm.files[root+"/infra/compose.yaml"], "caddy") {
		t.Error("the witness's compose file lost its Caddy")
	}
	if len(w.recreated) != 0 {
		t.Errorf("Patroni was recreated on %v, and home-a, the leader, is the only data site", w.recreated)
	}

	cfg, err := config.Load(w.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Etcd.Members, ",") != "home-a" || cfg.Sites["vm"].Has(config.RoleWitness) || !cfg.Sites["vm"].Has(config.RoleGateway) {
		t.Errorf("paisans.yaml after: etcd.members %v, vm's roles %v", cfg.Etcd.Members, cfg.Sites["vm"].Roles)
	}
	if result := validate.Check(cfg); result.Refused() {
		t.Errorf("the edited paisans.yaml is refused: %v", result.Refusals())
	}
	data, _ := os.ReadFile(w.configPath)
	if !strings.Contains(string(data), "# The monitor's own machine") {
		t.Error("the edit lost a comment")
	}
	remains := strings.Join(p.Remains(), "\n")
	for _, want := range []string{"home-a leads", "next restart", "etcd-initial is no longer rendered"} {
		if !strings.Contains(remains, want) {
			t.Errorf("the report does not say %q:\n%s", want, remains)
		}
	}
}

// A run stopped after the data site's member went resumes with only the
// witness's removal left in etcd.
func TestTheShrinkResumesAfterTheFirstMemberRemoval(t *testing.T) {
	w := witnessWorld(t)
	w.failOnce = "member remove 3333"
	err := siteremove.Execute(w.mustBuild("home-b", siteremove.Options{}))
	if err == nil || !strings.Contains(err.Error(), "stopped at stage 2") {
		t.Fatalf("err = %v", err)
	}
	if len(w.etcd) != 2 {
		t.Fatalf("etcd after the first removal: %v", w.etcd)
	}
	again := w.mustBuild("home-b", siteremove.Options{})
	if hasStep(again, 2, "home-a", "remove", "home-b's etcd member") {
		t.Errorf("the resumed plan removes home-b's member again:\n%s", printed(again))
	}
	if !hasStep(again, 2, "home-a", "remove", "the witness vm's etcd member 3333") || !hasStep(again, 2, "vm", "stop", "etcd") {
		t.Errorf("the resumed plan does not take the witness out:\n%s", printed(again))
	}
	if err := siteremove.Execute(again); err != nil {
		t.Fatal(err)
	}
	if len(w.etcd) != 1 || w.etcd[0].Name != "home-a" {
		t.Errorf("etcd after: %v", w.etcd)
	}
}

// Two voters go to one only when both are healthy.
func TestTheShrinkRefusesAnUnhealthyWitness(t *testing.T) {
	w := witnessWorld(t)
	w.etcdDown["vm"] = true
	_, err := w.build("home-b", siteremove.Options{})
	if err == nil || !strings.Contains(err.Error(), "vm's etcd is unhealthy") || !strings.Contains(err.Error(), "safe only while both are healthy") {
		t.Fatalf("err = %v", err)
	}
	for name, h := range w.hosts {
		for _, c := range h.commands {
			if strings.Contains(c, "member remove") || strings.Contains(c, "docker stop") {
				t.Errorf("a refused removal ran %q on %s", c, name)
			}
		}
	}
}

// A witness whose only role is the witness would be left with none, which is
// refused unless an app is pinned to it.
func TestAWitnessWithNoOtherRole(t *testing.T) {
	box := []func(string) string{
		replace("  vm:\n    roles: [gateway, witness]", "  box:\n    roles: [witness]\n    address: 10.44.0.6\n    ssh:\n      host: box.local\n      user: ubuntu\n      keys:\n        alice: "+alice+"\n  vm:\n    roles: [gateway]"),
		replace("members: [home-a, home-b, vm]", "members: [home-a, home-b, box]"),
	}
	t.Run("refused", func(t *testing.T) {
		cfg, _, _ := worldConfig(t, append(append([]func(string) string(nil), twoAndAWitness...), box...)...)
		err := siteremove.Refusal(cfg, "home-b", siteremove.Options{})
		if err == nil || !strings.Contains(err.Error(), "Witness is its only role") || !strings.Contains(err.Error(), "paisans site remove box") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an app pinned to it", func(t *testing.T) {
		edits := append(append(append([]func(string) string(nil), twoAndAWitness...), box...), replace("placement: { pinned: home-a }\n  status", "placement: { pinned: box }\n  status"))
		cfg, _, path := worldConfig(t, edits...)
		if err := siteremove.Refusal(cfg, "home-b", siteremove.Options{}); err != nil {
			t.Fatal(err)
		}
		end, witness, err := siteremove.EndState(cfg, "home-b")
		if err != nil || witness != "box" || len(end.Sites["box"].Roles) != 0 {
			t.Fatalf("end state: witness %q, box's roles %v, err %v", witness, end.Sites["box"].Roles, err)
		}
		if err := config.RemoveSiteAndWitness(path, "home-b", "box"); err != nil {
			t.Fatal(err)
		}
		after, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Sites["box"].Roles) != 0 || strings.Join(after.Etcd.Members, ",") != "home-a" {
			t.Errorf("box's roles %v, etcd.members %v", after.Sites["box"].Roles, after.Etcd.Members)
		}
		if result := validate.Check(after); result.Refused() {
			t.Errorf("the edited paisans.yaml is refused: %v", result.Refusals())
		}
	})
}

// Three data sites going to two still refuses: there is no witness to take
// out, and two data sites must not be two voters.
func TestThreeDataSitesToTwoIsRefused(t *testing.T) {
	cfg, _, _ := worldConfig(t, replace("members: [home-a, home-b, home-c, vm]", "members: [home-a, home-b, home-c]"))
	err := siteremove.Refusal(cfg, "home-c", siteremove.Options{})
	if err == nil || !strings.Contains(err.Error(), "two etcd voters") || !strings.Contains(err.Error(), "Add a witness first") {
		t.Fatalf("err = %v", err)
	}
}

// The same refusal with the lists derived says to give a site the witness
// role, since there is no etcd.members to add a witness to.
func TestThreeDerivedDataSitesToTwoIsRefused(t *testing.T) {
	cfg, _, _ := worldConfig(t,
		replace("  vm:\n    roles: [gateway, witness]", "  vm:\n    roles: [gateway]"),
		replace("  sites: [home-a, home-b, home-c]\n  port", "  port"),
		replace("  members: [home-a, home-b, home-c, vm]\n", ""),
	)
	err := siteremove.Refusal(cfg, "home-c", siteremove.Options{})
	if err == nil || !strings.Contains(err.Error(), "Give a site in a third location, one that fails independently, the witness role first") {
		t.Fatalf("err = %v", err)
	}
}

// fourData adds home-d, so home-b's removal leaves the leader and two
// replicas.
var fourData = []func(string) string{
	replace("  vm:\n    roles: [gateway, witness]", "  home-d:\n    roles: [data]\n    address: 10.44.0.7\n    ssh:\n      host: home-d.local\n      user: ubuntu\n      keys:\n        alice: "+alice+"\n  vm:\n    roles: [gateway, witness]"),
	replace("sites: [home-a, home-b, home-c]\n  port", "sites: [home-a, home-b, home-c, home-d]\n  port"),
	replace("members: [home-a, home-b, home-c, vm]", "members: [home-a, home-b, home-c, home-d, vm]"),
}

// Every remaining replica gets its patroni.env and a recreated Patroni, one
// at a time, each streaming again before the next; the leader's is noted.
func TestReplicasTakePatroniEnvOneAtATime(t *testing.T) {
	w := newWorld(t, fourData...)
	t.Cleanup(siteremove.SetFast())
	t.Cleanup(siteremove.SetInspect(w.inspect))
	p := w.mustBuild("home-b", siteremove.Options{})
	for _, site := range []string{"home-c", "home-d"} {
		if !hasStep(p, 2, site, "update", "patroni.env") || !hasStep(p, 2, site, "recreate", "a replica restart") {
			t.Errorf("%s's patroni.env is not planned:\n%s", site, printed(p))
		}
	}
	if hasStep(p, 2, "home-a", "update", "patroni.env") || hasStep(p, 2, "home-a", "recreate", "") {
		t.Errorf("the leader's Patroni is planned for:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if strings.Join(w.recreated, ",") != "home-c,home-d" {
		t.Errorf("recreated %v", w.recreated)
	}
	c, d := w.index("home-c: docker compose -f "+root+"/infra/compose.yaml up -d --no-deps --force-recreate patroni"), w.index("home-d: docker compose -f "+root+"/infra/compose.yaml up -d --no-deps --force-recreate patroni")
	gated := false
	for _, line := range w.log[c+1 : d] {
		gated = gated || strings.HasPrefix(line, "home-c: ") && strings.Contains(line, "python3 -c") && strings.Contains(line, "http://10.44.0.5:8008/patroni")
	}
	if c < 0 || d < c || !gated {
		t.Errorf("home-c recreated at %d, home-d at %d, home-c's own Patroni asked between them: %v", c, d, gated)
	}
	for _, site := range []string{"home-c", "home-d"} {
		if h := w.hosts[site]; strings.Contains(h.etcdHosts, "10.44.0.2:") || h.etcdHosts == "" {
			t.Errorf("%s's Patroni runs with %q", site, h.etcdHosts)
		}
	}
	if !strings.Contains(w.hosts["home-a"].etcdHosts, "10.44.0.2:") {
		t.Error("the leader's Patroni was recreated")
	}
	if remains := strings.Join(p.Remains(), "\n"); !strings.Contains(remains, "home-a leads") || !strings.Contains(remains, "next restart") {
		t.Errorf("the leader's patroni.env is not noted:\n%s", remains)
	}

	// The manifest records every file the stage wrote on a replica, the
	// mesh file and patroni.env alike, as it now is.
	for _, site := range []string{"home-c", "home-d"} {
		h := w.hosts[site]
		var m render.Manifest
		if err := json.Unmarshal([]byte(h.files[dep.Manifest()]), &m); err != nil {
			t.Fatal(err)
		}
		for _, e := range m.Files {
			if e.Path == dep.WireGuardConf() || e.Path == apply.PatroniEnv(dep) {
				if e.SHA256 != sum(h.files["/"+e.Path]) {
					t.Errorf("%s's manifest records /%s as it was before", site, e.Path)
				}
			}
		}
	}

	// Planned again from live state, nothing is left for either replica.
	again := w.mustBuild("home-b", siteremove.Options{})
	for _, site := range []string{"home-c", "home-d"} {
		if hasStep(again, 2, site, "recreate", "") || hasStep(again, 2, site, "update", "patroni.env") {
			t.Errorf("%s, already up to date, is planned again:\n%s", site, printed(again))
		}
	}
}

// A replica that does not stream again stops the removal before the next
// replica is touched, although `patronictl list`, reading the member key the
// old process wrote, still says streaming.
func TestAReplicaThatDoesNotStreamStopsTheNext(t *testing.T) {
	w := newWorld(t, fourData...)
	t.Cleanup(siteremove.SetFast())
	t.Cleanup(siteremove.SetInspect(w.inspect))
	w.neverStreams = "home-c"
	err := siteremove.Execute(w.mustBuild("home-b", siteremove.Options{}))
	if err == nil || !strings.Contains(err.Error(), "home-c does not stream again") || !strings.Contains(err.Error(), `answers "starting"`) {
		t.Fatalf("err = %v", err)
	}
	if state := w.member("home-c")["State"]; state != "streaming" {
		t.Fatalf("the list says home-c is %v, and this test is about a list that still says streaming", state)
	}
	if strings.Join(w.recreated, ",") != "home-c" {
		t.Errorf("recreated %v", w.recreated)
	}
}

// The plan says sign in goes while a standby takes over only when the leaving
// site holds the active Pocket ID instance.
func TestThePocketIDNoteOnlyWhenItIsActiveThere(t *testing.T) {
	for _, tc := range []struct {
		active, site string
		want         bool
	}{
		{"home-b", "home-b", true},
		{"home-a", "home-b", false},
		{"home-a", "home-c", false},
	} {
		w := setup(t)
		w.activePocket = tc.active
		p := w.mustBuild(tc.site, siteremove.Options{})
		if got := hasStep(p, 3, tc.site, "note", "sign in is unavailable for a few seconds while a standby on another site takes over, up to about 90 seconds if the instance does not stop cleanly"); got != tc.want {
			t.Errorf("active on %s, removing %s: note %v, want %v:\n%s", tc.active, tc.site, got, tc.want, printed(p))
		}
	}
}

// A run stopped after the witness left etcd plans no etcd change again.
func TestTheShrinkResumesAfterTheWitnessLeft(t *testing.T) {
	w := witnessWorld(t)
	w.failOnce = "restart haproxy"
	if err := siteremove.Execute(w.mustBuild("home-b", siteremove.Options{})); err == nil || !strings.Contains(err.Error(), "stopped at stage 2") {
		t.Fatalf("err = %v", err)
	}
	again := w.mustBuild("home-b", siteremove.Options{})
	for _, verb := range []string{"check", "remove"} {
		if hasStep(again, 2, "home-a", verb, "etcd") {
			t.Errorf("the resumed plan changes etcd again:\n%s", printed(again))
		}
	}
	if hasStep(again, 2, "vm", "stop", "etcd") {
		t.Errorf("the resumed plan stops the witness's etcd again:\n%s", printed(again))
	}
	if err := siteremove.Execute(again); err != nil {
		t.Fatal(err)
	}
}

// A replica that leads by the time its turn comes is not recreated: that
// would be a failover.
func TestAReplicaThatLeadsByThenIsLeftAlone(t *testing.T) {
	w := newWorld(t, fourData...)
	t.Cleanup(siteremove.SetFast())
	t.Cleanup(siteremove.SetInspect(w.inspect))
	p := w.mustBuild("home-b", siteremove.Options{})
	w.member("home-a")["Role"], w.member("home-a")["State"] = "Replica", "streaming"
	w.member("home-c")["Role"], w.member("home-c")["State"] = "Leader", "running"
	err := siteremove.Execute(p)
	if err == nil || !strings.Contains(err.Error(), "home-c leads now") {
		t.Fatalf("err = %v", err)
	}
	if len(w.recreated) != 0 {
		t.Errorf("recreated %v", w.recreated)
	}
}

// derivedLists leaves cluster.sites and etcd.members out of the two data
// sites and a witness, so the roles give the same lists.
var derivedLists = []func(string) string{
	replace("  sites: [home-a, home-b]\n  port", "  port"),
	replace("  members: [home-a, home-b, vm]\n", ""),
}

// With the lists derived, the end state is the one the written lists give,
// the shrink runs the same, and the edit is the site's block and the
// witness's role: no list is written into the file.
func TestTheShrinkWithDerivedLists(t *testing.T) {
	written, _, _ := worldConfig(t, twoAndAWitness...)
	cfg, _, _ := worldConfig(t, append(append([]func(string) string(nil), twoAndAWitness...), derivedLists...)...)
	if !cfg.Etcd.MembersDerived || !cfg.Cluster.SitesDerived {
		t.Fatal("the world still writes the lists")
	}
	wantEnd, wantWitness, err := siteremove.EndState(written, "home-b")
	if err != nil {
		t.Fatal(err)
	}
	end, witness, err := siteremove.EndState(cfg, "home-b")
	if err != nil {
		t.Fatal(err)
	}
	if witness != wantWitness || strings.Join(end.Etcd.Members, ",") != strings.Join(wantEnd.Etcd.Members, ",") || strings.Join(end.Cluster.Sites, ",") != strings.Join(wantEnd.Cluster.Sites, ",") {
		t.Fatalf("end state: witness %q, members %v, cluster %v; written: %q, %v, %v", witness, end.Etcd.Members, end.Cluster.Sites, wantWitness, wantEnd.Etcd.Members, wantEnd.Cluster.Sites)
	}

	w := witnessWorld(t, derivedLists...)
	p := w.mustBuild("home-b", siteremove.Options{})
	if !hasStep(p, 4, "home-b", "remove", "its name from storage.garage.sites") || hasStep(p, 4, "home-b", "remove", "cluster.sites") {
		t.Errorf("stage 4 names a list the file does not write:\n%s", printed(p))
	}
	if !hasStep(p, 4, "vm", "remove", "witness from sites.vm.roles, which takes vm out of etcd.members, derived from the roles") {
		t.Errorf("stage 4 does not say the witness role is the whole edit:\n%s", printed(p))
	}
	rec := &ui.Recorder{}
	p.Report = rec
	if err := siteremove.Execute(p); err != nil {
		t.Fatalf("%v\n%s", err, rec.Lines())
	}
	if len(w.etcd) != 1 || w.etcd[0].Name != "home-a" {
		t.Errorf("etcd after: %v", w.etcd)
	}
	after, err := config.Load(w.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(after.Etcd.Members, ",") != "home-a" || !after.Etcd.MembersDerived || !after.Cluster.SitesDerived {
		t.Errorf("paisans.yaml after: etcd.members %v, derived %t and %t", after.Etcd.Members, after.Etcd.MembersDerived, after.Cluster.SitesDerived)
	}
	if result := validate.Check(after); result.Refused() {
		t.Errorf("the edited paisans.yaml is refused: %v", result.Refusals())
	}
}

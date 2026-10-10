package siteremove_test

import (
	"github.com/paisans-software/paisans-stack/internal/ui"
	"os"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

func setup(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	t.Cleanup(siteremove.SetFast())
	t.Cleanup(siteremove.SetInspect(w.inspect))
	return w
}

func steps(p *siteremove.Plan, stage int) []siteremove.Step { return p.Stages[stage-1].Steps }

func hasStep(p *siteremove.Plan, stage int, site, verb, text string) bool {
	for _, s := range steps(p, stage) {
		if s.Site == site && s.Verb == verb && strings.Contains(s.Text, text) {
			return true
		}
	}
	return false
}

func printed(p *siteremove.Plan) string {
	rec := &ui.Recorder{Verbose_: true}
	p.Show(rec)
	return rec.Lines()
}

// The dry run plans every stage from live state and changes nothing.
func TestTheDryRunPlansEveryStageAndChangesNothing(t *testing.T) {
	w := setup(t)
	before := map[string]int{}
	for name, h := range w.hosts {
		before[name] = len(h.files)
	}
	p := w.mustBuild("home-b", siteremove.Options{})
	if len(p.Stages) != 6 {
		t.Fatalf("want 6 stages, got %d", len(p.Stages))
	}
	for _, want := range []struct {
		stage            int
		site, verb, text string
	}{
		{1, "home-b", "stop", "Patroni"},
		{1, "home-a", "delete", "/service/paisans/members/home-b"},
		{1, "home-a", "remove", "bbbbbbbbbbbbbbbb"},
		{1, "home-a", "apply", "layout apply --version 4"},
		{2, "home-a", "remove", "etcd member"},
		{2, "home-a", "restart", "HAProxy"},
		{2, "vm", "update", "psns-f2a9.conf"},
		{3, "home-b", "remove", "paisans-f2a9-talk-app-1"},
		{3, "home-b", "stop", "wg-quick@psns-f2a9"},
		{3, "home-b", "remove", "paisans-f2a9-watchdog.service"},
		{3, "home-b", "remove", "drop-in /etc/systemd/system/docker.service.d/paisans-f2a9-after-wireguard.conf"},
		{3, "home-b", "delete", "allow in on psns-f2a9"},
		{3, "home-b", "remove", "deployment " + ourID},
		{5, "home-b", "remove", "sites.home-b"},
	} {
		if !hasStep(p, want.stage, want.site, want.verb, want.text) {
			t.Errorf("stage %d has no %s %s step with %q:\n%v", want.stage, want.verb, want.site, want.text, steps(p, want.stage))
		}
	}
	if hasStep(p, 1, "home-a", "switch", "") {
		t.Error("home-b does not lead, and a switchover was planned")
	}
	if hasStep(p, 3, "home-b", "delete", "volumes") {
		t.Error("volumes are deleted without --delete-data")
	}
	out := printed(p)
	t.Log("\n" + out)
	for _, want := range []string{"someone-elses-db", "allow 8080/tcp", "paisans-0c1d: wireguard", "the SSH allow", "patroni.env"} {
		if !strings.Contains(out+strings.Join(p.Remains(), "\n"), want) {
			t.Errorf("the plan and report do not mention %q", want)
		}
	}
	for name, h := range w.hosts {
		if len(h.files) != before[name] {
			t.Errorf("the dry run changed files on %s", name)
		}
		for _, c := range h.commands {
			for _, verb := range []string{"member remove", "switchover", "stop patroni", "layout apply", "syncconf", "restart", "docker rm", "ufw delete", "rm -f", "edit-config"} {
				if strings.Contains(c, verb) {
					t.Errorf("the dry run ran %q on %s", c, name)
				}
			}
		}
	}
}

// A whole removal: home-b leaves etcd, Patroni, Garage, the mesh and every
// HAProxy; its host keeps only what is not provably this deployment's; and
// paisans.yaml no longer declares it.
func TestARemovalCompletesAndKeepsWhatIsNotOurs(t *testing.T) {
	w := setup(t)
	edited := root + "/talk/compose.yaml"
	if _, ok := w.hosts["home-b"].files[edited]; !ok {
		t.Fatalf("home-b has no %s", edited)
	}
	w.hosts["home-b"].files[edited] += "# a hand edit\n"
	p := w.mustBuild("home-b", siteremove.Options{})
	rec := &ui.Recorder{}
	p.Report = rec
	if err := siteremove.Execute(p); err != nil {
		t.Fatalf("%v\n%s", err, rec.Lines())
	}
	t.Log("\n" + rec.Lines())
	for _, want := range []struct{ kind, text string }{
		{"section", "stage 1, data out of the site"},
		{"done", "gate: data is off the site"},
		{"done", "remove home-b's etcd member"},
		{"done", "remove containers and networks"},
		{"done", "gate: nothing of this deployment is left"},
		{"done", "remove home-b from"},
	} {
		if !rec.Has(want.kind, want.text) {
			t.Errorf("no %s %q:\n%s", want.kind, want.text, rec.Lines())
		}
	}

	for _, m := range w.etcd {
		if m.Name == "home-b" {
			t.Error("home-b is still an etcd member")
		}
	}
	if w.member("home-b") != nil {
		t.Error("Patroni still lists home-b")
	}
	if w.leader() != "home-a" || w.member("home-c")["Role"] != "Sync Standby" {
		t.Errorf("the cluster after: %v", w.members)
	}
	for _, zone := range w.layout {
		if zone == "home-b" {
			t.Error("Garage's layout still has home-b")
		}
	}
	key := w.publicKey("home-b")
	for _, name := range []string{"home-a", "home-c", "vm", "watch"} {
		h := w.hosts[name]
		if strings.Contains(h.files[wgConf], key) {
			t.Errorf("%s still has home-b as a peer", name)
		}
		if served, ok := w.served[name]; ok && strings.Contains(served, "server home-b ") {
			t.Errorf("%s's HAProxy still serves home-b", name)
		}
	}

	b := w.hosts["home-b"]
	for _, c := range b.containers {
		if c.Deployment == ourID {
			t.Errorf("container %s is still on home-b", c.Name)
		}
	}
	if len(b.containers) != 2 || len(b.volumes) != 1 {
		t.Errorf("home-b's foreign containers or this deployment's volume went: %v %v", b.containers, b.volumes)
	}
	for _, n := range b.networks {
		if n.Deployment == ourID {
			t.Errorf("network %s is still on home-b", n.Name)
		}
	}
	if _, ok := b.files[edited]; !ok {
		t.Error("an edited file was deleted")
	}
	if _, ok := b.files[root+"/infra/postgres/PG_VERSION"]; !ok {
		t.Error("data was deleted without --delete-data")
	}
	if _, ok := b.files[dep.Manifest()]; ok {
		t.Error("the manifest is still there")
	}
	if _, ok := b.files[wgConf]; ok || b.wgUp {
		t.Error("the mesh interface or its file is still there")
	}
	for _, gone := range []string{"/etc/systemd/system/paisans-f2a9-watchdog.service", "/etc/systemd/system/docker.service.d/paisans-f2a9-after-wireguard.conf", record} {
		if _, ok := b.files[gone]; ok {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, kept := range []string{"/etc/systemd/system/paisans-0c1d-watchdog.service", "/etc/systemd/system/docker.service.d/override.conf"} {
		if _, ok := b.files[kept]; !ok {
			t.Errorf("%s, not this deployment's, was deleted", kept)
		}
	}
	if strings.Join(b.rules, "|") != "allow 22/tcp comment 'paisans-f2a9: ssh, the bootstrap route'|allow 51821/udp comment 'paisans-0c1d: wireguard'|allow 8080/tcp" {
		t.Errorf("ufw rules left: %v", b.rules)
	}
	if !strings.Contains(b.files[keysAt], alice) || !strings.Contains(b.files[keysAt], "bob@example.org") || !strings.Contains(b.files[keysAt], "# managed by hand") {
		t.Errorf("authorized_keys after:\n%s", b.files[keysAt])
	}
	if reg := b.files["/var/lib/paisans/registry.json"]; strings.Contains(reg, ourID) || !strings.Contains(reg, otherID) {
		t.Errorf("the registry after:\n%s", reg)
	}

	cfg, err := config.Load(w.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Sites["home-b"]; ok || contains(cfg.Etcd.Members, "home-b") || contains(cfg.Cluster.Sites, "home-b") || contains(cfg.Storage.Garage.Sites, "home-b") {
		t.Errorf("paisans.yaml still names home-b")
	}
	data, _ := os.ReadFile(w.configPath)
	if !strings.Contains(string(data), "# The third data site.") {
		t.Error("the edit lost a comment")
	}
	remains := strings.Join(p.Remains(), "\n")
	for _, want := range []string{"sites.home-b", edited} {
		if !strings.Contains(remains, want) {
			t.Errorf("the report does not mention %q:\n%s", want, remains)
		}
	}
}

func (w *world) publicKey(site string) string {
	k, err := render.PublicKey(w.secrets.Sites[site].WireGuardPrivateKey)
	if err != nil {
		w.t.Fatal(err)
	}
	return k
}

// What a removal leaves for the operator shows its first sentence without
// --verbose, so every such sentence stays within 100 characters.
func TestTheLeftForYouHintsStayShort(t *testing.T) {
	w := setup(t)
	lines := siteremove.WorstCaseRemains()
	for _, opts := range []siteremove.Options{{}, {HostGone: true}} {
		lines = append(lines, w.mustBuild("home-b", opts).Remains()...)
	}
	for _, line := range lines {
		if hint, _, _ := strings.Cut(line, ". "); len(hint) > 100 {
			t.Errorf("%d characters: %s", len(hint), hint)
		}
	}
}

func stageNamed(p *siteremove.Plan, name string) *siteremove.Stage {
	for _, st := range p.Stages {
		if st.Name == name {
			return st
		}
	}
	return nil
}

func hasStepIn(st *siteremove.Stage, site, verb, text string) bool {
	if st == nil {
		return false
	}
	for _, s := range st.Steps {
		if s.Site == site && s.Verb == verb && strings.Contains(s.Text, text) {
			return true
		}
	}
	return false
}

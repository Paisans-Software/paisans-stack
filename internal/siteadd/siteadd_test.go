package siteadd_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/siteadd"
)

func build(t *testing.T, w *world) *siteadd.Plan {
	t.Helper()
	p, err := siteadd.Build(w.cfg, w.secrets, "home-b", w.transports())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func steps(p *siteadd.Plan, stage int) []siteadd.Step {
	return p.Stages[stage-1].Steps
}

func hasStep(p *siteadd.Plan, stage int, site, verb string) bool {
	for _, s := range steps(p, stage) {
		if s.Site == site && s.Verb == verb {
			return true
		}
	}
	return false
}

// The dry run plans every stage from live state and changes nothing.
func TestTheDryRunPlansEveryStageAndChangesNothing(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	p := build(t, w)

	if len(p.Stages) != 6 {
		t.Fatalf("want 6 stages, got %d", len(p.Stages))
	}
	for _, want := range []struct {
		stage      int
		site, verb string
	}{
		{2, "home-a", "update"}, {2, "home-a", "mesh"}, {2, "vm", "update"}, {2, "home-b", "create"}, {2, "home-b", "mesh"},
		{3, "vm", "add"}, {3, "vm", "write"}, {3, "vm", "start"}, {3, "vm", "promote"},
		{3, "home-b", "add"}, {3, "home-b", "promote"},
		{4, "home-b", "start"},
		{5, "home-a", "set"},
		{6, "home-a", "update"}, {6, "home-a", "restart"},
	} {
		if !hasStep(p, want.stage, want.site, want.verb) {
			t.Errorf("stage %d has no %s %s step:\n%v", want.stage, want.verb, want.site, steps(p, want.stage))
		}
	}
	// The witness joins first, and each joiner's --initial-cluster is the
	// membership right after its own add.
	var writes []string
	for _, s := range steps(p, 3) {
		if s.Verb == "write" {
			writes = append(writes, s.Site+" "+s.Text)
		}
	}
	if len(writes) != 2 || !strings.HasPrefix(writes[0], "vm ") ||
		!strings.Contains(writes[0], "--initial-cluster=home-a=http://10.44.0.1:2380,vm=http://10.44.0.3:2380") ||
		!strings.Contains(writes[1], "--initial-cluster=home-a=http://10.44.0.1:2380,home-b=http://10.44.0.2:2380,vm=http://10.44.0.3:2380") {
		t.Errorf("the learners' flags are wrong:\n%s", strings.Join(writes, "\n"))
	}

	for _, h := range w.hosts {
		if h.writes != 0 {
			t.Errorf("the dry run wrote %d file(s) on %s", h.writes, h.name)
		}
		for _, c := range h.commands {
			for _, verb := range []string{"member add", "promote", "syncconf", "up -d", "restart", "edit-config"} {
				if strings.Contains(c, verb) {
					t.Errorf("the dry run ran %q on %s", c, h.name)
				}
			}
		}
	}
	var out bytes.Buffer
	p.Print(&out)
	t.Log("\n" + out.String())
	if !strings.Contains(out.String(), "3. etcd") || !strings.Contains(out.String(), "rollback") {
		t.Errorf("the printed plan is missing its stages:\n%s", out.String())
	}
}

// A whole join: three voters, a streaming synchronous replica, HAProxy with
// both backends, and a second Build that has nothing left to do.
func TestAJoinCompletesAndLeavesNothingToDo(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	w.promoteRefusals = 2
	if err := siteadd.Execute(build(t, w)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.voters(), ","); got != "home-a,home-b,vm" {
		t.Errorf("voters %s", got)
	}
	vm := w.hosts["vm"].files["/srv/infra/compose.yaml"]
	if !strings.Contains(vm, "--initial-cluster-state=existing") || !strings.Contains(vm, "--initial-cluster=home-a=http://10.44.0.1:2380,vm=http://10.44.0.3:2380\n") {
		t.Errorf("vm's etcd was not started as a joiner of two:\n%s", vm)
	}
	homeA := w.hosts["home-a"].files["/srv/infra/compose.yaml"]
	if !strings.Contains(homeA, "--initial-cluster=home-a=http://10.44.0.1:2380\n") {
		t.Errorf("the founder's compose file changed")
	}
	if !w.syncMode || !strings.Contains(w.served["home-a"], "server home-b") {
		t.Errorf("synchronous mode %v, HAProxy serves:\n%s", w.syncMode, w.served["home-a"])
	}
	for _, c := range w.hosts["home-a"].commands {
		if strings.HasSuffix(c, "/srv/infra/compose.yaml restart") || strings.Contains(c, "/srv/infra/compose.yaml up -d") {
			t.Errorf("the primary's whole stack was acted on: %s", c)
		}
	}

	again := build(t, w)
	if again.Pending() {
		var out bytes.Buffer
		again.Print(&out)
		t.Errorf("a finished join still plans work:\n%s", out.String())
	}
	if !hasStep(again, 1, "home-b", "skip") {
		t.Error("a started join runs preflight again, which a site holding 51820/udp would fail")
	}
	if err := siteadd.Execute(again); err != nil {
		t.Errorf("re-running a finished join fails its gates: %v", err)
	}
	if len(again.Notes) == 0 {
		t.Error("home-a's stale patroni.env is not noted")
	}

	// apply would now see the scoped writes as its own.
	rendered := mustRender(t, w)
	for _, name := range []string{"home-a", "vm"} {
		ap, err := apply.Build(name, rendered, "", w.hosts[name])
		if err != nil {
			t.Fatal(err)
		}
		if c := ap.Conflicts(); len(c) > 0 {
			t.Errorf("%s: apply sees site add's files as edits: %v", name, c)
		}
	}
}

// A failed mesh gate restores every existing site's wg0.conf and stops
// before etcd is touched.
func TestAFailedMeshGateRollsBack(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	w.noPing = true
	before := w.hosts["home-a"].files["/etc/wireguard/wg0.conf"]
	err := siteadd.Execute(build(t, w))
	if err == nil || !strings.Contains(err.Error(), "stage 2") {
		t.Fatalf("want a stage 2 failure, got %v", err)
	}
	if w.hosts["home-a"].files["/etc/wireguard/wg0.conf"] != before {
		t.Error("home-a's wg0.conf was not restored")
	}
	if w.hosts["home-a"].ran("member add") > 0 {
		t.Error("etcd was touched after a failed mesh gate")
	}

	// Fixed, the next run starts again at stage 2 and carries on.
	w.noPing = false
	p := build(t, w)
	if !hasStep(p, 2, "home-a", "update") {
		t.Error("after a rollback the mesh is not planned again")
	}
	if err := siteadd.Execute(p); err != nil {
		t.Fatal(err)
	}
}

// etcd refuses a promotion until the learner has caught up; site add retries
// a bounded number of times and then stops with the learner still a learner.
func TestPromotionIsRetriedThenGivenUp(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	w.promoteRefusals = 1000
	err := siteadd.Execute(build(t, w))
	if err == nil || !strings.Contains(err.Error(), "stage 3") || !strings.Contains(err.Error(), "still a learner") {
		t.Fatalf("want a stage 3 promotion failure, got %v", err)
	}
	if n := w.hosts["home-a"].ran("member promote"); n != siteadd.PromoteAttempts() {
		t.Errorf("promote tried %d times, want %d", n, siteadd.PromoteAttempts())
	}
	if w.hosts["home-a"].ran("--peer-urls=http://10.44.0.2:2380") > 0 {
		t.Error("the new site was added while the witness was still a learner")
	}
}

// A run that stopped with a learner added and not started resumes without
// adding it again.
func TestAStoppedEtcdJoinResumes(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	w.failOnce = "up -d etcd"
	if err := siteadd.Execute(build(t, w)); err == nil || !strings.Contains(err.Error(), "stage 3") {
		t.Fatalf("want a stage 3 failure, got %v", err)
	}
	p := build(t, w)
	if len(steps(p, 2)) != 0 {
		t.Errorf("the mesh is planned again: %v", steps(p, 2))
	}
	if hasStep(p, 3, "vm", "add") || !hasStep(p, 3, "vm", "start") {
		t.Errorf("the learner is added again, or not started: %v", steps(p, 3))
	}
	if err := siteadd.Execute(p); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.voters(), ","); got != "home-a,home-b,vm" {
		t.Errorf("voters %s", got)
	}
}

// A replica whose lag rises fails stage 4, and the next run resumes there.
func TestARisingLagStopsAtTheReplicaAndResumes(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	w.lags = []float64{5, 6, 7}
	if err := siteadd.Execute(build(t, w)); err == nil || !strings.Contains(err.Error(), "stage 4") {
		t.Fatalf("want a stage 4 failure, got %v", err)
	}
	w.lags = []float64{9, 4, 0}
	p := build(t, w)
	for stage := 2; stage <= 4; stage++ {
		if len(steps(p, stage)) != 0 {
			t.Errorf("stage %d is planned again: %v", stage, steps(p, stage))
		}
	}
	if !hasStep(p, 5, "home-a", "set") {
		t.Error("stage 5 is not planned")
	}
	if err := siteadd.Execute(p); err != nil {
		t.Fatal(err)
	}
}

func TestNoSyncStandbyStopsAtStage5(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	w.noSyncStandby = true
	if err := siteadd.Execute(build(t, w)); err == nil || !strings.Contains(err.Error(), "Sync Standby") {
		t.Fatalf("want a stage 5 failure, got %v", err)
	}
}

func TestHAProxyRoutingToAReplicaStopsAtStage6(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	w.replicaUp = true
	err := siteadd.Execute(build(t, w))
	if err == nil || !strings.Contains(err.Error(), "stage 6") || !strings.Contains(err.Error(), "replica home-b UP") {
		t.Fatalf("want a stage 6 failure, got %v", err)
	}
}

func TestAJoinOutsideTheScopeIsRefused(t *testing.T) {
	w := newWorld(t)
	s := w.cfg.Sites["home-a"]
	s.Endpoint = ""
	w.cfg.Sites["home-a"] = s
	if _, err := siteadd.Build(w.cfg, w.secrets, "home-b", w.transports()); err == nil || !strings.Contains(err.Error(), "relay") {
		t.Errorf("a site without an endpoint was accepted: %v", err)
	}
	w = newWorld(t)
	if _, err := siteadd.Build(w.cfg, w.secrets, "home-a", w.transports()); err == nil {
		t.Error("an existing data and apps site was accepted as a new data site")
	}
}

// etcd refuses a learner add as an "unhealthy cluster" until every voter has
// been connected for five seconds, so the second learner, added right after the
// first is promoted, is refused at first. The first real join stopped there.
func TestALearnerAddRefusedAsUnhealthyIsRetried(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	w.addRefusals = 2
	if err := siteadd.Execute(build(t, w)); err != nil {
		t.Fatalf("a transient unhealthy cluster stopped the join: %v", err)
	}
}

// Stage 6 stops the apps that reach the database through HAProxy before it
// restarts, and starts and checks them after. A real join restarted HAProxy
// under a running Mbin, whose workers never reconnected. A pinned app with its
// own Postgres is left alone.
func TestHAProxyRestartsWithItsDatabaseAppsStopped(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	p := build(t, w)
	var out bytes.Buffer
	p.Print(&out)
	plan := out.String()
	for _, want := range []string{
		"stop      home-a: auth, docs, talk: they reach the database through this HAProxy",
		"restart   home-a: HAProxy alone",
		"start     home-a: auth, docs, talk (docker compose -f /srv/auth/compose.yaml up -d;",
		"check     home-a: auth, docs, talk: every container running",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan has no %q:\n%s", want, plan)
		}
	}
	if err := siteadd.Execute(p); err != nil {
		t.Fatal(err)
	}

	cmds := w.hosts["home-a"].commands
	index := func(sub string) int {
		for i, c := range cmds {
			if strings.Contains(c, sub) {
				return i
			}
		}
		return -1
	}
	restart := index("restart haproxy")
	if restart < 0 {
		t.Fatal("HAProxy was not restarted")
	}
	for _, app := range []string{"auth", "docs", "talk"} {
		compose := "/srv/" + app + "/compose.yaml"
		stop, start, check := index(compose+" stop"), index(compose+" up -d"), index(compose+" ps --all")
		if stop < 0 || stop > restart {
			t.Errorf("%s was not stopped before HAProxy's restart (stop at %d, restart at %d)", app, stop, restart)
		}
		if start < restart {
			t.Errorf("%s was not started after HAProxy's restart (start at %d, restart at %d)", app, start, restart)
		}
		if check < start {
			t.Errorf("%s was not checked after it started", app)
		}
	}
	for _, c := range cmds {
		if strings.Contains(c, "/srv/blog/") || strings.Contains(c, "/srv/gate/") {
			t.Errorf("an app that does not use the cluster's database was moved: %s", c)
		}
	}
}

// An app that does not come back healthy stops the join at stage 6, with its
// logs, and HAProxy's own gate is not reached.
func TestAnAppUnhealthyAfterHAProxyStopsAtStage6(t *testing.T) {
	defer siteadd.SetFast()()
	w := newWorld(t)
	w.unhealthy = "talk"
	err := siteadd.Execute(build(t, w))
	if err == nil || !strings.Contains(err.Error(), "stage 6") || !strings.Contains(err.Error(), "stack talk is not healthy") || !strings.Contains(err.Error(), "last 30 log lines") {
		t.Fatalf("want a stage 6 failure naming talk, got %v", err)
	}
	if w.hosts["home-a"].ran("/stats;csv") > 2 {
		t.Error("HAProxy's gate ran after an app failed")
	}
}

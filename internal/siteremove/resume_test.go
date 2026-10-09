package siteremove_test

import (
	"github.com/paisans-software/paisans-stack/internal/ui"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

// A run stopped at any stage resumes there: the next Build plans nothing for
// the stages already done, and the run completes.
func TestARemovalResumesFromEachStage(t *testing.T) {
	for _, tc := range []struct {
		stage int
		fail  string
		// done are the steps a resumed plan must not plan again.
		done []struct {
			stage            int
			site, verb, text string
		}
	}{
		{1, "layout apply", []struct {
			stage            int
			site, verb, text string
		}{{1, "home-b", "stop", "Patroni"}, {1, "home-a", "delete", "members/home-b"}}},
		{2, "restart haproxy", []struct {
			stage            int
			site, verb, text string
		}{{1, "home-a", "remove", "bbbbbbbbbbbbbbbb"}, {2, "home-a", "remove", "etcd member"}, {2, "home-a", "update", "haproxy.cfg"}}},
		{3, "ufw delete", []struct {
			stage            int
			site, verb, text string
		}{{2, "home-a", "remove", "etcd member"}, {3, "home-b", "remove", "every container"}, {3, "home-b", "delete", "the manifest"}}},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			w := setup(t)
			w.failOnce = tc.fail
			err := siteremove.Execute(w.mustBuild("home-b", siteremove.Options{}))
			if err == nil || !strings.Contains(err.Error(), "stopped at stage") || !strings.Contains(err.Error(), "failed by the test") {
				t.Fatalf("err = %v", err)
			}
			again := w.mustBuild("home-b", siteremove.Options{})
			for _, d := range tc.done {
				if hasStep(again, d.stage, d.site, d.verb, d.text) {
					t.Errorf("the resumed plan does %s %s %q again:\n%s", d.verb, d.site, d.text, printed(again))
				}
			}
			if !again.Pending() {
				t.Error("a stopped removal has nothing left to do")
			}
			rec := &ui.Recorder{}
			again.Report = rec
			if err := siteremove.Execute(again); err != nil {
				t.Fatalf("resuming: %v\n%s", err, rec.Lines())
			}
			if w.member("home-b") != nil || strings.Contains(w.hosts["home-b"].files["/var/lib/paisans/registry.json"], ourID) {
				t.Error("the resumed removal did not finish")
			}
		})
	}
}

// A run stopped at the configuration edit resumes there.
func TestARemovalResumesAtTheConfigEdit(t *testing.T) {
	w := setup(t)
	path := w.configPath
	w.configPath = path + ".missing/paisans.yaml"
	err := siteremove.Execute(w.mustBuild("home-b", siteremove.Options{}))
	if err == nil || !strings.Contains(err.Error(), "stage 4") {
		t.Fatalf("err = %v", err)
	}
	w.configPath = path
	again := w.mustBuild("home-b", siteremove.Options{})
	for _, d := range []struct {
		stage            int
		site, verb, text string
	}{{1, "home-a", "remove", "bbbbbbbbbbbbbbbb"}, {2, "home-a", "remove", "etcd member"}, {3, "home-b", "remove", "every container"}} {
		if hasStep(again, d.stage, d.site, d.verb, d.text) {
			t.Errorf("the resumed plan does %s %s %q again", d.verb, d.site, d.text)
		}
	}
	if err := siteremove.Execute(again); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := siteremove.Refusal(cfg, "home-b", siteremove.Options{}); err == nil || !strings.Contains(err.Error(), "declares no site") {
		t.Errorf("a finished removal is not refused: %v", err)
	}
}

// With the leader on the leaving site, the leadership goes to the Sync
// Standby before its Patroni stops.
func TestTheLeaderIsSwitchedOverFirst(t *testing.T) {
	w := setup(t)
	w.member("home-a")["Role"], w.member("home-a")["State"] = "Sync Standby", "streaming"
	w.member("home-b")["Role"], w.member("home-b")["State"] = "Leader", "running"
	p := w.mustBuild("home-b", siteremove.Options{})
	if !hasStep(p, 1, "home-a", "switch", "--leader home-b --candidate home-a") {
		t.Fatalf("no switchover planned:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if w.leader() != "home-a" {
		t.Errorf("leader %s", w.leader())
	}
	sw, stop := -1, -1
	for i, c := range w.hosts["home-a"].commands {
		if strings.Contains(c, "switchover") {
			sw = i
		}
	}
	for i, c := range w.hosts["home-b"].commands {
		if strings.Contains(c, "stop patroni") {
			stop = i
		}
	}
	if sw < 0 || stop < 0 {
		t.Fatalf("switchover %d, stop %d", sw, stop)
	}
}

// When one data site remains, synchronous mode is turned off before the
// standby leaves, so the leader does not wait on it.
func TestOneDataSiteLeftTurnsSynchronousModeOff(t *testing.T) {
	w := newWorld(t,
		replace("  home-c:\n    roles: [data]", "  home-c:\n    roles: [witness, storage]"),
		replace("sites: [home-a, home-b, home-c]\n  port", "sites: [home-a, home-b]\n  port"),
	)
	t.Cleanup(siteremove.SetFast())
	t.Cleanup(siteremove.SetInspect(w.inspect))
	p := w.mustBuild("home-b", siteremove.Options{})
	if !hasStep(p, 1, "home-a", "set", "synchronous_mode=false") {
		t.Fatalf("synchronous mode is left on:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if w.syncMode {
		t.Error("synchronous mode is still on")
	}
}

// --host-gone reaches the host not at all, runs every other stage, and says
// what is left on it.
func TestHostGoneSkipsTheHost(t *testing.T) {
	w := setup(t)
	w.unreachable["home-b"] = true
	w.etcdDown["home-b"] = true
	// Patroni has already moved the synchronous role off the dead member.
	w.member("home-b")["Role"], w.member("home-b")["State"] = "Replica", "stopped"
	w.member("home-c")["Role"] = "Sync Standby"
	p := w.mustBuild("home-b", siteremove.Options{HostGone: true})
	if p.Stages[2].Skipped == "" || hasStep(p, 1, "home-b", "stop", "Patroni") {
		t.Fatalf("the host is planned for:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if n := len(w.hosts["home-b"].commands); n != 0 {
		t.Errorf("home-b was sent %d command(s): %v", n, w.hosts["home-b"].commands)
	}
	if w.member("home-b") != nil {
		t.Error("its member key is still in etcd")
	}
	remains := strings.Join(p.Remains(), "\n")
	if !strings.Contains(remains, "--host-gone") || !strings.Contains(remains, "/var/lib/paisans/registry.json") {
		t.Errorf("the report does not say what is left:\n%s", remains)
	}
}

// --delete-data deletes the root and the named volumes.
func TestDeleteDataDeletesTheRootAndVolumes(t *testing.T) {
	w := setup(t)
	p := w.mustBuild("home-b", siteremove.Options{DeleteData: true})
	if !hasStep(p, 3, "home-b", "delete", "volumes paisans-f2a9-talk_media") || !hasStep(p, 3, "home-b", "delete", root+" and everything in it") {
		t.Fatalf("no deletion planned:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	b := w.hosts["home-b"]
	if _, ok := b.files[root+"/infra/postgres/PG_VERSION"]; ok || len(b.volumes) != 0 {
		t.Error("data is still there")
	}
}

// A key another deployment's record lists stays, as does a key that is the
// login user's last.
func TestKeysOnlyGoWhenTheRecordProvesThem(t *testing.T) {
	t.Run("another record lists it", func(t *testing.T) {
		w := setup(t)
		b := w.hosts["home-b"]
		b.files[theirs] = fingerprint(t, alice) + " alice@example.org\n"
		if err := siteremove.Execute(w.mustBuild("home-b", siteremove.Options{})); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.files[keysAt], alice) {
			t.Error("a key another deployment records was deleted")
		}
		if _, ok := b.files[record]; ok {
			t.Error("this deployment's record is still there")
		}
	})
	t.Run("the last key", func(t *testing.T) {
		w := setup(t)
		b := w.hosts["home-b"]
		b.files[keysAt] = alice + "\n"
		p := w.mustBuild("home-b", siteremove.Options{})
		if err := siteremove.Execute(p); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.files[keysAt], alice) {
			t.Error("the user's last key was deleted")
		}
		if !strings.Contains(strings.Join(p.Remains(), "\n"), "no authorized key") {
			t.Error("the report does not say why the key stayed")
		}
	})
}

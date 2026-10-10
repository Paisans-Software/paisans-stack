package siteremove_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/siteremove"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

func replace(old, new string) func(string) string {
	return func(s string) string {
		if !strings.Contains(s, old) {
			panic("no " + old)
		}
		return strings.Replace(s, old, new, 1)
	}
}

// Every refusal stops before anything changes, with what to do.
func TestRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		site  string
		edits []func(string) string
		world func(*world)
		opts  siteremove.Options
		want  string
		// hint, when set, is the Problem's hint main prints.
		hint string
	}{
		{name: "the only gateway", site: "vm",
			edits: []func(string) string{replace("placement: { pinned: vm }", "placement: { pinned: home-a }")},
			want:  "only gateway", hint: "vm is the only gateway"},
		{name: "the only apps site", site: "home-b",
			edits: []func(string) string{replace("  home-a:\n    roles: [data, apps]", "  home-a:\n    roles: [data]"), replace("placement: { pinned: home-a }\n    # An ini", "placement: { pinned: watch }\n    # An ini")},
			want:  "only apps site", hint: "home-b is the only apps site"},
		{name: "an app pinned to it", site: "home-a", want: "pinned to it", hint: "apps are pinned to home-a"},
		{name: "the only data site", site: "home-b",
			edits: []func(string) string{
				replace("  home-a:\n    roles: [data, apps]", "  home-a:\n    roles: [apps]"),
				replace("  home-c:\n    roles: [data]", "  home-c:\n    roles: [storage]"),
				replace("sites: [home-a, home-b, home-c]\n  port", "sites: [home-b]\n  port"),
				replace("members: [home-a, home-b, home-c, vm]", "members: [home-b]"),
			},
			want: "only data site", hint: "home-b is the only data site"},
		{name: "too few Garage nodes", site: "home-b",
			edits: []func(string) string{replace("replication: 2", "replication: 3")},
			want:  "would hold objects at storage.garage.replication 3", hint: "too few Garage nodes would be left without home-b"},
		{name: "an end state that does not validate", site: "home-b",
			edits: []func(string) string{replace("members: [home-a, home-b, home-c, vm]", "members: [home-a, home-b, vm]")},
			want:  "two etcd voters", hint: "the configuration without home-b would be refused"},
		{name: "the first Garage site", site: "home-a",
			edits: []func(string) string{replace("placement: { pinned: home-a }\n    # An ini", "placement: { pinned: watch }\n    # An ini"), replace("placement: { pinned: home-a }\n    settings:\n      homeserver", "placement: { pinned: watch }\n    settings:\n      homeserver"), replace("placement: { pinned: home-a }\n  status", "placement: { pinned: watch }\n  status")},
			want:  "first in storage.garage.sites", hint: "home-a is first in storage.garage.sites"},
		{name: "--host-gone with --delete-data", site: "home-b", opts: siteremove.Options{HostGone: true, DeleteData: true}, want: "Drop one of them", hint: "--host-gone and --delete-data cannot be used together"},
		{name: "an unhealthy etcd member", site: "home-b", world: func(w *world) { w.etcdDown["home-c"] = true }, want: "home-c's etcd is unhealthy"},
		{name: "no quorum after", site: "home-b", world: func(w *world) { w.etcdDown["home-c"], w.etcdDown["vm"] = true, true }, want: "which is no quorum"},
		{name: "an unhealthy Patroni member", site: "home-b", world: func(w *world) { w.member("home-c")["State"] = "starting" }, want: "home-c is starting, not streaming"},
		{name: "the leader with no Sync Standby", site: "home-b", world: func(w *world) {
			w.member("home-a")["Role"], w.member("home-a")["State"] = "Replica", "streaming"
			w.member("home-b")["Role"], w.member("home-b")["State"] = "Leader", "running"
		}, want: "no member is a streaming Sync Standby"},
		{name: "an unhealthy Garage node", site: "home-b", world: func(w *world) { w.garageDown["home-c"] = true }, want: "home-c (cccccccccccccccc) are not healthy"},
		{name: "a host that does not answer", site: "home-b", world: func(w *world) { w.unreachable["home-b"] = true }, want: "--host-gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, tc.edits...)
			t.Cleanup(siteremove.SetFast())
			t.Cleanup(siteremove.SetInspect(w.inspect))
			if tc.world != nil {
				tc.world(w)
			}
			_, err := w.build(tc.site, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if tc.hint != "" {
				var p *ui.Problem
				if !errors.As(err, &p) || p.Hint != tc.hint || p.Explain == "" {
					t.Errorf("not the Problem main prints: %#v", p)
				}
			}
			for name, h := range w.hosts {
				for _, c := range h.commands {
					for _, verb := range []string{"member remove", "switchover", "stop", "layout remove", "syncconf", "restart", "ufw delete", "rm -"} {
						if strings.Contains(c, verb) {
							t.Errorf("a refused removal ran %q on %s", c, name)
						}
					}
				}
			}
		})
	}
}

func TestRefusalNamesAnUndeclaredSite(t *testing.T) {
	cfg, _, _ := worldConfig(t)
	err := siteremove.Refusal(cfg, "home-z", siteremove.Options{})
	if err == nil || !strings.Contains(err.Error(), "declares no site") {
		t.Errorf("err = %v", err)
	}
	var p *ui.Problem
	if !errors.As(err, &p) || !strings.HasSuffix(p.Hint, "declares no site home-z") || !strings.Contains(p.Explain, "Declared sites are") {
		t.Errorf("not the Problem main prints: %#v", p)
	}
}

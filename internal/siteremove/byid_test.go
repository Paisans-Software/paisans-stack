package siteremove_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

func byID(t *testing.T, w *world, site, ref string, o siteremove.Options) *siteremove.Plan {
	t.Helper()
	dest, _ := config.ParseDestination("ubuntu@192.0.2.30")
	p, err := siteremove.BuildForcedByID(dest, w.hosts[site], ref, o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func hostTexts(p *siteremove.Plan) []string {
	var out []string
	for _, s := range stageNamed(p, "clean the host").Steps {
		if s.Verb != "note" {
			out = append(out, s.Site+": "+s.Text)
		}
	}
	return out
}

// A monitor-only deployment is cleaned with no configuration at all: the
// same host stage the forced removal plans with one, by its token or its
// full id, and the host is left with nothing of it.
func TestByIDCleansAMonitorWithNoConfiguration(t *testing.T) {
	w := setup(t)
	dest, _ := config.ParseDestination("ubuntu@192.0.2.30")
	want := hostTexts(forced(t, w, w.cfg.WithoutSite("watch"), "watch", dest, siteremove.Options{}))
	for _, ref := range []string{"f2a9", ourID} {
		p := byID(t, w, "watch", ref, siteremove.Options{})
		if p.Site != "watch" || p.Current {
			t.Errorf("%s: site %q, current %v", ref, p.Site, p.Current)
		}
		got := hostTexts(p)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: host stage differs from the forced removal's\nwant:\n%s\ngot:\n%s", ref, strings.Join(want, "\n"), strings.Join(got, "\n"))
		}
	}
	p := byID(t, w, "watch", "f2a9", siteremove.Options{DeleteData: true})
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	h := w.hosts["watch"]
	reg, _ := registry.Parse([]byte(h.files[registry.Path]))
	if _, ok := reg.Deployments[ourID]; ok {
		t.Error("the registry entry is still there")
	}
	for path := range h.files {
		if strings.HasPrefix(path, root+"/") {
			t.Errorf("%s is still there", path)
		}
	}
	if len(h.containers) != 0 {
		t.Errorf("containers left: %+v", h.containers)
	}
}

// What the configuration would have said is reported as not read, one line
// each.
func TestByIDSaysWhatItDidNotRead(t *testing.T) {
	w := setup(t)
	remains := strings.Join(byID(t, w, "watch", "f2a9", siteremove.Options{}).Remains(), "\n")
	for _, want := range []string{"secrets: not read", "other hosts: not reached", "DNS:"} {
		if !strings.Contains(remains, want) {
			t.Errorf("no %q line:\n%s", want, remains)
		}
	}
	if strings.Contains(remains, "Pocket ID") {
		t.Errorf("a monitor-only site is told about Pocket ID:\n%s", remains)
	}
	remains = strings.Join(byID(t, w, "home-a", "f2a9", siteremove.Options{}).Remains(), "\n")
	if !strings.Contains(remains, "Pocket ID: not checked") {
		t.Errorf("an apps site is not told about Pocket ID:\n%s", remains)
	}
}

// On a host shared with another deployment, nothing of that deployment's is
// planned or touched: its container, unit, ufw rule and registry entry stay.
func TestByIDLeavesAnotherDeploymentAlone(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	p := byID(t, w, "home-b", ourID, siteremove.Options{DeleteData: true})
	if plan := printed(p); strings.Contains(plan, "0c1d") {
		t.Errorf("the plan names the other deployment:\n%s", plan)
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	reg, _ := registry.Parse([]byte(b.files[registry.Path]))
	if _, ok := reg.Deployments[otherID]; !ok {
		t.Error("the other deployment's registry entry is gone")
	}
	if _, ok := b.files["/etc/systemd/system/paisans-0c1d-watchdog.service"]; !ok {
		t.Error("the other deployment's unit is gone")
	}
	if !contains(b.rules, "allow 51821/udp comment 'paisans-0c1d: wireguard'") {
		t.Error("the other deployment's ufw rule is gone")
	}
	found := false
	for _, c := range b.containers {
		found = found || c.Deployment == otherID
	}
	if !found {
		t.Error("the other deployment's container is gone")
	}
	for _, c := range b.commands {
		if strings.Contains(c, otherID) || strings.Contains(c, "paisans-0c1d") {
			t.Errorf("a command names the other deployment: %s", c)
		}
	}
}

// An entry that does not agree with its id is refused before anything is
// planned: every name removed is made from the id.
func TestByIDRefusesAnEntryThatDisagreesWithItsID(t *testing.T) {
	for name, edit := range map[string]func(registry.Registry){
		"token": func(r registry.Registry) {
			e := r.Deployments[ourID]
			e.Token = "beef"
			r.Deployments[ourID] = e
		},
		"root": func(r registry.Registry) {
			e := r.Deployments[ourID]
			e.Root = "/srv/paisans/beef"
			r.Deployments[ourID] = e
		},
		"key": func(r registry.Registry) {
			e := r.Deployments[ourID]
			delete(r.Deployments, ourID)
			r.Deployments["f2a9-not-an-id"] = e
		},
	} {
		w := setup(t)
		h := w.hosts["watch"]
		reg, _ := registry.Parse([]byte(h.files[registry.Path]))
		edit(reg)
		h.files[registry.Path] = encode(t, reg)
		dest, _ := config.ParseDestination("ubuntu@192.0.2.30")
		_, err := siteremove.BuildForcedByID(dest, h, "f2a9", siteremove.Options{})
		if err == nil || !strings.Contains(err.Error(), "Nothing was changed") {
			t.Errorf("%s: err = %v", name, err)
		}
		if n := h.ran("docker"); n != 0 {
			t.Errorf("%s: %d docker command(s) ran", name, n)
		}
	}
}

// No match and several are refused, listing what the host holds.
func TestByIDRefusesNoMatchAndSeveral(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	dest, _ := config.ParseDestination("ubuntu@192.0.2.30")
	if _, err := siteremove.BuildForcedByID(dest, b, "beef", siteremove.Options{}); err == nil || !strings.Contains(err.Error(), otherID) {
		t.Errorf("no match: %v", err)
	}
	reg, _ := registry.Parse([]byte(b.files[registry.Path]))
	reg.Deployments["f2a91111-2222-4333-8444-555566667777"] = registry.Entry{Token: "f2a9", Root: root, Domain: "example.com", Site: "y", Roles: "monitor", ClaimedAt: "2026-10-01T00:00:00Z", Interface: "psns-f2a9", Subnet: "10.46.0.0/24", Address: "10.46.0.2"}
	b.files[registry.Path] = encode(t, reg)
	if _, err := siteremove.BuildForcedByID(dest, b, "f2a9", siteremove.Options{}); err == nil || !strings.Contains(err.Error(), "full id") {
		t.Errorf("several: %v", err)
	}
	if n := b.ran("docker"); n != 0 {
		t.Errorf("%d docker command(s) ran", n)
	}
}

func TestByIDRefusesHostGone(t *testing.T) {
	w := setup(t)
	dest, _ := config.ParseDestination("ubuntu@192.0.2.30")
	if _, err := siteremove.BuildForcedByID(dest, w.hosts["watch"], "f2a9", siteremove.Options{HostGone: true}); err == nil || !strings.Contains(err.Error(), "Drop one of them") {
		t.Errorf("err = %v", err)
	}
}

// Another entry holding the same token or root is refused even when --id is
// the full id: every name but the label is made from the token, so the two
// would share them.
func TestByIDRefusesAFullIDWhoseTokenAnotherEntryHolds(t *testing.T) {
	for name, e := range map[string]registry.Entry{
		"token": {Token: "f2a9", Root: "/srv/paisans/beef", Domain: "example.com", Site: "y", Roles: "monitor", ClaimedAt: "2026-10-01T00:00:00Z", Interface: "psns-f2a9", Subnet: "10.46.0.0/24", Address: "10.46.0.2"},
		"root":  {Token: "beef", Root: root, Domain: "example.com", Site: "y", Roles: "monitor", ClaimedAt: "2026-10-01T00:00:00Z", Interface: "psns-beef", Subnet: "10.46.0.0/24", Address: "10.46.0.2"},
	} {
		w := setup(t)
		h := w.hosts["watch"]
		reg, _ := registry.Parse([]byte(h.files[registry.Path]))
		reg.Deployments["beef1111-2222-4333-8444-555566667777"] = e
		h.files[registry.Path] = encode(t, reg)
		dest, _ := config.ParseDestination("ubuntu@192.0.2.30")
		_, err := siteremove.BuildForcedByID(dest, h, ourID, siteremove.Options{})
		if err == nil || !strings.Contains(err.Error(), "beef1111") || !strings.Contains(err.Error(), "Nothing was changed") {
			t.Errorf("%s: err = %v", name, err)
		}
		if n := h.ran("docker"); n != 0 {
			t.Errorf("%s: %d docker command(s) ran", name, n)
		}
	}
}

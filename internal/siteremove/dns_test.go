package siteremove_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/dns"
	"github.com/paisans-software/paisans-stack/internal/dns/dnstest"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

const comment = "paisans-f2a9: created by paisans dns init"

// cloudflare declares Cloudflare as the provider and gives the gateway the
// public address every hostname points at.
func cloudflare(text string) string {
	text = strings.Replace(text, "  provider: desec", "  provider: cloudflare", 1)
	return strings.Replace(text, "    endpoint: vm.example.org:51820\n", "    endpoint: vm.example.org:51820\n    public_address: 203.0.113.10\n", 1)
}

// ownAddress gives home-b addresses no other site declares.
func ownAddress(text string) string {
	return strings.Replace(text, "    address: 10.44.0.2\n    public_address: 203.0.113.20\n", "    address: 10.44.0.2\n    public_address: 203.0.113.7\n    public_address6: 2001:db8::7\n", 1)
}

// dnsWorld is the world with a Cloudflare zone holding home-b's records and
// records the removal must leave alone, one reason each.
func dnsWorld(t *testing.T, edits ...func(string) string) (*world, *dnstest.Provider) {
	t.Helper()
	w := newWorld(t, append([]func(string) string{cloudflare}, edits...)...)
	t.Cleanup(siteremove.SetFast())
	t.Cleanup(siteremove.SetInspect(w.inspect))
	zone := dnstest.New()
	zone.Zones["example.org"] = "zone-1"
	zone.InZone["zone-1"] = []dns.Record{
		{ID: "rec-blog", Type: "A", Name: "gone.example.org", Content: "203.0.113.7", Comment: comment},
		{ID: "rec-blog6", Type: "AAAA", Name: "gone.example.org", Content: "2001:db8::7", Comment: comment},
		{ID: "rec-talk", Type: "A", Name: "talk.example.org", Content: "203.0.113.10", Comment: comment},
		{ID: "rec-wanted", Type: "A", Name: "talk.example.org", Content: "203.0.113.7", Comment: comment},
		{ID: "rec-other", Type: "A", Name: "old.example.org", Content: "203.0.113.7", Comment: "paisans-0c1d: created by paisans dns init"},
		{ID: "rec-hand", Type: "A", Name: "hand.example.org", Content: "203.0.113.7"},
	}
	return w, zone
}

func dnsOptions(zone *dnstest.Provider) siteremove.Options {
	return siteremove.Options{DNSProvider: zone.For("cloudflare")}
}

func shown(p *siteremove.Plan, verbose bool) *ui.Recorder {
	rec := &ui.Recorder{Verbose_: verbose}
	p.Show(rec)
	return rec
}

func TestTheDNSStageListsTheSitesRecordsAndDeletesNothing(t *testing.T) {
	w, zone := dnsWorld(t, ownAddress)
	p := w.mustBuild("home-b", dnsOptions(zone))
	st := stageNamed(p, "dns")
	if st == nil || st.Number != 4 || st.Skipped != "" {
		t.Fatalf("want stage 4, dns, planned: %+v", st)
	}
	if p.Stages[len(p.Stages)-2].Name != "config" || p.Stages[len(p.Stages)-1].Name != "monitor" {
		t.Errorf("the configuration edit and the monitor follow the DNS stage")
	}
	var titles []string
	for _, s := range st.Steps {
		titles = append(titles, s.Title)
	}
	if got := strings.Join(titles, "\n"); got != "delete A gone.example.org → 203.0.113.7\ndelete AAAA gone.example.org → 2001:db8::7" {
		t.Errorf("steps:\n%s", got)
	}
	if len(zone.Deletes) != 0 || zone.Token != "fixture-not-a-secret-desec" {
		t.Errorf("deletes %v, token handed over %v", zone.Deletes, zone.Token != "")
	}
	out := shown(p, false)
	t.Log("\n" + out.Lines())
	for _, want := range []string{"delete A gone.example.org → 203.0.113.7", "delete AAAA gone.example.org → 2001:db8::7", "gate: no record of home-b's is left"} {
		if !out.Has("item", want) {
			t.Errorf("no item %q", want)
		}
	}
	// A record at the site's address that a rule keeps shows why at every
	// verbosity.
	if !out.Has("note", "keep A talk.example.org → 203.0.113.7") || !strings.Contains(out.Lines(), "the configuration without home-b still wants a record of type A at this name") {
		t.Errorf("the kept record is not noted with its reason:\n%s", out.Lines())
	}
	// One pointing elsewhere is not the site's, and is listed with -v only.
	if strings.Contains(out.Lines(), "talk.example.org → 203.0.113.10") {
		t.Errorf("another site's record shows by default:\n%s", out.Lines())
	}
	if !strings.Contains(shown(p, true).Lines(), "keep A talk.example.org → 203.0.113.10") {
		t.Error("another site's record is not listed with -v")
	}
	// Records without this deployment's comment are not listed at all.
	if all := shown(p, true).Lines(); strings.Contains(all, "old.example.org") || strings.Contains(all, "hand.example.org") {
		t.Errorf("a record without this deployment's comment is listed:\n%s", all)
	}
	for _, l := range p.Remains() {
		if strings.HasPrefix(l, "DNS") {
			t.Errorf("the DNS stage replaces the remains line: %s", l)
		}
	}
}

func TestTheDNSStageDeletesAndConfirms(t *testing.T) {
	w, zone := dnsWorld(t, ownAddress)
	p := w.mustBuild("home-b", dnsOptions(zone))
	run := &ui.Recorder{}
	p.Report = run
	if err := siteremove.Execute(p); err != nil {
		t.Fatalf("%v\n%s", err, run.Lines())
	}
	if got := strings.Join(zone.Deletes, ","); got != "rec-blog,rec-blog6" {
		t.Errorf("deletes = %s", got)
	}
	if got := strings.Join(zone.IDs("zone-1"), ","); got != "rec-talk,rec-wanted,rec-other,rec-hand" {
		t.Errorf("left = %s", got)
	}
	for _, want := range []string{"delete A gone.example.org", "confirm deletes", "gate: no record of home-b's is left"} {
		if !run.Has("done", want) {
			t.Errorf("no done %q:\n%s", want, run.Lines())
		}
	}
	if !run.Has("note", "keep A talk.example.org → 203.0.113.7") {
		t.Errorf("the kept record is not noted on --execute:\n%s", run.Lines())
	}
	if strings.Contains(run.Lines(), "fixture-not-a-secret") {
		t.Error("the token was printed")
	}
}

// A provider error partway stops the removal before paisans.yaml is edited,
// says what was deleted and what was not, and the same command resumes.
func TestTheDNSStageStoppedPartwayResumes(t *testing.T) {
	w, zone := dnsWorld(t, ownAddress)
	zone.FailDelete = "rec-blog6"
	err := siteremove.Execute(w.mustBuild("home-b", dnsOptions(zone)))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"stage 4 (dns)", "deleted: A gone.example.org (record rec-blog)", "not deleted: AAAA gone.example.org (record rec-blog6)", "run site remove again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should say %q: %v", want, err)
		}
	}
	if cfg, err := config.Load(w.configPath); err != nil || cfg.Sites["home-b"].Address == "" {
		t.Fatalf("paisans.yaml was edited before the DNS stage finished: %v", err)
	}
	zone.FailDelete = ""
	w.cfg, _ = config.Load(w.configPath)
	if err := siteremove.Execute(w.mustBuild("home-b", dnsOptions(zone))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(zone.Deletes, ","); got != "rec-blog,rec-blog6" {
		t.Errorf("deletes = %s", got)
	}
}

// home-b and watch declare one address in the base world, so a record at it
// may be watch's, and is kept with that reason.
func TestTheDNSStageKeepsASharedAddress(t *testing.T) {
	w, zone := dnsWorld(t)
	zone.InZone["zone-1"] = append(zone.InZone["zone-1"], dns.Record{ID: "rec-shared", Type: "A", Name: "old-uptime.example.org", Content: "203.0.113.20", Comment: comment})
	p := w.mustBuild("home-b", dnsOptions(zone))
	st := stageNamed(p, "dns")
	if len(st.Steps) != 0 {
		t.Errorf("planned %+v", st.Steps)
	}
	out := shown(p, false)
	if !out.Has("note", "keep A old-uptime.example.org → 203.0.113.20") || !strings.Contains(out.Lines(), "203.0.113.20 is also sites.watch.public_address, which stays, so the record may be watch's") {
		t.Errorf("the shared record is not kept with its reason:\n%s", out.Lines())
	}
}

// Each case the stage cannot apply in skips it with one line, and the
// removal goes on.
func TestTheDNSStageSkipsWithOneLine(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(w *world)
		edits []func(string) string
		line  string
	}{
		{"no token", func(w *world) { delete(w.secrets.External, "acme_dns_token") }, nil, "skip dns: the DNS provider's token is not in the secrets"},
		{"no address", func(w *world) {
			b := w.cfg.Sites["home-b"]
			b.PublicAddress, b.PublicAddress6 = "", ""
			w.cfg.Sites["home-b"] = b
		}, nil, "skip dns: home-b has no public address"},
		{"not implemented", func(w *world) { w.cfg.ACME.Provider = "desec" }, nil, "skip dns: desec record management is not implemented"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, zone := dnsWorld(t, ownAddress)
			c.edit(w)
			p := w.mustBuild("home-b", dnsOptions(zone))
			st := stageNamed(p, "dns")
			if st == nil || st.Skipped == "" {
				t.Fatalf("not skipped: %+v", st)
			}
			out := shown(p, false)
			if !out.Has("item", c.line) {
				t.Errorf("no item %q:\n%s", c.line, out.Lines())
			}
			run := &ui.Recorder{}
			p.Report = run
			if err := siteremove.Execute(p); err != nil {
				t.Fatal(err)
			}
			if len(zone.Deletes) != 0 || zone.Listings != 0 {
				t.Errorf("the provider was used: %d listings, deletes %v", zone.Listings, zone.Deletes)
			}
		})
	}
}

// A gateway cannot render without a provider, so a full removal always has
// one; a forced removal, which renders nothing, is where none is declared.
func TestTheDNSStageSkipsWithoutAProvider(t *testing.T) {
	w, zone := dnsWorld(t, ownAddress)
	w.cfg.ACME.Provider = ""
	dest, _ := config.ParseDestination("ubuntu@192.0.2.10")
	p := forced(t, w, w.cfg.WithoutSite("vm"), "vm", dest, dnsOptions(zone))
	if out := shown(p, false); !out.Has("item", "skip dns: no DNS provider declared") {
		t.Errorf("printed:\n%s", out.Lines())
	}
}

// --host-gone does not reach the host, and still deletes its records: they
// point at a host that is gone.
func TestTheDNSStageRunsWithHostGone(t *testing.T) {
	w, zone := dnsWorld(t, ownAddress)
	w.unreachable["home-b"] = true
	w.etcdDown["home-b"] = true
	w.member("home-b")["Role"], w.member("home-b")["State"] = "Replica", "stopped"
	w.member("home-c")["Role"] = "Sync Standby"
	p := w.mustBuild("home-b", siteremove.Options{HostGone: true, DNSProvider: zone.For("cloudflare")})
	if st := stageNamed(p, "dns"); st == nil || st.Skipped != "" || len(st.Steps) != 2 {
		t.Fatalf("want the DNS stage planned: %+v", st)
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(zone.Deletes, ","); got != "rec-blog,rec-blog6" {
		t.Errorf("deletes = %s", got)
	}
}

// A forced removal of a site paisans.yaml still declares leaves its records,
// which are still wanted, and has no DNS stage. One it does not declare has
// no address in it, and skips.
func TestForcedRemovalsDNSStage(t *testing.T) {
	w, zone := dnsWorld(t, ownAddress)
	o := dnsOptions(zone)
	p := forced(t, w, w.cfg, "home-b", w.cfg.Sites["home-b"].Destination(), o)
	if st := stageNamed(p, "dns"); st != nil {
		t.Errorf("a declared site has a DNS stage: %+v", st)
	}
	dest, _ := config.ParseDestination("ubuntu@192.0.2.10")
	p = forced(t, w, w.cfg.WithoutSite("vm"), "vm", dest, o)
	if !shown(p, false).Has("item", "skip dns: vm is not declared; delete its records by hand") {
		t.Errorf("printed:\n%s", shown(p, false).Lines())
	}
	for _, p := range []*siteremove.Plan{p, forced(t, w, w.cfg, "home-b", w.cfg.Sites["home-b"].Destination(), o)} {
		for _, l := range p.Remains() {
			if strings.HasPrefix(l, "DNS") {
				t.Errorf("a forced removal still has the remains line: %s", l)
			}
		}
	}
	if zone.Listings != 0 {
		t.Errorf("a forced removal read the zone %d time(s)", zone.Listings)
	}
}

// Without a configuration nothing can be deleted safely, and every --id run
// says so in one line, with the comment to search for and the address --ssh
// named.
func TestByIDWarnsThatDNSWasNotModified(t *testing.T) {
	w := setup(t)
	var line string
	for _, l := range byID(t, w, "watch", "f2a9", siteremove.Options{}).Remains() {
		if strings.HasPrefix(l, "DNS") {
			line = l
		}
	}
	hint, detail, _ := strings.Cut(line, ". ")
	if hint != "DNS records for this deployment, if any exist, were not modified" {
		t.Errorf("hint = %q", hint)
	}
	if detail != `They carry the comment "paisans-f2a9: created by paisans dns init", and may point at 192.0.2.30` {
		t.Errorf("detail = %q", detail)
	}
	if siteremove.DNSNotModified(w.cfg.Deployment(), config.Destination{User: "u", Host: "box.example.org", Port: 22}) != `DNS records for this deployment, if any exist, were not modified. They carry the comment "paisans-f2a9: created by paisans dns init"` {
		t.Error("a host named by name should add no address")
	}
}

// A provider that cannot be read skips the stage, naming why, so it neither
// blocks a removal nor strands one a re-run must finish.
func TestTheDNSStageSkipsWhenTheProviderCannotBeRead(t *testing.T) {
	w, zone := dnsWorld(t, ownAddress)
	zone.FailList = true
	p := w.mustBuild("home-b", dnsOptions(zone))
	st := stageNamed(p, "dns")
	if st == nil || !strings.HasPrefix(st.SkipLine, "the DNS provider could not be read") {
		t.Fatalf("want a skip naming the provider: %+v", st)
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
}

// An undeclared site's records are not this command's to find; the line
// says so, and that they are deleted by hand.
func TestForcedUndeclaredSaysDeleteByHand(t *testing.T) {
	w, zone := dnsWorld(t, ownAddress)
	dest, _ := config.ParseDestination("ubuntu@192.0.2.10")
	p := forced(t, w, w.cfg.WithoutSite("vm"), "vm", dest, dnsOptions(zone))
	if out := shown(p, false); !out.Has("item", "skip dns: vm is not declared; delete its records by hand") {
		t.Errorf("printed:\n%s", out.Lines())
	}
}

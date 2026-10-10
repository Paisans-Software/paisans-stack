package dns

import (
	"context"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// withLeaving is declared() with home-b, a data site dialled by name and
// reached on its own addresses, which is the site a removal takes out.
func withLeaving() *config.Config {
	cfg := declared()
	cfg.Sites["home-b"] = config.Site{
		Roles: []config.Role{config.RoleData}, Address: "10.44.0.2",
		Endpoint: "home-b.example.org:51820", PublicAddress: "203.0.113.7", PublicAddress6: "2001:db8::7",
	}
	return cfg
}

// zoneWithLeaving is what dns init made for withLeaving(), and what a zone
// gathers besides: records the removal must leave alone for one reason each.
func zoneWithLeaving() *fakeCloudflare {
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	fake.records["zone-1"] = []cfRecord{
		{ID: "rec-endpoint", Type: "A", Name: "home-b.example.org", Content: "203.0.113.7", Comment: recordComment},
		{ID: "rec-endpoint6", Type: "AAAA", Name: "home-b.example.org", Content: "2001:db8::7", Comment: recordComment},
		{ID: "rec-stale", Type: "A", Name: "blog.example.org", Content: "203.0.113.7", Comment: recordComment},
		{ID: "rec-talk", Type: "A", Name: "talk.example.org", Content: "203.0.113.10", Comment: recordComment},
		{ID: "rec-wanted", Type: "A", Name: "docs.example.org", Content: "203.0.113.7", Comment: recordComment},
		{ID: "rec-theirs", Type: "A", Name: "old.example.org", Content: "203.0.113.7", Comment: "paisans-0c1d: created by paisans dns init"},
		{ID: "rec-hand", Type: "A", Name: "hand.example.org", Content: "203.0.113.7"},
		{ID: "rec-txt", Type: "TXT", Name: "home-b.example.org", Content: "v=placeholder", Comment: recordComment},
	}
	return fake
}

func sitePlan(t *testing.T, c Provider, before *config.Config, site string) *PrunePlan {
	t.Helper()
	p, err := BuildSiteRemoval(context.Background(), c, before, site, before.WithoutSite(site))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func removedIDs(p *PrunePlan) string {
	var ids []string
	for _, e := range p.Removes() {
		ids = append(ids, e.ID)
	}
	return strings.Join(ids, ",")
}

func TestSiteRemovalDeletesOnlyTheSitesRecords(t *testing.T) {
	fake := zoneWithLeaving()
	c := fake.serve(t)
	p := sitePlan(t, c, withLeaving(), "home-b")
	if len(fake.deletes) != 0 {
		t.Fatalf("planning deleted %v", fake.deletes)
	}
	if got := removedIDs(p); got != "rec-stale,rec-endpoint,rec-endpoint6" {
		t.Fatalf("removes = %s", got)
	}
	for id, reason := range map[string]string{
		// Another site's address.
		"rec-talk": "203.0.113.10 is not home-b's public_address or public_address6",
		// The site's address, at a name the configuration without it still
		// wants.
		"rec-wanted": "the configuration without home-b still wants a record of type A at this name (apps.docs.hostname)",
		"rec-txt":    "it is a TXT record",
	} {
		e, ok := entry(p, id)
		if !ok || e.Action != Keep {
			t.Errorf("%s: want keep, got %+v", id, e)
			continue
		}
		if !strings.Contains(strings.Join(e.Reasons, "\n"), reason) {
			t.Errorf("%s: reasons should include %q, got %v", id, reason, e.Reasons)
		}
	}
	// Another deployment's record, and one with no comment at all, are not
	// this deployment's to consider, and are not listed.
	for _, id := range []string{"rec-theirs", "rec-hand"} {
		if _, ok := entry(p, id); ok {
			t.Errorf("%s should not be listed", id)
		}
	}
}

func TestSiteRemovalExecuteDeletesAndConfirms(t *testing.T) {
	fake := zoneWithLeaving()
	c := fake.serve(t)
	run := &ui.Recorder{}
	if err := ExecutePrune(context.Background(), c, sitePlan(t, c, withLeaving(), "home-b"), run); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.deletes, ","); got != "rec-stale,rec-endpoint,rec-endpoint6" {
		t.Fatalf("deletes = %s", got)
	}
	if !run.Has("done", "confirm deletes") {
		t.Errorf("no confirmation:\n%s", run.Lines())
	}
	for _, rec := range fake.records["zone-1"] {
		if rec.ID == "rec-hand" || rec.ID == "rec-theirs" || rec.ID == "rec-talk" || rec.ID == "rec-wanted" || rec.ID == "rec-txt" {
			continue
		}
		t.Errorf("%s is still there", rec.ID)
	}
	if again := sitePlan(t, c, withLeaving(), "home-b"); len(again.Removes()) != 0 {
		t.Fatalf("a second plan should delete nothing: %+v", again.Removes())
	}
}

// An address another remaining site declares too may be that site's record,
// so it is kept, and that is the reason given.
func TestSiteRemovalKeepsASharedAddress(t *testing.T) {
	before := withLeaving()
	before.Sites["home-c"] = config.Site{Roles: []config.Role{config.RoleData}, Address: "10.44.0.5", PublicAddress: "203.0.113.7"}
	fake := zoneWithLeaving()
	p := sitePlan(t, fake.serve(t), before, "home-b")
	e, ok := entry(p, "rec-stale")
	if !ok || e.Action != Keep {
		t.Fatalf("want rec-stale kept, got %+v", e)
	}
	if want := "203.0.113.7 is also sites.home-c.public_address, which stays, so the record may be home-c's"; !strings.Contains(strings.Join(e.Reasons, "\n"), want) {
		t.Errorf("reasons = %v, want %q", e.Reasons, want)
	}
	// The AAAA address is home-b's alone, so its record still goes.
	if got := removedIDs(p); got != "rec-endpoint6" {
		t.Errorf("removes = %s", got)
	}
}

// A name outside the domain is in scope while the configuration before
// removal produces it, and only then.
func TestSiteRemovalScopeIsTheConfigurationBeforeRemoval(t *testing.T) {
	before := withLeaving()
	b := before.Sites["home-b"]
	b.Endpoint = "home-b.example.net:51820"
	before.Sites["home-b"] = b
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	fake.zones["example.net"] = "zone-2"
	fake.records["zone-2"] = []cfRecord{
		{ID: "rec-net", Type: "A", Name: "home-b.example.net", Content: "203.0.113.7", Comment: recordComment},
		{ID: "rec-other", Type: "A", Name: "old.example.net", Content: "203.0.113.7", Comment: recordComment},
	}
	p := sitePlan(t, fake.serve(t), before, "home-b")
	if got := removedIDs(p); got != "rec-net" {
		t.Fatalf("removes = %s", got)
	}
	if e, ok := entry(p, "rec-other"); !ok || e.Action != Keep || !strings.Contains(strings.Join(e.Reasons, "\n"), "outside community.domain") {
		t.Errorf("want rec-other kept as out of scope, got %+v", e)
	}
}

// The site must declare an address: with none, nothing points at it.
func TestSiteRemovalRefusesASiteWithNoAddress(t *testing.T) {
	before := withLeaving()
	_, err := BuildSiteRemoval(context.Background(), newFake().serve(t), before, "home-a", before.WithoutSite("home-a"))
	if err == nil || !strings.Contains(err.Error(), "no public address") {
		t.Fatalf("err = %v", err)
	}
}

// A provider error partway names what was deleted and what was not, and a
// fresh plan resumes with what is left.
func TestSiteRemovalStoppedPartwaySaysWhatWasDeleted(t *testing.T) {
	fake := zoneWithLeaving()
	fake.failDelete = "rec-endpoint"
	c := fake.serve(t)
	err := ExecutePrune(context.Background(), c, sitePlan(t, c, withLeaving(), "home-b"), ui.Discard)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"deleted: A blog.example.org (record rec-stale)", "not deleted: A home-b.example.org (record rec-endpoint), AAAA home-b.example.org (record rec-endpoint6)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should say %q: %v", want, err)
		}
	}
	fake.failDelete = ""
	if got := removedIDs(sitePlan(t, c, withLeaving(), "home-b")); got != "rec-endpoint,rec-endpoint6" {
		t.Errorf("a re-run should plan what is left, got %s", got)
	}
}

// A record's type must match its address's family: an AAAA holding an
// IPv4-mapped address canonicalises to the site's IPv4 address, and dns init
// never made one.
func TestARecordWhoseFamilyDoesNotMatchItsTypeIsKept(t *testing.T) {
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	fake.records["zone-1"] = []cfRecord{
		{ID: "rec-mapped", Type: "AAAA", Name: "gone.example.org", Content: "::ffff:203.0.113.7", Comment: recordComment},
		{ID: "rec-a6", Type: "A", Name: "gone6.example.org", Content: "2001:db8::7", Comment: recordComment},
	}
	p := sitePlan(t, fake.serve(t), withLeaving(), "home-b")
	if got := removedIDs(p); got != "" {
		t.Fatalf("removes = %s", got)
	}
	for _, id := range []string{"rec-mapped", "rec-a6"} {
		if e, _ := entry(p, id); !strings.Contains(strings.Join(e.Reasons, "\n"), "is not an address of the family") {
			t.Errorf("%s: reasons = %v", id, e.Reasons)
		}
	}
}

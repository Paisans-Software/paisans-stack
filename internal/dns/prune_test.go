package dns

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// zoneAfterTheMove is the zone a deployment leaves behind when it moves from
// one shared media hostname to one per app: the old name's record is still
// there, beside records prune must leave alone for one reason each.
func zoneAfterTheMove() *fakeCloudflare {
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	fake.records["zone-1"] = []cfRecord{
		{ID: "rec-stale", Type: "A", Name: "media.example.org", Content: "203.0.113.10", Comment: recordComment},
		{ID: "rec-wanted", Type: "A", Name: "talk.example.org", Content: "203.0.113.10", Comment: recordComment},
		{ID: "rec-hand", Type: "A", Name: "old.example.org", Content: "203.0.113.10"},
		{ID: "rec-edited", Type: "A", Name: "older.example.org", Content: "203.0.113.10", Comment: recordComment + " by hand"},
		{ID: "rec-foreign", Type: "A", Name: "blog.example.org", Content: "198.51.100.7", Comment: recordComment},
		{ID: "rec-cname", Type: "CNAME", Name: "www.example.org", Content: "talk.example.org", Comment: recordComment},
	}
	return fake
}

func prunePlan(t *testing.T, c *cloudflare) *PrunePlan {
	t.Helper()
	cfg := deployment()
	wants, err := Desired(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildPrune(context.Background(), c, cfg, wants)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func entry(p *PrunePlan, id string) (PruneEntry, bool) {
	for _, e := range p.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return PruneEntry{}, false
}

func TestPruneDryRunRemovesOnlyTheStaleToolkitRecord(t *testing.T) {
	fake := zoneAfterTheMove()
	c := fake.serve(t)
	p := prunePlan(t, c)

	var out bytes.Buffer
	p.Write(&out)
	t.Logf("dry run:\n%s", out.String())

	if len(fake.deletes) != 0 {
		t.Fatalf("a dry run deleted %v", fake.deletes)
	}
	if got := p.Removes(); len(got) != 1 || got[0].ID != "rec-stale" {
		t.Fatalf("want only rec-stale removed, got %+v", got)
	}
	if !strings.Contains(out.String(), "remove A    media.example.org -> 203.0.113.10  (zone example.org, record rec-stale)") {
		t.Errorf("dry run should name type, name, content, zone and id:\n%s", out.String())
	}
	for id, reason := range map[string]string{
		"rec-wanted":  "still wants a record of type A at this name (apps.talk.hostname)",
		"rec-foreign": "198.51.100.7 is not any site's public_address",
		"rec-cname":   "it is a CNAME record",
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
	// Without the exact comment a record was never the toolkit's, and is not
	// even listed.
	for _, id := range []string{"rec-hand", "rec-edited"} {
		if _, ok := entry(p, id); ok {
			t.Errorf("%s has no toolkit comment and should not be listed", id)
		}
	}
}

func TestPruneExecuteDeletesAndConfirms(t *testing.T) {
	fake := zoneAfterTheMove()
	c := fake.serve(t)
	if err := ExecutePrune(context.Background(), c, prunePlan(t, c)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.deletes, ",") != "rec-stale" {
		t.Fatalf("want one DELETE of rec-stale, got %v", fake.deletes)
	}
	if len(fake.records["zone-1"]) != 5 {
		t.Fatalf("every other record should survive, got %+v", fake.records["zone-1"])
	}
	// A second run finds nothing to remove.
	if again := prunePlan(t, c); len(again.Removes()) != 0 {
		t.Fatalf("second run should remove nothing: %+v", again.Entries)
	}
}

func TestPruneExecuteCatchesADeleteThatDidNotLand(t *testing.T) {
	fake := zoneAfterTheMove()
	fake.dropDeletes = true
	c := fake.serve(t)
	err := ExecutePrune(context.Background(), c, prunePlan(t, c))
	if err == nil || !strings.Contains(err.Error(), "still listed: A media.example.org (record rec-stale)") {
		t.Fatalf("want a confirmation failure, got %v", err)
	}
}

func TestPruneKeepsAToolkitRecordOutsideTheDomain(t *testing.T) {
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	fake.zones["example.net"] = "zone-2"
	fake.records["zone-2"] = []cfRecord{
		{ID: "rec-net", Type: "A", Name: "old.example.net", Content: "203.0.113.10", Comment: recordComment},
	}
	cfg := deployment()
	talk := cfg.Apps["talk"]
	talk.Hostname = "talk.example.net"
	cfg.Apps["talk"] = talk
	wants, err := Desired(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildPrune(context.Background(), fake.serve(t), cfg, wants)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := entry(p, "rec-net")
	if !ok || e.Action != Keep || !strings.Contains(strings.Join(e.Reasons, "\n"), "outside community.domain") {
		t.Fatalf("want rec-net kept as outside the domain, got %+v", e)
	}
}

func TestPrunePagesThroughTheWholeZone(t *testing.T) {
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	for i := 0; i < 250; i++ {
		fake.records["zone-1"] = append(fake.records["zone-1"], cfRecord{
			ID: fmt.Sprintf("rec-txt-%d", i), Type: "TXT", Name: fmt.Sprintf("t%d.example.org", i), Content: "v=placeholder",
		})
	}
	fake.records["zone-1"] = append(fake.records["zone-1"],
		cfRecord{ID: "rec-stale", Type: "A", Name: "media.example.org", Content: "203.0.113.10", Comment: recordComment})
	c := fake.serve(t)
	p := prunePlan(t, c)
	if strings.Join(fake.pages, ",") != "1,2,3" {
		t.Fatalf("want pages 1,2,3 asked for, got %v", fake.pages)
	}
	if got := p.Removes(); len(got) != 1 || got[0].ID != "rec-stale" {
		t.Fatalf("the record on the last page should be found, got %+v", got)
	}
}

func TestPruneTokenNeverAppearsInAnError(t *testing.T) {
	fake := newFake()
	fake.echoToken = true
	c := fake.serve(t)
	cfg := deployment()
	wants, err := Desired(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		func() error { _, err := BuildPrune(context.Background(), c, cfg, wants); return err }(),
		func() error { _, err := c.AllRecords(context.Background(), "zone-1"); return err }(),
		c.Delete(context.Background(), "zone-1", "rec-stale"),
	} {
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatalf("the token leaked into an error: %v", err)
		}
		if !strings.Contains(err.Error(), "redacted") {
			t.Errorf("expected the echoed token to be redacted: %v", err)
		}
	}
}

// --name vouches for one exact name outside community.domain that the
// configuration no longer produces, lifting the scope rule for it alone. The
// first real use: a media hostname configured explicitly beside the domain and
// dropped when media moved to per-app hostnames.
func TestPruneRemovesAVouchedNameOutsideTheDomain(t *testing.T) {
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	fake.zones["example.net"] = "zone-2"
	fake.records["zone-2"] = []cfRecord{
		{ID: "rec-net", Type: "A", Name: "old.example.net", Content: "203.0.113.10", Comment: recordComment},
		{ID: "rec-foreign", Type: "A", Name: "other.example.net", Content: "198.51.100.7", Comment: recordComment},
	}
	cfg := deployment()
	talk := cfg.Apps["talk"]
	talk.Hostname = "talk.example.net"
	cfg.Apps["talk"] = talk
	wants, err := Desired(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildPrune(context.Background(), fake.serve(t), cfg, wants, "old.example.net", "other.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := entry(p, "rec-net"); !ok || e.Action != Remove {
		t.Fatalf("want the vouched rec-net removed, got %+v", e)
	}
	if e, ok := entry(p, "rec-foreign"); !ok || e.Action != Keep {
		t.Fatalf("vouching lifts only the scope rule; a foreign address must still be kept, got %+v", e)
	}
	if _, err := BuildPrune(context.Background(), fake.serve(t), cfg, wants, "typo.example.net"); err == nil || !strings.Contains(err.Error(), "matches no record") {
		t.Fatalf("a --name matching nothing was accepted: %v", err)
	}
}

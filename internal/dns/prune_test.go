package dns

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// fixtureID is the fixture deployment's id, and recordComment the comment its
// dns init writes.
const fixtureID = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"

var recordComment = RecordComment(deployment.Deployment{ID: fixtureID})

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
	cfg := declared()
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

	out := &ui.Recorder{Verbose_: true}
	p.Show(out)
	t.Logf("dry run:\n%s", out.Lines())

	if len(fake.deletes) != 0 {
		t.Fatalf("a dry run deleted %v", fake.deletes)
	}
	if got := p.Removes(); len(got) != 1 || got[0].ID != "rec-stale" {
		t.Fatalf("want only rec-stale removed, got %+v", got)
	}
	if !out.Has("pending", "delete A media.example.org -> 203.0.113.10 (zone example.org, record rec-stale)") || out.Has("item", "") {
		t.Errorf("dry run should mark the delete pending, naming type, name, content, zone and id:\n%s", out.Lines())
	}
	// A kept record is done, with each rule it fails under it.
	if i := out.Index("done", "keep A talk.example.org -> 203.0.113.10 (zone example.org, record rec-wanted)"); i < 0 ||
		!out.Has("detail", "still wants a record of type A at this name") {
		t.Errorf("a kept record is not done with its reasons:\n%s", out.Lines())
	}
	quiet := &ui.Recorder{}
	p.Show(quiet)
	if quiet.Has("done", "") || quiet.Has("note", "") || !quiet.Has("pending", "delete A media.example.org") {
		t.Errorf("without --verbose, only the delete is a line:\n%s", quiet.Lines())
	}
	var b strings.Builder
	p.Show(ui.NewPlain(&b, true))
	shown := b.String()
	t.Logf("\n%s", shown)
	for _, want := range []string{
		"  todo delete A media.example.org -> 203.0.113.10 (zone example.org, record rec-stale)\n",
		"  ok   keep A talk.example.org -> 203.0.113.10 (zone example.org, record rec-wanted)\n",
		"      it is a CNAME record",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, line := range strings.Split(shown, "\n") {
		for _, label := range []string{"remove ", "keep   ", "    "} {
			if strings.HasPrefix(strings.TrimPrefix(line, "      "), label) {
				t.Errorf("a padded label: %q", line)
			}
		}
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
	if err := ExecutePrune(context.Background(), c, prunePlan(t, c), ui.Discard); err != nil {
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
	err := ExecutePrune(context.Background(), c, prunePlan(t, c), ui.Discard)
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
	cfg := declared()
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
	// Every other rule would delete it, so it is the operator's to vouch
	// for or delete: a note, with the reason, at every verbosity.
	out := &ui.Recorder{}
	p.Show(out)
	i := out.Index("note", "keep A old.example.net -> 203.0.113.10 (zone example.net, record rec-net)")
	if i < 0 || !strings.Contains(out.Events[i].Extra, "--name old.example.net") {
		t.Errorf("the record outside the domain is not a note naming --name:\n%s", out.Lines())
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
	cfg := declared()
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
	cfg := declared()
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

// A record is this deployment's only under its own token's comment. One
// carrying another deployment's token, or the comment without any token, is
// not listed, even at a name and address this deployment would otherwise
// prune.
func TestPruneMatchesOnlyThisDeploymentsToken(t *testing.T) {
	if recordComment != "paisans-f2a9: created by paisans dns init" {
		t.Fatalf("the comment is %q, want it to carry paisans-f2a9", recordComment)
	}
	fake := newFake()
	fake.zones["example.org"] = "zone-1"
	fake.records["zone-1"] = []cfRecord{
		{ID: "rec-ours", Type: "A", Name: "media.example.org", Content: "203.0.113.10", Comment: recordComment},
		{ID: "rec-theirs", Type: "A", Name: "old.example.org", Content: "203.0.113.10", Comment: "paisans-0c1d: created by paisans dns init"},
		{ID: "rec-untokened", Type: "A", Name: "older.example.org", Content: "203.0.113.10", Comment: "created by paisans dns init"},
	}
	c := fake.serve(t)
	p := prunePlan(t, c)
	if got := p.Removes(); len(got) != 1 || got[0].ID != "rec-ours" {
		t.Fatalf("want only rec-ours removed, got %+v", got)
	}
	for _, id := range []string{"rec-theirs", "rec-untokened"} {
		if _, ok := entry(p, id); ok {
			t.Errorf("%s does not carry this deployment's token and should not be listed", id)
		}
	}
}

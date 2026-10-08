package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// A held stack's files are neither compared nor written and its action does
// not run, while the mesh and every other stack apply as usual. The next
// whole apply sees the held stack's files as a first write, not a conflict.
func TestExceptHoldsAStackBackAndAppliesTheRest(t *testing.T) {
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Except("talk"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Changes {
		if c.Stack == "talk" {
			t.Errorf("talk is held back, yet %s is planned", c.Path)
		}
	}
	for _, a := range p.Actions {
		if a.Stack == "talk" {
			t.Errorf("talk is held back, yet it is acted on")
		}
	}
	if p.WireGuard == apply.WireGuardNone {
		t.Error("holding an app back held the mesh back too")
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if host.ran("/srv/talk/compose.yaml") {
		t.Error("talk's stack was acted on")
	}
	if _, ok := host.files["/srv/talk/.env"]; ok {
		t.Error("talk's files were written")
	}

	whole, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.Conflicts()) != 0 {
		t.Fatalf("the whole apply after a held one sees conflicts: %v", whole.Conflicts())
	}
	if len(whole.Actions) != 1 || whole.Actions[0].Stack != "talk" {
		t.Errorf("the whole apply after a held one plans %v, want talk alone", whole.Actions)
	}
}

// A held stack a stopped apply still owes stays owed, so the apply that
// releases it force-recreates it.
func TestExceptKeepsAHeldStackOwed(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/srv/.paisans-pending.json"] = `{"version":1,"actions":[{"stack":"docs","recreate":true},{"stack":"talk","recreate":true}]}`
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Except("talk"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	record, ok := host.files["/srv/.paisans-pending.json"]
	if !ok || !strings.Contains(record, `"talk"`) || strings.Contains(record, `"docs"`) {
		t.Fatalf("after holding talk back the record of owed actions is %q, want talk alone", record)
	}
	next, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Actions) != 1 || next.Actions[0].Stack != "talk" || !next.Actions[0].Force {
		t.Errorf("releasing talk plans %+v, want talk force-recreated", next.Actions)
	}
}

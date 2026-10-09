package main

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// The dry-run plan is one item per step, in the order Execute takes them;
// the reasons are details.
func TestListPlanItemsInOrderWithReasonsAsDetail(t *testing.T) {
	plan := &apply.Plan{
		Site: "home-a", Transport: "ubuntu@192.0.2.10",
		Changes:       []apply.Change{{Path: "/srv/paisans/f2a9/infra/compose.yaml", Kind: apply.Create}, {Path: "/srv/paisans/f2a9/talk/.env", Kind: apply.Update}},
		Actions:       []apply.Action{{Stack: "infra", Recreate: true, Reason: "an environment or compose file changed"}},
		GatewayReload: true,
	}
	rec := &ui.Recorder{Verbose_: true}
	listPlan(rec, plan)
	order := []int{rec.Index("item", "write 2 files"), rec.Index("item", "recreate infra"), rec.Index("item", "reload gateway")}
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] < order[i-1] {
			t.Fatalf("items missing or out of order:\n%s", rec.Lines())
		}
	}
	if !rec.Has("detail", "an environment or compose file changed") {
		t.Errorf("reason not a detail:\n%s", rec.Lines())
	}
	if !rec.Has("detail", "/srv/paisans/f2a9/talk/.env") {
		t.Errorf("file paths not details:\n%s", rec.Lines())
	}
}

// A file edited on the host is a refusal in the plan itself, so a dry run
// says at once that nothing would be applied.
func TestListPlanRefusesConflicts(t *testing.T) {
	plan := &apply.Plan{
		Site: "home-a", Transport: "ubuntu@192.0.2.10",
		Changes: []apply.Change{{Path: "/srv/paisans/f2a9/talk/.env", Kind: apply.Conflict}},
	}
	rec := &ui.Recorder{}
	listPlan(rec, plan)
	i := rec.Index("refuse", "1 file was edited on the host")
	if i < 0 {
		t.Fatalf("no refusal for the conflict:\n%s", rec.Lines())
	}
	if !strings.Contains(rec.Events[i].Extra, "/srv/paisans/f2a9/talk/.env") {
		t.Errorf("the refusal does not name the file by default:\n%s", rec.Lines())
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// The dry-run plan marks each step pending, in the order Execute takes them;
// the reasons are details.
func TestListPlanMarksStepsPendingInOrderWithReasonsAsDetail(t *testing.T) {
	plan := &apply.Plan{
		Site: "home-a", Transport: "ubuntu@192.0.2.10",
		Changes:       []apply.Change{{Path: "/srv/paisans/f2a9/infra/compose.yaml", Kind: apply.Create}, {Path: "/srv/paisans/f2a9/talk/.env", Kind: apply.Update}},
		Actions:       []apply.Action{{Stack: "infra", Recreate: true, Reason: "an environment or compose file changed"}},
		GatewayReload: true,
	}
	rec := &ui.Recorder{Verbose_: true}
	listPlan(rec, plan)
	order := []int{rec.Index("pending", "write 2 files"), rec.Index("pending", "recreate infra"), rec.Index("pending", "reload gateway")}
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] < order[i-1] {
			t.Fatalf("steps missing or out of order:\n%s", rec.Lines())
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

// A note the operator has to act on is a warning on a dry run and on
// --execute alike, by default, where --execute lists no plan; a note that is
// only information is a detail. The same note found again by a later pass is
// said once.
func TestPlanNotesShowByDefaultOnDryRunAndExecute(t *testing.T) {
	plan := &apply.Plan{Notes: []apply.Note{
		{Hint: "infra network differs from its compose file: run infra down, then apply --recreate infra", Text: "the infrastructure stack's network paisans-f2a9-infra_default does not match"},
		{Text: "gone was owed an action by a stopped apply"},
	}}
	for _, execute := range []bool{false, true} {
		var b strings.Builder
		r := ui.NewPlain(&b, false)
		seen := map[string]bool{}
		presentPlan(r, plan, execute, seen)
		presentPlan(r, plan, execute, seen)
		out := b.String()
		if strings.Count(out, "  WARN infra network differs from its compose file: run infra down, then apply --recreate infra\n") != 1 {
			t.Errorf("execute=%v: the note to act on is not one warning: %q", execute, out)
		}
		if strings.Contains(out, "gone was owed") {
			t.Errorf("execute=%v: an informational note shows by default: %q", execute, out)
		}
	}
	rec := &ui.Recorder{Verbose_: true}
	presentPlan(rec, plan, true, map[string]bool{})
	if !rec.Has("detail", "gone was owed") {
		t.Errorf("the informational note is not a detail:\n%s", rec.Lines())
	}
}

// Piped, each step of the plan carries the writer's own word and no padded
// column.
func TestListPlanPlainMarksStepsTodo(t *testing.T) {
	plan := &apply.Plan{
		Site: "home-a", Transport: "ubuntu@192.0.2.10",
		Changes:       []apply.Change{{Path: "/srv/paisans/f2a9/infra/compose.yaml", Kind: apply.Create}, {Path: "/srv/paisans/f2a9/talk/.env", Kind: apply.Update}},
		Actions:       []apply.Action{{Stack: "infra", Recreate: true, Reason: "an environment or compose file changed"}},
		GatewayReload: true,
	}
	var b strings.Builder
	listPlan(ui.NewPlain(&b, true), plan)
	out := b.String()
	t.Logf("\n%s", out)
	for _, want := range []string{
		"  todo write 2 files\n",
		"      create /srv/paisans/f2a9/infra/compose.yaml\n",
		"  todo recreate infra\n",
		"      an environment or compose file changed\n",
		"  todo wait for infra\n",
		"  todo reload gateway\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "  -    ") {
		t.Errorf("a step is a padded item:\n%s", out)
	}
}

package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// A note said while a step is open (a pruned image) belongs to that step:
// it never breaks the step's line, and it shows with --verbose.
func TestSayDuringStepBecomesDetail(t *testing.T) {
	var b strings.Builder
	p := apply.NewPlanForTest(ui.NewPlain(&b, false))
	done := apply.StepForTest(p, "prune old images")
	apply.SayForTest(p, "removed %s", "ghcr.io/x/y:1")
	done.Done("")
	if strings.Contains(b.String(), "removed") {
		t.Errorf("note shown without --verbose: %q", b.String())
	}
	if strings.Count(b.String(), "\n") != 1 {
		t.Errorf("note broke the step line: %q", b.String())
	}
}

func TestActionTitles(t *testing.T) {
	for _, c := range []struct {
		a    apply.Action
		want string
	}{
		{apply.Action{Stack: "infra", Recreate: true}, "recreate infra"},
		{apply.Action{Stack: "infra", Recreate: true, Force: true}, "recreate infra (forced)"},
		{apply.Action{Stack: "talk", Down: true}, "replace talk"},
		{apply.Action{Stack: "talk", Services: []string{"php", "messenger"}}, "restart talk (php, messenger)"},
		{apply.Action{Stack: "talk"}, "restart talk"},
	} {
		if got := apply.ActionTitle(c.a); got != c.want {
			t.Errorf("%+v: %q, want %q", c.a, got, c.want)
		}
	}
}

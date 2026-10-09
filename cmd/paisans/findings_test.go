package main

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/ui"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

func TestRefusalShowsHintAndExplanation(t *testing.T) {
	var b strings.Builder
	res := validate.Result{Findings: []validate.Finding{
		{Level: validate.Refuse, Rule: "witness-shares-failure-domain", Key: "sites.home-c", Hint: "witness shares a failure domain with its only voter", Message: "Put the witness in another failure domain."},
		{Level: validate.Warn, Rule: "garage-consistency-dangerous", Key: "storage.garage.consistency", Hint: "garage consistency is dangerous", Message: "Reads can miss a recent upload."},
	}}
	reportFindings(ui.NewPlain(&b, false), "paisans.yaml", res)
	out := b.String()
	for _, want := range []string{"paisans.yaml\n", "FAIL witness shares a failure domain", "Put the witness in another failure domain.", "WARN garage consistency is dangerous", "paisans.yaml: 1 refusal, 1 warning"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, hidden := range []string{"Reads can miss", "witness-shares-failure-domain", "garage-consistency-dangerous"} {
		if strings.Contains(out, hidden) {
			t.Errorf("default output shows %q:\n%s", hidden, out)
		}
	}
	b.Reset()
	reportFindings(ui.NewPlain(&b, true), "paisans.yaml", res)
	for _, want := range []string{"Reads can miss", "garage-consistency-dangerous", "storage.garage.consistency"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("verbose output lacks %q:\n%s", want, b.String())
		}
	}
}

func TestNoFindingsPrintsNothing(t *testing.T) {
	var b strings.Builder
	reportFindings(ui.NewPlain(&b, false), "paisans.yaml", validate.Result{})
	if b.Len() != 0 {
		t.Errorf("printed %q for a clean file", b.String())
	}
}

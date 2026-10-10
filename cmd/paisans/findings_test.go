package main

import (
	"errors"
	"path/filepath"
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
	for _, want := range []string{"paisans.yaml\n", "FAIL witness shares a failure domain", "sites.home-c: Put the witness in another failure domain.", "WARN garage consistency is dangerous", "paisans.yaml: 1 refusal, 1 warning"} {
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
	// A verbose warning's detail is prose, wrapped to the width: compare it
	// as words.
	flat := strings.Join(strings.Fields(b.String()), " ")
	for _, want := range []string{"storage.garage.consistency: Reads can miss a recent upload. (garage-consistency-dangerous)"} {
		if !strings.Contains(flat, want) {
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

func TestCountsPluralAndWarningOnly(t *testing.T) {
	var b strings.Builder
	two := validate.Result{Findings: []validate.Finding{
		{Level: validate.Refuse, Rule: "a", Key: "k1", Hint: "h1", Message: "m1"},
		{Level: validate.Refuse, Rule: "b", Key: "k2", Hint: "h2", Message: "m2"},
	}}
	reportFindings(ui.NewPlain(&b, false), "paisans.yaml", two)
	if !strings.Contains(b.String(), "paisans.yaml: 2 refusals") {
		t.Errorf("no plural count in\n%s", b.String())
	}
	b.Reset()
	warns := validate.Result{Findings: []validate.Finding{
		{Level: validate.Warn, Rule: "a", Key: "k1", Hint: "h1", Message: "m1"},
		{Level: validate.Warn, Rule: "b", Key: "k2", Hint: "h2", Message: "m2"},
	}}
	reportFindings(ui.NewPlain(&b, false), "paisans.yaml", warns)
	if !strings.Contains(b.String(), "paisans.yaml: 2 warnings\n") || strings.Contains(b.String(), "refusal") {
		t.Errorf("bad warning-only count in\n%s", b.String())
	}
}

// A refused configuration ends the command with a short hint naming the file
// and the count, and says what to do.
func TestRefusedConfigurationIsAProblem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paisans.yaml")
	err := refused(path, 2, "")
	var p *ui.Problem
	if !errors.As(err, &p) {
		t.Fatalf("not a ui.Problem: %v", err)
	}
	if p.Hint != ui.ShortPath(path)+" was refused: 2 refusals above" {
		t.Errorf("hint: %q", p.Hint)
	}
	if !strings.Contains(p.Explain, "Fix each refusal above") {
		t.Errorf("explanation: %q", p.Explain)
	}
	err = refused("paisans.yaml", 1, "Secrets are not generated for a configuration that cannot be deployed.")
	if !errors.As(err, &p) || p.Hint != "paisans.yaml was refused: 1 refusal above" || !strings.Contains(p.Explain, "Secrets are not generated") {
		t.Errorf("got %#v", p)
	}
}

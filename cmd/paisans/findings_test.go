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
	reportValidation(ui.NewPlain(&b, false), "paisans.yaml", two)
	if !strings.Contains(b.String(), "paisans.yaml: 2 refusals") {
		t.Errorf("no plural count in\n%s", b.String())
	}
	b.Reset()
	warns := validate.Result{Findings: []validate.Finding{
		{Level: validate.Warn, Rule: "a", Key: "k1", Hint: "h1", Message: "m1"},
		{Level: validate.Warn, Rule: "b", Key: "k2", Hint: "h2", Message: "m2"},
	}}
	reportValidation(ui.NewPlain(&b, false), "paisans.yaml", warns)
	if !strings.Contains(b.String(), "paisans.yaml: 2 warnings\n") || strings.Contains(b.String(), "refusal") {
		t.Errorf("bad warning-only count in\n%s", b.String())
	}
}

// A command other than validate goes on past a warning, so its findings end
// with no count line naming the file again; a refusal stops it, and keeps
// its count.
func TestCommandsCountOnlyARefusal(t *testing.T) {
	var b strings.Builder
	warn := validate.Finding{Level: validate.Warn, Rule: "a", Key: "k1", Hint: "h1", Message: "m1"}
	reportFindings(ui.NewPlain(&b, false), "paisans.yaml", validate.Result{Findings: []validate.Finding{warn}})
	if got := strings.Count(b.String(), "paisans.yaml"); got != 1 || strings.Contains(b.String(), "1 warning") {
		t.Errorf("a warning alone is counted, or the path shown %d times:\n%s", got, b.String())
	}
	b.Reset()
	refuse := validate.Finding{Level: validate.Refuse, Rule: "b", Key: "k2", Hint: "h2", Message: "m2"}
	reportFindings(ui.NewPlain(&b, false), "paisans.yaml", validate.Result{Findings: []validate.Finding{refuse, warn}})
	if !strings.Contains(b.String(), "paisans.yaml: 1 refusal, 1 warning\n") {
		t.Errorf("a refusal is not counted:\n%s", b.String())
	}
}

// Under a section of a command's own, the findings follow its header and
// the path is not a section: it appears only in a refusal's count.
func TestFindingsUnderASection(t *testing.T) {
	warn := validate.Finding{Level: validate.Warn, Rule: "a", Key: "k1", Hint: "h1", Message: "m1"}
	rec := &ui.Recorder{}
	reportFindingsUnder(rec, "configuration", "paisans.yaml", validate.Result{Findings: []validate.Finding{warn}})
	if len(rec.Events) != 2 || rec.Events[0] != (ui.Event{Kind: "section", Text: "configuration"}) || rec.Events[1].Kind != "warn" {
		t.Errorf("got:\n%s", rec.Lines())
	}
	rec = &ui.Recorder{}
	refuse := validate.Finding{Level: validate.Refuse, Rule: "b", Key: "k2", Hint: "h2", Message: "m2"}
	reportFindingsUnder(rec, "configuration", "paisans.yaml", validate.Result{Findings: []validate.Finding{refuse}})
	if last := rec.Events[len(rec.Events)-1]; last.Kind != "result" || last.Text != "paisans.yaml: 1 refusal" || rec.Has("section", "paisans.yaml") {
		t.Errorf("got:\n%s", rec.Lines())
	}
	rec = &ui.Recorder{}
	reportFindingsUnder(rec, "configuration", "paisans.yaml", validate.Result{})
	if len(rec.Events) != 0 {
		t.Errorf("printed for a clean file:\n%s", rec.Lines())
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

// A configuration under the current directory is named relative to it by
// default, and in full with -v.
func TestFindingsNameTheFileShortByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "staging", "paisans.yaml")
	res := validate.Result{Findings: []validate.Finding{{Level: validate.Refuse, Rule: "r", Key: "k", Hint: "h", Message: "m"}}}
	var b strings.Builder
	reportFindings(ui.NewPlain(&b, false), path, res)
	if strings.Contains(b.String(), dir) || !strings.HasPrefix(b.String(), "staging/paisans.yaml\n") || !strings.Contains(b.String(), "\nstaging/paisans.yaml: 1 refusal") {
		t.Errorf("default:\n%s", b.String())
	}
	b.Reset()
	reportFindings(ui.NewPlain(&b, true), path, res)
	if !strings.HasPrefix(b.String(), path+"\n") {
		t.Errorf("verbose:\n%s", b.String())
	}
}

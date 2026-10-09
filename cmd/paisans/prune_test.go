package main

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

type danglingHost struct{ listing string }

func (h danglingHost) Run(string) (string, error)           { return h.listing, nil }
func (h danglingHost) RunInput(c, _ string) (string, error) { return h.Run(c) }
func (danglingHost) ReadFile(string) (string, bool, error)  { return "", false, nil }
func (danglingHost) WriteFile(string, string, uint32) error { return nil }
func (danglingHost) Describe() string                       { return "ubuntu@home-a.local" }

// The dry run says what its verdicts rest on before listing them, and shows
// each volume's size and contents, so an operator can tell a leaked cache
// from somebody's data before agreeing.
func TestPruneDryRunShowsTheAssumptionAndEachVolume(t *testing.T) {
	host := danglingHost{listing: "volume\t3a37a98261c4f658850d43b3d0ddc746ae25d9ec6bb58e83132662b7ea646191\t48234496\t{\"community.paisans.deployment\":\"f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01\"}\tcache log\n" +
		"volume\tother_db\t9999\t{\"com.docker.compose.project\":\"other\"}\tpgdata\nend\n"}
	plan, err := apply.BuildVolumePrune(deployment.Deployment{ID: "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}, "home-a", host)
	if err != nil {
		t.Fatal(err)
	}
	rec := &ui.Recorder{Verbose_: true}
	showVolumePrune(rec, plan)
	out := rec.Lines()
	t.Log("\n" + out)
	if !rec.Has("item", "remove volume 3a37a98261c4") || rec.Has("item", "other_db") {
		t.Errorf("only the volume to remove is an item:\n%s", out)
	}
	for _, want := range []string{
		"only when it carries this deployment's label",
		"remove    3a37a98261c4f658850d43b3d0ddc746ae25d9ec6bb58e83132662b7ea646191  46.0 MiB  [cache log]",
		"keep      other_db",
		"labelled by compose project other",
		"1 of 2 dangling volume(s), 46.0 MiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not say %q:\n%s", want, out)
		}
	}
}

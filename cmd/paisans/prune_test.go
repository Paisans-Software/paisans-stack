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
	if !rec.Has("pending", "remove volume 3a37a98261c4") || rec.Has("pending", "other_db") || rec.Has("item", "") {
		t.Errorf("only the volume to remove is pending:\n%s", out)
	}
	if !rec.Has("done", "keep volume other_db") {
		t.Errorf("the kept volume is not marked done:\n%s", out)
	}
	for _, want := range []string{
		"only when it carries this deployment's label",
		"detail: 3a37a98261c4f658850d43b3d0ddc746ae25d9ec6bb58e83132662b7ea646191 (46.0 MiB): cache log",
		"detail: other_db",
		"labelled by compose project other",
		"detail: total: 1 of 2 dangling volume(s), 46.0 MiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not say %q:\n%s", want, out)
		}
	}

	// Without --verbose only what would be removed is a line.
	quiet := &ui.Recorder{}
	showVolumePrune(quiet, plan)
	if !quiet.Has("pending", "remove volume 3a37a98261c4") || quiet.Has("done", "") {
		t.Errorf("got:\n%s", quiet.Lines())
	}

	// Plain output carries the writer's own words and no label column.
	var b strings.Builder
	showVolumePrune(ui.NewPlain(&b, true), plan)
	plain := b.String()
	t.Logf("\n%s", plain)
	for _, want := range []string{
		"  todo remove volume 3a37a98261c4\n",
		"      3a37a98261c4f658850d43b3d0ddc746ae25d9ec6bb58e83132662b7ea646191 (46.0 MiB): cache log\n",
		"  ok   keep volume other_db\n",
		"      total: 1 of 2 dangling volume(s), 46.0 MiB\n",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, line := range strings.Split(plain, "\n") {
		for _, label := range []string{"remove ", "keep ", "total "} {
			if strings.HasPrefix(strings.TrimSpace(line), label) {
				t.Errorf("a padded label: %q", line)
			}
		}
	}
}

// Only an anonymous volume's 64 hex name is shortened. A named volume is
// shown whole, since this list is the consent to delete its data.
func TestPruneShortensOnlyAnonymousVolumeNames(t *testing.T) {
	anon := strings.Repeat("ab", 32)
	plan := &apply.VolumePrune{Volumes: []apply.DanglingVolume{
		{Name: anon, Remove: true}, {Name: "paisans-f2a9-old_data", Remove: true}, {Name: "paisans-f2a9-old_logs", Remove: true},
	}}
	rec := &ui.Recorder{}
	showVolumePrune(rec, plan)
	if !rec.Has("pending", "remove volume abababababab") || rec.Has("pending", anon) {
		t.Errorf("anonymous name not shortened:\n%s", rec.Lines())
	}
	if !rec.Has("pending", "remove volume paisans-f2a9-old_data") || !rec.Has("pending", "remove volume paisans-f2a9-old_logs") {
		t.Errorf("named volumes are not told apart:\n%s", rec.Lines())
	}
}

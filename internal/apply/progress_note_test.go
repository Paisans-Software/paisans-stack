package apply

import (
	"strings"
	"testing"
	"time"
)

// A note said while a step is open, such as an image pruned, starts its own
// line, and the step's ending follows on a line of its own rather than being
// lost at the end of the note.
func TestANoteDuringAStepKeepsBothLines(t *testing.T) {
	at := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	t.Cleanup(SetNow(func() time.Time { at = at.Add(time.Second); return at }))
	var said strings.Builder
	p := &Plan{Progress: &said}

	done := p.step("pruning", "superseded images of talk")
	p.say("  %-9s %s\n", "pruned", "ghcr.io/example/talk:1")
	done(nil)

	want := "  pruning   superseded images of talk ... \n" +
		"  pruned    ghcr.io/example/talk:1\n" +
		"            done (1.0s)\n"
	if said.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", said.String(), want)
	}
}

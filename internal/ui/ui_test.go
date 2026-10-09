package ui_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/ui"
)

func clock() func() time.Time {
	at := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	return func() time.Time { at = at.Add(1500 * time.Millisecond); return at }
}

// Piped output is for logs: one line per finished step, words not glyphs,
// and nothing a terminal would interpret.
func TestPlainStepIsOneLineWhenItEnds(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, false, false, clock())
	r.Section("home-a (ubuntu@192.0.2.10)")
	s := r.Step("recreate infra")
	if b.String() != "home-a (ubuntu@192.0.2.10)\n" {
		t.Fatalf("a plain step printed before it ended: %q", b.String())
	}
	s.Done("")
	want := "home-a (ubuntu@192.0.2.10)\n  ok   recreate infra          1.5s\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

func TestPlainHasNoEscapes(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, true, false, clock())
	r.Section("s")
	r.Step("a").Done("x")
	r.Step("b").Fail(errors.New("boom"))
	r.Warn("w", "detail")
	r.Refuse("r", "because")
	r.Trace("ufw status", "Status: active\n")
	if strings.ContainsAny(b.String(), "\x1b\r") {
		t.Fatalf("plain output carries escapes: %q", b.String())
	}
	for _, want := range []string{"  ok   a", "  FAIL b", "  WARN w", "  FAIL r", "       because", "      detail", "      | Status: active"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %q", want, b.String())
		}
	}
}

// On a terminal the step line is drawn with a spinner while it runs and is
// replaced in place by the finished line.
func TestTerminalSpinnerIsReplacedByFinishedLine(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, false, true, clock())
	s := r.Step("recreate infra")
	ui.Frame(r)
	s.Done("")
	out := b.String()
	if !strings.Contains(out, "⠋ recreate infra") {
		t.Errorf("no spinner frame: %q", out)
	}
	last := out[strings.LastIndex(out, "\r\x1b[K"):]
	if !strings.HasPrefix(last, "\r\x1b[K  \x1b[32m✓\x1b[0m recreate infra") || !strings.HasSuffix(last, "s\n") {
		t.Errorf("finished line did not replace the spinner: %q", last)
	}
}

func TestFailStopsSpinnerBeforeErrorLine(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, false, true, clock())
	s := r.Step("reload gateway")
	ui.Frame(r)
	s.Fail(errors.New("caddy rejected the config"))
	ended := b.String()
	ui.Frame(r) // a late tick must draw nothing
	if b.String() != ended {
		t.Errorf("spinner drew after the step ended: %q", b.String()[len(ended):])
	}
	last := ended[strings.LastIndex(ended, "\r\x1b[K"):]
	if !strings.Contains(last, "✗") || !strings.Contains(last, "reload gateway") || !strings.HasSuffix(last, "\n") {
		t.Fatalf("last drawn line is not the finished failure: %q", last)
	}
}

// Detail is the verbose layer: absent by default, under its step otherwise,
// and printed after the step's line so it never breaks it.
func TestDetailDuringOpenStepPrintsAfterStepLine(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		var b strings.Builder
		r := ui.NewForTest(&b, verbose, false, clock())
		s := r.Step("prune images")
		r.Detail("removed %s", "ghcr.io/x/y:1")
		s.Detail("kept %d", 2)
		s.Done("")
		out := b.String()
		has := strings.Contains(out, "removed ghcr.io/x/y:1")
		if has != verbose {
			t.Fatalf("verbose=%v: detail present=%v in %q", verbose, has, out)
		}
		if verbose && strings.Index(out, "prune images") > strings.Index(out, "removed") {
			t.Errorf("detail printed before its step line: %q", out)
		}
	}
}

func TestWarnHidesDetailUnlessVerbose(t *testing.T) {
	var quiet, loud strings.Builder
	ui.NewForTest(&quiet, false, false, clock()).Warn("garage consistency is dangerous", "reads can miss recent uploads")
	ui.NewForTest(&loud, true, false, clock()).Warn("garage consistency is dangerous", "reads can miss recent uploads")
	if strings.Contains(quiet.String(), "reads can miss") {
		t.Errorf("warning detail shown without --verbose: %q", quiet.String())
	}
	if !strings.Contains(loud.String(), "reads can miss") {
		t.Errorf("warning detail missing with --verbose: %q", loud.String())
	}
}

func TestRefuseAlwaysShowsExplanation(t *testing.T) {
	var b strings.Builder
	ui.NewForTest(&b, false, false, clock()).Refuse("witness shares a failure domain", "Put the witness elsewhere.")
	if !strings.Contains(b.String(), "Put the witness elsewhere.") {
		t.Errorf("refusal explanation hidden: %q", b.String())
	}
}

func TestStepResultAndShortStepsHaveNoTime(t *testing.T) {
	var b strings.Builder
	at := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	r := ui.NewForTest(&b, false, false, func() time.Time { at = at.Add(100 * time.Millisecond); return at })
	r.Step("disk space").Done("13.6 GiB free")
	r.Step("claim site").Done("")
	if !strings.Contains(b.String(), "  ok   disk space              13.6 GiB free\n") {
		t.Errorf("result not in the second column: %q", b.String())
	}
	if !strings.Contains(b.String(), "  ok   claim site\n") {
		t.Errorf("a short step printed a time: %q", b.String())
	}
}

func TestRecorderKeepsOrder(t *testing.T) {
	rec := &ui.Recorder{}
	rec.Section("home-a")
	s := rec.Step("recreate infra")
	s.Detail("an environment file changed")
	s.Done("")
	if rec.Index("step", "recreate infra") > rec.Index("done", "recreate infra") {
		t.Fatal("events out of order")
	}
	if !rec.Has("detail", "an environment file changed") {
		t.Fatal("step detail not recorded")
	}
}

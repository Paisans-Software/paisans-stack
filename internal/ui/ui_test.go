package ui_test

import (
	"errors"
	"slices"
	"strings"
	"sync"
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
	want := "home-a (ubuntu@192.0.2.10)\n  ok   recreate infra: 1.5s\n"
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

// A hint is one line, up to 100 characters. A terminal narrower than that
// line gets it wrapped at spaces, its continuation under the hint's text;
// a wider one, or output whose width is not known, gets it whole.
func TestAHintWrapsOnlyOnANarrowTerminal(t *testing.T) {
	hint := "garage consistency is dangerous: an upload is confirmed even if only 1 of 2 copies is written"
	for _, c := range []struct {
		terminal bool
		cols     int
		mark     string
	}{{true, 0, "!"}, {true, 120, "!"}, {false, 0, "WARN"}} {
		var b strings.Builder
		ui.NewForTestWidth(&b, c.terminal, c.cols, clock()).Warn(hint, "")
		if lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n"); len(lines) != 1 || !strings.HasSuffix(lines[0], " "+hint) {
			t.Errorf("terminal %v, %d columns: not one whole line:\n%s", c.terminal, c.cols, b.String())
		}
	}
	for _, c := range []struct {
		terminal bool
		indent   string
	}{{true, "    "}, {false, "       "}} {
		var b strings.Builder
		ui.NewForTestWidth(&b, c.terminal, 60, clock()).Warn(hint, "")
		lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
		if len(lines) < 2 {
			t.Fatalf("terminal %v: not wrapped at 60 columns:\n%s", c.terminal, b.String())
		}
		var words []string
		for i, l := range lines {
			visible := strings.NewReplacer("\x1b[33m", "", "\x1b[0m", "").Replace(l)
			if n := len([]rune(visible)); n > 60 {
				t.Errorf("terminal %v: line %d is %d columns: %q", c.terminal, i, n, visible)
			}
			if i > 0 && (!strings.HasPrefix(l, c.indent) || strings.HasPrefix(l, c.indent+" ")) {
				t.Errorf("terminal %v: continuation not under the hint's text: %q", c.terminal, l)
			}
			words = append(words, strings.Fields(visible)...)
		}
		if got := strings.Join(words[1:], " "); got != hint {
			t.Errorf("terminal %v: words changed: %q", c.terminal, got)
		}
	}
}

// A hold clears the spinner's row and its resume redraws the spinner on
// the row the cursor is on, with no newline: a prompt erased during the hold
// leaves no empty line between the section header and the step.
func TestAResumedHoldRedrawsWithoutANewline(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, false, true, clock())
	r.Section("etcd members")
	s := r.Step("read home-a's etcd record")
	b.Reset()
	resume := ui.Hold(r)
	resume()
	if out := b.String(); strings.Contains(out, "\n") || !strings.Contains(out, "read home-a's etcd record") {
		t.Errorf("hold and resume wrote %q", out)
	}
	s.End(ui.Pending, "not founded yet")
	if out := b.String(); strings.Count(out, "\n") != 1 {
		t.Errorf("the step's line is not the only line: %q", out)
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
	if !strings.Contains(b.String(), "  ok   disk space: 13.6 GiB free\n") {
		t.Errorf("result does not follow its title: %q", b.String())
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

// syncBuf lets a test read output while a spinner goroutine may be drawing
// into it. A bare strings.Builder would race with the goroutine under -race.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls until cond holds, so a test does not depend on how many ticks
// the scheduler happens to fit into a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// assertSilent fails if any output is written over a window of spinner ticks.
func assertSilent(t *testing.T, b *syncBuf, what string) {
	t.Helper()
	before := b.String()
	time.Sleep(20 * time.Millisecond)
	if after := b.String(); after != before {
		t.Errorf("%s: output grew: %q", what, after[len(before):])
	}
}

// A spinner with a real tick runs on its own goroutine. Ending the step must
// stop it, so nothing is drawn once Done returns.
func TestSpinnerStopsWhenStepEnds(t *testing.T) {
	var b syncBuf
	r := ui.NewForTestTick(&b, false, true, time.Now, time.Millisecond)
	s := r.Step("recreate infra")
	waitFor(t, "spinner ticks", func() bool { return strings.Count(b.String(), "recreate infra") >= 2 })
	s.Done("")
	assertSilent(t, &b, "after Done")
}

// Two steps open at once must not share a spinner. Opening the second stops
// the first's spinner, so the first's frames stop appearing; ending either
// step must not stop the other's, and once both have ended nothing draws.
func TestOverlappingStepsEachStopTheirOwnSpinner(t *testing.T) {
	var b syncBuf
	r := ui.NewForTestTick(&b, false, true, time.Now, time.Millisecond)
	first := r.Step("first step")
	waitFor(t, "first spinner ticks", func() bool { return strings.Count(b.String(), "first step") >= 2 })

	second := r.Step("second step")
	mark := b.String()
	waitFor(t, "second spinner ticks", func() bool { return strings.Count(b.String(), "second step") >= 2 })
	if strings.Contains(b.String()[len(mark):], "first step") {
		t.Fatalf("earlier step's spinner kept drawing after a later step opened: %q", b.String()[len(mark):])
	}

	first.Done("")
	mark = b.String()
	waitFor(t, "second spinner after first ended", func() bool { return strings.Count(b.String(), "second step") > strings.Count(mark, "second step") })

	second.Done("")
	assertSilent(t, &b, "after both steps ended")
	if !strings.Contains(b.String(), "✓") {
		t.Errorf("finished lines missing: %q", b.String())
	}
}

// A prompt on the terminal (a sudo password, an ssh host key) would be wiped
// by the next spinner frame. While held, the spinner line is cleared and no
// frame draws; resuming draws the open step again.
func TestHoldClearsSpinnerAndStopsFramesUntilResumed(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, false, true, clock())
	s := r.Step("check mesh subnet")
	resume := ui.Hold(r)
	if !strings.HasSuffix(b.String(), "\r\x1b[K") {
		t.Fatalf("holding did not clear the spinner line: %q", b.String())
	}
	held := b.String()
	ui.Frame(r)
	if b.String() != held {
		t.Fatalf("a frame drew while held: %q", b.String()[len(held):])
	}
	resume()
	if !strings.Contains(b.String()[len(held):], "check mesh subnet") {
		t.Fatalf("resuming did not draw the step again: %q", b.String()[len(held):])
	}
	resumed := b.String()
	ui.Frame(r)
	if b.String() == resumed {
		t.Fatal("frames did not restart after resume")
	}
	resume() // a second call must not release someone else's hold
	s.Done("")
}

// Holds nest: the spinner draws again only once every hold is released.
func TestNestedHoldsResumeOnlyAtTheLast(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, false, true, clock())
	r.Step("a")
	outer := ui.Hold(r)
	inner := ui.Hold(r)
	inner()
	held := b.String()
	ui.Frame(r)
	if b.String() != held {
		t.Fatalf("drew with a hold still in place: %q", b.String()[len(held):])
	}
	outer()
	ui.Frame(r)
	if b.String() == held {
		t.Fatal("did not draw once every hold was released")
	}
}

// Plain output has no spinner, and Discard and Recorder draw nothing, so a
// hold there writes nothing.
func TestHoldIsANoOpWithoutASpinner(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, false, false, clock())
	r.Step("a")
	ui.Hold(r)()
	ui.Hold(ui.Discard)()
	ui.Hold(&ui.Recorder{})()
	if b.String() != "" {
		t.Fatalf("a plain hold wrote %q", b.String())
	}
}

// A note is something left for the operator: its hint and its detail show at
// every verbosity, since the detail names the object the hint is about.
func TestNoteAlwaysShowsDetail(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		var b strings.Builder
		ui.NewForTest(&b, false, terminal, clock()).Note("a client kept; delete it in Pocket ID", "It is talk (id 1234)\nat pocket-id")
		out := b.String()
		for _, want := range []string{"a client kept; delete it in Pocket ID", "       It is talk (id 1234)\n", "       at pocket-id\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("terminal=%v: missing %q in %q", terminal, want, out)
			}
		}
		if !terminal && !strings.HasPrefix(out, "  WARN ") {
			t.Errorf("plain note is not a warning line: %q", out)
		}
	}
	rec := &ui.Recorder{}
	rec.Note("h", "d")
	if !rec.Has("note", "h") || rec.Events[0].Extra != "d" {
		t.Errorf("recorder lost the note: %s", rec.Lines())
	}
}

// A section or an item said while a step is drawing must not land on the
// spinner's line.
func TestSectionAndItemClearTheSpinnerLine(t *testing.T) {
	for name, say := range map[string]func(ui.Reporter){
		"section": func(r ui.Reporter) { r.Section("home-b") },
		"item":    func(r ui.Reporter) { r.Item("recreate app") },
	} {
		var b strings.Builder
		r := ui.NewForTest(&b, false, true, clock())
		r.Step("a")
		before := b.String()
		say(r)
		if !strings.HasPrefix(b.String()[len(before):], "\r\x1b[K") {
			t.Errorf("%s printed onto the spinner line: %q", name, b.String()[len(before):])
		}
	}
}

// A dry run's status line ends a step with one of four marks. Piped, each is
// a word in the mark's column; the result follows the title either way.
func TestPlainEndMarksAreWords(t *testing.T) {
	var b strings.Builder
	at := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	r := ui.NewForTest(&b, false, false, func() time.Time { return at })
	r.Step("host prepare --site a").End(ui.OK, "")
	r.Step("apply --site a").End(ui.Pending, "3 changes")
	r.Step("apply --site b").End(ui.Waiting, "after host prepare --site b")
	r.Step("dns init").End(ui.Failed, "the provider did not answer")
	want := "  ok   host prepare --site a\n" +
		"  todo apply --site a: 3 changes\n" +
		"  wait apply --site b: after host prepare --site b\n" +
		"  FAIL dns init: the provider did not answer\n"
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// On a terminal pending is a yellow ○ and waiting a dim line with a ·, each
// replacing the spinner as a finished step does.
func TestTerminalEndMarks(t *testing.T) {
	for _, c := range []struct {
		mark ui.Mark
		want string
	}{
		{ui.OK, "\r\x1b[K  \x1b[32m✓\x1b[0m a"},
		{ui.Failed, "\r\x1b[K  \x1b[31m✗\x1b[0m a: boom"},
		{ui.Pending, "\r\x1b[K  \x1b[33m○\x1b[0m a: 3 changes"},
		{ui.Waiting, "\r\x1b[K  \x1b[2m· a: after b\x1b[0m"},
	} {
		var b strings.Builder
		r := ui.NewForTest(&b, false, true, func() time.Time { return time.Time{} })
		result := map[ui.Mark]string{ui.Failed: "boom", ui.Pending: "3 changes", ui.Waiting: "after b"}[c.mark]
		r.Step("a").End(c.mark, result)
		out := b.String()
		last := out[strings.LastIndex(out, "\r\x1b[K"):]
		if last != c.want+"\n" {
			t.Errorf("mark %v: %q, want %q", c.mark, last, c.want+"\n")
		}
	}
}

func TestRecorderKeepsEachMark(t *testing.T) {
	rec := &ui.Recorder{}
	rec.Step("a").End(ui.OK, "")
	rec.Step("b").End(ui.Pending, "3 changes")
	rec.Step("c").End(ui.Waiting, "after a")
	rec.Step("d").End(ui.Failed, "boom")
	ui.Discard.Step("e").End(ui.Pending, "x")
	for _, want := range []ui.Event{{"done", "a", ""}, {"pending", "b", "3 changes"}, {"waiting", "c", "after a"}, {"fail", "d", "boom"}} {
		if !slices.Contains(rec.Events, want) {
			t.Errorf("no %+v in:\n%s", want, rec.Lines())
		}
	}
}

// A status line (End) shows its own result and never the elapsed time: a
// dry run's ✓ says the step is up to date, not how long checking took. A
// finished step (Done) still shows it.
func TestEndShowsNoElapsedTime(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, false, false, clock())
	r.Step("checked").End(ui.OK, "")
	r.Step("ran").Done("")
	out := b.String()
	if first := strings.Split(out, "\n")[0]; strings.HasSuffix(first, "s") && strings.ContainsAny(first, "0123456789") {
		t.Errorf("a status line shows a time: %q", out)
	}
	if !strings.Contains(out, "1.5s") {
		t.Errorf("a finished step lost its time: %q", out)
	}
}

// A result follows its title after a colon, with no column to pad to: a
// short title is not followed by a run of spaces sized to a long one.
func TestAResultFollowsItsTitle(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, false, false, clock())
	r.Step("init").End(ui.Pending, "generated 4 secrets")
	r.Step("host prepare --site luthen-rael").End(ui.OK, "")
	want := "  todo init: generated 4 secrets\n  ok   host prepare --site luthen-rael\n"
	if b.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", b.String(), want)
	}
}

// Prose under a refusal, a note and a verbose warning wraps to 80 columns,
// aligned under the hint, and never splits a path.
func TestRefuseNoteAndWarnWrapTheirProse(t *testing.T) {
	long := "Move what holds each one, or change the paisans.yaml key it names, and run again; the file is /var/lib/paisans/registry.json on the host, read with sudo."
	for _, terminal := range []bool{false, true} {
		var b strings.Builder
		r := ui.NewForTest(&b, true, terminal, clock())
		r.Refuse("refused", long)
		r.Note("noted", long)
		r.Warn("warned", long)
		lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
		if len(lines) < 9 {
			t.Fatalf("terminal %v: not wrapped:\n%s", terminal, b.String())
		}
		for _, l := range lines {
			if len(l) > 80 {
				t.Errorf("terminal %v: a line is wider than 80: %q", terminal, l)
			}
		}
		if !strings.Contains(b.String(), " /var/lib/paisans/registry.json ") && !strings.Contains(b.String(), " /var/lib/paisans/registry.json\n") && !strings.Contains(b.String(), "\n       /var/lib/paisans/registry.json") {
			t.Errorf("terminal %v: the path was split:\n%s", terminal, b.String())
		}
		if !strings.Contains(b.String(), "\n       Move what holds") || !strings.Contains(b.String(), "\n      Move what holds") {
			t.Errorf("terminal %v: the prose is not aligned as before:\n%s", terminal, b.String())
		}
	}
}

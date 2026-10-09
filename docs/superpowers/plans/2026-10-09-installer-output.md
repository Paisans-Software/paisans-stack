# Installer-style output Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every `paisans` command prints one short line per step, with the reasoning, configuration values and raw command output behind `-v/--verbose`.

**Architecture:** A new `internal/ui` package holds one `Reporter` that every command and library writes through. It renders steps with a spinner and colour on a terminal and as plain lines elsewhere, and it drops `Detail`/`Trace` unless verbose. Libraries that took an `io.Writer` (`Plan.Progress`, `Print(w)`) take a `ui.Reporter` instead and stop formatting text themselves.

**Tech Stack:** Go 1.x standard library, `golang.org/x/term` (already a dependency) for terminal detection.

**Spec:** `docs/specs/2026-10-09-installer-output.md`

## Global Constraints

- Errors print in full at every verbosity. Never put error text behind `--verbose`.
- Default mode: no rationale, no configuration values, no request bodies, no image digests, no sub-steps. Warnings print their hint only; refusals print hint and explanation.
- `--verbose` loses nothing relative to today's output: every line removed from default output becomes a `Detail` or `Trace`.
- Terminal rendering only when the writer is a TTY and `NO_COLOR` is unset and `TERM` is not `dumb`. Otherwise no ANSI codes, no `\r`, no spinner; words `ok`, `FAIL`, `WARN` instead of glyphs.
- No em dashes in any text you write: code comments, strings, docs, commit messages (repo `AGENTS.md`).
- Every rule in code comments says why (repo `AGENTS.md`). Do not write rejected alternatives anywhere.
- No real hostnames or addresses: fixtures use `example.org`, `home-a`, `home-b`, `192.0.2.x`, `203.0.113.x`.
- Commits: `<type>: <lowercase imperative subject>`, a body explaining why, ending `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Never use `git stash` (worktrees share it).
- After every task: `gofmt -l .` prints nothing, `go vet ./...` and `go test ./...` pass.

## Review Focus

1. **A step that fails while a spinner is drawing.** The spinner goroutine must stop and the line must be replaced by the `✗` line before the error prints, with no leftover spinner frame. Test in Task 1 (`TestFailStopsSpinnerBeforeErrorLine`).
2. **A note said while a step is open** (today's `Plan.say` from prune, anon volumes, database). It must not break the step line in default mode and must appear under the step in verbose mode. Test in Task 1 (`TestDetailDuringOpenStepPrintsAfterStepLine`) and Task 4 (`TestSayDuringStepBecomesDetail`).
3. **Output piped to a file or CI.** No `\x1b` and no `\r` anywhere. Test in Task 1 (`TestPlainHasNoEscapes`) and Task 11 (`TestDefaultOutputIsShortAndPlain`).
4. **A refusal in default mode.** Hint and explanation both shown; the rule ID only with `--verbose`. Test in Task 3 (`TestRefusalShowsHintAndExplanation`).
5. **A command whose run fails half way (`--execute`).** The failed step's `✗` line is the last step line, and the error text follows in full on stderr. Test in Task 5 (`TestExecuteFailureShowsFailedStepAndFullError`).

---

## File structure

| File | Responsibility |
|------|----------------|
| `internal/ui/ui.go` | `Reporter`, `Step` interfaces; `New`, `Discard` |
| `internal/ui/render.go` | the writer-backed reporter: plain and terminal rendering, spinner |
| `internal/ui/recorder.go` | `Recorder` for tests: structured events |
| `internal/ui/ui_test.go` | rendering tests |
| `cmd/paisans/flags.go` | `commonFlags(fs)`: `-v`, `--verbose`, returns a reporter factory |
| `cmd/paisans/findings.go` | `reportFindings(r, path, result)`, replacing `report()` in `main.go` |
| `internal/validate/*.go` | `Finding.Hint`; `warn`/`refuse` take a hint |
| `internal/apply/progress.go`, `apply.go` | `Plan.Report ui.Reporter` replaces `Progress io.Writer` |
| `cmd/paisans/plan.go` | `listPlan(r, plan)`, replacing `printPlan`/`printBootstrap` in `main.go` |
| `internal/hostcheck/classify.go` | `Report.Show(r)` replacing `Print(w)` |
| `internal/oidcclient/oidcclient.go` | `Step.Title` (short) + `Step.Detail` (today's `Line`) |
| staged packages (`siteadd`, `siteremove`, `storageadd`, `rotatekey`, `appremove`) | `Plan.Report` replacing `Progress`; `Show(r)` replacing `Print(w)` |
| `internal/hostprep`, `doctor`, `preflight`, `failover`, `ingress`, `garage`, `dns` | `Show(r)` replacing `Print(w)` / `Out` |
| `README.md`, `docs/development.md` | output samples and the progress section |

---

### Task 1: `internal/ui` package

**Files:**
- Create: `internal/ui/ui.go`, `internal/ui/render.go`, `internal/ui/recorder.go`, `internal/ui/export_test.go`, `internal/ui/ui_test.go`

**Interfaces:**
- Produces:
  - `type Reporter interface { Section(title string); Step(title string) Step; Item(title string); Warn(hint, detail string); Refuse(hint, explanation string); Detail(format string, args ...any); Trace(label, output string); Result(format string, args ...any); Verbose() bool }`
  - `type Step interface { Done(result string); Fail(err error); Detail(format string, args ...any) }`
  - `func New(w io.Writer, verbose bool) Reporter` (detects TTY when `w` is an `*os.File`)
  - `func NewPlain(w io.Writer, verbose bool) Reporter` (never terminal; tests and stderr-bound reports)
  - `var Discard Reporter`
  - `type Recorder struct { Verbose_ bool; Events []Event }` implementing `Reporter`; `type Event struct { Kind, Text, Extra string }` with kinds `section step done fail item warn refuse detail trace result`; `func (r *Recorder) Has(kind, text string) bool`; `func (r *Recorder) Index(kind, text string) int`

- [ ] **Step 1: Write the failing tests**

`internal/ui/export_test.go`:

```go
package ui

import (
	"io"
	"time"
)

// NewForTest builds a reporter with terminal rendering forced on or off and
// a clock the test controls, so spinner frames and elapsed times are fixed.
func NewForTest(w io.Writer, verbose, terminal bool, clock func() time.Time) Reporter {
	r := newWriter(w, verbose, terminal)
	r.now = clock
	r.tick = 0 // no spinner goroutine; tests call Frame
	return r
}

// Frame draws one spinner frame for the open step, as the goroutine would.
func Frame(r Reporter) { r.(*writer).frame() }
```

`internal/ui/ui_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/ui/`
Expected: FAIL, package `ui` has no non-test Go files / undefined: `newWriter`.

- [ ] **Step 3: Implement**

`internal/ui/ui.go`:

```go
// Package ui is how every paisans command reports to the operator. Commands
// and libraries say what happened (a step started, ended, failed; a warning;
// a detail) and the reporter decides how it looks, so the toolkit reads like
// one installer rather than a dozen programs with their own habits.
//
// There are two levels. By default an operator sees one line per step and
// its result. With --verbose they also see every Detail and Trace: the
// reasoning, configuration values and raw command output that explain a
// step, which matter when something is wrong and are noise when it is not.
// Errors are never a detail: a failed command must not need a second run to
// say why it failed.
package ui

import (
	"io"
	"os"

	"golang.org/x/term"
)

type Reporter interface {
	// Section starts a block: a site, a configuration file, a stage.
	Section(title string)
	// Step starts something that ends in success or failure.
	Step(title string) Step
	// Item is one line of a dry run's plan: a step that would run.
	Item(title string)
	// Warn shows hint always and detail only when verbose.
	Warn(hint, detail string)
	// Refuse shows hint and explanation always: a refusal stops the command,
	// and the operator needs the reason to fix it.
	Refuse(hint, explanation string)
	// Detail is shown only when verbose. While a step is open it belongs to
	// that step and prints after the step's line.
	Detail(format string, args ...any)
	// Trace is raw output from a command or an API call, shown only when
	// verbose.
	Trace(label, output string)
	// Result is the closing line of a command.
	Result(format string, args ...any)
	// Verbose reports whether details are shown, for callers that would
	// otherwise do work only to produce one.
	Verbose() bool
}

type Step interface {
	// Done ends the step. result is shown in the second column; empty means
	// the elapsed time, when the step took a second or more.
	Done(result string)
	// Fail ends the step as failed. The error itself is the caller's to
	// return and print; Fail marks the line.
	Fail(err error)
	// Detail attaches a verbose line to this step.
	Detail(format string, args ...any)
}

// New reports to w, drawing with colour and a spinner when w is a terminal
// that allows it.
func New(w io.Writer, verbose bool) Reporter {
	return newWriter(w, verbose, isTerminal(w))
}

// NewPlain reports to w without any terminal drawing.
func NewPlain(w io.Writer, verbose bool) Reporter {
	return newWriter(w, verbose, false)
}

// Discard reports nothing.
var Discard Reporter = discard{}

// isTerminal is whether w is a terminal that should be drawn on. NO_COLOR
// (no-color.org) and TERM=dumb turn drawing off, so an operator or a CI
// system can ask for plain lines.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return false
	}
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

type discard struct{}

func (discard) Section(string)               {}
func (discard) Step(string) Step             { return discardStep{} }
func (discard) Item(string)                  {}
func (discard) Warn(string, string)          {}
func (discard) Refuse(string, string)        {}
func (discard) Detail(string, ...any)        {}
func (discard) Trace(string, string)         {}
func (discard) Result(string, ...any)        {}
func (discard) Verbose() bool                { return false }

type discardStep struct{}

func (discardStep) Done(string)           {}
func (discardStep) Fail(error)            {}
func (discardStep) Detail(string, ...any) {}
```

`internal/ui/render.go`:

```go
package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// titleWidth is the first column. Steps stream, so the column cannot be
// sized to the widest title in advance; a fixed width keeps results aligned
// for every title an installer line should have.
const titleWidth = 24

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	green  = "\x1b[32m"
	red    = "\x1b[31m"
	yellow = "\x1b[33m"
	reset  = "\x1b[0m"
	clear  = "\r\x1b[K"
)

type writer struct {
	mu       sync.Mutex
	w        io.Writer
	verbose  bool
	terminal bool
	now      func() time.Time
	tick     time.Duration

	open    *step
	frame_  int
	stopped chan struct{}
}

func newWriter(w io.Writer, verbose, terminal bool) *writer {
	return &writer{w: w, verbose: verbose, terminal: terminal, now: time.Now, tick: 100 * time.Millisecond}
}

type step struct {
	r       *writer
	title   string
	started time.Time
	details []string
	ended   bool
}

func (r *writer) Verbose() bool { return r.verbose }

func (r *writer) Section(title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.w, "%s\n", title)
}

func (r *writer) Step(title string) Step {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &step{r: r, title: title, started: r.now()}
	r.open = s
	if r.terminal {
		r.frame_ = 0
		r.drawLocked()
		if r.tick > 0 {
			r.stopped = make(chan struct{})
			go r.spin(r.stopped)
		}
	}
	return s
}

func (r *writer) spin(stop chan struct{}) {
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.frame()
		}
	}
}

// frame draws the next spinner frame for the open step, if there is one.
func (r *writer) frame() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.open == nil || !r.terminal {
		return
	}
	r.frame_++
	r.drawLocked()
}

func (r *writer) drawLocked() {
	s := r.open
	elapsed := r.now().Sub(s.started)
	fmt.Fprintf(r.w, "%s  %s %s", clear, frames[r.frame_%len(frames)], pad(s.title))
	if elapsed >= time.Second {
		fmt.Fprintf(r.w, "%ds", int(elapsed.Seconds()))
	}
}

func (s *step) Detail(format string, args ...any) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if s.r.verbose && !s.ended {
		s.details = append(s.details, fmt.Sprintf(format, args...))
	}
}

func (s *step) Done(result string) { s.end(true, result) }

func (s *step) Fail(err error) { s.end(false, "") }

func (s *step) end(ok bool, result string) {
	r := s.r
	r.mu.Lock()
	if s.ended {
		r.mu.Unlock()
		return
	}
	s.ended = true
	stop := r.stopped
	r.stopped = nil
	if r.open == s {
		r.open = nil
	}
	if result == "" {
		if d := r.now().Sub(s.started); d >= time.Second {
			result = fmt.Sprintf("%.1fs", d.Seconds())
		}
	}
	mark := r.mark(ok)
	line := "  " + mark + " " + strings.TrimRight(pad(s.title)+result, " ")
	if r.terminal {
		line = clear + line
	}
	fmt.Fprintln(r.w, line)
	for _, d := range s.details {
		r.detailLocked(d)
	}
	r.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

func (r *writer) mark(ok bool) string {
	switch {
	case r.terminal && ok:
		return green + "✓" + reset
	case r.terminal:
		return red + "✗" + reset
	case ok:
		return "ok  "
	default:
		return "FAIL"
	}
}

func (r *writer) Item(title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminal {
		fmt.Fprintf(r.w, "  · %s\n", title)
		return
	}
	fmt.Fprintf(r.w, "  -    %s\n", title)
}

func (r *writer) Warn(hint, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interruptLocked()
	if r.terminal {
		fmt.Fprintf(r.w, "  %s!%s %s\n", yellow, reset, hint)
	} else {
		fmt.Fprintf(r.w, "  WARN %s\n", hint)
	}
	if r.verbose && detail != "" {
		r.detailLocked(detail)
	}
}

func (r *writer) Refuse(hint, explanation string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interruptLocked()
	if r.terminal {
		fmt.Fprintf(r.w, "  %s✗%s %s\n", red, reset, hint)
	} else {
		fmt.Fprintf(r.w, "  FAIL %s\n", hint)
	}
	for _, line := range strings.Split(strings.TrimSpace(explanation), "\n") {
		if line != "" {
			fmt.Fprintf(r.w, "       %s\n", line)
		}
	}
}

func (r *writer) Detail(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.verbose {
		return
	}
	text := fmt.Sprintf(format, args...)
	if r.open != nil {
		r.open.details = append(r.open.details, text)
		return
	}
	r.detailLocked(text)
}

func (r *writer) Trace(label, output string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.verbose {
		return
	}
	text := label + ":"
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		text += "\n| " + line
	}
	if r.open != nil {
		r.open.details = append(r.open.details, text)
		return
	}
	r.detailLocked(text)
}

func (r *writer) Result(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interruptLocked()
	fmt.Fprintf(r.w, format+"\n", args...)
}

// detailLocked prints a verbose line indented under what it belongs to.
func (r *writer) detailLocked(text string) {
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(r.w, "      %s\n", line)
	}
}

// interruptLocked ends a drawn spinner line before something else is
// printed, so a warning said mid-step does not land on the spinner's line.
// The spinner redraws on its next tick.
func (r *writer) interruptLocked() {
	if r.terminal && r.open != nil {
		fmt.Fprint(r.w, clear)
	}
}

func pad(title string) string {
	if len(title) >= titleWidth {
		return title + " "
	}
	return title + strings.Repeat(" ", titleWidth-len(title))
}
```

`internal/ui/recorder.go`:

```go
package ui

import (
	"fmt"
	"strings"
	"sync"
)

// Event is one thing a Recorder was told.
type Event struct {
	Kind  string // section step done fail item warn refuse detail trace result
	Text  string // the title, hint or text
	Extra string // a step's result, a warning's detail, a refusal's explanation, a fail's error
}

// Recorder is a Reporter for tests: it keeps what was said, in order, so a
// test asserts on steps and outcomes rather than on spacing.
type Recorder struct {
	mu      sync.Mutex
	Verbose_ bool
	Events  []Event
}

func (r *Recorder) add(kind, text, extra string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Events = append(r.Events, Event{kind, text, extra})
}

func (r *Recorder) Section(title string)        { r.add("section", title, "") }
func (r *Recorder) Item(title string)           { r.add("item", title, "") }
func (r *Recorder) Warn(hint, detail string)    { r.add("warn", hint, detail) }
func (r *Recorder) Refuse(hint, expl string)    { r.add("refuse", hint, expl) }
func (r *Recorder) Detail(f string, a ...any)   { r.add("detail", fmt.Sprintf(f, a...), "") }
func (r *Recorder) Trace(label, output string)  { r.add("trace", label, output) }
func (r *Recorder) Result(f string, a ...any)   { r.add("result", fmt.Sprintf(f, a...), "") }
func (r *Recorder) Verbose() bool               { return r.Verbose_ }

func (r *Recorder) Step(title string) Step {
	r.add("step", title, "")
	return &recStep{r, title}
}

type recStep struct {
	r     *Recorder
	title string
}

func (s *recStep) Done(result string)          { s.r.add("done", s.title, result) }
func (s *recStep) Fail(err error)              { s.r.add("fail", s.title, err.Error()) }
func (s *recStep) Detail(f string, a ...any)   { s.r.add("detail", fmt.Sprintf(f, a...), s.title) }

// Index is the position of the first event of kind whose Text contains text,
// or -1.
func (r *Recorder) Index(kind, text string) int {
	for i, e := range r.Events {
		if e.Kind == kind && strings.Contains(e.Text, text) {
			return i
		}
	}
	return -1
}

// Has reports whether any event of kind has Text containing text.
func (r *Recorder) Has(kind, text string) bool { return r.Index(kind, text) >= 0 }

// Lines is every event as "kind: text", for a failure message.
func (r *Recorder) Lines() string {
	var b strings.Builder
	for _, e := range r.Events {
		fmt.Fprintf(&b, "%s: %s %s\n", e.Kind, e.Text, e.Extra)
	}
	return b.String()
}
```

- [ ] **Step 4: Run tests until they pass**

Run: `go test ./internal/ui/ -v`
Expected: PASS. If an exact-string test disagrees with the rendering by whitespace only, fix the implementation to match the test, not the reverse: the tests pin the format the rest of the plan relies on (`"  ok   title<pad>result"`, `"  FAIL "`, `"  WARN "`, details at 6 spaces, refusal explanation at 7, trace lines prefixed `| `).

- [ ] **Step 5: Run with the race detector**

Run: `go test -race ./internal/ui/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go vet ./internal/ui/
git add internal/ui
git commit  # feat: add internal/ui, the reporter every command prints through
```

---

### Task 2: `--verbose` flag and the reporter in every command

**Files:**
- Create: `cmd/paisans/flags.go`, `cmd/paisans/flags_test.go`
- Modify: every `run*` function in `cmd/paisans/*.go` that builds a `flag.FlagSet` (`main.go:289,319,420,482,746,819`, `site.go:24`, `doctor.go:44`, `oidc.go:40`, and the rest: find them with `grep -n "flag.NewFlagSet" cmd/paisans/*.go`)

**Interfaces:**
- Consumes: `ui.New`, `ui.NewPlain` (Task 1).
- Produces: `func commonFlags(fs *flag.FlagSet) func() ui.Reporter`. Call it right after `flag.NewFlagSet`; call the returned function after `fs.Parse` to get the reporter for stdout. Also `func errReporter(r ui.Reporter) ui.Reporter` returning a plain reporter on stderr with the same verbosity, for commands whose stdout carries data.

- [ ] **Step 1: Failing test** (`cmd/paisans/flags_test.go`)

```go
package main

import (
	"flag"
	"testing"
)

func TestVerboseFlagShortAndLong(t *testing.T) {
	for _, args := range [][]string{{"-v"}, {"--verbose"}} {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		reporter := commonFlags(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		if !reporter().Verbose() {
			t.Errorf("%v did not turn on verbose", args)
		}
	}
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	reporter := commonFlags(fs)
	_ = fs.Parse(nil)
	if reporter().Verbose() {
		t.Error("verbose by default")
	}
}

// Every command accepts the flag, so an operator never has to remember
// which do. Each run* registers commonFlags; this reads the source so a new
// command cannot forget it.
func TestEveryCommandAcceptsVerbose(t *testing.T) {
	assertEveryFlagSetHasCommonFlags(t)
}
```

and in the same file:

```go
func assertEveryFlagSetHasCommonFlags(t *testing.T) {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		sets := strings.Count(string(src), "flag.NewFlagSet(")
		common := strings.Count(string(src), "commonFlags(")
		if f == "flags.go" {
			continue
		}
		if common < sets {
			t.Errorf("%s: %d FlagSet(s) but %d commonFlags call(s)", f, sets, common)
		}
	}
}
```

(Add imports `os`, `path/filepath`, `strings`.)

- [ ] **Step 2: Run, verify FAIL** (`go test ./cmd/paisans/ -run 'Verbose'`: undefined `commonFlags`).

- [ ] **Step 3: Implement** `cmd/paisans/flags.go`:

```go
package main

import (
	"flag"
	"os"

	"github.com/paisans-software/paisans-stack/internal/ui"
)

// commonFlags registers the flags every command takes, and returns the
// reporter for the command's stdout once the flags are parsed. -v and
// --verbose show the detail behind each step: reasons, configuration values
// and raw command output. Without them a command prints one line per step.
func commonFlags(fs *flag.FlagSet) func() ui.Reporter {
	verbose := fs.Bool("verbose", false, "show the detail behind each step: reasons, values and command output")
	fs.BoolVar(verbose, "v", false, "short for --verbose")
	return func() ui.Reporter { return ui.New(os.Stdout, *verbose) }
}

// errReporter is r's verbosity on stderr, for a command whose stdout carries
// data that a script reads.
func errReporter(r ui.Reporter) ui.Reporter { return ui.New(os.Stderr, r.Verbose()) }
```

Then add `reporter := commonFlags(fs)` after every `flag.NewFlagSet` and `r := reporter()` after its `fs.Parse`. Where `r` is not yet used, write `_ = r` for now; later tasks use it. Do not change any output in this task.

- [ ] **Step 4: Run** `go test ./cmd/paisans/` → PASS.
- [ ] **Step 5: Commit** (`feat: accept -v and --verbose on every command`).

---

### Task 3: validate findings get a hint

**Files:**
- Modify: `internal/validate/validate.go:44-58,106-112` and every `c.warn(` / `c.refuse(` call in `internal/validate/*.go` (about 90: `grep -n "c\.\(warn\|refuse\)(" internal/validate/*.go`)
- Create: `cmd/paisans/findings.go`, `cmd/paisans/findings_test.go`
- Modify: `cmd/paisans/main.go:971-981` (delete `report`), and its 18 callers (`grep -n "report(os" cmd/paisans/*.go`)
- Test: `internal/validate/validate_test.go`

**Interfaces:**
- Consumes: `ui.Reporter`, `ui.Recorder` (Task 1); `r` from Task 2.
- Produces: `Finding{Level, Rule, Key, Hint, Message}`; `checker.warn(rule, key, hint, format string, args ...any)`, `checker.refuse(rule, key, hint, format string, args ...any)`; `func reportFindings(r ui.Reporter, path string, result validate.Result)`.

- [ ] **Step 1: Failing tests**

`internal/validate/validate_test.go`, add (imports `os`, `path/filepath`, `regexp`, `strings`):

```go
// A static check: every warn and refuse call passes a hint literal.
func TestEveryCallPassesAHint(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	// rule literal, key expression (which may hold one level of call
	// parentheses with commas, Eg: fmt.Sprintf("sites.%s", name)), then the
	// hint literal.
	re := regexp.MustCompile(`c\.(warn|refuse)\(\s*"[^"]+",\s*(?:[^,()]|\([^()]*\))+,\s*"([^"]*)"`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		calls := regexp.MustCompile(`c\.(warn|refuse)\(`).FindAllIndex(src, -1)
		hinted := re.FindAllSubmatch(src, -1)
		if len(calls) != len(hinted) {
			t.Errorf("%s: %d warn/refuse calls, %d with a hint literal", f, len(calls), len(hinted))
		}
		for _, m := range hinted {
			if h := string(m[2]); h == "" || len(h) > 80 {
				t.Errorf("%s: bad hint %q", f, h)
			}
		}
	}
}
```

`cmd/paisans/findings_test.go`:

```go
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
```

(`paisans validate` itself, `main.go:299`, still prints `paisans.yaml: no problems found` for a clean file: that command's whole output is the verdict. Do that in `runValidate`, not in `reportFindings`.)

- [ ] **Step 2: Run, verify FAIL.**

- [ ] **Step 3: Implement**

In `validate.go`:

```go
type Finding struct {
	Level Level
	// Rule is a stable identifier, quoted in tests and in verbose output.
	Rule string
	// Key is the configuration key the finding is about.
	Key string
	// Hint is one line saying what is wrong, in an operator's words. It is
	// what a warning shows by default.
	Hint string
	// Message says why it matters and what to do instead.
	Message string
}

func (c *checker) refuse(rule, key, hint, format string, args ...any) {
	c.findings = append(c.findings, Finding{Refuse, rule, key, hint, fmt.Sprintf(format, args...)})
}

func (c *checker) warn(rule, key, hint, format string, args ...any) {
	c.findings = append(c.findings, Finding{Warn, rule, key, hint, fmt.Sprintf(format, args...)})
}
```

Keep `String()` working (tests may use it): `fmt.Sprintf("%s  %s\n  %s: %s", f.Level, f.Rule, f.Key, f.Message)`.

Then add a hint to every call. Rules for writing hints:
- One line, at most 80 characters, lowercase start, no trailing period.
- Names the problem, not the mechanism: `garage consistency is dangerous` not `Garage confirms an upload once one copy exists`.
- No rationale words: no "because", "so that", "that is the trade", "deliberately".
- Where the message starts with `is <x>:` (the key is printed before it), the hint names the key's subject: for `storage.garage.consistency` / `is dangerous: ...` the hint is `garage consistency is dangerous`.
- While you are in each message, strip installer-voice offenders from the explanation too: drop "that is the trade", first-person asides, and repetition of the hint. The explanation stays complete.

Worked example (`validate.go:431`):

```go
c.warn("garage-consistency-dangerous", "storage.garage.consistency",
	"garage consistency is dangerous",
	"Garage confirms an upload once one copy exists and sends the rest in the background, so an upload can live on one disk until the other sites catch up, and a read can miss a recent change. Uploads continue while a site is down.")
```

`cmd/paisans/findings.go`:

```go
package main

import (
	"fmt"

	"github.com/paisans-software/paisans-stack/internal/ui"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// reportFindings shows a configuration's findings under its path. A
// refusal shows its hint and explanation, since the operator must act on
// it; a warning shows its hint, and its explanation with --verbose. Nothing
// is printed for a clean file: a command that goes on to do its work does
// not need to say that the file was fine.
func reportFindings(r ui.Reporter, path string, result validate.Result) {
	if len(result.Findings) == 0 {
		return
	}
	r.Section(path)
	for _, f := range result.Findings {
		detail := f.Message
		if r.Verbose() {
			detail = fmt.Sprintf("%s (%s, %s)", f.Message, f.Key, f.Rule)
		}
		if f.Level == validate.Refuse {
			r.Refuse(f.Hint, detail)
		} else {
			r.Warn(f.Hint, detail)
		}
	}
	r.Result("%s: %s", path, counts(len(result.Refusals()), len(result.Warnings())))
}

func counts(refusals, warnings int) string {
	var parts []string
	if refusals > 0 {
		parts = append(parts, plural(refusals, "refusal"))
	}
	if warnings > 0 {
		parts = append(parts, plural(warnings, "warning"))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
```

(add `strings` to the imports). Replace each `report(os.Stderr, path, result)` with `reportFindings(r, path, result)` (stdout, per the spec's stdout/stderr section) and delete `report` from `main.go`. `runValidate` prints `r.Result("%s: no problems found", path)` when there are no findings.

- [ ] **Step 4: Run** `go test ./internal/validate/ ./cmd/paisans/` → PASS. Fix any test that matched old message text by asserting on `Rule` instead.
- [ ] **Step 5: Commit** (`feat: give every validate finding a one-line hint`). Body says the explanation is unchanged in substance and moves behind --verbose for warnings.

---

### Task 4: apply's plan reports through `ui`

**Files:**
- Modify: `internal/apply/progress.go` (all), `internal/apply/apply.go:215-238` (`Progress`, `stepOpen`, `say`), `apply.go:375-392,495` (`Progress` option), every `p.step(`/`p.say(` call (`grep -n "\.step(\|\.say(" internal/apply/*.go`), `internal/apply/disk.go:41`, `volumes.go:50,228`, `WireGuardStep.Describe`
- Test: `internal/apply/progress_test.go`, `progress_note_test.go`

**Interfaces:**
- Consumes: `ui.Reporter`, `ui.Recorder`.
- Produces:
  - `Plan.Report ui.Reporter` (nil means `ui.Discard`), replacing `Plan.Progress io.Writer`; `stepOpen` is deleted.
  - `func Report(r ui.Reporter) Option`, replacing `Progress(w)`.
  - `func (p *Plan) step(title string) ui.Step` and `func (p *Plan) say(format string, args ...any)` (now a `Detail`).
  - `func (d *DiskCheck) Summary() string` (Eg: `13.6 GiB free`), `func (v *VolumeCheck) Summary() string` (Eg: `2 images checked`); `Describe()` stays as the verbose detail.
  - `func (w WireGuardStep) Title(deployment string) string` (Eg: `start mesh psns-566c`).
  - `func ActionTitle(a Action) string`: `recreate infra`, `restart talk (php, messenger)`, `replace talk` for a down, `recreate infra (forced)`.

Step titles (default output) replace today's verbs; today's text becomes the step's `Detail`:

| Today (`progress.go`, `apply.go`) | Title | Detail (verbose) |
|---|---|---|
| `reading N rendered files` | `read rendered files` | count |
| `checking images and free disk space` | `disk space` (Done with `Disk.Summary()`) | `Disk.Describe()`, `Volumes.Describe()` |
| `writing ...` | `write N files` | each path with its kind |
| mesh | `WireGuard.Title(dep)` | `WireGuard.Describe(dep)` |
| gateway pull / module / validate / reload | `pull gateway image`, `check gateway modules`, `validate gateway config`, `reload gateway` | today's text |
| `stackActionStep` | `ActionTitle(action)` | the compose command and `action.Reason` |
| `waiting X to be healthy` | `wait for X` | |
| prune | `prune old images` | each ref |
| database bootstrap | `bootstrap database X` | role, owner |

- [ ] **Step 1: Rewrite the tests to use the recorder**

In `progress_test.go`, replace `apply.Progress(progressLog{host})` with a recorder that also logs into the fake host's command list, so ordering against commands still holds:

```go
// hostRecorder records reporter events into the fake host's command log,
// so a test can check that a step was announced before its command ran.
type hostRecorder struct {
	ui.Recorder
	host *fakeHost
}

func (r *hostRecorder) Step(title string) ui.Step {
	r.host.log("step: " + title)
	return r.Recorder.Step(title)
}
```

and change the assertions: `host.indexOf("progress:   starting  " + action.Stack)` becomes `host.indexOf("step: recreate " + action.Stack)`; `"progress:   waiting   "` becomes `"step: wait for "`. (`fakeHost.log` is whatever method the existing `progressLog` writes through; reuse it.) Replace the `done (1.0s)` assertions with `rec.Has("done", "recreate infra")`.

Rewrite `progress_note_test.go` as:

```go
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
```

with, in `internal/apply/export_test.go`:

```go
func NewPlanForTest(r ui.Reporter) *Plan                     { return &Plan{Report: r} }
func StepForTest(p *Plan, title string) ui.Step             { return p.step(title) }
func SayForTest(p *Plan, format string, args ...any)        { p.say(format, args...) }
```

Add a test that `ActionTitle` covers each case:

```go
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
```

- [ ] **Step 2: Run, verify FAIL.**

- [ ] **Step 3: Implement** `progress.go`:

```go
// step starts one thing an apply does on the host. It is reported before
// the command runs, so a terminal shows what the host is doing now and an
// apply that stops part way shows the step it stopped in.
func (p *Plan) step(title string) ui.Step {
	return p.reporter().Step(title)
}

// say is a note about what an apply decided that is not an error (a pruned
// image, a replica leaving the bootstrap to its leader). It belongs to the
// open step when there is one, and shows with --verbose.
func (p *Plan) say(format string, args ...any) {
	p.reporter().Detail(strings.TrimRight(format, "\n"), args...)
}

func (p *Plan) reporter() ui.Reporter {
	if p.Report == nil {
		return ui.Discard
	}
	return p.Report
}
```

Each call site changes from `done := p.step("verb", "fmt", args...); ...; done(err)` to:

```go
s := p.step(ActionTitle(action))
s.Detail("%s", action.Reason)
err := runAction(...)
if err != nil {
	s.Fail(err)
	return err
}
s.Done("")
```

Delete `apply.Step(w, ...)`, `announce`, `ended`, `stackActionStep` (replaced by `ActionTitle`) and `stepOpen`. Keep `waitHealthyStep`, using `p.step("wait for " + stack)`.

- [ ] **Step 4: Run** `go test ./internal/apply/` → PASS. Then `go build ./...` shows every caller of `apply.Progress`/`plan.Progress`/`apply.Step` in `cmd/paisans` and the staged packages. In this task, make them compile with the minimal change: `apply.Progress(os.Stdout)` → `apply.Report(r)`, `plan.Progress = os.Stdout` → `plan.Report = r`, and `apply.Step(os.Stdout, "checking", "nothing on %s overlaps the mesh subnet", site)` → `s := r.Step("check mesh subnet")` with `s.Done("")` / `s.Fail(err)`. The staged packages' own `Progress` fields are Task 7's; leave them.
- [ ] **Step 5: Run** `go test ./...` → PASS. **Commit** (`feat: report apply's steps through the ui reporter`).

---

### Task 5: `apply` command: dry run lists, `--execute` reports progress only

**Files:**
- Create: `cmd/paisans/plan.go`, `cmd/paisans/plan_test.go`
- Modify: `cmd/paisans/main.go` (`runApply` 481-669; delete `printPlan` 894-969 and `printBootstrap` 1101-1111, moved to `plan.go`), `cmd/paisans/hostcheck.go`, `cmd/paisans/claim.go:39`, `cmd/paisans/clients.go` (`printClientPlan` 116-129, `ensure` 274, 376, `executeWithClients` 584-599), `cmd/paisans/leftover.go:20`
- Modify: `internal/hostcheck/classify.go:401-426`, `internal/oidcclient/oidcclient.go:137,192,231`
- Test: `cmd/paisans/apply_plan_test.go:133-136`, `hostcheck_test.go:103`, `clients_test.go:198`, `oidc_test.go:197`, `app_test.go:137`, `internal/hostcheck/classify_test.go:56,265`

**Interfaces:**
- Consumes: Tasks 1 to 4.
- Produces:
  - `func listPlan(r ui.Reporter, plan *apply.Plan)`: `r.Section(site + " (" + transport + ")")`, then one `r.Item` per step in the same order `printPlan` printed, each with its `Detail`s.
  - `func (r *Report) Show(rep ui.Reporter)` in hostcheck: called by `hostGate` inside its step (below).
  - `oidcclient.Step{Kind, Group, Title, Detail}`: `Title` short (`create OIDC client uptime2`, `allow group admins on uptime2`, `create client secret for uptime2`), `Detail` is today's `Line` (request and body).
  - `hostGate(r ui.Reporter, cfg, site, t)`; `gateSites(r ui.Reporter, ...)`; `claimHosts(r ui.Reporter, cfg, execute, hosts)`.

Behaviour (spec, *Plans and progress*):

```
dry run, default:                        --execute, default:
luthen-rael (ubuntu@192.0.2.10)          luthen-rael (ubuntu@192.0.2.10)
  ok   check mesh subnet                   ok   check mesh subnet
  ok   host check        clean             ok   host check        clean
  ok   read rendered files                 ok   claim site
  ok   disk space        13.6 GiB free     ok   OIDC client uptime2  created
  -    create OIDC client uptime2          ok   read rendered files
  -    write 8 files                       ok   disk space        13.6 GiB free
  -    start mesh psns-566c                ok   write 8 files
  -    recreate infra                      ok   start mesh psns-566c
  -    recreate uptime2                    ok   recreate infra    4.1s
  -    reload gateway                      ok   wait for infra
Nothing changed. Re-run with              ...
--execute to apply.                      Applied 8 files to luthen-rael.
```

With `--verbose` and `--execute`, `listPlan` runs before executing, as today's `printPlan` did.

- [ ] **Step 1: Failing tests** (`cmd/paisans/plan_test.go`)

```go
package main

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// The dry-run plan is one item per step, in the order Execute takes them;
// the reasons are details.
func TestListPlanItemsInOrderWithReasonsAsDetail(t *testing.T) {
	plan := &apply.Plan{
		Site: "home-a", Transport: "ubuntu@192.0.2.10",
		Changes: []apply.Change{{Path: "/srv/paisans/f2a9/infra/compose.yaml", Kind: apply.Create}, {Path: "/srv/paisans/f2a9/talk/.env", Kind: apply.Update}},
		Actions: []apply.Action{{Stack: "infra", Recreate: true, Reason: "an environment or compose file changed"}},
		GatewayReload: true,
	}
	rec := &ui.Recorder{Verbose_: true}
	listPlan(rec, plan)
	order := []int{rec.Index("item", "write 2 files"), rec.Index("item", "recreate infra"), rec.Index("item", "reload gateway")}
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i] < order[i-1] {
			t.Fatalf("items missing or out of order:\n%s", rec.Lines())
		}
	}
	if !rec.Has("detail", "an environment or compose file changed") {
		t.Errorf("reason not a detail:\n%s", rec.Lines())
	}
	if !rec.Has("detail", "/srv/paisans/f2a9/talk/.env") {
		t.Errorf("file paths not details:\n%s", rec.Lines())
	}
}
```

In `apply_plan_test.go:133-136`, replace the `printPlan` order assertions (`"mesh "`, `"recreate  infra"`, `"check     infra:"`, `"bootstrap database talk"`) with `listPlan` + recorder `Index` checks for items `start mesh`, `recreate infra`, `bootstrap database talk` in that order.

Add the Review Focus test (5) to `cmd/paisans/apply_plan_test.go`, using whatever fake transport `apply_plan_test.go` already builds an apply with, and making the infra `up -d` fail:

```go
func TestExecuteFailureShowsFailedStepAndFullError(t *testing.T) {
	rec := &ui.Recorder{}
	err := executeForTest(t, rec, failOn("infra/compose.yaml up -d", "Error response from daemon: port is already allocated"))
	if err == nil || !strings.Contains(err.Error(), "port is already allocated") {
		t.Fatalf("error lost its detail: %v", err)
	}
	last := -1
	for i, e := range rec.Events {
		if e.Kind == "done" || e.Kind == "fail" {
			last = i
		}
	}
	if last < 0 || rec.Events[last].Kind != "fail" || rec.Events[last].Text != "recreate infra" {
		t.Fatalf("last ended step is not the failed one:\n%s", rec.Lines())
	}
}
```

`executeForTest` and `failOn` are thin helpers over the fixture apply already used in `apply_plan_test.go`: build the plan with `apply.Report(rec)` and call `apply.Execute`. Write them in that file next to the existing helpers.

In `hostcheck_test.go:103`, assert `rec.Has("done", "host check")` with Extra `clean` instead of the `"  checking  home-a's host"` string. In `classify_test.go` `wantLine`, switch to `Report.Show(rec)` and assert `rec.Has("detail", "route ...")` for notes.

In `clients_test.go:198`, `oidc_test.go:197`, `app_test.go:137`: assert `rec.Has("item", "create OIDC client talk")` and `rec.Has("detail", "POST /api/oidc/clients")` (with `Verbose_: true`), and `rec.Has("result", "Nothing changed")`.

- [ ] **Step 2: Run, verify FAIL.**

- [ ] **Step 3: Implement**

`cmd/paisans/plan.go`: move `printPlan`/`printBootstrap` here as `listPlan(r, plan)`/`listBootstrap(r, b)`. Each `fmt.Fprintf(os.Stdout, "  %-9s ...")` becomes an `r.Item(title)` followed by `r.Detail(...)` with the old text. Mapping:

| `printPlan` line | Item | Detail |
|---|---|---|
| `note X` | none | `r.Detail(note)` |
| `check Disk.Describe()` | none (the disk step already ran in Build) | `Disk.Describe()` |
| `check Volumes.Describe()` | none | `Volumes.Describe()` |
| `refuse u.Describe()` | `r.Refuse("volume path not mounted: "+u.Path, u.Describe())` | |
| each create/update/overwrite | one `write N files` (N = changed) | `"<kind> <path>"` each |
| `unchanged N file(s)` | none | `"unchanged N files"` |
| `mesh` | `WireGuard.Title(dep)` | `WireGuard.Describe(dep)` |
| `down X, so that ...` | none (folded into `ActionTitle`) | the old text |
| `restart/recreate X` + reason | `ActionTitle(action)` | `action.Reason` |
| `check X: every container running...` | `wait for X` | old text |
| `prune ref, superseded ...` | `prune old images` (once per stack that has prunes) | each ref |
| `ensure HostSitesDir` | `ensure host sites directory` | old text |
| `check ACME module` | `check gateway modules` | old text |
| `reload` / `validate` | `reload gateway` / `validate gateway config` | old text |
| conflicts count | `r.Refuse(fmt.Sprintf("%d files were edited on the host", n), "Nothing will be applied until that is resolved.")` | |

`runApply`:
- `reportFindings(r, ...)` (Task 3).
- The mesh check, host gate and claim are steps (`check mesh subnet`, `host check`, `claim site`).
- If `!*execute`: `listPlan(r, plan)`; `r.Result("Nothing changed. Re-run with --execute to apply.")`; return.
- If `*execute`: `if r.Verbose() { listPlan(r, plan) }`; execute with `plan.Report = r`; `r.Result("Applied %s to %s.", plural(written, "file"), site)`.

`hostGate`:

```go
func hostGate(r ui.Reporter, cfg *config.Config, site string, t hostcheck.Transport) (*hostcheck.Report, error) {
	s := r.Step("host check")
	report, err := hostcheck.Run(cfg, site, t)
	if err != nil {
		s.Fail(err)
		return nil, err
	}
	report.Show(s)
	if err := report.Refusal(); err != nil {
		s.Fail(err)
		return nil, err
	}
	s.Done(string(report.Class))
	return report, nil
}
```

`hostcheck.Report.Show(s ui.Step)` attaches what `Print` printed as `s.Detail` lines (docker, firewall, foreign, note, shared). It is `Show(d detailer)` with `type detailer interface{ Detail(string, ...any) }`, so it takes a step. Conflicts are part of `Refusal()`'s error, which prints in full.

`claimHosts(r, ...)`: per site, `s := r.Step("claim site")`; `s.Detail("claimed for deployment %s in %s", cfg.ID, registry.Path)`; with more than one site, the title is `"claim " + site`.

OIDC (`clients.go`):
- `printClientPlan` becomes `listClientPlan(r, app, idp, where, plan)`: `r.Detail("%s's client at %s on %s (pocket-id)", ...)`; `present` lines become details; each step `r.Item(step.Title)` + `r.Detail(step.Detail)`; each warning `r.Warn(w, "")`.
- `ensure` executing: one step per mutation, titled `step.Title`, `Done("created")`/`Done("")`; `recorded oidc_clients...` becomes a detail of the secret step.
- Drop the `"\nOIDC clients for X, before they start"` header (`r.Detail` it).
- `executeWithClients`: delete the second `printPlan` (`clients.go:599`) and the `"the site, rendered with the clients in place"` header; the final pass is built with `apply.Report(r)` only for Execute, and its Build gets `apply.Report(ui.Discard)` when the first pass already reported the reads. `"starting X's Pocket ID ... before the apps"` becomes a step `start pocket-id` (its own action steps follow).

`oidcclient.go:192,231`: build `Title` and `Detail` instead of `Line`. Titles: `create OIDC client <app>`, `allow group <g> on <app>`, `create client secret for <app>`, `record client id for <app>`, `set launch URL for <app>`. `Detail` keeps the exact text that `Line` had.

`leftover.go:20` `printLeftovers(r, ...)`: each leftover is `r.Warn(<one-line hint>, <today's text>)`.

- [ ] **Step 4: Run** `go test ./...` → PASS.
- [ ] **Step 5: Smoke the rendering by hand** with the fixture config in `examples/` (no host needed: a dry run against an unreachable host fails at the host check, which still exercises sections, findings and the step failure line):

```bash
go run ./cmd/paisans apply --config examples/paisans.yaml --site home-a --ssh nobody@192.0.2.1 2>&1 | head -30
go run ./cmd/paisans apply --config examples/paisans.yaml --site home-a --ssh nobody@192.0.2.1 -v 2>&1 | head -60
```

Expected: default output has no line longer than 100 characters and no rule IDs; verbose shows the rule IDs and the ssh error output. Check `ls examples/` for the actual fixture file name first.

- [ ] **Step 6: Commit** (`feat: apply lists its plan on a dry run and reports progress on --execute`).

---

### Task 6: other host commands (`host prepare`, `storage init`, `prune`, `doctor`, `preflight`, `failover`, `init`, `dns`, `ingress`, `app admin create`, `oidc client create`)

**Files:**
- Modify: `internal/hostprep/hostprep.go:284` (`Print` → `Show`), `internal/doctor/doctor.go:106`, `internal/preflight/preflight.go:57`, `internal/failover/failover.go:39` (`Out io.Writer` → `Report ui.Reporter`), `internal/ingress/*.go` (32 prints), `internal/garage/*.go` (11), `internal/dns/*.go` (8)
- Modify: `cmd/paisans/main.go` (`runInit` 334-416, `runHostPrepare` 819-880, `runStorageInit` 746-, `printGaragePlan` 884), `prune.go`, `doctor.go`, `preflight.go`, `oidc.go:118`, `app.go`, `mesh.go`
- Test: `internal/hostprep/hostprep_test.go` (21 asserts), `internal/doctor/doctor_test.go`, `cmd/paisans/init_test.go`, `cmd/paisans/main_test.go`, `internal/failover/failover_test.go`

**Interfaces:**
- Consumes: Tasks 1 to 5 (`hostGate(r, ...)`, `claimHosts(r, ...)`, `listClientPlan`).
- Produces: `hostprep.(*Plan).Show(r ui.Reporter)`, `doctor.Report.Show(r ui.Reporter)`, `preflight.Report.Show(r ui.Reporter)`, `failover` `Report ui.Reporter` field; each package's executor reports steps through the reporter it is given.

Conversion rules (the same for every file in this task):
1. A line announcing work that can fail is a `Step`, ended with `Done`/`Fail`.
2. A line that only describes what a dry run would do is an `Item`.
3. A line that explains, quotes a value, a path, a command or a body is a `Detail` of the step or item it follows.
4. Raw output from a remote command shown today (Eg: ufw status, docker ps) is a `Trace`.
5. A problem that does not stop the command is `Warn(hint, today's text)`; one that does is `Refuse(hint, explanation)` or the returned error.
6. Each command ends with one `Result`: `Nothing changed. Re-run with --execute to apply.` for a dry run, a one-line outcome otherwise (Eg: `Host prepared.`, `3 checks passed, 1 warning.`).
7. Titles are an imperative verb and its object, at most about 40 characters. Use the wording table in Task 4 as the model.

`doctor` specifically: each check is a `Step` whose `Done` result is the short finding (`ok`, `2 of 3 healthy`); its `More` lines are `Detail`s; a failing check is `Fail` plus `Warn` or `Refuse` per its level today.

`init` (`main.go:334-416`): `  + name` becomes `r.Step("create " + name)` / `Done`; `  ? name\n      why` becomes `r.Warn(name+" needs a decision", why)`.

- [ ] **Step 1: Update the tests first.** For each test file listed, replace `strings.Contains(out, "<old text>")` with recorder assertions: the same fact, asserted as `rec.Has("<kind>", "<new title or detail>")`. Keep each test's intent; when an assertion was about wording a person reads (a refusal), assert on the hint and the explanation. Run them: FAIL.
- [ ] **Step 2: Convert the packages, then the commands,** applying the rules.
- [ ] **Step 3: Run** `go test ./...` → PASS.
- [ ] **Step 4: Commit** (`feat: report host commands through the ui reporter`). If the diff is too large to review as one commit, commit per package: `hostprep`, `doctor`+`preflight`, `failover`, `ingress`+`dns`, `garage`+`storage init`, `init`.

---

### Task 7: staged commands (`site add`, `site remove`, `storage add`, `rotate-key`, `app remove`)

**Files:**
- Modify: `internal/siteadd/siteadd.go:58-60,444-455,478` (+ `patroni.go:144`), `internal/siteremove/siteremove.go:81,478`, `internal/storageadd/storageadd.go:95-97,170,419`, `internal/rotatekey/rotatekey.go:104,501`, `internal/appremove/print.go:13`, `internal/appremove/execute.go:77`
- Modify: `cmd/paisans/site.go:104,110`, `siteremove.go:130`, `storage.go:96`, `rotatekey.go:111,171`, `appremove.go:202`
- Test: `internal/siteadd/siteadd_test.go:319` and the other tests in those packages that read `Progress` output

**Interfaces:**
- Consumes: Tasks 1, 4, 5.
- Produces: in each package, `Plan.Report ui.Reporter` replacing `Progress io.Writer`; `(*Plan).Show(r ui.Reporter)` replacing `Print(w)`; `appremove.Executor.Report` replacing `Progress`.

Shape (spec, *Plans and progress*, staged commands):

```
stage 1, move data off home-b
  ok   hand over Patroni leader     2.3s
  ok   gate: home-a is leader       passed
stage 2, ...
```

- `p.say("stage %d, %s\n", ...)` → `r.Section(fmt.Sprintf("stage %d, %s", st.Number, st.Name))`.
- Each of a stage's steps → `Step(title)`; today's step line → `Detail`.
- The gate → `s := r.Step("gate: " + shortGate)`; `s.Detail(st.Gate)`; `s.Done("passed")` or `s.Fail(err)`.
- `Print(w)` (dry run) → `Show(r)`: `Section` per stage, `Item` per step, the gate as an `Item` `"gate: ..."` with its long text as a `Detail`.

- [ ] **Step 1: Update tests to recorder assertions** (Eg: `siteadd_test.go:319` `"check     home-a: auth, docs, talk: every container running"` becomes `rec.Has("item", "wait for home-a")` plus `rec.Has("detail", "auth, docs, talk")` with `Verbose_: true`). Run: FAIL.
- [ ] **Step 2: Convert each package and its command.**
- [ ] **Step 3: Run** `go test ./...` → PASS.
- [ ] **Step 4: Commit** per package or together (`feat: report staged commands through the ui reporter`).

---

### Task 8: wording sweep and the long-line guard

**Files:**
- Modify: any remaining `fmt.Fprint*` to stdout/stderr in `cmd/paisans/*.go` and `internal/*` (`grep -rn "fmt.Fprint\|fmt.Print" --include='*.go' cmd internal | grep -v _test`)
- Create: `cmd/paisans/output_test.go`

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Write the guard test** (`cmd/paisans/output_test.go`):

```go
package main

import (
	"strings"
	"testing"
)

// Default output is for reading at a glance: plain when piped, and no line
// long enough to be an explanation. Run over the fixture dry runs that the
// command tests already drive.
func TestDefaultOutputIsShortAndPlain(t *testing.T) {
	for name, out := range fixtureRuns(t) {
		if strings.ContainsAny(out, "\x1b\r") {
			t.Errorf("%s: escapes in piped output", name)
		}
		for _, line := range strings.Split(out, "\n") {
			if len(line) > 100 && !strings.HasPrefix(strings.TrimSpace(line), "FAIL") && !strings.HasPrefix(line, "       ") {
				t.Errorf("%s: long line in default output: %q", name, line)
			}
		}
	}
}
```

`fixtureRuns(t) map[string]string` runs, with stdout captured into a `strings.Builder` via `ui.NewPlain(&b, false)`, the dry runs that `main_test.go`, `init_test.go`, `app_test.go`, `oidc_test.go` and `doctor_test.go` already drive against their fakes. Refactor those tests' setup into helpers if needed so they can be called from here. Refusal explanations (indented 7 spaces) and failure lines are exempt: they are allowed to be long.

- [ ] **Step 2: Run, fix every long line it finds** by moving text into a `Detail`.
- [ ] **Step 3: Grep for leftover direct prints** with the grep command above. Every remaining hit must be either data output (a command whose stdout is data) or an error printed by `main` before exit. Convert the rest.
- [ ] **Step 4: Grep for rationale phrasing in default-mode strings:** `grep -rn "that is the trade\|deliberately\|on purpose" --include='*.go' cmd internal | grep -v _test | grep -v "^\s*//"`. Each hit inside a `Step`/`Item`/`Warn` hint/`Result` string is moved or reworded.
- [ ] **Step 5: Run** `go vet ./... && go test ./...` → PASS. **Commit** (`chore: move the last explanatory output behind --verbose`).

---

### Task 9: documentation

**Files:**
- Modify: `docs/development.md:299-310` (*Every step on the host is announced as it starts*)
- Modify: `README.md:3988-3998` (OIDC dry-run sample), `README.md:3691` (`app admin create` sample), the `README.md` sudo section around line 2434 if its surrounding text describes output, and `README.md:2491` (*The host check*) where it describes what is printed
- Modify: the usage text in `cmd/paisans/main.go:35-197`: add a line documenting `-v, --verbose` once, in the general section

- [ ] **Step 1: Rewrite `docs/development.md`'s section** as *Every command reports through one reporter*. Say what the two levels are and why (the spec's *Why* and *The two levels*), that a step is reported before its command runs, that a note during a step is its detail, that errors are never a detail, and that tests assert on `ui.Recorder` events. No em dashes.
- [ ] **Step 2: Replace each README sample** with real output: run the corresponding dry run against the test fakes (or reconstruct it from the recorder tests) in default mode, and paste it. Where the README explained the old line-by-line output, cut that explanation to what the new output shows, and say that `--verbose` shows the rest.
- [ ] **Step 3: Verify** every sample matches the renderer's actual format (`  ok   `, `  -    `, `  WARN `, `  FAIL `), and `grep -c "—"` over the lines you changed is 0.
- [ ] **Step 4: Commit** (`docs: describe installer-style output and --verbose`).

---

## Finishing

- [ ] `gofmt -l .` prints nothing; `go vet ./...`, `go test ./...` and `go test -race ./internal/ui/` pass.
- [ ] Push the branch, open one pull request into `develop` titled `feat: installer-style output with --verbose for the detail`, body summarising the spec and listing the commits, ending with the Claude Code footer.

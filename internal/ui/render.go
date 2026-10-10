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
	dim    = "\x1b[2m"
	reset  = "\x1b[0m"
	clear  = "\r\x1b[K"
)

type writer struct {
	mu sync.Mutex
	// width is the title column Align set, 0 for titleWidth.
	width int
	// cols is how wide prose is wrapped: the terminal's width when it is
	// known, never more than maxWidth.
	cols     int
	w        io.Writer
	verbose  bool
	terminal bool
	now      func() time.Time
	tick     time.Duration

	open   *step
	frame_ int
	// held counts the holds in place (see Hold). While any is, nothing is
	// drawn, so a prompt on the terminal stays readable.
	held int
}

func newWriter(w io.Writer, verbose, terminal bool) *writer {
	return &writer{w: w, cols: maxWidth, verbose: verbose, terminal: terminal, now: time.Now, tick: 100 * time.Millisecond}
}

type step struct {
	r       *writer
	title   string
	started time.Time
	details []string
	ended   bool
	// stop closes when this step's spinner must stop. It lives on the step,
	// not the writer, so ending one step cannot stop another step's spinner.
	stop chan struct{}
}

func (r *writer) Verbose() bool { return r.verbose }

func (r *writer) Section(title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interruptLocked()
	fmt.Fprintf(r.w, "%s\n", title)
}

func (r *writer) Step(title string) Step {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A step that opens while another is still open has no end to wait for
	// from the caller's side, so the earlier spinner stops here. Two spinners
	// drawing one line would interleave; the earlier step still prints its
	// own line when it ends.
	if prev := r.open; prev != nil && prev.stop != nil {
		close(prev.stop)
		prev.stop = nil
	}
	s := &step{r: r, title: title, started: r.now()}
	r.open = s
	if r.terminal {
		r.frame_ = 0
		r.drawLocked()
		if r.tick > 0 {
			s.stop = make(chan struct{})
			go r.spin(s, s.stop)
		}
	}
	return s
}

func (r *writer) spin(s *step, stop chan struct{}) {
	t := time.NewTicker(r.tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.spinFrame(s, stop)
		}
	}
}

// spinFrame draws a tick only while its own step is still the open one. A
// tick can be waiting on the mutex when its step ends, and it must not draw
// over whatever step came next.
func (r *writer) spinFrame(s *step, stop chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.open != s || s.stop != stop || !r.terminal {
		return
	}
	r.frame_++
	r.drawLocked()
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

// drawLocked draws the open step's spinner line, unless a hold is in place.
func (r *writer) drawLocked() {
	s := r.open
	if s == nil || r.held > 0 {
		return
	}
	elapsed := r.now().Sub(s.started)
	fmt.Fprintf(r.w, "%s  %s %s", clear, frames[r.frame_%len(frames)], r.pad(s.title))
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

func (s *step) Done(result string) { s.End(OK, s.timed(result)) }

func (s *step) Fail(err error) { s.End(Failed, s.timed("")) }

// timed is result, or for none the elapsed time when the step took a second
// or more. Only a step that did work is timed: End is also a dry run's
// status line, where the time it took to check says nothing.
func (s *step) timed(result string) string {
	if result != "" {
		return result
	}
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if d := s.r.now().Sub(s.started); d >= time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return ""
}

func (s *step) End(m Mark, result string) {
	r := s.r
	r.mu.Lock()
	if s.ended {
		r.mu.Unlock()
		return
	}
	s.ended = true
	stop := s.stop
	s.stop = nil
	if r.open == s {
		r.open = nil
	}
	text := strings.TrimRight(r.pad(s.title)+result, " ")
	line := "  " + r.mark(m) + " " + text
	if r.terminal && m == Waiting {
		// A step that waits is dimmed whole: it is not this run's to plan.
		line = "  " + dim + "· " + text + reset
	}
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

func (r *writer) mark(m Mark) string {
	if r.terminal {
		switch m {
		case OK:
			return green + "✓" + reset
		case Pending:
			return yellow + "○" + reset
		case Waiting:
			return dim + "·" + reset
		default:
			return red + "✗" + reset
		}
	}
	switch m {
	case OK:
		return "ok  "
	case Pending:
		return "todo"
	case Waiting:
		return "wait"
	default:
		return "FAIL"
	}
}

func (r *writer) Item(title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interruptLocked()
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
		r.detailLocked(strings.Join(Wrap(detail, r.cols-len(detailIndent)), "\n"))
	}
}

func (r *writer) Note(hint, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interruptLocked()
	if r.terminal {
		fmt.Fprintf(r.w, "  %s!%s %s\n", yellow, reset, hint)
	} else {
		fmt.Fprintf(r.w, "  WARN %s\n", hint)
	}
	r.explainLocked(detail)
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
	r.explainLocked(explanation)
}

// explainIndent aligns the lines that belong to a hint under the hint's text
// rather than under its mark; detailIndent sets a verbose line under what it
// belongs to.
const (
	explainIndent = "       "
	detailIndent  = "      "
)

// explainLocked prints the lines that belong to a hint at every verbosity,
// wrapped to the width.
func (r *writer) explainLocked(text string) {
	for _, line := range Wrap(text, r.cols-len(explainIndent)) {
		fmt.Fprintf(r.w, "%s%s\n", explainIndent, line)
	}
}

// Hold clears the spinner line and draws nothing until every hold is
// resumed, then draws the open step again. Plain output draws nothing between
// lines, so there it does nothing. Each resume releases its own hold once,
// however often it is called, so a deferred resume beside an explicit one
// cannot release a hold that belongs to someone else.
func (r *writer) Hold() (resume func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.terminal {
		return func() {}
	}
	r.held++
	r.interruptLocked()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.held--
			r.drawLocked()
		})
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
		fmt.Fprintf(r.w, "%s%s\n", detailIndent, line)
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

func (r *writer) pad(title string) string {
	width := titleWidth
	if r.width > width {
		width = r.width
	}
	if len(title) >= width {
		return title + " "
	}
	return title + strings.Repeat(" ", width-len(title))
}

func (r *writer) Align(titles ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.width = 0
	for _, t := range titles {
		if len(t)+1 > r.width {
			r.width = len(t) + 1
		}
	}
}

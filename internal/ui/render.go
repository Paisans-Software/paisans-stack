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

	open   *step
	frame_ int
	// held counts the holds in place (see Hold). While any is, nothing is
	// drawn, so a prompt on the terminal stays readable.
	held int
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
	stop := s.stop
	s.stop = nil
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
		r.detailLocked(detail)
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

// explainLocked prints the lines that belong to a hint at every verbosity,
// aligned under the hint's text rather than under its mark.
func (r *writer) explainLocked(text string) {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line != "" {
			fmt.Fprintf(r.w, "       %s\n", line)
		}
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

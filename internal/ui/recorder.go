package ui

import (
	"fmt"
	"strings"
	"sync"
)

// Event is one thing a Recorder was told.
type Event struct {
	Kind  string // section step done fail pending waiting item warn note refuse detail trace result
	Text  string // the title, hint or text
	Extra string // a step's result, a warning's or note's detail, a refusal's explanation, a fail's error
}

// Recorder is a Reporter for tests: it keeps what was said, in order, so a
// test asserts on steps and outcomes rather than on spacing.
type Recorder struct {
	mu       sync.Mutex
	Verbose_ bool
	Events   []Event
}

func (r *Recorder) add(kind, text, extra string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Events = append(r.Events, Event{kind, text, extra})
}

func (r *Recorder) Section(title string)       { r.add("section", title, "") }
func (r *Recorder) Item(title string)          { r.add("item", title, "") }
func (r *Recorder) Warn(hint, detail string)   { r.add("warn", hint, detail) }
func (r *Recorder) Note(hint, detail string)   { r.add("note", hint, detail) }
func (r *Recorder) Explain(text string)        { r.add("explain", text, "") }
func (r *Recorder) Refuse(hint, expl string)   { r.add("refuse", hint, expl) }
func (r *Recorder) Detail(f string, a ...any)  { r.add("detail", fmt.Sprintf(f, a...), "") }
func (r *Recorder) Trace(label, output string) { r.add("trace", label, output) }
func (r *Recorder) Result(f string, a ...any)  { r.add("result", fmt.Sprintf(f, a...), "") }
func (r *Recorder) Verbose() bool              { return r.Verbose_ }

func (r *Recorder) Step(title string) Step {
	r.add("step", title, "")
	return &recStep{r, title}
}

type recStep struct {
	r     *Recorder
	title string
}

func (s *recStep) Done(result string) { s.r.add("done", s.title, result) }
func (s *recStep) Fail(err error)     { s.r.add("fail", s.title, err.Error()) }
func (s *recStep) End(m Mark, result string) {
	s.r.add(map[Mark]string{OK: "done", Failed: "fail", Pending: "pending", Waiting: "waiting"}[m], s.title, result)
}
func (s *recStep) Detail(f string, a ...any) { s.r.add("detail", fmt.Sprintf(f, a...), s.title) }

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

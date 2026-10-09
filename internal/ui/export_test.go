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

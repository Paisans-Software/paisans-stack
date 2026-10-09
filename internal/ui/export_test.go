package ui

import (
	"io"
	"time"
)

// NewForTest builds a reporter with terminal rendering forced on or off and
// a clock the test controls, so spinner frames and elapsed times are fixed.
func NewForTest(w io.Writer, verbose, terminal bool, clock func() time.Time) Reporter {
	return NewForTestTick(w, verbose, terminal, clock, 0)
}

// NewForTestTick is NewForTest with a real spinner tick. A zero tick starts
// no goroutine; a small one exercises the spinner's concurrency under -race.
func NewForTestTick(w io.Writer, verbose, terminal bool, clock func() time.Time, tick time.Duration) Reporter {
	r := newWriter(w, verbose, terminal)
	r.now = clock
	r.tick = tick
	return r
}

// Frame draws one spinner frame for the open step, as the goroutine would.
func Frame(r Reporter) { r.(*writer).frame() }

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

// PrintErrorForTest is PrintError with the rendering and the terminal's width
// chosen by the test.
func PrintErrorForTest(w io.Writer, err error, verbose, terminal bool, width int) {
	printError(w, err, verbose, terminal, width)
}

// ShortPathFrom is ShortPath from a given directory and home.
var ShortPathFrom = shortPath

// NewForTestWidth is NewForTest on a terminal of cols columns, as New would
// find it; 0 is a width that is not known.
func NewForTestWidth(w io.Writer, terminal bool, cols int, clock func() time.Time) Reporter {
	r := NewForTest(w, false, terminal, clock).(*writer)
	r.termCols = cols
	if cols > 0 && cols < maxWidth {
		r.cols = cols
	}
	return r
}

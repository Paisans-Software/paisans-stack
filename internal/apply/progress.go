package apply

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// step announces one thing an apply is about to do on the host, and returns
// the function that reports how it ended. A pull, a health wait or a database
// bootstrap takes minutes on a real site, and an apply that spoke only when
// it had finished looked hung for all of them.
//
// The announcement is written before the command runs, so the line on the
// operator's terminal names what the host is doing now, and an apply that
// stops part way shows the step it stopped in. The ending goes on the same
// line when nothing was said in between, and on a line of its own when a note
// was (Eg: a pruned image), so neither is lost.
//
// A plan without a Progress writer says nothing, as it always has.
func (p *Plan) step(verb, format string, args ...any) func(error) {
	if p.Progress == nil {
		return func(error) {}
	}
	announce(p.Progress, verb, format, args...)
	p.stepOpen = true
	started := now()
	return func(err error) {
		if !p.stepOpen {
			fmt.Fprintf(p.Progress, "  %-9s ", "")
		}
		ended(p.Progress, err, started)
		p.stepOpen = false
	}
}

// Step is step for a command's own work outside a plan, such as the host
// check before one is built. Nothing may write to w between the call and the
// ending, since the ending finishes the same line.
func Step(w io.Writer, verb, format string, args ...any) func(error) {
	announce(w, verb, format, args...)
	started := now()
	return func(err error) { ended(w, err, started) }
}

func announce(w io.Writer, verb, format string, args ...any) {
	fmt.Fprintf(w, "  %-9s %s ... ", verb, fmt.Sprintf(format, args...))
}

func ended(w io.Writer, err error, started time.Time) {
	outcome := "done"
	if err != nil {
		outcome = "failed"
	}
	fmt.Fprintf(w, "%s (%.1fs)\n", outcome, now().Sub(started).Seconds())
}

// stackActionStep describes what runAction is about to do to a stack, in the
// words of the command it runs.
func stackActionStep(action Action) (verb, what string) {
	switch {
	case action.Down:
		return "replacing", action.Stack + " (down, then up -d)"
	case action.Recreate && action.Force:
		return "starting", action.Stack + " (up -d --force-recreate)"
	case action.Recreate:
		return "starting", action.Stack + " (up -d)"
	case len(action.Services) > 0:
		return "restart", action.Stack + " (" + strings.Join(action.Services, ", ") + ")"
	default:
		return "restart", action.Stack
	}
}

// waitHealthyStep is waitHealthy announced, since the health gate is the
// longest wait in most applies.
func waitHealthyStep(plan *Plan, stack string, t Transport) error {
	done := plan.step("waiting", "%s to be healthy", stack)
	err := waitHealthy(plan, stack, t)
	done(err)
	return err
}

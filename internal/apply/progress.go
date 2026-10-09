package apply

import (
	"strings"

	"github.com/paisans-software/paisans-stack/internal/ui"
)

// step starts one thing an apply does on the host. It is reported before the
// command runs, so a terminal shows what the host is doing now and an apply
// that stops part way shows the step it stopped in. A pull, a health wait or
// a database bootstrap takes minutes on a real site, and an apply that spoke
// only when it had finished looked hung for all of them.
func (p *Plan) step(title string) ui.Step {
	return p.reporter().Step(title)
}

// say is a note about what an apply decided that is not an error (a pruned
// image, a replica leaving the bootstrap to its leader). It belongs to the
// open step when there is one, and shows with --verbose.
func (p *Plan) say(format string, args ...any) {
	p.reporter().Detail(strings.TrimRight(format, "\n"), args...)
}

// warn is a note the operator should see at any verbosity: something the
// apply could not do that costs disk or time, never the apply itself.
func (p *Plan) warn(hint, detail string) {
	p.reporter().Warn(hint, detail)
}

// reporter is the plan's Report, or ui.Discard for a plan built without one,
// so a plan that was given no reporter says nothing rather than failing.
func (p *Plan) reporter() ui.Reporter {
	if p.Report == nil {
		return ui.Discard
	}
	return p.Report
}

// finish ends a step by how its work ended. It only marks the step: the
// caller still returns the error, so it is printed in full at any verbosity.
func finish(s ui.Step, err error) {
	if err != nil {
		s.Fail(err)
		return
	}
	s.Done("")
}

// ActionTitle names what runAction does to a stack, as the title of its step.
// The compose command and the reason are the step's detail.
func ActionTitle(a Action) string {
	switch {
	case a.Down:
		return "replace " + a.Stack
	case a.Recreate && a.Force:
		return "recreate " + a.Stack + " (forced)"
	case a.Recreate:
		return "recreate " + a.Stack
	case len(a.Services) > 0:
		return "restart " + a.Stack + " (" + strings.Join(a.Services, ", ") + ")"
	default:
		return "restart " + a.Stack
	}
}

// waitHealthyStep is waitHealthy reported, since the health gate is the
// longest wait in most applies.
func waitHealthyStep(plan *Plan, stack string, t Transport) error {
	s := plan.step("wait for " + stack)
	err := waitHealthy(plan, stack, t)
	finish(s, err)
	return err
}

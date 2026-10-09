package apply_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// hostRecorder records reporter events into the fake host's command log,
// so a test can check that a step was announced before its command ran.
type hostRecorder struct {
	ui.Recorder
	host *fakeHost
}

func (r *hostRecorder) Step(title string) ui.Step {
	r.host.commands = append(r.host.commands, "step: "+title)
	return r.Recorder.Step(title)
}

// An apply that only spoke at the end looked hung for the minutes a pull or a
// health wait takes. Each step is announced as it starts, before the command
// that carries it out, and finishes with how long it took.
func TestEachStackStepIsAnnouncedBeforeItRuns(t *testing.T) {
	host := newHost()
	rec := &hostRecorder{host: host}
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Report(rec))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}

	if len(p.Actions) == 0 {
		t.Fatal("a first apply planned no actions")
	}
	for _, action := range p.Actions {
		up := host.indexOf("/srv/paisans/f2a9/" + action.Stack + "/compose.yaml up -d")
		if up < 0 {
			t.Fatalf("%s was never started", action.Stack)
		}
		starting := host.indexOf("step: " + apply.ActionTitle(action))
		if starting < 0 || starting > up {
			t.Errorf("starting %s was not announced before it ran (announced at %d, ran at %d)", action.Stack, starting, up)
		}
		waiting := host.indexOf("step: wait for " + action.Stack)
		if waiting < up {
			t.Errorf("the health wait for %s was not announced after it started (announced at %d, started at %d)", action.Stack, waiting, up)
		}
	}
	if !rec.Has("done", "recreate infra") {
		t.Errorf("no step reported its end:\n%s", rec.Lines())
	}
}

// The gateway's gates are the slowest part of an apply that touches no app:
// a pull and two throwaway containers. Each is its own step.
func TestTheGatewayGatesAreAnnounced(t *testing.T) {
	host := newHost()
	rec := &hostRecorder{host: host}
	host.running = false
	p, err := apply.Build("vm", plan(t), acmeModule(t), host, apply.Report(rec))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct{ said, ran string }{
		{"step: write ", "pull caddy"},
		{"step: pull gateway image", "pull caddy"},
		{"step: check gateway modules", "list-modules"},
		{"step: validate gateway config", "caddy validate"},
	} {
		said, ran := host.indexOf(step.said), host.indexOf(step.ran)
		if said < 0 || said > ran {
			t.Errorf("%q was not said before %q ran (said at %d, ran at %d):\n%s", step.said, step.ran, said, ran, rec.Lines())
		}
	}
}

// Planning reads every rendered file from the host, one round trip each, and
// on a real site that is the first long silence. It is announced too.
func TestPlanningAnnouncesWhatItReads(t *testing.T) {
	host := newHost()
	rec := &hostRecorder{host: host}
	if _, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Report(rec)); err != nil {
		t.Fatal(err)
	}
	if !rec.Has("step", "read rendered files") {
		t.Errorf("planning said nothing about reading the host:\n%s", rec.Lines())
	}
}

// A plan built without a reporter says nothing, as before, and an
// Execute of it does not fail for want of one.
func TestNoReporterIsSilent(t *testing.T) {
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}

}

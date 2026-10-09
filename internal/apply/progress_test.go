package apply_test

import (
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// progressLog interleaves what an apply says with what it runs, in one
// record, so a test can ask whether a step was announced before the command
// it names. An operator watching the terminal sees exactly that order.
type progressLog struct{ host *fakeHost }

func (l progressLog) Write(b []byte) (int, error) {
	l.host.commands = append(l.host.commands, "progress: "+string(b))
	return len(b), nil
}

// fixedClock makes every step take the same second, so the timing a step
// reports is predictable.
func fixedClock(t *testing.T) {
	t.Helper()
	at := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	t.Cleanup(apply.SetNow(func() time.Time {
		at = at.Add(time.Second)
		return at
	}))
}

// An apply that only spoke at the end looked hung for the minutes a pull or a
// health wait takes. Each step is announced as it starts, before the command
// that carries it out, and finishes with how long it took.
func TestEachStackStepIsAnnouncedBeforeItRuns(t *testing.T) {
	fixedClock(t)
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Progress(progressLog{host}))
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
		starting := host.indexOf("progress:   starting  " + action.Stack)
		if starting < 0 || starting > up {
			t.Errorf("starting %s was not announced before it ran (announced at %d, ran at %d)", action.Stack, starting, up)
		}
		waiting := host.indexOf("progress:   waiting   " + action.Stack)
		if waiting < up {
			t.Errorf("the health wait for %s was not announced after it started (announced at %d, started at %d)", action.Stack, waiting, up)
		}
	}
	if !host.ran("progress: done (1.0s)\n") {
		t.Errorf("no step reported how long it took:\n%s", progressOf(host))
	}
}

// The gateway's gates are the slowest part of an apply that touches no app:
// a pull and two throwaway containers. Each is its own step.
func TestTheGatewayGatesAreAnnounced(t *testing.T) {
	fixedClock(t)
	host := newHost()
	host.running = false
	p, err := apply.Build("vm", plan(t), acmeModule(t), host, apply.Progress(progressLog{host}))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct{ said, ran string }{
		{"progress:   writing   ", "pull caddy"},
		{"progress:   pulling   the gateway's Caddy image", "pull caddy"},
		{"progress:   checking  the gateway's Caddy", "list-modules"},
		{"progress:   checking  the gateway configuration", "caddy validate"},
	} {
		said, ran := host.indexOf(step.said), host.indexOf(step.ran)
		if said < 0 || said > ran {
			t.Errorf("%q was not said before %q ran (said at %d, ran at %d):\n%s", step.said, step.ran, said, ran, progressOf(host))
		}
	}
}

// Planning reads every rendered file from the host, one round trip each, and
// on a real site that is the first long silence. It is announced too.
func TestPlanningAnnouncesWhatItReads(t *testing.T) {
	fixedClock(t)
	host := newHost()
	if _, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Progress(progressLog{host})); err != nil {
		t.Fatal(err)
	}
	if !host.ran("progress:   reading   ") {
		t.Errorf("planning said nothing about reading the host:\n%s", progressOf(host))
	}
}

// A plan built without a progress writer says nothing, as before, and an
// Execute of it does not fail for want of one.
func TestNoProgressWriterIsSilent(t *testing.T) {
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if host.ran("progress:") {
		t.Error("a plan without a progress writer reported progress")
	}
}

func progressOf(h *fakeHost) string {
	var said []string
	for _, command := range h.commands {
		if line, ok := strings.CutPrefix(command, "progress: "); ok {
			said = append(said, line)
		}
	}
	return strings.Join(said, "")
}

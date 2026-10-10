package ui_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/ui"
)

// Run's work happens while its step is open, so the spinner shows for as
// long as the work takes, and the step ends by the work's error.
func TestRunOpensAStepAroundTheWork(t *testing.T) {
	r := &ui.Recorder{}
	if err := ui.Run(r, "read home-b", func() error {
		r.Detail("working")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := r.Lines()
	want := "step: read home-b \ndetail: working \ndone: read home-b \n"
	if got != want {
		t.Fatalf("got\n%swant\n%s", got, want)
	}
}

func TestRunFailsTheStepAndReturnsTheError(t *testing.T) {
	r := &ui.Recorder{}
	boom := errors.New("ssh: connection refused")
	if err := ui.Run(r, "read home-b", func() error { return boom }); err != boom {
		t.Fatalf("got %v, want the work's own error", err)
	}
	if !r.Has("fail", "read home-b") || r.Has("done", "read home-b") {
		t.Fatalf("the step did not fail:\n%s", r.Lines())
	}
}

func TestGetReturnsTheWorksValue(t *testing.T) {
	r := &ui.Recorder{}
	v, err := ui.Get(r, "read plan", func() (int, error) { return 3, nil })
	if err != nil || v != 3 {
		t.Fatalf("got %d, %v", v, err)
	}
	if !r.Has("done", "read plan") {
		t.Fatalf("no finished step:\n%s", r.Lines())
	}
}

// On a terminal the spinner is drawn while the work runs.
func TestRunDrawsTheSpinnerWhileWorking(t *testing.T) {
	var b strings.Builder
	r := ui.NewForTest(&b, false, true, clock())
	_ = ui.Run(r, "read home-b", func() error {
		ui.Frame(r)
		if !strings.Contains(b.String(), "read home-b") {
			t.Errorf("no spinner while working: %q", b.String())
		}
		return nil
	})
	if !strings.Contains(b.String(), "✓") {
		t.Errorf("no finished line: %q", b.String())
	}
}

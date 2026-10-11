package appadmin

import (
	"errors"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// stubCreator is a kind whose probes come from a queue, so the shared
// planning and the check after Execute can be tested apart from any one kind.
type stubCreator struct {
	probes []State
	ran    int
}

func (s *stubCreator) Probe(Transport, Request) (State, error) {
	if len(s.probes) == 0 {
		return State{}, errors.New("unexpected probe")
	}
	st := s.probes[0]
	s.probes = s.probes[1:]
	return st, nil
}

func (s *stubCreator) Steps(state State, _ Request) []Action {
	if state.Admin {
		return nil
	}
	return []Action{ActionGrantAdmin}
}

func (s *stubCreator) Execute(Transport, Request, []Action) (Outcome, error) {
	s.ran++
	return Outcome{}, nil
}

type nullTransport struct{}

func (nullTransport) Describe() string                        { return "vm" }
func (nullTransport) Run(string) (string, error)              { return "", errors.New("unused") }
func (nullTransport) RunInput(string, string) (string, error) { return "", errors.New("unused") }

func TestPresentAdminPlansNothingAndExecutesNothing(t *testing.T) {
	c := &stubCreator{probes: []State{{Exists: true, Verified: true, Admin: true}}}
	state, _ := c.Probe(nil, Request{})
	plan := &Plan{State: state, Actions: c.Steps(state, Request{}), creator: c, req: Request{Username: "founder"}}
	if got := plan.Lines(); len(got) != 1 || got[0] != "founder" {
		t.Errorf("lines %v", got)
	}
	if _, err := Execute(plan, nullTransport{}); err != nil {
		t.Fatal(err)
	}
	if c.ran != 0 {
		t.Error("a present admin ran the creator")
	}
}

// A command that exits zero without doing its job is what the second probe
// exists to catch.
func TestExecuteRefusesWhenTheResultIsNotAnAdmin(t *testing.T) {
	c := &stubCreator{probes: []State{{Exists: true, Verified: true}}}
	plan := &Plan{Actions: []Action{ActionGrantAdmin}, creator: c, req: Request{Username: "founder"}}
	if _, err := Execute(plan, nullTransport{}); err == nil || !strings.Contains(err.Error(), "admin=false") {
		t.Errorf("got %v, want a refusal naming the missing admin role", err)
	}
}

func TestUnsupportedKind(t *testing.T) {
	_, err := Build(config.KindOutline, nullTransport{}, Request{Username: "founder"})
	if err == nil || err.Error() != "outline admin creation is not implemented yet. Implemented kinds: pocket-id" {
		t.Errorf("got %v", err)
	}
}

func TestMbinHasNoCreator(t *testing.T) {
	if _, err := For(config.KindMbin); err == nil {
		t.Error("mbin has a Creator; its administrators come only through single sign on")
	}
}

func TestRedactRemovesEachSecretInBothShapes(t *testing.T) {
	err := redact(errors.New("echo key-not-real-0001 a2V5LW5vdC1yZWFsLTAwMDE="), "key-not-real-0001")
	if strings.Contains(err.Error(), "key-not-real") || strings.Contains(err.Error(), "a2V5LW5vdC1yZWFsLTAwMDE=") {
		t.Errorf("redact left the secret: %s", err)
	}
}

package appadmin

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

const password = "not-a-real-password-0001"

type call struct{ command, stdin string }

// fakeTransport answers probes from a queue of states and records every call.
type fakeTransport struct {
	calls  []call
	probes []State
	// failExec makes a mutating run fail with output that echoes the
	// password and the script, the worst case for a leak.
	failExec bool
}

func (f *fakeTransport) Describe() string { return "vm" }

func (f *fakeTransport) Run(command string) (string, error) {
	f.calls = append(f.calls, call{command: command})
	return "", errors.New("Run is not used by admin creation")
}

func (f *fakeTransport) RunInput(command, stdin string) (string, error) {
	f.calls = append(f.calls, call{command: command, stdin: stdin})
	if strings.Contains(stdin, "findOneByUsername") {
		if len(f.probes) == 0 {
			return "", errors.New("unexpected probe")
		}
		s := f.probes[0]
		f.probes = f.probes[1:]
		return "PHP Deprecated: noise\n" + mbinProbeMarker + jsonState(s) + "\n", nil
	}
	if f.failExec {
		return "boom " + password + " " + stdin, errors.New("exit status 1: boom " + password + "\n" + stdin)
	}
	return "[OK] done\n", nil
}

func jsonState(s State) string {
	b := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	return `{"exists":` + b(s.Exists) + `,"verified":` + b(s.Verified) + `,"admin":` + b(s.Admin) + `}`
}

func request() Request {
	return Request{App: "talk", Username: "founder", Email: "founder@example.org", Password: password}
}

func TestStepsFromProbe(t *testing.T) {
	cases := []struct {
		name  string
		state State
		reset bool
		want  []Action
	}{
		{"absent", State{}, false, []Action{ActionCreate, ActionVerify, ActionGrantAdmin}},
		{"absent with reset creates, does not reset", State{}, true, []Action{ActionCreate, ActionVerify, ActionGrantAdmin}},
		{"existing member", State{Exists: true, Verified: true}, false, []Action{ActionGrantAdmin}},
		{"unverified member", State{Exists: true}, false, []Action{ActionVerify, ActionGrantAdmin}},
		{"existing admin", State{Exists: true, Verified: true, Admin: true}, false, nil},
		{"existing admin, reset asked", State{Exists: true, Verified: true, Admin: true}, true, []Action{ActionResetPassword}},
	}
	for _, c := range cases {
		req := request()
		req.ResetPassword = c.reset
		if got := Steps(c.state, req); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// A dry run is Build alone: one probe, which carries no password and runs no
// Mbin command that changes anything.
func TestBuildOnlyProbes(t *testing.T) {
	f := &fakeTransport{probes: []State{{Exists: true, Verified: true}}}
	plan, err := Build(config.KindMbin, f, request())
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Lines(); !reflect.DeepEqual(got, []string{"grant admin founder"}) {
		t.Errorf("lines %v", got)
	}
	if len(f.calls) != 1 {
		t.Fatalf("%d calls, want one probe", len(f.calls))
	}
	stdin := f.calls[0].stdin
	if strings.Contains(stdin, "mbin:user:") || strings.Contains(stdin, "flush") {
		t.Errorf("the probe runs a mutating command:\n%s", stdin)
	}
	assertNoPassword(t, f, nil)
}

func TestPresentAdminPlansNothingAndExecutesNothing(t *testing.T) {
	f := &fakeTransport{probes: []State{{Exists: true, Verified: true, Admin: true}}}
	plan, err := Build(config.KindMbin, f, request())
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Lines(); !reflect.DeepEqual(got, []string{"present founder"}) {
		t.Errorf("lines %v", got)
	}
	if _, err := Execute(plan, f); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Errorf("%d calls, want only the probe", len(f.calls))
	}
}

// The password goes in on stdin and nowhere else, and the create runs the
// fork's commands in order.
func TestExecuteCreateKeepsThePasswordOnStdin(t *testing.T) {
	f := &fakeTransport{probes: []State{{}, {Exists: true, Verified: true, Admin: true}}}
	plan, err := Build(config.KindMbin, f, request())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(plan, f); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("%d calls, want probe, run, probe", len(f.calls))
	}
	run := decodePayload(t, f.calls[1].stdin)
	for _, want := range []string{`"command":"mbin:user:create"`, `"command":"mbin:user:verify"`, `"command":"mbin:user:admin"`, password} {
		if !strings.Contains(run, want) {
			t.Errorf("the run's payload lacks %s:\n%s", want, run)
		}
	}
	if strings.Index(run, "mbin:user:create") > strings.Index(run, "mbin:user:admin") {
		t.Errorf("admin is granted before the user is created:\n%s", run)
	}
	assertNoPassword(t, f, nil)
}

func TestExecuteNeverResetsAnExistingPasswordUnasked(t *testing.T) {
	f := &fakeTransport{probes: []State{{Exists: true, Verified: true}, {Exists: true, Verified: true, Admin: true}}}
	plan, err := Build(config.KindMbin, f, request())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(plan, f); err != nil {
		t.Fatal(err)
	}
	run := decodePayload(t, f.calls[1].stdin)
	if strings.Contains(run, password) || strings.Contains(run, "mbin:user:password") {
		t.Errorf("granting admin carried the password or reset it:\n%s", run)
	}
}

func TestExecuteFailureRedactsThePassword(t *testing.T) {
	f := &fakeTransport{probes: []State{{}}, failExec: true}
	plan, err := Build(config.KindMbin, f, request())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Execute(plan, f)
	if err == nil {
		t.Fatal("a failed run reported success")
	}
	assertNoPassword(t, f, err)
}

func TestExecuteRefusesWhenTheResultIsNotAnAdmin(t *testing.T) {
	f := &fakeTransport{probes: []State{{Exists: true, Verified: true}, {Exists: true, Verified: true}}}
	plan, err := Build(config.KindMbin, f, request())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(plan, f); err == nil || !strings.Contains(err.Error(), "admin=false") {
		t.Errorf("got %v, want a refusal naming the missing admin role", err)
	}
}

func TestUnsupportedKind(t *testing.T) {
	_, err := Build(config.KindOutline, &fakeTransport{}, request())
	if err == nil || err.Error() != "outline admin creation is not implemented yet. Implemented kinds: mbin, pocket-id" {
		t.Errorf("got %v", err)
	}
}

func TestParseProbeNeedsTheMarker(t *testing.T) {
	if _, err := parseMbinProbe(`{"exists":true}`); err == nil {
		t.Error("output without the marker was accepted")
	}
}

// assertNoPassword checks every command line, and err if given, for the
// password in plain or base64 form.
func assertNoPassword(t *testing.T, f *fakeTransport, err error) {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString([]byte(password))
	for _, c := range f.calls {
		if strings.Contains(c.command, password) || strings.Contains(c.command, encoded) {
			t.Errorf("a command line carries the password: %s", c.command)
		}
		if strings.Contains(c.command, "base64") || strings.Contains(c.command, "<?php") {
			t.Errorf("a command line carries the script: %s", c.command)
		}
	}
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, password) || strings.Contains(msg, encoded) {
		t.Errorf("the error carries the password: %s", msg)
	}
	for _, c := range f.calls {
		if p := payloadOf(c.stdin); p != "" && strings.Contains(msg, p) {
			t.Errorf("the error carries the encoded payload: %s", msg)
		}
	}
}

func payloadOf(stdin string) string {
	_, rest, ok := strings.Cut(stdin, "base64_decode('")
	if !ok {
		return ""
	}
	p, _, _ := strings.Cut(rest, "'")
	return p
}

func decodePayload(t *testing.T, stdin string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(payloadOf(stdin))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

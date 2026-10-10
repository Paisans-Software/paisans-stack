package ui_test

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/ui"
)

var errSentinel = errors.New("the host never answered")

// A Problem is an error like any other to the code that handles it: a
// sentinel under it is still found, and so is the Problem under a wrapper.
func TestProblemUnwrapsToItsCause(t *testing.T) {
	cause := fmt.Errorf("reading x: %w", errSentinel)
	err := fmt.Errorf("apply: %w", &ui.Problem{Hint: "x cannot be read", Explain: "Check x.", Cause: cause})
	if !errors.Is(err, errSentinel) {
		t.Error("errors.Is does not see the sentinel through the Problem")
	}
	var p *ui.Problem
	if !errors.As(err, &p) || p.Hint != "x cannot be read" {
		t.Errorf("errors.As did not find the Problem: %v", p)
	}
}

// Error() loses nothing: a caller that logs or embeds the error has the hint,
// the cause and the explanation.
func TestProblemErrorCarriesEverything(t *testing.T) {
	err := &ui.Problem{Hint: "x cannot be read", Explain: "Check x.", Cause: fs.ErrNotExist}
	got := err.Error()
	for _, want := range []string{"x cannot be read", fs.ErrNotExist.Error(), "Check x."} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() lacks %q: %q", want, got)
		}
	}
	if (&ui.Problem{Hint: "h"}).Error() != "h" {
		t.Errorf("a bare hint is not its own Error(): %q", (&ui.Problem{Hint: "h"}).Error())
	}
}

func TestWrapBreaksAtSpacesOnly(t *testing.T) {
	text := "Point SOPS_AGE_KEY_FILE at /home/someone/.config/sops/age/keys.txt and run `paisans apply --site home-a --execute` again."
	lines := ui.Wrap(text, 30)
	for _, l := range lines {
		if len(l) > 30 && strings.Contains(l, " ") {
			t.Errorf("a line longer than the width holds a space it could have broken at: %q", l)
		}
	}
	if strings.Join(lines, " ") != text {
		t.Errorf("wrapping changed the text:\n%q\n%q", strings.Join(lines, " "), text)
	}
	// The path is longer than the width: it gets a line of its own, whole.
	found := false
	for _, l := range lines {
		if l == "/home/someone/.config/sops/age/keys.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("the long path is not whole on its own line: %q", lines)
	}
}

func TestWrapKeepsIndentAndListItems(t *testing.T) {
	text := "  - ports 51820/udp: claimed by mesh.listen_port, held by another deployment named here"
	lines := ui.Wrap(text, 40)
	if len(lines) < 2 {
		t.Fatalf("not wrapped: %q", lines)
	}
	if !strings.HasPrefix(lines[0], "  - ports") {
		t.Errorf("first line lost its indent: %q", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "     ") {
			t.Errorf("a continuation is not aligned under the item's text: %q", l)
		}
	}
}

func TestWrapKeepsShortLinesAndBlankText(t *testing.T) {
	if got := ui.Wrap("short", 80); len(got) != 1 || got[0] != "short" {
		t.Errorf("got %q", got)
	}
	if got := ui.Wrap("", 80); len(got) != 0 {
		t.Errorf("blank text gave lines: %q", got)
	}
}

const secretsCause = "decrypting /Users/someone/deploy/staging/secrets.enc.yaml: Error getting data key: 0 successful groups required, got 0"

func secretsProblem() error {
	return fmt.Errorf("apply: %w", &ui.Problem{
		Hint:    "staging/secrets.enc.yaml cannot be decrypted: no usable age key was found",
		Explain: "Set SOPS_AGE_KEY_CMD to a command that prints your key, Eg: a Keychain lookup, or SOPS_AGE_KEY_FILE to the key's path. Your public key must be a recipient in .sops.yaml; if it was added recently, run sops updatekeys on the file.",
		Cause:   errors.New(secretsCause),
	})
}

func TestErrorPlain(t *testing.T) {
	var b strings.Builder
	ui.PrintErrorForTest(&b, secretsProblem(), false, false, 80)
	want := `FAIL staging/secrets.enc.yaml cannot be decrypted: no usable age key was found
  Set SOPS_AGE_KEY_CMD to a command that prints your key, Eg: a Keychain lookup,
  or SOPS_AGE_KEY_FILE to the key's path. Your public key must be a recipient in
  .sops.yaml; if it was added recently, run sops updatekeys on the file.
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestErrorTerminal(t *testing.T) {
	var b strings.Builder
	ui.PrintErrorForTest(&b, secretsProblem(), false, true, 80)
	if !strings.HasPrefix(b.String(), "\x1b[31m✗\x1b[0m staging/secrets.enc.yaml cannot be decrypted") {
		t.Errorf("the hint is not under a red ✗: %q", b.String())
	}
	if strings.Contains(b.String(), "successful groups") || strings.Contains(b.String(), "/Users/") {
		t.Errorf("the cause shows without -v: %q", b.String())
	}
}

func TestErrorVerboseShowsTheCauseChain(t *testing.T) {
	var b strings.Builder
	ui.PrintErrorForTest(&b, secretsProblem(), true, false, 80)
	out := b.String()
	for _, want := range []string{
		"FAIL staging/secrets.enc.yaml cannot be decrypted",
		"\n  Set SOPS_AGE_KEY_CMD",
		"\n    apply\n",
		"\n    " + secretsCause + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Index(out, "on the file.") > strings.Index(out, "    apply") {
		t.Errorf("the cause comes before the explanation:\n%s", out)
	}
}

// Only the narrower of the terminal and 80 columns is used.
func TestErrorWrapsAtTheNarrowerWidth(t *testing.T) {
	var b strings.Builder
	ui.PrintErrorForTest(&b, secretsProblem(), false, false, 40)
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n")[1:] {
		if len(l) > 40 {
			t.Errorf("a line is wider than the terminal: %q", l)
		}
	}
	b.Reset()
	ui.PrintErrorForTest(&b, secretsProblem(), false, false, 200)
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n")[1:] {
		if len(l) > 80 {
			t.Errorf("a line is wider than 80 on a wide terminal: %q", l)
		}
	}
}

// An error that is not a Problem prints in full, in the same form: its first
// line is the hint and the rest is the explanation.
func TestErrorFallbackSplitsOnTheFirstLine(t *testing.T) {
	var b strings.Builder
	err := errors.New("render: --out is required\nArtifacts are written to a local directory and pushed by a later step")
	ui.PrintErrorForTest(&b, err, false, false, 80)
	want := "FAIL render: --out is required\n  Artifacts are written to a local directory and pushed by a later step\n"
	if b.String() != want {
		t.Errorf("got %q\nwant %q", b.String(), want)
	}
}

// A first line too long to be a hint is cut at its first sentence.
func TestErrorFallbackCutsALongFirstLineAtASentence(t *testing.T) {
	var b strings.Builder
	err := errors.New("site remove: --delete-data deletes member data, which nothing brings back. It asks for the site's name at a terminal and stdin is not one. Run it from an interactive shell")
	ui.PrintErrorForTest(&b, err, false, false, 80)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if lines[0] != "FAIL site remove: --delete-data deletes member data, which nothing brings back" {
		t.Errorf("hint: %q", lines[0])
	}
	rest := strings.Join(lines[1:], " ")
	if !strings.Contains(rest, "It asks for the site's name") || !strings.Contains(rest, "interactive shell") {
		t.Errorf("the rest is not the explanation: %q", rest)
	}
	for _, l := range lines {
		if len(l) > 80 {
			t.Errorf("long line: %q", l)
		}
	}
}

// A long first line with no sentence break stays the hint, whole.
func TestErrorFallbackKeepsALongLineWithoutASentence(t *testing.T) {
	var b strings.Builder
	long := "a: " + strings.Repeat("x", 100)
	ui.PrintErrorForTest(&b, errors.New(long), false, false, 80)
	if b.String() != "FAIL "+long+"\n" {
		t.Errorf("got %q", b.String())
	}
}

func TestShortPath(t *testing.T) {
	for _, c := range []struct{ path, want string }{
		{"/work/deploy/staging/secrets.enc.yaml", "staging/secrets.enc.yaml"},
		{"/work/deploy", "."},
		{"/home/someone/keys/age.txt", "~/keys/age.txt"},
		{"/etc/paisans.yaml", "/etc/paisans.yaml"},
		{"staging/paisans.yaml", "staging/paisans.yaml"},
		{"/work/deployed/x", "/work/deployed/x"},
	} {
		if got := ui.ShortPathFrom(c.path, "/work/deploy", "/home/someone"); got != c.want {
			t.Errorf("ShortPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// A caller that wraps a Problem with text on both sides (ssh's output after
// it) keeps both under -v.
func TestErrorVerboseKeepsTextAroundAProblem(t *testing.T) {
	p := &ui.Problem{Hint: "u@h did not accept your ssh key", Explain: "Check the key.", Cause: errors.New("exit status 255")}
	err := fmt.Errorf("%s: %s: %w\n%s", "u@h", "docker ps", p, "u@h: Permission denied (publickey).")
	var b strings.Builder
	ui.PrintErrorForTest(&b, err, true, false, 80)
	for _, want := range []string{"\n    u@h: docker ps\n", "\n    u@h: Permission denied (publickey).\n", "\n    exit status 255\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
}

// fresh unwraps to a new Problem on every call, as a typed error with its
// own wording may.
type fresh struct{}

func (fresh) Error() string { return "fresh" }
func (fresh) Unwrap() error {
	return &ui.Problem{Hint: "h", Explain: "e", Cause: fmt.Errorf("ctx-inner: %w", errors.New("leaf"))}
}

// The chain stops at the Problem however it is reached, so its cause is
// shown once.
func TestErrorVerboseShowsACauseOnce(t *testing.T) {
	var b strings.Builder
	ui.PrintErrorForTest(&b, fmt.Errorf("outer: %w", fresh{}), true, false, 80)
	if n := strings.Count(b.String(), "ctx-inner"); n != 1 {
		t.Errorf("the cause shows %d times:\n%s", n, b.String())
	}
	if !strings.Contains(b.String(), "\n    outer\n") || !strings.Contains(b.String(), "\n    leaf\n") {
		t.Errorf("chain:\n%s", b.String())
	}
}

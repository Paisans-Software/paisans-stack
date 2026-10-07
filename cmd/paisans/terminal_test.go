package main

import (
	"os"
	"strings"
	"testing"
)

// TestReadPasswordAcceptsDevNull is the real host finding: `< /dev/null` is a
// character device, and the old check refused it as a terminal. It must get
// the ordinary "nothing on stdin" answer instead.
func TestReadPasswordAcceptsDevNull(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	_, err = readPassword(devNull)
	if err == nil || strings.Contains(err.Error(), "terminal") || !strings.Contains(err.Error(), "nothing on stdin") {
		t.Errorf("got %v, want the empty stdin refusal and not the terminal one", err)
	}
	if err := refusePipedPassword(devNull, "pocket-id"); err != nil {
		t.Errorf("/dev/null for a passwordless kind: %v", err)
	}
}

func TestReadPasswordAcceptsAPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.WriteString("pw\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if got, err := readPassword(r); err != nil || got != "pw" {
		t.Errorf("got %q, %v from a pipe", got, err)
	}
}

func TestReadPasswordRefusesATerminal(t *testing.T) {
	saved := isTerminal
	isTerminal = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminal = saved })
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	if _, err := readPassword(devNull); err == nil || !strings.Contains(err.Error(), "stdin is a terminal") {
		t.Errorf("got %v, want the terminal refusal", err)
	}
	// A passwordless kind leaves a terminal unread rather than waiting on it.
	if err := refusePipedPassword(devNull, "pocket-id"); err != nil {
		t.Errorf("terminal for a passwordless kind: %v", err)
	}
}

func TestStdinIsTerminalIgnoresAReader(t *testing.T) {
	if stdinIsTerminal(strings.NewReader("x")) {
		t.Error("a strings.Reader was taken for a terminal")
	}
}

package main

import (
	"os"
	"strings"
	"testing"
)

// TestRefuseStdinAcceptsDevNull is the real host finding: `< /dev/null` is a
// character device, and an older check refused it as a terminal. It is how an
// unattended run says there is nothing to read, and must be accepted.
func TestRefuseStdinAcceptsDevNull(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	if err := refuseStdin(devNull, "pocket-id"); err != nil {
		t.Errorf("/dev/null: %v", err)
	}
}

func TestRefuseStdinRefusesAPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.WriteString("pw\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := refuseStdin(r, "pocket-id"); err == nil {
		t.Error("something piped in was accepted")
	}
}

// A terminal is left unread rather than waited on.
func TestRefuseStdinLeavesATerminalUnread(t *testing.T) {
	saved := isTerminal
	isTerminal = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminal = saved })
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	if err := refuseStdin(devNull, "pocket-id"); err != nil {
		t.Errorf("terminal: %v", err)
	}
}

func TestStdinIsTerminalIgnoresAReader(t *testing.T) {
	if stdinIsTerminal(strings.NewReader("x")) {
		t.Error("a strings.Reader was taken for a terminal")
	}
}

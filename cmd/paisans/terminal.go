package main

import (
	"io"
	"os"

	"golang.org/x/term"
)

// isTerminal reports whether a file is a terminal. Tests replace it, since a
// test has no terminal to hand.
var isTerminal = func(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// stdinIsTerminal reports whether stdin is an interactive terminal, the one
// input a credential must not be typed into.
//
// It asks the terminal driver (an ioctl) rather than testing for a character
// device. Every terminal is a character device but not every character device
// is a terminal: /dev/null is one, and `< /dev/null` is how an unattended run
// says there is nothing to read. The character device test refused it as a
// terminal on a real host. A pipe and a regular file are neither, and both
// are accepted either way.
func stdinIsTerminal(stdin io.Reader) bool {
	file, ok := stdin.(*os.File)
	return ok && isTerminal(file)
}

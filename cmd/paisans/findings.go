package main

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/ui"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// reportFindings shows a configuration's findings under its path. A
// refusal shows its hint and explanation, since the operator must act on
// it; a warning shows its hint, and its explanation with --verbose. Nothing
// is printed for a clean file: a command that goes on to do its work does
// not need to say that the file was fine.
func reportFindings(r ui.Reporter, path string, result validate.Result) {
	if len(result.Findings) == 0 {
		return
	}
	r.Section(path)
	for _, f := range result.Findings {
		// The key goes first because many explanations are written to follow
		// it ("is %q, which ..."), and it says which site or app the
		// finding is about.
		detail := fmt.Sprintf("%s: %s", f.Key, f.Message)
		if r.Verbose() {
			detail += fmt.Sprintf(" (%s)", f.Rule)
		}
		if f.Level == validate.Refuse {
			r.Refuse(f.Hint, detail)
		} else {
			r.Warn(f.Hint, detail)
		}
	}
	r.Result("%s: %s", path, counts(len(result.Refusals()), len(result.Warnings())))
}

// refused is the error that ends a command whose configuration validate
// refused. The refusals are above it, each with its own explanation, so it
// names the file and the count, and then says what the refusal stopped.
func refused(path string, n int, consequence string) error {
	explain := "Fix each refusal above, then run the command again."
	if consequence != "" {
		explain += " " + consequence
	}
	return &ui.Problem{
		Hint:    fmt.Sprintf("%s was refused: %s above", ui.ShortPath(path), plural(n, "refusal")),
		Explain: explain,
		Cause:   fmt.Errorf("%s: %d refusal(s)", path, n),
	}
}

func counts(refusals, warnings int) string {
	var parts []string
	if refusals > 0 {
		parts = append(parts, plural(refusals, "refusal"))
	}
	if warnings > 0 {
		parts = append(parts, plural(warnings, "warning"))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

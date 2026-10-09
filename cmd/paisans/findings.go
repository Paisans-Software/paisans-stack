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

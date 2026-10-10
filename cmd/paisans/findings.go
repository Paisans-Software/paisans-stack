package main

import (
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/ui"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// reportFindings shows a configuration's findings under its path, for a
// command that goes on to do its work. A refusal shows its hint and
// explanation, since the operator must act on it; a warning shows its hint,
// and its explanation with --verbose. A count line closes them only when
// there is a refusal, since the command stops on it: after warnings alone it
// would name the file again and say nothing the lines above do not. Nothing
// is printed for a clean file: a command that goes on to do its work does
// not need to say that the file was fine.
func reportFindings(r ui.Reporter, path string, result validate.Result) {
	reportFindingsUnder(r, findingsPath(r, path), path, result)
}

// reportFindingsUnder is reportFindings under section, a heading of the
// command's own, rather than the path.
func reportFindingsUnder(r ui.Reporter, section, path string, result validate.Result) {
	if len(result.Findings) == 0 {
		return
	}
	r.Section(section)
	showFindings(r, result)
	if result.Refused() {
		r.Result("%s: %s", findingsPath(r, path), counts(len(result.Refusals()), len(result.Warnings())))
	}
}

// reportValidation is reportFindings for paisans validate, whose subject is
// the file: the count line closes every report that has findings.
func reportValidation(r ui.Reporter, path string, result validate.Result) {
	if len(result.Findings) == 0 {
		return
	}
	path = findingsPath(r, path)
	r.Section(path)
	showFindings(r, result)
	r.Result("%s: %s", path, counts(len(result.Refusals()), len(result.Warnings())))
}

// findingsPath is path as findings name it: short by default, in full with
// --verbose.
func findingsPath(r ui.Reporter, path string) string {
	if r.Verbose() {
		return path
	}
	return ui.ShortPath(path)
}

// showFindings is each finding's lines.
func showFindings(r ui.Reporter, result validate.Result) {
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

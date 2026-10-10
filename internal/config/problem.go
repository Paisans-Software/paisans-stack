package config

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/paisans-software/paisans-stack/internal/ui"
)

// readProblem is the Problem for a file that cannot be read. missing says
// what to do when it is not there at all, the case an operator meets first.
func readProblem(path, missing string, err error) error {
	cause := fmt.Errorf("reading %s: %w", path, err)
	if errors.Is(err, fs.ErrNotExist) {
		return &ui.Problem{Hint: ui.ShortPath(path) + " does not exist", Explain: missing, Cause: cause}
	}
	why := err.Error()
	var pe *fs.PathError
	if errors.As(err, &pe) {
		why = pe.Err.Error()
	}
	return &ui.Problem{Hint: ui.ShortPath(path) + " cannot be read", Explain: "Reading it failed: " + why + ".", Cause: cause}
}

var unknownField = regexp.MustCompile(`field (\S+) not found in type \S+`)

// parseProblem is the Problem for a file that is not the YAML it should be:
// each thing the decoder found is a line of the explanation, a key the
// schema lacks named as a key rather than as a Go type's field.
func parseProblem(path, what string, err error) error {
	var lines []string
	var te *yaml.TypeError
	if errors.As(err, &te) {
		lines = append([]string(nil), te.Errors...)
	} else {
		lines = []string{strings.TrimPrefix(err.Error(), "yaml: ")}
	}
	unknown := false
	for i, l := range lines {
		if fixed := unknownField.ReplaceAllString(l, "unknown key $1"); fixed != l {
			lines[i], unknown = fixed, true
		}
		lines[i] = "- " + strings.TrimSpace(lines[i])
	}
	explain := strings.Join(lines, "\n")
	if unknown {
		explain += "\nEvery key is checked, so a misspelt one is refused rather than ignored."
	}
	return &ui.Problem{
		Hint:    ui.ShortPath(path) + " cannot be read as " + what,
		Explain: explain,
		Cause:   fmt.Errorf("parsing %s: %w", path, err),
	}
}

package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Problem is an error said for an operator: what is wrong, what to do, and
// what it was found from. The command's last word is printed from it (see
// PrintError), and every other caller sees an ordinary error: Unwrap returns
// the cause, so errors.Is and errors.As see through it to a sentinel below.
type Problem struct {
	// Hint is one line, at most 80 characters, naming the problem in an
	// operator's words rather than the mechanism that found it.
	Hint string
	// Explain is what to do, and the reason when the hint is not enough.
	// Everything needed to act is here or in the hint, since the cause is
	// shown only with --verbose. A list is one line per item, each starting
	// "- ".
	Explain string
	// Cause is the underlying error, shown only with --verbose.
	Cause error
}

// Error is the hint, the cause's text and the explanation, so a caller that
// logs or embeds the error loses nothing.
func (p *Problem) Error() string {
	s := p.Hint
	if p.Cause != nil {
		s += ": " + p.Cause.Error()
	}
	if p.Explain != "" {
		s += "\n" + p.Explain
	}
	return s
}

func (p *Problem) Unwrap() error { return p.Cause }

// maxWidth is the widest prose is wrapped to, however wide the terminal: a
// longer line is harder to read, not easier.
const maxWidth = 80

// Describe is what the error printer shows for err without --verbose: the
// hint and explanation of the outermost Problem in its chain, or, for any
// other error, its first line as the hint and the rest as the explanation.
// A first line longer than 80 characters is cut at its first sentence.
func Describe(err error) (hint, explain string) {
	var p *Problem
	if errors.As(err, &p) {
		return p.Hint, p.Explain
	}
	msg := strings.TrimSpace(err.Error())
	hint, explain, _ = strings.Cut(msg, "\n")
	if len(hint) > maxWidth {
		if i := strings.Index(hint, ". "); i > 0 {
			explain = strings.TrimSpace(hint[i+2:] + "\n" + explain)
			hint = hint[:i]
		}
	}
	return hint, explain
}

// PrintError writes the error that ends a command to w, in the visual
// language of a refusal: a red ✗ (FAIL off a terminal) and the hint, then the
// explanation indented and wrapped to the terminal, at most 80 columns. With
// verbose, the cause chain follows, one link a line, with full paths.
func PrintError(w io.Writer, err error, verbose bool) {
	printError(w, err, verbose, isTerminal(w), terminalWidth(w))
}

func printError(w io.Writer, err error, verbose, terminal bool, width int) {
	if width <= 0 || width > maxWidth {
		width = maxWidth
	}
	hint, explain := Describe(err)
	if terminal {
		fmt.Fprintf(w, "%s✗%s %s\n", red, reset, hint)
	} else {
		fmt.Fprintf(w, "FAIL %s\n", hint)
	}
	for _, line := range Wrap(explain, width-2) {
		fmt.Fprintf(w, "  %s\n", line)
	}
	if !verbose {
		return
	}
	for _, link := range causeChain(err) {
		for _, line := range strings.Split(link, "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
}

// causeChain is what --verbose adds under a Problem: the context each caller
// wrapped around it, outermost first, then its cause, one link at a time.
// A link's own text is what it adds to the error it wraps; one that does not
// end with the error it wraps is shown whole and ends the chain. Without a
// Problem the error has already printed in full, and there is nothing to add.
func causeChain(err error) []string {
	var p *Problem
	if !errors.As(err, &p) {
		return nil
	}
	var links []string
	for e := err; e != nil && e != error(p); e = errors.Unwrap(e) {
		// A typed error that unwraps to a Problem in place of wrapping it
		// in text (storageadd.Waiting, config.LoadError) adds no context.
		if own, ok := ownText(e, errors.Unwrap(e)); ok && own != "" {
			links = append(links, own)
		}
	}
	for e := p.Cause; e != nil; {
		if q, ok := e.(*Problem); ok {
			links = append(links, q.Hint)
			e = q.Cause
			continue
		}
		next := errors.Unwrap(e)
		own, ok := ownText(e, next)
		if !ok {
			links = append(links, strings.TrimSpace(e.Error()))
			break
		}
		if own != "" {
			links = append(links, own)
		}
		e = next
	}
	return links
}

// ownText is what e says beyond next, the error it wraps, when e's text ends
// with next's.
func ownText(e, next error) (string, bool) {
	if next == nil {
		return "", false
	}
	s, n := e.Error(), next.Error()
	if !strings.HasSuffix(s, n) {
		return "", false
	}
	return strings.TrimRight(strings.TrimSuffix(s, n), ": \n"), true
}

// Wrap breaks text into lines of at most width characters, at spaces only,
// so a path, a command or a URL is never split; a word longer than width
// gets a line of its own. Each line of text wraps on its own and keeps its
// indentation, and a list item's continuation lines align under its text
// after "- ". Blank lines are dropped.
func Wrap(text string, width int) []string {
	var out []string
	for _, line := range strings.Split(strings.Trim(text, "\n"), "\n") {
		line = strings.TrimRight(line, " ")
		if strings.TrimSpace(line) == "" {
			continue
		}
		body := strings.TrimLeft(line, " ")
		lead := line[:len(line)-len(body)]
		hang := lead
		if strings.HasPrefix(body, "- ") {
			hang += "  "
		}
		cur := lead
		fresh := true
		for _, word := range strings.Fields(body) {
			if !fresh && len(cur)+1+len(word) > width {
				out = append(out, cur)
				cur, fresh = hang, true
			}
			if fresh {
				cur += word
				fresh = false
			} else {
				cur += " " + word
			}
		}
		out = append(out, cur)
	}
	return out
}

// ShortPath is how default output names a file: relative to the current
// directory when it is under it, with $HOME shortened to ~ otherwise, and as
// given when it is neither. It is for text a human reads, never for a command
// meant to be copied, where ~ is not expanded everywhere.
func ShortPath(path string) string {
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	return shortPath(path, wd, home)
}

func shortPath(path, wd, home string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	if wd != "" {
		if rel, err := filepath.Rel(wd, path); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return rel
		}
	}
	if home != "" {
		if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			if rel == "." {
				return "~"
			}
			return "~/" + rel
		}
	}
	return path
}

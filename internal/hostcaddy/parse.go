package hostcaddy

import (
	"strings"
)

// Parsed is what the toolkit reads from a Caddyfile it does not own: enough
// to decide where a site block can go, and nothing more. It is not a
// Caddyfile parser, and nothing here rewrites what it reads.
type Parsed struct {
	// Imports are the first argument of each `import` at the top level, as
	// written.
	Imports []string
	// Admin is the global admin endpoint's address, and AdminOff whether it
	// is turned off.
	Admin    string
	AdminOff bool
	// Sites are the addresses of every site block at the top level,
	// lowercased, without scheme, port or path.
	Sites []string
}

// Parse reads a Caddyfile's top level as Caddy's lexer splits it: tokens
// separated by space, a `#` starting a comment only where a token would
// start, and a brace opening or closing a block only as a token of its own,
// so a placeholder such as {$TOKEN} is a word.
func Parse(content string) Parsed {
	var p Parsed
	depth := 0
	inGlobal, seenBlock := false, false
	for _, line := range strings.Split(content, "\n") {
		tokens := tokenize(line)
		if len(tokens) == 0 {
			continue
		}
		if depth == 0 {
			opens := tokens[len(tokens)-1] == "{"
			header := tokens
			if opens {
				header = tokens[:len(tokens)-1]
			}
			switch {
			case len(header) == 0 && opens && !seenBlock:
				inGlobal = true
			case len(header) > 0 && header[0] == "import":
				if len(header) > 1 {
					p.Imports = append(p.Imports, header[1])
				}
			case len(header) > 0 && strings.HasPrefix(header[0], "("):
				// A snippet's definition, not a site.
			case opens:
				for _, h := range header {
					for _, a := range strings.Split(h, ",") {
						if a = siteAddress(a); a != "" {
							p.Sites = append(p.Sites, a)
						}
					}
				}
			}
			if opens {
				seenBlock = true
			}
		} else if depth == 1 && inGlobal && tokens[0] == "admin" {
			if len(tokens) > 1 && tokens[1] == "off" {
				p.AdminOff = true
			} else if len(tokens) > 1 && tokens[1] != "{" {
				p.Admin = tokens[1]
			}
		}
		for _, t := range tokens {
			switch t {
			case "{":
				depth++
			case "}":
				depth--
				if depth == 0 {
					inGlobal = false
				}
			}
		}
		if depth < 0 {
			depth = 0
		}
	}
	return p
}

// tokenize splits one line as Caddy's lexer does, dropping a comment. A
// quoted token keeps its content without the quotes.
func tokenize(line string) []string {
	var out []string
	var cur strings.Builder
	inToken, quote := false, rune(0)
	escaped := false
	for _, r := range line {
		switch {
		case quote != 0:
			if escaped {
				cur.WriteRune(r)
				escaped = false
			} else if r == '\\' && quote == '"' {
				escaped = true
			} else if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == ' ' || r == '\t' || r == '\r':
			if inToken {
				out = append(out, cur.String())
				cur.Reset()
				inToken = false
			}
		case r == '#' && !inToken:
			return out
		case (r == '"' || r == '`') && !inToken:
			quote, inToken = r, true
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	if inToken {
		out = append(out, cur.String())
	}
	return out
}

// siteAddress is a site block's address reduced to its host: no scheme,
// port or path, lowercased.
func siteAddress(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return ""
	}
	if _, after, ok := strings.Cut(a, "://"); ok {
		a = after
	}
	if i := strings.IndexByte(a, '/'); i >= 0 {
		a = a[:i]
	}
	if strings.HasPrefix(a, "[") {
		if i := strings.IndexByte(a, ']'); i >= 0 {
			a = a[1:i]
		}
	} else if i := strings.LastIndexByte(a, ':'); i >= 0 {
		a = a[:i]
	}
	return strings.ToLower(a)
}

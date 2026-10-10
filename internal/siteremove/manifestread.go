package siteremove

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// manifestRead is what the host holds at one path the manifest lists.
type manifestRead struct {
	found bool
	// sha256 is the file's sum, in hex.
	sha256 string
	// content is a compose file's content, which names its images. It is
	// empty for every other file.
	content string
}

func isCompose(path string) bool { return strings.HasSuffix(path, "/compose.yaml") }

// manifestFilesCommand reads every file the manifest lists in one command,
// with the privilege every other read has: one line per file, in the
// manifest's order, then `end`. A line is `gone` for a path that is not a
// regular file, `sum <hex>` for a file's SHA-256, or, for a compose file,
// `file <base64>` for its content, which both proves it and names its images.
// A file that exists and cannot be read stops the command before its line.
// Paths are quoted in single quotes; the caller has refused any that holds
// one, or a newline.
func manifestFilesCommand(entries []render.ManifestFile) string {
	var b strings.Builder
	for _, e := range entries {
		p := quote("/" + e.Path)
		if isCompose(e.Path) {
			fmt.Fprintf(&b, `m=%s; if [ -f "$m" ]; then c=$(base64 < "$m") || exit 4; printf 'file %%s\n' "$(printf %%s "$c" | tr -d '\n')"; else echo gone; fi`+"\n", p)
		} else {
			fmt.Fprintf(&b, `m=%s; if [ -f "$m" ]; then h=$(sha256sum < "$m") || exit 4; echo "sum ${h%%%% *}"; else echo gone; fi`+"\n", p)
		}
	}
	b.WriteString("echo end")
	return b.String()
}

// readManifestFiles reads every file the manifest lists, in one call to the
// host, and answers for each in the manifest's order. An answer that is cut
// short, garbled or carries anything past its end is an error naming the
// file, never a file taken as gone.
func readManifestFiles(t apply.Transport, entries []render.ManifestFile) ([]manifestRead, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out, err := t.Run(manifestFilesCommand(entries))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if err != nil {
		// The command stops at the file it cannot read, so the answers
		// before it say which one that is.
		answered := 0
		for _, l := range lines {
			if kind, _, _ := strings.Cut(l, " "); kind == "gone" || kind == "sum" || kind == "file" {
				answered++
			}
		}
		if answered < len(entries) {
			return nil, fmt.Errorf("reading /%s: %w", entries[answered].Path, err)
		}
		return nil, fmt.Errorf("reading the manifest's files: %w", err)
	}
	read := make([]manifestRead, len(entries))
	for i, e := range entries {
		if i >= len(lines) || lines[i] == "end" {
			return nil, fmt.Errorf("reading /%s: the host's answer is cut short before it", e.Path)
		}
		kind, value, _ := strings.Cut(lines[i], " ")
		switch {
		case kind == "gone" && value == "":
		case kind == "sum" && !isCompose(e.Path) && isSHA256(value):
			read[i] = manifestRead{found: true, sha256: value}
		case kind == "file" && isCompose(e.Path):
			content, derr := base64.StdEncoding.DecodeString(value)
			if derr != nil {
				return nil, fmt.Errorf("reading /%s: its content is not base64: %w", e.Path, derr)
			}
			read[i] = manifestRead{found: true, sha256: sum(string(content)), content: string(content)}
		default:
			return nil, fmt.Errorf("reading /%s: an unreadable answer %q", e.Path, lines[i])
		}
	}
	switch rest := lines[len(entries):]; {
	case len(rest) == 0 || rest[0] != "end":
		return nil, fmt.Errorf("reading the manifest's files: the host's answer has no end")
	case len(rest) > 1:
		return nil, fmt.Errorf("reading the manifest's files: the host answered %q after its end", rest[1])
	}
	return read, nil
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

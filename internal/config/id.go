package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// versionLine is the top level `version:` line an id is written after.
var versionLine = regexp.MustCompile(`(?m)^version:[^\n]*\n`)

// EnsureID gives the declaration at path an id if it has none, and reports
// the id it now has. An id that is already there is returned as it is, even a
// malformed one, for Load to refuse by name: the id is a deployment's
// identity, so nothing but an operator's own edit ever replaces it.
//
// The id is written as one line after `version:`, and the rest of the file is
// left byte for byte as it was. paisans.yaml is a file people read and
// comment, and a round trip through a YAML encoder would reformat it.
func EnsureID(path string) (id string, added bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", path, err)
	}
	existing, found, err := topLevelID(data)
	if err != nil {
		return "", false, fmt.Errorf("parsing %s: %w", path, err)
	}
	if found {
		return existing, false, nil
	}
	loc := versionLine.FindIndex(data)
	if loc == nil {
		return "", false, fmt.Errorf("%s: no top level `version:` line to write the deployment id after. Add `version: 1` as the file's first key", path)
	}
	id, err = deployment.NewID()
	if err != nil {
		return "", false, err
	}
	var out bytes.Buffer
	out.Write(data[:loc[1]])
	fmt.Fprintf(&out, "id: %s\n", id)
	out.Write(data[loc[1]:])

	if err := replaceFile(path, out.Bytes()); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// topLevelID reads the document's top level `id`, if it has one.
func topLevelID(data []byte) (string, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", false, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", false, nil
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "id" {
			return m.Content[i+1].Value, true, nil
		}
	}
	return "", false, nil
}

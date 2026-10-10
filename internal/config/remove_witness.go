package config

import (
	"bytes"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// RemoveSiteAndWitness is RemoveSite, and in the same write the witness taken
// out of etcd.members and the witness role taken off its site, every other
// byte as it was. It is `site remove`'s last stage when the removal leaves one
// data site, so that the witness leaves etcd with it; one write, so that a
// stopped run never leaves the file with the site gone and the witness still
// listed.
func RemoveSiteAndWitness(path, site, witness string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	out, err := removeSite(data, site)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if out, err = removeWitness(out, witness); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return replaceFile(path, out)
}

// removeWitness takes witness out of etcd.members, when the file has the key,
// and "witness" out of sites.<witness>.roles. A roles list left empty is written `[]` when it was a
// flow list on one line, and refused otherwise.
func removeWitness(data []byte, witness string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the top level is not a mapping")
	}
	top := doc.Content[0]
	lines := lineStarts(data)
	var edits []edit

	// With etcd.members left out, the voters are derived from the roles,
	// and the witness role coming off below is what takes it out of etcd.
	if members := walk(top, membersKey...); members != nil {
		e, found, err := removeItem(data, lines, members, witness)
		if err != nil {
			return nil, fmt.Errorf("etcd.members: %w", err)
		}
		if found {
			edits = append(edits, e)
		}
	}

	key := fmt.Sprintf("sites.%s.roles", witness)
	roles := walk(top, "sites", witness, "roles")
	if roles == nil || roles.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s is not a list this command can edit in place", key)
	}
	if len(roles.Content) == 1 && roles.Content[0].Value == string(RoleWitness) {
		if roles.Style&yaml.FlowStyle == 0 || roles.Line != roles.Content[0].Line {
			return nil, fmt.Errorf("%s names only witness; write it as [witness] on one line, or edit it by hand", key)
		}
		line := lines.text(data, roles.Line)
		open := lines.start(roles.Line) + len(string([]rune(line)[:roles.Column-1]))
		closing := bytes.IndexByte(data[open:], ']')
		if data[open] != '[' || closing < 0 || open+closing >= lines.next(roles.Line) {
			return nil, fmt.Errorf("%s on line %d is not written as a [ ] list on one line", key, roles.Line)
		}
		edits = append(edits, edit{open, open + closing + 1, "[]"})
	} else {
		e, found, err := removeItem(data, lines, roles, string(RoleWitness))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if found {
			edits = append(edits, e)
		}
	}

	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]byte(nil), data...)
	for _, e := range edits {
		var b bytes.Buffer
		b.Write(out[:e.start])
		b.WriteString(e.text)
		b.Write(out[e.end:])
		out = b.Bytes()
	}
	return out, nil
}

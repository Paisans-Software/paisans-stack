package config

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// WithoutSite is the configuration with one site taken out: its entry under
// sites, its name in cluster.sites, etcd.members and storage.garage.sites,
// and its storage.garage.capacities entry. It is what `site remove` renders
// the remaining sites from, and exactly what RemoveSite writes. The
// original is not changed.
func (c *Config) WithoutSite(site string) *Config {
	out := *c
	out.Sites = map[string]Site{}
	for name, s := range c.Sites {
		if name != site {
			out.Sites[name] = s
		}
	}
	out.Cluster.Sites = without(c.Cluster.Sites, site)
	out.Etcd.Members = without(c.Etcd.Members, site)
	out.Storage.Garage.Sites = without(c.Storage.Garage.Sites, site)
	if c.Storage.Garage.Capacities != nil {
		out.Storage.Garage.Capacities = map[string]string{}
		for name, v := range c.Storage.Garage.Capacities {
			if name != site {
				out.Storage.Garage.Capacities[name] = v
			}
		}
	}
	return &out
}

func without(list []string, s string) []string {
	var out []string
	for _, item := range list {
		if item != s {
			out = append(out, item)
		}
	}
	return out
}

// siteLists are the lists of site names RemoveSite takes the site out of,
// as paths from the top of the file.
var siteLists = [][]string{{"cluster", "sites"}, {"etcd", "members"}, {"storage", "garage", "sites"}}

// RemoveSite takes one site out of the declaration at path, the way
// WithoutSite does in memory, and leaves every other byte as it was: the
// site's block goes with the comment lines directly above it, each list
// loses the one item, and storage.garage.capacities loses its line. It is
// `site remove`'s last stage, and refuses a layout it cannot edit in place
// rather than reformatting the file, as SetMesh does.
func RemoveSite(path, site string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	out, err := removeSite(data, site)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return replaceFile(path, out)
}

func removeSite(data []byte, site string) ([]byte, error) {
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

	_, sites := lookup(top, "sites")
	if sites == nil || sites.Kind != yaml.MappingNode || sites.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("sites: is not a block mapping this command can edit in place")
	}
	key, _ := lookup(sites, site)
	if key == nil {
		return nil, fmt.Errorf("sites.%s is not declared", site)
	}
	e, err := removeEntry(data, lines, top, sites, key)
	if err != nil {
		return nil, fmt.Errorf("sites.%s: %w", site, err)
	}
	edits = append(edits, e)

	for _, keys := range siteLists {
		list := walk(top, keys...)
		if list == nil {
			continue
		}
		name := strings.Join(keys, ".")
		e, found, err := removeItem(data, lines, list, site)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if found {
			edits = append(edits, e)
		}
	}

	if caps := walk(top, "storage", "garage", "capacities"); caps != nil {
		if caps.Kind != yaml.MappingNode || caps.Style&yaml.FlowStyle != 0 {
			if k, _ := lookup(caps, site); k != nil {
				return nil, fmt.Errorf("storage.garage.capacities is not a block mapping this command can edit in place")
			}
		} else if k, v := lookup(caps, site); k != nil {
			if v.Line != k.Line {
				return nil, fmt.Errorf("storage.garage.capacities.%s spans lines; write it on one line", site)
			}
			edits = append(edits, edit{lines.start(k.Line), lines.next(k.Line), ""})
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

// walk follows keys down from a mapping, nil when any is missing.
func walk(m *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		_, m = lookup(m, k)
		if m == nil {
			return nil
		}
	}
	return m
}

// removeEntry deletes one key of a block mapping and its value: from the
// comment lines directly above the key, at its indentation, to the line
// before whatever follows it. Comment and blank lines just before what
// follows, at the key's indentation or less, are left, since they introduce
// what follows.
func removeEntry(data []byte, lines lineIndex, top, parent, key *yaml.Node) (edit, error) {
	next := 0
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i] == key && i+2 < len(parent.Content) {
			next = parent.Content[i+2].Line
		}
	}
	if next == 0 {
		// The last entry: it ends where the next top level key starts.
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Line > key.Line && (next == 0 || top.Content[i].Line < next) {
				next = top.Content[i].Line
			}
		}
	}
	last := len(lines)
	if next == 0 {
		next = last + 1
		if lines.start(last) == len(data) {
			next = last
		}
	}
	end := next - 1
	for end > key.Line && introduces(lines.text(data, end), key.Column) {
		end--
	}
	start := key.Line
	for start > 1 && commentAt(lines.text(data, start-1), key.Column) {
		start--
	}
	endOffset := len(data)
	if end < len(lines) {
		endOffset = lines.start(end + 1)
	}
	return edit{lines.start(start), endOffset, ""}, nil
}

// introduces reports whether a line is blank, or a comment starting at or
// left of column (1-based).
func introduces(line string, column int) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true
	}
	indent := len(line) - len(strings.TrimLeft(line, " "))
	return strings.HasPrefix(trimmed, "#") && indent+1 <= column
}

// commentAt reports whether a line is a comment starting exactly at column.
func commentAt(line string, column int) bool {
	trimmed := strings.TrimLeft(line, " ")
	return strings.HasPrefix(trimmed, "#") && len(line)-len(trimmed)+1 == column
}

// removeItem takes one name out of a list of site names: its line in a block
// sequence, or its word in a flow sequence written on one line.
func removeItem(data []byte, lines lineIndex, list *yaml.Node, name string) (edit, bool, error) {
	if list.Kind != yaml.SequenceNode {
		return edit{}, false, fmt.Errorf("is not a list this command can edit in place")
	}
	at := -1
	for i, item := range list.Content {
		if item.Kind == yaml.ScalarNode && item.Value == name {
			at = i
		}
	}
	if at < 0 {
		return edit{}, false, nil
	}
	if len(list.Content) == 1 {
		return edit{}, false, fmt.Errorf("names only %s; edit it by hand", name)
	}
	item := list.Content[at]
	if list.Style&yaml.FlowStyle == 0 {
		for i, other := range list.Content {
			if i != at && other.Line == item.Line {
				return edit{}, false, fmt.Errorf("has more than one item on line %d; write one per line", item.Line)
			}
		}
		return edit{lines.start(item.Line), lines.next(item.Line), ""}, true, nil
	}
	first, final := list.Content[0], list.Content[len(list.Content)-1]
	if list.Line != final.Line || first.Line != list.Line {
		return edit{}, false, fmt.Errorf("is a flow list over several lines; write it on one line")
	}
	line := lines.text(data, list.Line)
	open := lines.start(list.Line) + len(string([]rune(line)[:list.Column-1]))
	if data[open] != '[' {
		return edit{}, false, fmt.Errorf("on line %d is not written as a [ ] list", list.Line)
	}
	closing := bytes.IndexByte(data[open:], ']')
	if closing < 0 || open+closing >= lines.next(list.Line) {
		return edit{}, false, fmt.Errorf("on line %d has no closing ] on the same line", list.Line)
	}
	var kept []string
	for i, it := range list.Content {
		if i == at {
			continue
		}
		if it.Style != 0 && it.Style != yaml.FlowStyle {
			return edit{}, false, fmt.Errorf("on line %d quotes an item; write the names plain", list.Line)
		}
		kept = append(kept, it.Value)
	}
	return edit{open, open + closing + 1, "[" + strings.Join(kept, ", ") + "]"}, true, nil
}

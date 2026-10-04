// Package configmerge places an operator's passthrough `config` keys into a
// file a template has already rendered, in that file's own syntax. It knows
// nothing about kinds, apps or templates: one function per format, each taking
// the rendered text and the keys, so the merge is testable on its own.
//
// Env and ini are line oriented rather than parsed and re-emitted. Both files
// are read by people, a round trip through a general data structure would
// reformat them, and the insertion is small enough to be obvious: env appends
// a labelled block, and ini adds lines inside the section that is already
// there. Every line the template wrote, comments and blank lines included,
// survives unchanged.
//
// Yaml is parsed into yaml.Node, which keeps comments, quoting, flow style and
// key order. A key whose top level name the rendered file does not have is
// appended as a block after the file, which is then left byte for byte as the
// template wrote it. Only a key that lands inside a mapping the template wrote
// needs the document re-emitted, and that re-emit drops the blank lines
// between top level keys: yaml.v3 does not record them. Comments survive it.
//
// Json has no comments. It is decoded preserving key order and re-emitted in
// encoding/json's indented style, which is the style the template writes, so
// the only lines that change are the ones a key was added to.
//
// In every format the keys are sorted before anything is written, so the same
// declaration renders the same bytes. A key the rendered file already carries
// is a Collision rather than an overwrite, and a value is a scalar: nesting is
// expressed by a dotted key, never by a map or a list.
package configmerge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Collision is returned when the rendered file already carries a key.
type Collision struct{ Key string }

func (c *Collision) Error() string {
	return fmt.Sprintf("config key %s is already set by the rendered file", c.Key)
}

// label is the comment written above added keys, so a reader of the rendered
// file can tell them from what the template wrote.
const label = "Set by `config` in paisans.yaml, not by a template."

func sortedKeys(keys map[string]any) []string {
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkScalar refuses anything that is not a string, a number or a boolean.
// A map value would also hide its inner names from validate's credential name
// check, which reads only the top level keys.
func checkScalar(key string, v any) error {
	switch v.(type) {
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return nil
	case nil:
		return fmt.Errorf("config key %s has no value", key)
	default:
		return fmt.Errorf("config key %s: a value must be a string, a number or a boolean, not %T; nest with a dotted key instead", key, v)
	}
}

// plain is a scalar's text in Go's ordinary formatting: true, 3, 2.5.
func plain(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// withNewline makes s end in a newline unless it is empty, so appended lines
// start on a line of their own.
func withNewline(s string) string {
	if s != "" && !strings.HasSuffix(s, "\n") {
		return s + "\n"
	}
	return s
}

// ---- env ----

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Env appends the keys to a rendered .env file as a labelled block.
func Env(rendered string, keys map[string]any) (string, error) {
	if len(keys) == 0 {
		return rendered, nil
	}
	present := envAssigned(rendered)
	var block strings.Builder
	for _, k := range sortedKeys(keys) {
		if !envName.MatchString(k) {
			return "", fmt.Errorf("config key %q is not an environment variable name: letters, digits and underscores, not starting with a digit", k)
		}
		if err := checkScalar(k, keys[k]); err != nil {
			return "", err
		}
		if present[k] {
			return "", &Collision{Key: k}
		}
		v, err := envValue(k, plain(keys[k]))
		if err != nil {
			return "", err
		}
		block.WriteString(k + "=" + v + "\n")
	}
	out := withNewline(rendered)
	if out != "" {
		out += "\n"
	}
	return out + "# " + label + "\n" + block.String(), nil
}

// envAssigned is the set of names the rendered file assigns, ignoring
// indentation, `export ` and comment lines.
func envAssigned(rendered string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(rendered, "\n") {
		line = strings.TrimLeft(line, " \t")
		if strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			line = strings.TrimLeft(rest, " \t")
		}
		if name, _, ok := strings.Cut(line, "="); ok {
			names[strings.TrimRight(name, " \t")] = true
		}
	}
	return names
}

// envValue writes a value so that docker compose, reading the file as an
// env_file, delivers it byte for byte. Both behaviours were observed by
// running `docker compose config`, not reasoned from documentation.
//
// A value with none of the characters compose treats specially is written
// bare, which is the style every .env template uses. Unquoted, compose strips
// leading and trailing whitespace, ends the value at " #", expands `$`, and
// strips a pair of surrounding quotes. Anything that could meet one of those
// is double quoted instead, with the four escapes compose's parser undoes
// inside double quotes: backslash, double quote, dollar and newline. A
// carriage return or other control character is refused, since no observed
// form carries it.
func envValue(key, v string) (string, error) {
	needsQuotes := strings.TrimSpace(v) != v || strings.ContainsAny(v, "#$'\"\\")
	for _, r := range v {
		if r == '\n' || r == '\t' {
			needsQuotes = true
			continue
		}
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("config key %s: the value contains a control character (%U) an env file cannot carry", key, r)
		}
	}
	if !needsQuotes {
		return v, nil
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`, nil
}

// ---- ini ----

var iniKey = regexp.MustCompile(`^([A-Za-z0-9_-]+)\.([A-Za-z0-9_-]+)$`)

type iniSection struct {
	after int // index of the line new keys go after
	keys  map[string]bool
}

// INI inserts each key into its section, creating a missing section once at
// the end. A key is exactly `section.key`, one dot.
//
// Keys go after the last assignment in the section, or after its header when
// it has none, so comments and blank lines that introduce the next section
// stay with it. A repeated section is never written: whether a second
// [section] merges with the first or shadows it belongs to whichever ini
// parser the application uses.
func INI(rendered string, keys map[string]any) (string, error) {
	if len(keys) == 0 {
		return rendered, nil
	}
	lines := strings.SplitAfter(rendered, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	sections := map[string]*iniSection{}
	var cur *iniSection
	for i, line := range lines {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"):
			name := strings.TrimSpace(t[1 : len(t)-1])
			if s, ok := sections[name]; ok {
				cur = s // a repeated header continues the first, for collisions
				continue
			}
			cur = &iniSection{after: i, keys: map[string]bool{}}
			sections[name] = cur
		case t == "" || strings.HasPrefix(t, ";") || strings.HasPrefix(t, "#"):
		case cur != nil:
			end := strings.IndexAny(t, "=:")
			if end < 0 {
				end = len(t)
			}
			cur.keys[strings.TrimSpace(t[:end])] = true
			cur.after = i
		}
	}

	insert := map[int][]string{} // line index -> lines to add after it
	var created []string
	var createdKeys = map[string][]string{}
	for _, k := range sortedKeys(keys) {
		m := iniKey.FindStringSubmatch(k)
		if m == nil {
			return "", fmt.Errorf("config key %q is not section.key: an ini key names exactly one section and one key, with one dot", k)
		}
		if err := checkScalar(k, keys[k]); err != nil {
			return "", err
		}
		v, err := iniValue(k, plain(keys[k]))
		if err != nil {
			return "", err
		}
		section, name := m[1], m[2]
		line := name + " = " + v + "\n"
		if s, ok := sections[section]; ok {
			if s.keys[name] {
				return "", &Collision{Key: k}
			}
			if len(insert[s.after]) == 0 {
				insert[s.after] = append(insert[s.after], "; "+label+"\n")
			}
			insert[s.after] = append(insert[s.after], line)
			continue
		}
		if _, ok := createdKeys[section]; !ok {
			created = append(created, section)
		}
		createdKeys[section] = append(createdKeys[section], line)
	}

	var b strings.Builder
	for i, line := range lines {
		b.WriteString(line)
		if add := insert[i]; len(add) > 0 {
			if !strings.HasSuffix(line, "\n") {
				b.WriteString("\n")
			}
			for _, a := range add {
				b.WriteString(a)
			}
		}
	}
	out := b.String()
	for _, section := range created {
		out = withNewline(out)
		if out != "" {
			out += "\n"
		}
		out += "[" + section + "]\n; " + label + "\n" + strings.Join(createdKeys[section], "")
	}
	return out, nil
}

// iniValue refuses what an ini parser could read as something other than the
// value: a newline ends it, `;` and `#` start an inline comment, edge
// whitespace is trimmed, and surrounding quotes or backticks are stripped.
// Each of those was observed with github.com/go-ini/ini v1.67.0, the version
// the WriteFreely fork pins, loading with its default options: `a;b` and
// `a#b` both read back as `a`. The template writes values bare, and so does
// this.
func iniValue(key, v string) (string, error) {
	switch {
	case strings.ContainsAny(v, "\r\n"):
		return "", fmt.Errorf("config key %s: an ini value cannot span lines", key)
	case strings.ContainsAny(v, ";#"):
		return "", fmt.Errorf("config key %s: an ini value cannot contain ; or #, which an ini parser may read as a comment", key)
	case strings.TrimSpace(v) != v:
		return "", fmt.Errorf("config key %s: an ini value cannot start or end with whitespace, which an ini parser trims", key)
	case v != "" && strings.ContainsRune("\"'`", rune(v[0])):
		return "", fmt.Errorf("config key %s: an ini value cannot start with a quote, which an ini parser may strip", key)
	}
	return v, nil
}

// ---- paths, for yaml and json ----

// paths splits each dotted key into its segments, sorted by key, and refuses
// an empty segment or two keys where one is a prefix of the other: `a` and
// `a.b` cannot both hold, since one makes `a` a value and the other a mapping.
func paths(keys map[string]any) ([]string, map[string][]string, error) {
	sorted := sortedKeys(keys)
	segs := map[string][]string{}
	for _, k := range sorted {
		parts := strings.Split(k, ".")
		for _, p := range parts {
			if p == "" {
				return nil, nil, fmt.Errorf("config key %q has an empty path segment", k)
			}
		}
		if err := checkScalar(k, keys[k]); err != nil {
			return nil, nil, err
		}
		segs[k] = parts
	}
	for i := 1; i < len(sorted); i++ {
		// Sorted, a prefix sorts directly before something it prefixes, or
		// before a run of keys that all share it; checking every earlier key
		// keeps this obviously right at the sizes involved.
		for j := 0; j < i; j++ {
			if strings.HasPrefix(sorted[i], sorted[j]+".") {
				return nil, nil, fmt.Errorf("config keys %s and %s contradict each other: one sets %s to a value, the other nests under it", sorted[j], sorted[i], sorted[j])
			}
		}
	}
	return sorted, segs, nil
}

// ---- yaml ----

// YAML sets each dotted key as a nested path in a rendered yaml document.
func YAML(rendered string, keys map[string]any) (string, error) {
	if len(keys) == 0 {
		return rendered, nil
	}
	sorted, segs, err := paths(keys)
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
		return "", fmt.Errorf("the rendered yaml does not parse: %w", err)
	}
	var root *yaml.Node
	if len(doc.Content) > 0 {
		root = doc.Content[0]
		if root.Kind != yaml.MappingNode {
			return "", errors.New("the rendered yaml is not a mapping, so a config key has nowhere to go")
		}
	}
	// A flow mapping cannot have block text appended after it, so everything
	// goes through the re-emit.
	flow := root != nil && root.Style&yaml.FlowStyle != 0

	appended := &yaml.Node{Kind: yaml.MappingNode}
	reemit := false
	for _, k := range sorted {
		value := &yaml.Node{}
		if err := value.Encode(keys[k]); err != nil {
			return "", fmt.Errorf("config key %s: %w", k, err)
		}
		path := segs[k]
		if root == nil || (!flow && yamlLookup(root, path[0]) == nil) {
			yamlSet(appended, path, value)
			continue
		}
		node := root
		for i, seg := range path {
			child := yamlLookup(node, seg)
			if child == nil {
				// Inside the template's own mapping the label goes on the
				// key, since there is no block of its own to head.
				yamlSet(node, path[i:], value).HeadComment = label
				reemit = true
				break
			}
			if i == len(path)-1 || child.Kind != yaml.MappingNode {
				return "", &Collision{Key: k}
			}
			node = child
		}
	}

	out := rendered
	if reemit {
		enc, err := yamlEncode(&doc)
		if err != nil {
			return "", err
		}
		out = enc
	}
	if len(appended.Content) > 0 {
		block, err := yamlEncode(appended)
		if err != nil {
			return "", err
		}
		out = withNewline(out)
		if out != "" {
			out += "\n"
		}
		out += "# " + label + "\n" + block
	}
	// Appending after a document end marker would start a second document,
	// which the application would not read. Refuse rather than ship that.
	dec := yaml.NewDecoder(strings.NewReader(out))
	var first, second yaml.Node
	if err := dec.Decode(&first); err != nil {
		return "", fmt.Errorf("the merged yaml does not parse: %w", err)
	}
	if err := dec.Decode(&second); err != io.EOF {
		return "", errors.New("the merged yaml is more than one document; the rendered file must be a single document without an end marker")
	}
	return out, nil
}

func yamlLookup(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// yamlSet creates path under mapping, reusing a mapping an earlier key made
// for a shared prefix, and returns the first key node it created.
func yamlSet(mapping *yaml.Node, path []string, value *yaml.Node) *yaml.Node {
	var first *yaml.Node
	add := func(k string, v *yaml.Node) {
		key := yamlKey(k)
		if first == nil {
			first = key
		}
		mapping.Content = append(mapping.Content, key, v)
	}
	for len(path) > 1 {
		child := yamlLookup(mapping, path[0])
		if child == nil {
			child = &yaml.Node{Kind: yaml.MappingNode}
			add(path[0], child)
		}
		mapping, path = child, path[1:]
	}
	add(path[0], value)
	return first
}

func yamlKey(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

// yamlEncode uses an indent of two, which is what the templates write.
func yamlEncode(n *yaml.Node) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ---- json ----

// object is a json object that remembers its key order, so a re-emit keeps
// the template's order instead of sorting it.
type object struct {
	keys []string
	vals map[string]any
}

// JSON sets each dotted key as a nested path in a rendered json document.
func JSON(rendered string, keys map[string]any) (string, error) {
	if len(keys) == 0 {
		return rendered, nil
	}
	sorted, segs, err := paths(keys)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(strings.NewReader(rendered))
	dec.UseNumber()
	top, err := jsonDecode(dec)
	if err != nil {
		return "", fmt.Errorf("the rendered json does not parse: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", errors.New("the rendered json has trailing content after its value")
	}
	root, ok := top.(*object)
	if !ok {
		return "", errors.New("the rendered json is not an object, so a config key has nowhere to go")
	}
	for _, k := range sorted {
		node := root
		path := segs[k]
		for i, seg := range path {
			child, exists := node.vals[seg]
			if i == len(path)-1 {
				if exists {
					return "", &Collision{Key: k}
				}
				node.set(seg, keys[k])
				break
			}
			if !exists {
				child = &object{vals: map[string]any{}}
				node.set(seg, child)
			}
			next, ok := child.(*object)
			if !ok {
				return "", &Collision{Key: k}
			}
			node = next
		}
	}
	var b strings.Builder
	if err := jsonEncode(&b, root, ""); err != nil {
		return "", err
	}
	if strings.HasSuffix(rendered, "\n") {
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func jsonDecode(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		o := &object{vals: map[string]any{}}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := jsonDecode(dec)
			if err != nil {
				return nil, err
			}
			o.set(kt.(string), v)
		}
		_, err := dec.Token()
		return o, err
	case json.Delim('['):
		var a []any
		for dec.More() {
			v, err := jsonDecode(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := dec.Token()
		if a == nil {
			a = []any{}
		}
		return a, err
	default:
		return tok, nil
	}
}

// jsonEncode writes v in encoding/json's MarshalIndent style with a two space
// indent, without escaping <, > and &, which a browser config has no reason
// to see escaped.
func jsonEncode(b *strings.Builder, v any, indent string) error {
	inner := indent + "  "
	switch v := v.(type) {
	case *object:
		if len(v.keys) == 0 {
			b.WriteString("{}")
			return nil
		}
		b.WriteString("{\n")
		for i, k := range v.keys {
			b.WriteString(inner)
			if err := jsonScalar(b, k); err != nil {
				return err
			}
			b.WriteString(": ")
			if err := jsonEncode(b, v.vals[k], inner); err != nil {
				return err
			}
			if i < len(v.keys)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "}")
	case []any:
		if len(v) == 0 {
			b.WriteString("[]")
			return nil
		}
		b.WriteString("[\n")
		for i, e := range v {
			b.WriteString(inner)
			if err := jsonEncode(b, e, inner); err != nil {
				return err
			}
			if i < len(v)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "]")
	default:
		return jsonScalar(b, v)
	}
	return nil
}

func jsonScalar(b *strings.Builder, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	b.WriteString(strings.TrimSuffix(buf.String(), "\n"))
	return nil
}

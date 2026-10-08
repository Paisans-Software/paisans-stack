package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseEndpointPort is the port of an endpoint written host:port.
func ParseEndpointPort(endpoint string) (int, error) {
	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return 0, fmt.Errorf("is not host:port (%v)", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("has port %q, which is not a number from 1 to 65535", port)
	}
	return n, nil
}

// ListenPort is the UDP port this site's WireGuard interface listens on: its
// endpoint's port, since that is the port every other site dials. A site with
// no endpoint is never dialled, so it has no fixed port: its interface is
// rendered without ListenPort and WireGuard picks one ("Optional; if not
// specified, chosen randomly", wireguard-tools v1.0.20210914, src/man/wg.8).
// That is also what lets two deployments share a host behind NAT without
// choosing ports. Zero means none. It assumes the endpoint was checked.
func (s Site) ListenPort() int {
	if s.Endpoint == "" {
		return 0
	}
	port, err := ParseEndpointPort(s.Endpoint)
	if err != nil {
		return 0
	}
	return port
}

// SetMesh writes mesh.subnet and the named sites' addresses into the
// declaration at path, and leaves every other byte as it was: each value is
// replaced where it stands, in the quoting it was written in, so comments,
// order and spacing survive. A missing mesh.subnet is added: inside an
// existing `mesh:` block, or as a new block after the top level `id:` line.
// It is `paisans init`'s write, and refuses a layout it cannot edit in place
// rather than reformatting the file.
func SetMesh(path, subnet string, addresses map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	out, err := setMesh(data, subnet, addresses)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return replaceFile(path, out)
}

// edit replaces data[start:end] with text.
type edit struct {
	start, end int
	text       string
}

func setMesh(data []byte, subnet string, addresses map[string]string) ([]byte, error) {
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

	meshKey, mesh := lookup(top, "mesh")
	switch {
	case mesh == nil:
		_, id := lookup(top, "id")
		if id == nil {
			return nil, fmt.Errorf("no top level `id:` line to write mesh.subnet after. Run `paisans init` on a file with `version: 1` as its first key")
		}
		at := lines.next(id.Line)
		edits = append(edits, edit{at, at, "mesh:\n  subnet: " + subnet + "\n"})
	case mesh.Kind == yaml.ScalarNode && mesh.Tag == "!!null":
		at := lines.next(meshKey.Line)
		if mesh.Line != meshKey.Line {
			return nil, fmt.Errorf("mesh: is written in a form this command cannot edit in place. Write `mesh:` on a line of its own")
		}
		// `mesh:` with nothing after it: the subnet goes on the next line.
		end := lines.start(meshKey.Line) + len(strings.TrimRight(lines.text(data, meshKey.Line), "\n"))
		edits = append(edits, edit{end, at, "\n  subnet: " + subnet + "\n"})
	case mesh.Kind != yaml.MappingNode || mesh.Style&yaml.FlowStyle != 0:
		return nil, fmt.Errorf("mesh: is written in a form this command cannot edit in place. Write it as a block, `mesh:` with `subnet:` indented under it")
	default:
		if _, value := lookup(mesh, "subnet"); value != nil {
			e, err := replaceScalar(data, lines, value, subnet)
			if err != nil {
				return nil, fmt.Errorf("mesh.subnet: %w", err)
			}
			edits = append(edits, e)
		} else if len(mesh.Content) > 0 {
			first := mesh.Content[0]
			at := lines.start(first.Line)
			edits = append(edits, edit{at, at, strings.Repeat(" ", first.Column-1) + "subnet: " + subnet + "\n"})
		} else {
			return nil, fmt.Errorf("mesh: is empty in a form this command cannot edit in place")
		}
	}

	_, sites := lookup(top, "sites")
	names := make([]string, 0, len(addresses))
	for name := range addresses {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var value *yaml.Node
		if sites != nil && sites.Kind == yaml.MappingNode {
			if _, site := lookup(sites, name); site != nil && site.Kind == yaml.MappingNode {
				_, value = lookup(site, "address")
			}
		}
		if value == nil {
			return nil, fmt.Errorf("sites.%s.address: not found as a value this command can replace", name)
		}
		e, err := replaceScalar(data, lines, value, addresses[name])
		if err != nil {
			return nil, fmt.Errorf("sites.%s.address: %w", name, err)
		}
		edits = append(edits, e)
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

// lookup finds key in a mapping node: the key node and its value.
func lookup(m *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// replaceScalar replaces a scalar's text where it stands, keeping its
// quoting. The text found there must be the value as the parser read it, so
// an escape or a multi-line scalar is refused rather than half replaced.
func replaceScalar(data []byte, lines lineIndex, n *yaml.Node, value string) (edit, error) {
	if n.Kind != yaml.ScalarNode {
		return edit{}, fmt.Errorf("is not a single value")
	}
	var text string
	switch n.Style {
	case 0:
		text = n.Value
	case yaml.DoubleQuotedStyle:
		text, value = `"`+n.Value+`"`, `"`+value+`"`
	case yaml.SingleQuotedStyle:
		text, value = "'"+n.Value+"'", "'"+value+"'"
	default:
		return edit{}, fmt.Errorf("is written in a style this command cannot edit in place. Write it plain, on one line")
	}
	line := lines.text(data, n.Line)
	runes := []rune(line)
	if n.Column-1 > len(runes) {
		return edit{}, fmt.Errorf("could not be found on line %d", n.Line)
	}
	start := lines.start(n.Line) + len(string(runes[:n.Column-1]))
	if !bytes.HasPrefix(data[start:], []byte(text)) {
		return edit{}, fmt.Errorf("on line %d is not written as %s, so it was not replaced", n.Line, text)
	}
	return edit{start, start + len(text), value}, nil
}

// lineIndex is the byte offset each line of a file starts at.
type lineIndex []int

func lineStarts(data []byte) lineIndex {
	out := lineIndex{0}
	for i, b := range data {
		if b == '\n' {
			out = append(out, i+1)
		}
	}
	return out
}

// start is where 1-based line n begins.
func (l lineIndex) start(n int) int { return l[n-1] }

// next is where the line after line n begins.
func (l lineIndex) next(n int) int {
	if n < len(l) {
		return l[n]
	}
	return l[len(l)-1]
}

// text is line n with its newline.
func (l lineIndex) text(data []byte, n int) string {
	end := len(data)
	if n < len(l) {
		end = l[n]
	}
	return string(data[l[n-1]:end])
}

// replaceFile writes data over path through a temporary file beside it, with
// path's permissions, so a failed write never leaves half a declaration.
func replaceFile(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

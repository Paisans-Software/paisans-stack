// Package deployrecord is the record each gateway keeps of what the
// deployment has deployed: its sites, apps and Pocket ID groups, by name.
// apply adds to it and only the removal commands take from it, so a
// paisans.yaml edited by mistake cannot shrink it, and `secrets prune` removes
// only what the yaml does not declare and no record lists
// (docs/specs/2026-10-09-deployment-record.md).
package deployrecord

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/registry"
)

// Version is the record's layout.
const Version = 1

// ChangedMarker is what the write prints when the record changed between
// the read and the write: another command wrote it, and running again reads
// that.
const ChangedMarker = "paisans: the deployment record changed while it was read"

// ErrMalformed is a record that cannot be read as one: corrupt, or written
// by a toolkit with a newer layout. Nothing rewrites it, since what it held
// is unknown; it is deleted by hand and the gateway applied again.
var ErrMalformed = errors.New("not a deployment record this toolkit reads")

// Record is the names a deployment has deployed, each list sorted.
type Record struct {
	Version int `json:"version"`
	// UpdatedAt is when the change was made, RFC 3339 by the writer's
	// clock, for a person reading the file. Nothing compares it.
	UpdatedAt      string   `json:"updated_at"`
	Sites          []string `json:"sites"`
	Apps           []string `json:"apps"`
	PocketIDGroups []string `json:"pocket_id_groups"`
}

// Path is the record on a gateway, named by the deployment's token, which is
// what proves it this deployment's.
func Path(d deployment.Deployment) string {
	return registry.Dir + "/deployed." + d.Token() + ".json"
}

// FromConfig is everything cfg declares.
func FromConfig(cfg *config.Config) Record {
	r := Record{Sites: cfg.SiteNames(), Apps: cfg.AppNames()}
	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Kind == config.KindPocketID {
			r.PocketIDGroups = append(r.PocketIDGroups, kinds.PocketIDSignupGroups(cfg.Apps[name].Settings)...)
		}
	}
	return normal(r)
}

// List is the names the record holds under kind: sites, apps or
// pocket_id_groups.
func (r Record) List(kind string) []string {
	switch kind {
	case "sites":
		return r.Sites
	case "apps":
		return r.Apps
	case "pocket_id_groups":
		return r.PocketIDGroups
	}
	return nil
}

// Lists reports whether the record names name under kind: sites, apps or
// pocket_id_groups, the secrets file's own keys.
func (r Record) Lists(kind, name string) bool {
	for _, n := range r.List(kind) {
		if n == name {
			return true
		}
	}
	return false
}

// normal sorts each list, drops repeats, and makes absent lists empty, so
// that equal records compare the same.
func normal(r Record) Record {
	clean := func(list []string) []string {
		out := []string{}
		for _, n := range list {
			if n != "" && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
		sort.Strings(out)
		return out
	}
	return Record{Version: Version, UpdatedAt: r.UpdatedAt, Sites: clean(r.Sites), Apps: clean(r.Apps), PocketIDGroups: clean(r.PocketIDGroups)}
}

// entry is one name's history: a tag for every add of it, and the tags of
// the adds a removal saw.
type entry struct {
	Adds    []string `json:"adds,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// live is the tags of the adds no removal saw, sorted.
func (e entry) live() []string {
	var out []string
	for _, t := range e.Adds {
		if !slices.Contains(e.Removed, t) {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// deployed is a name with an add no removal saw: an add made where a removal
// was never seen survives that removal.
func (e entry) deployed() bool { return len(e.live()) > 0 }

// doc is the record as stored: every name with its add and removed tags.
type doc struct {
	Version        int              `json:"version"`
	UpdatedAt      string           `json:"updated_at"`
	Sites          map[string]entry `json:"sites"`
	Apps           map[string]entry `json:"apps"`
	PocketIDGroups map[string]entry `json:"pocket_id_groups"`
}

var kindNames = []string{"sites", "apps", "pocket_id_groups"}

func newDoc() doc {
	return doc{Version: Version, Sites: map[string]entry{}, Apps: map[string]entry{}, PocketIDGroups: map[string]entry{}}
}

func (m *doc) kind(kind string) map[string]entry {
	switch kind {
	case "sites":
		return m.Sites
	case "apps":
		return m.Apps
	}
	return m.PocketIDGroups
}

// view is the deployed names of m.
func view(m doc) Record {
	r := Record{UpdatedAt: m.UpdatedAt}
	lists := map[string]*[]string{"sites": &r.Sites, "apps": &r.Apps, "pocket_id_groups": &r.PocketIDGroups}
	for _, kind := range kindNames {
		for name, e := range m.kind(kind) {
			if e.deployed() {
				*lists[kind] = append(*lists[kind], name)
			}
		}
	}
	return normal(r)
}

func tagSet(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		for _, t := range l {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out
}

// merge is, for every name, every add tag and every removed tag any of ds
// holds: no add one record alone saw, and no removal one alone saw, is lost.
func merge(ds ...doc) doc {
	out := newDoc()
	for _, m := range ds {
		out.UpdatedAt = max(out.UpdatedAt, m.UpdatedAt)
		for _, kind := range kindNames {
			into := out.kind(kind)
			for name, e := range m.kind(kind) {
				cur := into[name]
				into[name] = entry{Adds: tagSet(cur.Adds, e.Adds), Removed: tagSet(cur.Removed, e.Removed)}
			}
		}
	}
	return out
}

// compact drops the removed tags and the names that are not deployed, and
// leaves each deployed name one live tag: its own when it has one, and a new
// one when it has several. A new tag is one no removal anywhere can have
// seen, so a removal held by a record this write did not read, or made
// between its read and its write, cannot cancel the add that survives here.
// It runs only when every gateway in the write answered, so that the removed
// tags it drops are, as far as this write can tell, held nowhere else.
func compact(m doc) doc {
	out := newDoc()
	out.UpdatedAt = m.UpdatedAt
	for _, kind := range kindNames {
		for name, e := range m.kind(kind) {
			switch live := e.live(); {
			case len(live) == 1:
				out.kind(kind)[name] = entry{Adds: live}
			case len(live) > 1:
				out.kind(kind)[name] = entry{Adds: []string{newTag()}}
			}
		}
	}
	return out
}

func encodeDoc(m doc) string {
	m = merge(m)
	m.Version = Version
	data, _ := json.Marshal(m)
	return string(data) + "\n"
}

func parseDoc(content string) (doc, error) {
	dec := json.NewDecoder(strings.NewReader(content))
	dec.DisallowUnknownFields()
	m := newDoc()
	if err := dec.Decode(&m); err != nil {
		return doc{}, err
	}
	if m.Version != Version {
		return doc{}, fmt.Errorf("version %d, and this toolkit reads version %d", m.Version, Version)
	}
	return merge(m), nil
}

// newTag is a fresh tag for an add. Tests replace it.
var newTag = func() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Encode is a stored record holding every name of r, each with one tag,
// "t-" and its name. It is for tests: everything that writes a gateway goes
// through Update.
func Encode(r Record) string {
	m := newDoc()
	for _, kind := range kindNames {
		for _, name := range r.List(kind) {
			m.kind(kind)[name] = entry{Adds: []string{"t-" + name}}
		}
	}
	return encodeDoc(m)
}

func read(t registry.Runner, d deployment.Deployment) (doc, string, bool, error) {
	content, found, err := t.ReadFile(Path(d))
	if err != nil {
		return doc{}, "", false, fmt.Errorf("%s: reading %s: %w", t.Describe(), Path(d), err)
	}
	if !found {
		return newDoc(), "", false, nil
	}
	m, err := parseDoc(content)
	if err != nil {
		return doc{}, "", false, fmt.Errorf("%s: %s is %w: %v", t.Describe(), Path(d), ErrMalformed, err)
	}
	return m, content, true, nil
}

// ErrNoRecord is a gateway that answered with no deployment record.
var ErrNoRecord = errors.New("it has no deployment record, which its next apply writes")

// ErrChanged is a write refused because another command changed the record
// between the read and the write.
var ErrChanged = errors.New(ChangedMarker)

// Gather reads the record on every host, keyed by gateway: the deployed
// names of their merge, how many records were found, and why each other
// gateway gave none, ErrNoRecord for one that answered without one.
func Gather(hosts map[string]registry.Runner, d deployment.Deployment) (Record, int, map[string]error) {
	missing := map[string]error{}
	var found []doc
	for _, gw := range slices.Sorted(maps.Keys(hosts)) {
		m, _, ok, err := read(hosts[gw], d)
		switch {
		case err != nil:
			missing[gw] = err
		case !ok:
			missing[gw] = ErrNoRecord
		default:
			found = append(found, m)
		}
	}
	return view(merge(found...)), len(found), missing
}

// Result is what Update did: whether the deployed names changed, which
// gateways it wrote, and why it missed each one it could not.
type Result struct {
	Changed bool
	Wrote   []string
	Missed  map[string]error
}

// Change is what Update does to the record: names added, names taken out.
type Change struct{ add, forget Record }

// Adding adds names to the record, each with a new tag whether or not it is
// already deployed, so that the add survives a removal it could not see.
func Adding(names Record) Change { return Change{add: names} }

// Forgetting takes names out of the record: a removal of every add of each
// that the record holds.
func Forgetting(names Record) Change { return Change{forget: names} }

func (m *doc) apply(ch Change) {
	for _, kind := range kindNames {
		names := m.kind(kind)
		for _, n := range ch.add.List(kind) {
			if n == "" {
				continue
			}
			e := names[n]
			e.Adds = tagSet(e.Adds, []string{newTag()})
			names[n] = e
		}
		for _, n := range ch.forget.List(kind) {
			if e, ok := names[n]; ok && e.deployed() {
				e.Removed = tagSet(e.Removed, e.Adds)
				names[n] = e
			}
		}
	}
}

// Update changes the record on every gateway at once: it reads each one that
// answers, merges them name by name, makes ch's change, and writes the result
// to every gateway that answered whose record differs. When every gateway
// answered, the result is compacted first. A change that leaves the record as
// it was still writes a gateway whose record differs from the merge: that is
// how one that missed changes is brought up to date, and how what it alone
// knew reaches the others. A gateway that does not answer, or whose write is
// refused, is in Missed, ErrChanged for one another command changed
// meanwhile; a malformed record stops it before anything is written, since
// what that record held is unknown.
func Update(hosts map[string]registry.Runner, d deployment.Deployment, ch Change, now time.Time) (Result, error) {
	res := Result{Missed: map[string]error{}}
	type seen struct {
		m     doc
		raw   string
		found bool
	}
	got := map[string]seen{}
	var found []doc
	for _, gw := range slices.Sorted(maps.Keys(hosts)) {
		m, raw, ok, err := read(hosts[gw], d)
		switch {
		case errors.Is(err, ErrMalformed):
			return res, err
		case err != nil:
			res.Missed[gw] = err
			continue
		}
		got[gw] = seen{m, raw, ok}
		if ok {
			found = append(found, m)
		}
	}
	base := merge(found...)
	target := merge(base)
	target.apply(ch)
	if len(found) == 0 && encodeDoc(target) == encodeDoc(base) {
		return res, nil
	}
	res.Changed = !sameNames(view(target), view(base))
	if encodeDoc(target) != encodeDoc(base) {
		target.UpdatedAt = now.UTC().Format(time.RFC3339)
	}
	if len(res.Missed) == 0 {
		target = compact(target)
	}
	want := encodeDoc(target)
	for _, gw := range slices.Sorted(maps.Keys(got)) {
		s := got[gw]
		if s.found && encodeDoc(s.m) == want {
			continue
		}
		if err := write(hosts[gw], d, s.raw, s.found, want); err != nil {
			res.Missed[gw] = err
			continue
		}
		res.Wrote = append(res.Wrote, gw)
	}
	return res, nil
}

func sameNames(a, b Record) bool {
	return slices.Equal(a.Sites, b.Sites) && slices.Equal(a.Apps, b.Apps) && slices.Equal(a.PocketIDGroups, b.PocketIDGroups)
}

// write replaces the record with content, under the registry's lock, only if
// the file still hashes to what was read: a record another command changed
// meanwhile is refused rather than overwritten.
func write(t registry.Runner, d deployment.Deployment, raw string, found bool, content string) error {
	want := "none"
	if found {
		sum := sha256.Sum256([]byte(raw))
		want = hex.EncodeToString(sum[:])
	}
	out, err := t.Run(writeCommand(d, want, content))
	if err != nil {
		if strings.Contains(out, ChangedMarker) {
			return fmt.Errorf("%s: %w", t.Describe(), ErrChanged)
		}
		return fmt.Errorf("writing %s: %w", Path(d), err)
	}
	return nil
}

func writeCommand(d deployment.Deployment, want, content string) string {
	return strings.Join([]string{
		"set -e",
		"umask 077",
		"mkdir -p " + registry.Dir,
		"chmod 700 " + registry.Dir,
		"exec 9>>" + registry.LockPath,
		fmt.Sprintf("flock -w 60 9 || { echo 'paisans: another command holds %s'; exit 1; }", registry.LockPath),
		"f=" + quote(Path(d)),
		"cur=none",
		`if [ -f "$f" ]; then cur=$(sha256sum "$f" | cut -d' ' -f1); fi`,
		`[ "$cur" = ` + quote(want) + ` ] || { echo ` + quote(ChangedMarker) + `; exit 1; }`,
		"tmp=$(mktemp " + registry.Dir + "/.deployed.XXXXXX)",
		`trap 'rm -f "$tmp"' EXIT`,
		"printf %s " + quote(base64.StdEncoding.EncodeToString([]byte(content))) + ` | base64 -d > "$tmp"`,
		`chmod 600 "$tmp"`,
		`mv "$tmp" "$f"`,
	}, "; ")
}

// RemoveCommand deletes the record, for cleaning a gateway host.
func RemoveCommand(d deployment.Deployment) string { return "rm -f -- " + quote(Path(d)) }

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

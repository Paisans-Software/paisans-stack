// Package deployrecord is the record each gateway keeps of what the
// deployment has deployed: its sites, apps and Pocket ID groups, by name.
// apply adds to it and only the removal commands take from it, so a
// paisans.yaml edited by mistake cannot shrink it, and `secrets prune` removes
// only what the yaml does not declare and no record lists
// (docs/specs/2026-10-09-deployment-record.md).
package deployrecord

import (
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
	// Revision orders records: every change raises it by one, and the
	// highest is the newest.
	Revision int `json:"revision"`
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

// Union is every name any of rs lists.
func Union(rs ...Record) Record {
	var out Record
	for _, r := range rs {
		out.Sites = append(out.Sites, r.Sites...)
		out.Apps = append(out.Apps, r.Apps...)
		out.PocketIDGroups = append(out.PocketIDGroups, r.PocketIDGroups...)
	}
	return normal(out)
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
	return Record{Version: Version, Revision: r.Revision, UpdatedAt: r.UpdatedAt, Sites: clean(r.Sites), Apps: clean(r.Apps), PocketIDGroups: clean(r.PocketIDGroups)}
}

// entry is one name's history: the revision of the change that last added it
// and of the one that last removed it.
type entry struct {
	Added   int `json:"added,omitempty"`
	Removed int `json:"removed,omitempty"`
}

// deployed is a name added at least as late as it was removed: an add and a
// removal at the same revision, by two writers at once, keep it.
func (e entry) deployed() bool { return e.Added > 0 && e.Added >= e.Removed }

// doc is the record as stored: every name the deployment ever recorded, with
// its last add and removal. A removed name stays, so that a gateway that
// missed the removal cannot bring it back.
type doc struct {
	Version        int              `json:"version"`
	Revision       int              `json:"revision"`
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
	r := Record{Revision: m.Revision, UpdatedAt: m.UpdatedAt}
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

// merge is, for every name, the latest add and the latest removal any of ds
// holds: no add one record alone saw, and no removal one alone saw, is lost.
func merge(ds ...doc) doc {
	out := newDoc()
	for _, m := range ds {
		out.Revision = max(out.Revision, m.Revision)
		out.UpdatedAt = max(out.UpdatedAt, m.UpdatedAt)
		for _, kind := range kindNames {
			into := out.kind(kind)
			for name, e := range m.kind(kind) {
				cur := into[name]
				into[name] = entry{Added: max(cur.Added, e.Added), Removed: max(cur.Removed, e.Removed)}
			}
		}
	}
	return out
}

func encodeDoc(m doc) string {
	m.Version = Version
	for _, kind := range kindNames {
		if m.kind(kind) == nil {
			switch kind {
			case "sites":
				m.Sites = map[string]entry{}
			case "apps":
				m.Apps = map[string]entry{}
			default:
				m.PocketIDGroups = map[string]entry{}
			}
		}
	}
	data, _ := json.Marshal(m)
	return string(data) + "\n"
}

func parseDoc(content string) (doc, error) {
	m := newDoc()
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return doc{}, err
	}
	if m.Version != Version {
		return doc{}, fmt.Errorf("version %d, and this toolkit reads version %d", m.Version, Version)
	}
	return merge(m), nil
}

// Encode is a stored record in which every name of r was added at r's
// revision, 1 when it has none. It is for tests: everything that writes a
// gateway goes through Update.
func Encode(r Record) string {
	rev := max(r.Revision, 1)
	m := newDoc()
	m.Revision = rev
	for _, kind := range kindNames {
		for _, name := range r.List(kind) {
			m.kind(kind)[name] = entry{Added: rev}
		}
	}
	return encodeDoc(m)
}

// Read reads the record on the gateway t reaches: its deployed names. found
// is false for a gateway with none, which is not an error.
func Read(t registry.Runner, d deployment.Deployment) (Record, bool, error) {
	m, _, found, err := read(t, d)
	return view(m), found, err
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

// Result is what Update did: whether the names changed, which gateways it
// wrote, and why it missed each one it could not.
type Result struct {
	Changed bool
	Wrote   []string
	Missed  map[string]error
}

// Change is what Update does to the record: names added, names taken out.
type Change struct{ add, forget Record }

// Adding adds names to the record.
func Adding(names Record) Change { return Change{add: names} }

// Forgetting takes names out of the record.
func Forgetting(names Record) Change { return Change{forget: names} }

// apply makes ch's events in m at rev: an add for each name added that is not
// deployed, a removal for each name taken out that is. It reports whether it
// made any.
func (m *doc) apply(ch Change, rev int) bool {
	changed := false
	for _, kind := range kindNames {
		names := m.kind(kind)
		for _, n := range ch.add.List(kind) {
			if e := names[n]; n != "" && !e.deployed() {
				e.Added = rev
				names[n] = e
				changed = true
			}
		}
		for _, n := range ch.forget.List(kind) {
			if e := names[n]; e.deployed() {
				e.Removed = rev
				names[n] = e
				changed = true
			}
		}
	}
	return changed
}

// Update changes the record on every gateway at once: it reads each one that
// answers, merges them name by name, makes ch's events at the next revision,
// and writes the result to every gateway that answered whose record differs.
// With no event to make, nothing is raised, and only a gateway whose record
// differs from the merge is written: that is how one that missed changes is
// brought up to date, and how what it alone knew reaches the others. A
// gateway that does not answer, or whose write is refused, is in Missed; a
// malformed record stops it before anything is written, since what that
// record held is unknown.
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
	target := merge(found...)
	if target.apply(ch, target.Revision+1) {
		target.Revision++
		target.UpdatedAt = now.UTC().Format(time.RFC3339)
		res.Changed = true
	} else if len(found) == 0 {
		return res, nil
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

// Add adds names to the record on one gateway, creating it. It reports
// whether it wrote.
func Add(t registry.Runner, d deployment.Deployment, names Record) (bool, error) {
	return one(t, d, Adding(names))
}

// Forget takes names out of the record on one gateway. A gateway with none
// is left without one.
func Forget(t registry.Runner, d deployment.Deployment, names Record) (bool, error) {
	return one(t, d, Forgetting(names))
}

func one(t registry.Runner, d deployment.Deployment, ch Change) (bool, error) {
	res, err := Update(map[string]registry.Runner{"": t}, d, ch, time.Now())
	if err != nil {
		return false, err
	}
	if err := res.Missed[""]; err != nil {
		return false, err
	}
	return len(res.Wrote) > 0, nil
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
			return fmt.Errorf("%s: %s. Run the command again", t.Describe(), ChangedMarker)
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

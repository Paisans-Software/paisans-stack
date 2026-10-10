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

func minus(r, names Record) Record {
	drop := func(list, gone []string) []string {
		var out []string
		for _, n := range list {
			if !slices.Contains(gone, n) {
				out = append(out, n)
			}
		}
		return out
	}
	return normal(Record{Sites: drop(r.Sites, names.Sites), Apps: drop(r.Apps, names.Apps), PocketIDGroups: drop(r.PocketIDGroups, names.PocketIDGroups)})
}

// normal sorts each list, drops repeats, and makes absent lists empty, so
// that equal records encode the same.
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

func encode(r Record) string {
	data, _ := json.Marshal(normal(r))
	return string(data) + "\n"
}

func parse(content string) (Record, error) {
	var r Record
	if err := json.Unmarshal([]byte(content), &r); err != nil {
		return Record{}, err
	}
	if r.Version != Version {
		return Record{}, fmt.Errorf("version %d, and this toolkit reads version %d", r.Version, Version)
	}
	return normal(r), nil
}

// Read reads the record on the gateway t reaches. found is false for a
// gateway with none, which is not an error.
func Read(t registry.Runner, d deployment.Deployment) (Record, bool, error) {
	r, _, found, err := read(t, d)
	return r, found, err
}

func read(t registry.Runner, d deployment.Deployment) (Record, string, bool, error) {
	content, found, err := t.ReadFile(Path(d))
	if err != nil {
		return Record{}, "", false, fmt.Errorf("%s: reading %s: %w", t.Describe(), Path(d), err)
	}
	if !found {
		return normal(Record{}), "", false, nil
	}
	r, err := parse(content)
	if err != nil {
		return Record{}, "", false, fmt.Errorf("%s: %s is %w: %v", t.Describe(), Path(d), ErrMalformed, err)
	}
	return r, content, true, nil
}

// ErrNoRecord is a gateway that answered with no deployment record.
var ErrNoRecord = errors.New("it has no deployment record, which its next apply writes")

// Newest is the record with the highest revision. Records at that revision
// that disagree, left by two writers at once, are read as their union, which
// keeps secrets rather than deleting them.
func Newest(rs ...Record) Record {
	if len(rs) == 0 {
		return normal(Record{})
	}
	top := rs[0].Revision
	for _, r := range rs {
		top = max(top, r.Revision)
	}
	var at []Record
	updated := ""
	for _, r := range rs {
		if r.Revision == top {
			at = append(at, r)
			updated = max(updated, r.UpdatedAt)
		}
	}
	out := Union(at...)
	out.Revision, out.UpdatedAt = top, updated
	return out
}

// Gather reads the record on every host, keyed by gateway: the newest of
// those found, how many were found, and why each other gateway gave none,
// ErrNoRecord for one that answered without one.
func Gather(hosts map[string]registry.Runner, d deployment.Deployment) (Record, int, map[string]error) {
	missing := map[string]error{}
	var found []Record
	for _, gw := range slices.Sorted(maps.Keys(hosts)) {
		rec, _, ok, err := read(hosts[gw], d)
		switch {
		case err != nil:
			missing[gw] = err
		case !ok:
			missing[gw] = ErrNoRecord
		default:
			found = append(found, rec)
		}
	}
	return Newest(found...), len(found), missing
}

// Result is what Update did: whether the names changed, which gateways it
// wrote, and why it missed each one it could not.
type Result struct {
	Changed bool
	Wrote   []string
	Missed  map[string]error
}

// Adding adds names to a record.
func Adding(names Record) func(Record) Record {
	return func(r Record) Record { return Union(r, names) }
}

// Forgetting takes names out of a record.
func Forgetting(names Record) func(Record) Record {
	return func(r Record) Record { return minus(r, names) }
}

// Update changes the record on every gateway at once: it reads each one that
// answers, applies change to the newest, and writes the result with the
// revision raised to every gateway that answered. When the names come out as
// they were, nothing is raised, and only a gateway holding another record is
// written, which is how one that missed changes is brought up to date. A
// gateway that does not answer, or whose write is refused, is in Missed; a
// malformed record stops it before anything is written, since what that
// record held is unknown.
func Update(hosts map[string]registry.Runner, d deployment.Deployment, change func(Record) Record, now time.Time) (Result, error) {
	res := Result{Missed: map[string]error{}}
	type seen struct {
		rec   Record
		raw   string
		found bool
	}
	read1 := map[string]seen{}
	var found []Record
	for _, gw := range slices.Sorted(maps.Keys(hosts)) {
		rec, raw, ok, err := read(hosts[gw], d)
		switch {
		case errors.Is(err, ErrMalformed):
			return res, err
		case err != nil:
			res.Missed[gw] = err
			continue
		}
		read1[gw] = seen{rec, raw, ok}
		if ok {
			found = append(found, rec)
		}
	}
	base := Newest(found...)
	target := normal(change(base))
	if sameNames(target, base) {
		if len(found) == 0 {
			return res, nil
		}
		target = base
	} else {
		target.Revision = base.Revision + 1
		target.UpdatedAt = now.UTC().Format(time.RFC3339)
		res.Changed = true
	}
	want := encode(target)
	for _, gw := range slices.Sorted(maps.Keys(read1)) {
		s := read1[gw]
		if s.found && encode(s.rec) == want {
			continue
		}
		if err := write(hosts[gw], d, s.raw, s.found, target); err != nil {
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

func one(t registry.Runner, d deployment.Deployment, change func(Record) Record) (bool, error) {
	res, err := Update(map[string]registry.Runner{"": t}, d, change, time.Now())
	if err != nil {
		return false, err
	}
	if err := res.Missed[""]; err != nil {
		return false, err
	}
	return len(res.Wrote) > 0, nil
}

// write replaces the record with next, under the registry's lock, only if
// the file still hashes to what was read: a record another command changed
// meanwhile is refused rather than overwritten.
func write(t registry.Runner, d deployment.Deployment, raw string, found bool, next Record) error {
	want := "none"
	if found {
		sum := sha256.Sum256([]byte(raw))
		want = hex.EncodeToString(sum[:])
	}
	out, err := t.Run(writeCommand(d, want, encode(next)))
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

// Package appremove takes an app that has left paisans.yaml off every host:
// its containers and networks, its left over rendered files, its stack
// directory when nothing else is in it, and its client at Pocket ID. With
// --delete-data it also deletes the app's member data: its database and role
// in the cluster, its Garage bucket and keys, its named volumes and its data
// directories.
//
// Only what is provably this deployment's, and provably this app's, is
// touched. A container or network counts only with this deployment's label
// and the paisans-<token>-<app> compose project; a file only with a manifest
// entry marked left over; a client, database or bucket only by what this
// deployment recorded or created under the app's name (see each Plan type).
// Anything else found is reported as kept, with why.
//
// The gate is the left over mark. A whole apply marks the files of an app
// it no longer renders, so a site whose entries for the app are all marked
// has been applied since the app left the configuration, and on a gateway
// that apply took the app's routes out of the Caddyfile. An entry still
// unmarked means that apply has not happened, and the command refuses.
//
// Build is a pure function of the configuration and what the probes read,
// tested without a host, the way internal/ownership is. Execute runs each
// step from live state, so every step is idempotent and a run stopped part
// way resumes where it stopped when run again.
package appremove

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/ownership"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// infraStack is the infrastructure stack, which every site with Patroni,
// HAProxy, Garage or Caddy runs and which is never an app.
const infraStack = "infra"

// nameShape is what an app name must look like to be removed: one path
// segment, safe in a shell word and a compose project name. Every name this
// command puts into a path or a command passes it first.
var nameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Refusal is why app cannot be removed with this configuration, or nil.
// It is checked before any host is reached.
func Refusal(cfg *config.Config, app string) error {
	switch {
	case !nameShape.MatchString(app) || strings.Contains(app, ".."):
		return fmt.Errorf("app remove: %q is not an app's name. A stack is named by one lowercase word, as it was under apps: in paisans.yaml", app)
	case app == infraStack:
		return fmt.Errorf("app remove: %s is the infrastructure stack (Patroni, HAProxy, Garage and the gateway's Caddy), not an app, and is never removed by this command", app)
	}
	if _, ok := cfg.Apps[app]; ok {
		return fmt.Errorf("app remove: %s is still in paisans.yaml. Take it out of paisans.yaml and run `paisans apply --site <site> --execute` on every site first: that apply stops routing to it and marks its files left over, which is what this command checks", app)
	}
	for _, site := range cfg.SiteNames() {
		stacks, err := render.SiteStacks(cfg, site)
		if err != nil {
			return err
		}
		for _, s := range stacks {
			if s == app {
				return fmt.Errorf("app remove: %s renders a stack named %s, which is not an app this command can remove", site, app)
			}
		}
	}
	return nil
}

// Dir is what a probe found in the app's stack directory.
type Dir struct {
	Path   string
	Exists bool
	// Bytes is its size, Files how many files and links are under it, and
	// Entries its top level entries, sorted.
	Bytes   int64
	Files   int
	Entries []string
}

// SiteState is what one site's probes read.
type SiteState struct {
	Site      string
	Inventory *hostcheck.Inventory
	// Hashes is the SHA256 of each of the app's manifest files as the host
	// holds it now, by manifest path, and absent for a file not there.
	Hashes map[string]string
	Dir    Dir
}

// FileState is what will happen to one left over file.
type FileState int

const (
	// Remove is a file whose content still matches the manifest.
	Remove FileState = iota
	// Edited is a file somebody changed on the host. It is kept, with its
	// entry.
	Edited
	// Gone is an entry whose file is no longer there. The entry is dropped.
	Gone
)

// File is one of the app's manifest entries on a site.
type File struct {
	Entry render.ManifestFile
	State FileState
}

// SitePlan is what happens on one site.
type SitePlan struct {
	Site string
	// Project is the app's compose project here.
	Project string
	// Containers, Networks and Volumes are this deployment's, in Project,
	// by name. Volumes are named volumes, deleted only with --delete-data.
	Containers []string
	Running    int
	Networks   []string
	Volumes    []string
	Files      []File
	Dir        Dir
	// DataEntries are the stack directory's top level entries that hold no
	// manifest file: what the app wrote, bind mounted data and uploads.
	DataEntries []string
}

// Empty reports whether nothing of the app is on the site.
func (s SitePlan) Empty() bool {
	return len(s.Containers) == 0 && len(s.Networks) == 0 && len(s.Volumes) == 0 && len(s.Files) == 0 && !s.Dir.Exists
}

// edited reports whether any file under dir is kept as edited.
func (s SitePlan) edited(dir string) bool {
	for _, f := range s.Files {
		if f.State == Edited && strings.HasPrefix("/"+f.Entry.Path, dir+"/") {
			return true
		}
	}
	return false
}

// ClientState is what Pocket ID holds for the app's client, read from the
// site `oidc client create` calls.
type ClientState struct {
	// Provider is the deployment's pocket-id app, empty when it declares
	// none, and Site where it was asked.
	Provider string
	Site     string
	// RecordedID is oidc_clients.<app>.client_id in the secrets file.
	RecordedID string
	// ByID is the client with RecordedID, ByName the client named after
	// the app. Either is nil when Pocket ID holds none.
	ByID   *pocketid.OIDCClient
	ByName *pocketid.OIDCClient
}

// ClientPlan is the client's fate. A client is this deployment's app's only
// when the secrets file records its ID under oidc_clients.<app> and Pocket ID
// holds a client with that ID and the app's name: `oidc client create` and
// apply name the client after the app and record the ID Pocket ID gave it.
// A client with the name and no recorded ID could be anybody's, so it is
// kept and named for an operator to delete by hand.
type ClientPlan struct {
	Delete *pocketid.OIDCClient
	Kept   []string
	// Provider and Site are where the client is deleted.
	Provider, Site string
}

// DatabaseState is what the cluster's leader holds under the app's name.
type DatabaseState struct {
	// Leader is the site whose Patroni holds the leader key.
	Leader string
	// Name is render.DBIdentifier(app), the role and database apply
	// creates for a clustered app.
	Name string
	// Database is whether a database of that name exists, and Owner its
	// owner. Role is whether a role of that name exists.
	Database bool
	Owner    string
	Role     bool
}

// DatabasePlan is the database and role to drop on the leader. apply
// creates exactly one pair for a clustered app that uses Postgres, a role
// and a database both named render.DBIdentifier(app), the database owned by
// the role, and nothing else in the cluster. So the database is dropped only
// when it has that name and that owner, and the role only when it has that
// name and owns no database still there.
type DatabasePlan struct {
	Leader       string
	Name         string
	DropDatabase bool
	DropRole     bool
	Kept         []string
}

// KeyState is one of the app's recorded S3 keys as Garage holds it.
type KeyState struct {
	ID string
	// Secret is the recorded secret, for listing and emptying a bucket. It
	// is never printed.
	Secret  string
	Absent  bool
	Buckets []string
}

// StorageState is what Garage holds for the app, read on Anchor.
type StorageState struct {
	// Anchor is the site Garage is asked on, and Address its mesh address,
	// where its S3 API listens.
	Anchor  string
	Address string
	Keys    []KeyState
	// Buckets is each bucket a key is authorized on, by the ID prefix
	// `key info` printed.
	Buckets map[string]garage.BucketState
	// Shared are recorded key IDs a declared app also records.
	Shared []string
}

// BucketPlan is one bucket to empty and delete.
type BucketPlan struct {
	Name    string
	Objects int
	// KeyID and secret sign the requests that empty it.
	KeyID  string
	secret string
}

// StoragePlan is the app's buckets and keys to delete. A key is the app's
// when this deployment's secrets file records it under apps.<app>, and a
// bucket is the app's when every key authorized on it is one of those:
// `storage init` creates one bucket per app and grants it the app's key
// alone. A bucket another key reaches is kept, since another app may read
// it.
type StoragePlan struct {
	Anchor  string
	Address string
	Buckets []BucketPlan
	Keys    []string
	Kept    []string
}

// Input is everything the probes read.
type Input struct {
	Sites      []SiteState
	Client     ClientState
	Database   *DatabaseState
	Storage    *StorageState
	DeleteData bool
}

// Plan is the whole removal.
type Plan struct {
	App        string
	Deployment deployment.Deployment
	DeleteData bool
	Sites      []SitePlan
	Client     ClientPlan
	Database   *DatabasePlan
	Storage    *StoragePlan
	// Monitors is every monitor site's uptime stack, applied last so that
	// its monitors.json, rendered without the app, drops the app's checks
	// (see apply.MonitorReseed). Build leaves it nil, since it is planned
	// from the render and the sites rather than from the probes; the caller
	// sets it with PlanMonitors.
	Monitors []*apply.MonitorReseed
}

// PlanMonitors plans the monitors' reseed for the configuration the app has
// left, reading each monitor site and changing none.
func (p *Plan) PlanMonitors(cfg *config.Config, rendered *render.Plan, acmeModule string, transports map[string]apply.Transport) error {
	reseeds, err := apply.PlanMonitorReseeds(cfg, rendered, acmeModule, transports)
	if err != nil {
		return fmt.Errorf("app remove: %w", err)
	}
	p.Monitors = reseeds
	return nil
}

// Empty reports whether nothing of the app was found anywhere, and no
// monitor still checks it.
func (p *Plan) Empty() bool {
	for _, s := range p.Sites {
		if !s.Empty() {
			return false
		}
	}
	for _, m := range p.Monitors {
		if m.Pending() {
			return false
		}
	}
	if p.Client.Delete != nil || len(p.Client.Kept) > 0 {
		return false
	}
	if p.Database != nil && (p.Database.DropDatabase || p.Database.DropRole || len(p.Database.Kept) > 0) {
		return false
	}
	if p.Storage != nil && (len(p.Storage.Buckets) > 0 || len(p.Storage.Keys) > 0 || len(p.Storage.Kept) > 0) {
		return false
	}
	return true
}

// AppFile reports whether a manifest path is one of app's files: under its
// stack directory, or one of its routes in the infrastructure stack's
// snippets. A snippet name a declared app's route could also carry is not
// app's, so a declared app's file is never read as the removed one's.
func AppFile(cfg *config.Config, app, rel string) bool {
	d := cfg.Deployment()
	token, stack, _, ok := deployment.SplitRel(rel)
	if !ok || token != d.Token() {
		return false
	}
	if stack == app {
		return true
	}
	dir, file := path.Split(rel)
	if dir != render.SnippetsDir(d)+"/" {
		return false
	}
	name, ok := strings.CutSuffix(file, ".caddy")
	if !ok || !ownership.RouteOf(app, name) {
		return false
	}
	for _, declared := range cfg.AppNames() {
		if ownership.RouteOf(declared, name) {
			return false
		}
	}
	return true
}

// Build decides the removal from what the probes read. It refuses when any
// site still has an entry of the app's that is not marked left over.
func Build(cfg *config.Config, app string, in Input) (*Plan, error) {
	if err := Refusal(cfg, app); err != nil {
		return nil, err
	}
	d := cfg.Deployment()
	p := &Plan{App: app, Deployment: d, DeleteData: in.DeleteData}
	var unmarked []string
	for _, st := range in.Sites {
		sp, err := planSite(cfg, app, st)
		if err != nil {
			return nil, err
		}
		for _, f := range sp.Files {
			if !f.Entry.Leftover {
				unmarked = append(unmarked, fmt.Sprintf("%s: /%s", st.Site, f.Entry.Path))
			}
		}
		p.Sites = append(p.Sites, sp)
	}
	sort.Slice(p.Sites, func(i, j int) bool { return p.Sites[i].Site < p.Sites[j].Site })
	if len(unmarked) > 0 {
		return nil, fmt.Errorf("app remove: %s still has rendered files that no whole apply has marked left over, so an apply may still route to it:\n  %s\nRun `paisans apply --site <site> --execute` on each site named, then run this again. Nothing was changed", app, strings.Join(unmarked, "\n  "))
	}
	p.Client = planClient(app, in.Client)
	if in.DeleteData {
		if in.Database != nil {
			p.Database = planDatabase(cfg, *in.Database)
		}
		if in.Storage != nil {
			p.Storage = planStorage(*in.Storage)
		}
	}
	return p, nil
}

func planSite(cfg *config.Config, app string, st SiteState) (SitePlan, error) {
	d := cfg.Deployment()
	sp := SitePlan{Site: st.Site, Project: d.Project(app), Dir: st.Dir}
	inv := st.Inventory
	if inv == nil {
		return sp, fmt.Errorf("app remove: %s: the host was not read", st.Site)
	}
	if inv.Manifest && inv.ManifestFiles == nil {
		return sp, fmt.Errorf("app remove: %s: %s is not readable as a manifest, so no file there is provably this deployment's. Nothing was changed", st.Site, inv.ManifestPath)
	}
	for _, c := range inv.Containers {
		if c.Deployment == d.ID && c.Project == sp.Project {
			sp.Containers = append(sp.Containers, c.Name)
			if c.PID > 0 {
				sp.Running++
			}
		}
	}
	for _, n := range inv.Networks {
		if n.Deployment == d.ID && n.Project == sp.Project {
			sp.Networks = append(sp.Networks, n.Name)
		}
	}
	for _, v := range inv.Volumes {
		if v.Deployment == d.ID && v.Project == sp.Project && !v.Anonymous {
			sp.Volumes = append(sp.Volumes, v.Name)
		}
	}
	sort.Strings(sp.Containers)
	sort.Strings(sp.Networks)
	sort.Strings(sp.Volumes)

	owned := map[string]bool{}
	for _, e := range inv.ManifestFiles {
		if !AppFile(cfg, app, e.Path) {
			continue
		}
		if strings.ContainsAny(e.Path, "'\n") {
			return sp, fmt.Errorf("app remove: %s: the manifest names %q, which this command will not put in a command line", st.Site, e.Path)
		}
		f := File{Entry: e}
		switch got, ok := st.Hashes[e.Path]; {
		case !ok:
			f.State = Gone
		case got == e.SHA256:
			f.State = Remove
		default:
			f.State = Edited
		}
		sp.Files = append(sp.Files, f)
		if _, stack, rest, ok := deployment.SplitRel(e.Path); ok && stack == app {
			owned[strings.SplitN(rest, "/", 2)[0]] = true
		}
	}
	sort.Slice(sp.Files, func(i, j int) bool { return sp.Files[i].Entry.Path < sp.Files[j].Entry.Path })
	for _, e := range st.Dir.Entries {
		if !owned[e] {
			sp.DataEntries = append(sp.DataEntries, e)
		}
	}
	return sp, nil
}

func planClient(app string, st ClientState) ClientPlan {
	cp := ClientPlan{Provider: st.Provider, Site: st.Site}
	if st.Provider == "" {
		return cp
	}
	byHand := func(c *pocketid.OIDCClient, why string) string {
		return clientKept(app, c.Name, c.ID, st.Provider, why)
	}
	switch {
	case st.ByID != nil && st.ByID.Name == app:
		cp.Delete = st.ByID
	case st.ByID != nil:
		cp.Kept = append(cp.Kept, byHand(st.ByID, fmt.Sprintf("its ID is the one recorded under oidc_clients.%s, and its name is not %s", app, app)))
	}
	if st.ByName != nil && (cp.Delete == nil || st.ByName.ID != cp.Delete.ID) && (st.ByID == nil || st.ByName.ID != st.ByID.ID) {
		why := "no ID is recorded for it under oidc_clients." + app + ", so nothing proves this deployment made it"
		if st.RecordedID != "" {
			why = "its ID is not the one recorded under oidc_clients." + app
		}
		cp.Kept = append(cp.Kept, byHand(st.ByName, why))
	}
	return cp
}

// reservedRoles are the roles the rendered Patroni defines for itself, never
// an app's (internal/apply's database bootstrap refuses the same names).
var reservedRoles = map[string]bool{"postgres": true, "admin": true, "standby": true}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func planDatabase(cfg *config.Config, st DatabaseState) *DatabasePlan {
	dp := &DatabasePlan{Leader: st.Leader, Name: st.Name}
	if !st.Database && !st.Role {
		return dp
	}
	if !identifier.MatchString(st.Name) || len(st.Name) > 63 || reservedRoles[st.Name] || strings.HasPrefix(st.Name, "pg_") {
		dp.Kept = append(dp.Kept, databaseNotApps(st.Name))
		return dp
	}
	for _, declared := range cfg.AppNames() {
		if render.DBIdentifier(declared) == st.Name {
			dp.Kept = append(dp.Kept, databaseDeclared(st.Name, declared))
			return dp
		}
	}
	switch {
	case st.Database && st.Owner == st.Name:
		dp.DropDatabase = true
		dp.DropRole = st.Role
	case st.Database:
		dp.Kept = append(dp.Kept, databaseForeign(st.Name, st.Leader, st.Owner))
		if st.Role {
			dp.Kept = append(dp.Kept, roleKept(st.Name, st.Leader))
		}
	case st.Role:
		dp.DropRole = true
	}
	return dp
}

func planStorage(st StorageState) *StoragePlan {
	sp := &StoragePlan{Anchor: st.Anchor, Address: st.Address}
	shared := map[string]bool{}
	for _, id := range st.Shared {
		shared[id] = true
		sp.Kept = append(sp.Kept, keyDeclared(id))
	}
	ours := map[string]KeyState{}
	for _, k := range st.Keys {
		if !k.Absent && !shared[k.ID] {
			ours[k.ID] = k
		}
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(ours))
	for id := range ours {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, prefix := range ours[id].Buckets {
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			b, ok := st.Buckets[prefix]
			switch {
			case !ok || b.Absent:
				continue
			case !b.Parsed:
				sp.Kept = append(sp.Kept, bucketUnread(prefix))
				continue
			}
			name := strings.Join(b.Aliases, ", ")
			if name == "" {
				name = b.ID
			}
			var foreign []string
			for _, k := range b.Keys {
				if _, mine := ours[k]; !mine {
					foreign = append(foreign, k)
				}
			}
			switch {
			case len(foreign) > 0:
				sp.Kept = append(sp.Kept, bucketGranted(name, foreign))
			case len(b.Aliases) != 1 || b.LocalAliases > 0:
				sp.Kept = append(sp.Kept, bucketAliases(name, len(b.Aliases), b.LocalAliases))
			default:
				sp.Buckets = append(sp.Buckets, BucketPlan{Name: b.Aliases[0], Objects: b.Objects, KeyID: id, secret: ours[id].Secret})
			}
		}
	}
	sort.Slice(sp.Buckets, func(i, j int) bool { return sp.Buckets[i].Name < sp.Buckets[j].Name })
	sp.Keys = ids
	return sp
}

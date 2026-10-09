package appremove

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// composeProjectLabel is the label Compose puts on every container, network
// and volume of a project.
const composeProjectLabel = "com.docker.compose.project"

// Region is the S3 region every rendered Garage answers to
// (templates/garage.toml.tmpl, s3_region).
const Region = "garage"

// ContainersCommand stops and removes the project's containers and then its
// networks, selecting both by this deployment's label and the project's
// label together (Docker ANDs repeated label filters), so an object without
// this deployment's label is never selected, whatever its name. With
// deleteData the containers' anonymous volumes go with them (`docker rm
// -v`); named volumes are VolumesCommand's. It is a no-op once both are gone.
func ContainersCommand(d deployment.Deployment, project string, deleteData bool) string {
	filters := fmt.Sprintf("--filter %s --filter label=%s=%s", quote(d.LabelFilter()), composeProjectLabel, quote(project))
	rm := "docker rm"
	if deleteData {
		rm += " -v"
	}
	return fmt.Sprintf(`set -e; ids=$(docker ps -aq --no-trunc %[1]s); if [ -n "$ids" ]; then docker stop $ids >/dev/null; %[2]s $ids >/dev/null; fi; nets=$(docker network ls -q --no-trunc %[1]s); if [ -n "$nets" ]; then docker network rm $nets >/dev/null; fi`, filters, rm)
}

// VolumesCommand removes the project's named volumes that carry this
// deployment's label.
func VolumesCommand(d deployment.Deployment, project string) string {
	return fmt.Sprintf(`set -e; vols=$(docker volume ls -q %s --filter label=%s=%s); if [ -n "$vols" ]; then docker volume rm $vols >/dev/null; fi`, "--filter "+quote(d.LabelFilter()), composeProjectLabel, quote(project))
}

// FilesCommand removes each file whose content still hashes to what the
// manifest recorded, and prints one line per file: `removed`, `kept` (its
// content differs, so somebody edited it) or `gone`, then the path. The
// hash is checked on the host at the moment of removal.
func FilesCommand(files []File) string {
	var b strings.Builder
	for _, f := range files {
		p := "/" + f.Entry.Path
		fmt.Fprintf(&b, `f=%s; if [ ! -e "$f" ] && [ ! -L "$f" ]; then echo "gone $f"; elif [ -f "$f" ] && [ "$(sha256sum < "$f" | cut -d' ' -f1)" = %s ]; then rm -f -- "$f"; echo "removed $f"; else echo "kept $f"; fi`+"\n", quote(p), quote(f.Entry.SHA256))
	}
	return b.String()
}

// DirCommandFor empties what can go of the stack directory. Without
// deleteData it removes only empty directories, the stack directory last,
// so whatever the app wrote stays. With it, the whole directory goes.
func DirCommandFor(dir string, deleteData bool) string {
	if deleteData {
		return "rm -rf -- " + quote(dir)
	}
	return fmt.Sprintf(`d=%s; if [ -d "$d" ]; then find "$d" -mindepth 1 -depth -type d -empty -delete; rmdir "$d" 2>/dev/null || true; fi`, quote(dir))
}

// Executor runs a plan.
type Executor struct {
	// Hosts reaches each site by name.
	Hosts map[string]Host
	// Clients is Pocket ID's API at the plan's client site, nil when the
	// plan deletes no client.
	Clients Clients
	// Report receives a section per place the removal acts and a step per
	// thing it does there. Nil discards it.
	Report ui.Reporter
	// Kept collects what a step found had to stay, for the report.
	Kept []string

	// open is the step being done, ended when the next starts or the run does.
	open ui.Step
}

func (e *Executor) reporter() ui.Reporter {
	if e.Report == nil {
		return ui.Discard
	}
	return e.Report
}

// work starts the step now being done and ends the one before it, so a
// failure marks the step that failed.
func (e *Executor) work(title string) ui.Step {
	e.idle()
	e.open = e.reporter().Step(title)
	return e.open
}

// idle ends the open step as done.
func (e *Executor) idle() {
	if e.open != nil {
		e.open.Done("")
		e.open = nil
	}
}

// section starts a block of steps, after the step before it has ended.
func (e *Executor) section(format string, args ...any) {
	e.idle()
	e.reporter().Section(fmt.Sprintf(format, args...))
}

// Execute runs every step in order: on each site its containers and
// networks, its files and their manifest entries, its directory and, with
// --delete-data, its named volumes; then the client at Pocket ID; then,
// with --delete-data, the database and the Garage buckets and keys; then
// each monitor's reseed, last, so that a failure there stops nothing of the
// removal and names the apply that finishes it. Each step reads live state
// and is a no-op once done, and the first failure stops the run, so running
// it again resumes.
func (e *Executor) Execute(p *Plan) error {
	err := e.execute(p)
	if err != nil && e.open != nil {
		e.open.Fail(err)
		e.open = nil
	}
	e.idle()
	return err
}

func (e *Executor) execute(p *Plan) error {
	d := p.Deployment
	for _, s := range p.Sites {
		if s.Empty() {
			continue
		}
		h := e.Hosts[s.Site]
		if h == nil {
			return fmt.Errorf("%s: no way to reach the host", s.Site)
		}
		e.section("%s", s.Site)
		e.work("remove containers and networks").Detail("containers and networks of %s", s.Project)
		if out, err := h.Run(ContainersCommand(d, s.Project, p.DeleteData)); err != nil {
			return fmt.Errorf("%s: stopping and removing %s: %w: %s", s.Site, s.Project, err, firstLine(out))
		}
		if err := e.files(h, d, s); err != nil {
			return fmt.Errorf("%s: %w", s.Site, err)
		}
		dir := d.Dir(p.App)
		deleteDir := p.DeleteData && !s.edited(dir)
		if deleteDir {
			e.work("delete " + dir)
		} else {
			e.work("remove empty directories")
		}
		if out, err := h.Run(DirCommandFor(dir, deleteDir)); err != nil {
			return fmt.Errorf("%s: %s: %w: %s", s.Site, dir, err, firstLine(out))
		}
		if p.DeleteData && !deleteDir {
			e.Kept = append(e.Kept, fmt.Sprintf("%s: %s and its data, because a file in it was edited on the host", s.Site, dir))
		}
		if dd, err := probeDir(h, dir); err != nil {
			return fmt.Errorf("%s: %w", s.Site, err)
		} else if dd.Exists {
			e.Kept = append(e.Kept, fmt.Sprintf("%s: %s, %d file(s), %s, not written by apply", s.Site, dir, dd.Files, size(dd.Bytes)))
		}
		if p.DeleteData {
			e.work("delete volumes").Detail("volumes %s", strings.Join(s.Volumes, ", "))
			if out, err := h.Run(VolumesCommand(d, s.Project)); err != nil {
				return fmt.Errorf("%s: removing %s's volumes: %w: %s", s.Site, s.Project, err, firstLine(out))
			}
		}
	}
	if c := p.Client.Delete; c != nil {
		e.section("Pocket ID (%s)", p.Client.Provider)
		e.work("delete client "+c.Name).Detail("client %s (id %s) at %s", c.Name, c.ID, p.Client.Provider)
		if e.Clients == nil {
			return fmt.Errorf("no way to reach Pocket ID to delete client %s", c.ID)
		}
		if err := e.Clients.DeleteOIDCClient(c.ID); err != nil {
			return fmt.Errorf("deleting client %s (id %s) at %s: %w", c.Name, c.ID, p.Client.Provider, err)
		}
	}
	if db := p.Database; db != nil && (db.DropDatabase || db.DropRole) {
		e.section("Postgres (the leader, %s)", db.Leader)
		if err := e.database(d, db); err != nil {
			return err
		}
	}
	if st := p.Storage; st != nil && (len(st.Buckets) > 0 || len(st.Keys) > 0) {
		e.section("Garage (on %s)", st.Anchor)
		if err := e.storage(d, st); err != nil {
			return err
		}
	}
	// The monitor's reseed is not a step of the removal above, so the last of
	// those ends first: a failure here must not mark an unrelated line.
	e.idle()
	for _, m := range p.Monitors {
		if m.Pending() {
			e.section("%s (the monitor)", m.Site)
			e.work("apply "+m.App+" on "+m.Site).Detail("%s on %s, on the seed rendered without %s", m.App, m.Site, p.App)
		}
		if err := m.Execute(); err != nil {
			return &MonitorError{Err: err, Monitors: p.Monitors}
		}
		if err := m.Gate(); err != nil {
			return &MonitorError{Err: err, Monitors: p.Monitors}
		}
	}
	return nil
}

// MonitorError is a monitor's reseed failing, after everything of the app's
// is gone. It names the apply that does the same, since the app itself is
// removed and a run of this command again finds nothing but the monitor.
type MonitorError struct {
	Err      error
	Monitors []*apply.MonitorReseed
}

func (e *MonitorError) Error() string {
	return fmt.Sprintf("reseeding the monitor: %v\nThe app is removed, and only the monitor is out of date. Fix the cause and run %s, which does the same as this step", e.Err, apply.MonitorCommands(e.Monitors))
}

func (e *MonitorError) Unwrap() error { return e.Err }

// files removes the app's unedited files on one site and then drops the
// entries of every file removed or already gone, keeping every other entry.
// The manifest is read again just before it is written, and written the way
// apply writes it: whole, through a temporary file moved into place, 0600.
func (e *Executor) files(h Host, d deployment.Deployment, s SitePlan) error {
	if len(s.Files) == 0 {
		return nil
	}
	e.work("remove files")
	out, err := h.Run(FilesCommand(s.Files))
	if err != nil {
		return fmt.Errorf("removing files: %w: %s", err, firstLine(out))
	}
	drop := map[string]bool{}
	answered := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		verb, p, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		rel := strings.TrimPrefix(p, "/")
		answered[rel] = true
		switch verb {
		case "removed", "gone":
			drop[rel] = true
			if verb == "removed" {
				e.open.Detail("%s", p)
			}
		case "kept":
			e.Kept = append(e.Kept, fmt.Sprintf("%s: an edited file kept; delete it by hand once nothing needs it, then run this again. It is %s, edited on the host since apply wrote it, so it stays with its manifest entry", s.Site, p))
		}
	}
	for _, f := range s.Files {
		if !answered[f.Entry.Path] {
			return fmt.Errorf("removing files: no answer for /%s, so the manifest is left as it was", f.Entry.Path)
		}
	}
	if len(drop) == 0 {
		return nil
	}
	content, found, err := h.ReadFile(d.Manifest())
	if err != nil {
		return fmt.Errorf("reading %s: %w", d.Manifest(), err)
	}
	if !found {
		return nil
	}
	var m render.Manifest
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return fmt.Errorf("%s is not readable as a manifest, so it was not rewritten: %w", d.Manifest(), err)
	}
	kept := m.Files[:0]
	for _, entry := range m.Files {
		if !drop[entry.Path] {
			kept = append(kept, entry)
		}
	}
	m.Files = kept
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := h.WriteFile(d.Manifest(), string(data)+"\n", 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", d.Manifest(), err)
	}
	return nil
}

// DropSQL drops the database and then the role. DROP DATABASE ... WITH
// (FORCE) ends the sessions still connected to it (PostgreSQL 13 and later,
// "DROP DATABASE"), which a stopped app leaves none of, but a replica of
// the app on another site might. Both are IF EXISTS, so a re-run is a no-op.
func DropSQL(db *DatabasePlan) string {
	var b strings.Builder
	if db.DropDatabase {
		fmt.Fprintf(&b, "DROP DATABASE IF EXISTS %s WITH (FORCE);\n", quoteIdent(db.Name))
	}
	if db.DropRole {
		fmt.Fprintf(&b, "DROP ROLE IF EXISTS %s;\n", quoteIdent(db.Name))
	}
	return b.String()
}

func (e *Executor) database(d deployment.Deployment, db *DatabasePlan) error {
	h := e.Hosts[db.Leader]
	if h == nil {
		return fmt.Errorf("%s: no way to reach Patroni's leader", db.Leader)
	}
	e.work("drop database "+db.Name).Detail("database and role %s", db.Name)
	if out, err := h.RunInput(psql(d), DropSQL(db)); err != nil {
		return fmt.Errorf("%s: dropping %s: %w: %s", db.Leader, db.Name, err, strings.TrimSpace(out))
	}
	return nil
}

// maxPages bounds how many listings emptying one bucket may take, at a
// thousand objects a page.
const maxPages = 100000

func (e *Executor) storage(d deployment.Deployment, st *StoragePlan) error {
	h := e.Hosts[st.Anchor]
	if h == nil {
		return fmt.Errorf("%s: no way to reach Garage", st.Anchor)
	}
	for _, b := range st.Buckets {
		e.work("delete bucket " + b.Name)
		if err := e.empty(h, st.Address, b); err != nil {
			return fmt.Errorf("%s: emptying bucket %s: %w", st.Anchor, b.Name, err)
		}
		state, err := garage.ReadBucketState(h, d, b.Name)
		if err != nil {
			return err
		}
		if !state.Absent {
			if err := garage.Execute(&garage.Plan{Steps: []garage.Step{garage.DeleteBucketStep(d, b.Name)}}, h); err != nil {
				return fmt.Errorf("%s: %w", st.Anchor, err)
			}
		}
	}
	for _, id := range st.Keys {
		e.work("delete S3 key " + id)
		kb, err := garage.ReadKeyBuckets(h, d, id)
		if err != nil {
			return err
		}
		if !kb.Absent {
			if err := garage.Execute(&garage.Plan{Steps: []garage.Step{garage.DeleteKeyStep(d, id)}}, h); err != nil {
				return fmt.Errorf("%s: %w", st.Anchor, err)
			}
		}
	}
	return nil
}

// empty deletes every object in a bucket through Garage's S3 API with the
// app's own key, a page of up to a thousand at a time, until a listing
// comes back empty. A page that comes back unchanged after its deletes is
// an error rather than a loop.
func (e *Executor) empty(h Host, address string, b BucketPlan) error {
	req := garage.S3Request{Address: address, Bucket: b.Name, KeyID: b.KeyID, Secret: b.secret, Region: Region,
		Query: url.Values{"list-type": {"2"}, "max-keys": {"1000"}}}
	if b.secret == "" {
		return fmt.Errorf("no secret is recorded for key %s, so the bucket cannot be listed", b.KeyID)
	}
	last := ""
	for i := 0; i < maxPages; i++ {
		out, err := h.RunInput(garage.CurlStdin, req.ListConfig())
		if err != nil {
			return fmt.Errorf("listing: %s", req.Redact(fmt.Sprintf("%v: %s", err, firstLine(out))))
		}
		page, err := garage.ParseListPage(out)
		if err != nil {
			return err
		}
		if len(page.Keys) == 0 {
			return nil
		}
		if page.Keys[0] == last {
			return fmt.Errorf("object %q is still listed after its delete", last)
		}
		last = page.Keys[0]
		if out, err := h.RunInput(garage.CurlStdin, req.DeleteConfig(page.Keys)); err != nil {
			return fmt.Errorf("deleting objects: %s", req.Redact(fmt.Sprintf("%v: %s", err, firstLine(out))))
		}
	}
	return fmt.Errorf("still not empty after %d pages", maxPages)
}

func size(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

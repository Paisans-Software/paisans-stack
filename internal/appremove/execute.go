package appremove

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/garage"
	"github.com/paisans-software/paisans-stack/internal/render"
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
	// Progress receives one line per step done.
	Progress io.Writer
	// Kept collects what a step found had to stay, for the report.
	Kept []string
}

func (e *Executor) say(format string, args ...any) {
	if e.Progress != nil {
		fmt.Fprintf(e.Progress, format+"\n", args...)
	}
}

// Execute runs every step in order: on each site its containers and
// networks, its files and their manifest entries, its directory and, with
// --delete-data, its named volumes; then the client at Pocket ID; then,
// with --delete-data, the database and the Garage buckets and keys. Each
// step reads live state and is a no-op once done, and the first failure
// stops the run, so running it again resumes.
func (e *Executor) Execute(p *Plan) error {
	d := p.Deployment
	for _, s := range p.Sites {
		if s.Empty() {
			continue
		}
		h := e.Hosts[s.Site]
		if h == nil {
			return fmt.Errorf("%s: no way to reach the host", s.Site)
		}
		if out, err := h.Run(ContainersCommand(d, s.Project, p.DeleteData)); err != nil {
			return fmt.Errorf("%s: stopping and removing %s: %w: %s", s.Site, s.Project, err, firstLine(out))
		}
		e.say("  %-9s %s: containers and networks of %s", "removed", s.Site, s.Project)
		if err := e.files(h, d, s); err != nil {
			return fmt.Errorf("%s: %w", s.Site, err)
		}
		dir := d.Dir(p.App)
		deleteDir := p.DeleteData && !s.edited(dir)
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
		} else {
			e.say("  %-9s %s: %s", "removed", s.Site, dir)
		}
		if p.DeleteData {
			if out, err := h.Run(VolumesCommand(d, s.Project)); err != nil {
				return fmt.Errorf("%s: removing %s's volumes: %w: %s", s.Site, s.Project, err, firstLine(out))
			}
			if len(s.Volumes) > 0 {
				e.say("  %-9s %s: volumes %s", "removed", s.Site, strings.Join(s.Volumes, ", "))
			}
		}
	}
	if c := p.Client.Delete; c != nil {
		if e.Clients == nil {
			return fmt.Errorf("no way to reach Pocket ID to delete client %s", c.ID)
		}
		if err := e.Clients.DeleteOIDCClient(c.ID); err != nil {
			return fmt.Errorf("deleting client %s (id %s) at %s: %w", c.Name, c.ID, p.Client.Provider, err)
		}
		e.say("  %-9s client %s (id %s) at %s", "deleted", c.Name, c.ID, p.Client.Provider)
	}
	if db := p.Database; db != nil && (db.DropDatabase || db.DropRole) {
		if err := e.database(d, db); err != nil {
			return err
		}
	}
	if st := p.Storage; st != nil && (len(st.Buckets) > 0 || len(st.Keys) > 0) {
		if err := e.storage(d, st); err != nil {
			return err
		}
	}
	return nil
}

// files removes the app's unedited files on one site and then drops the
// entries of every file removed or already gone, keeping every other entry.
// The manifest is read again just before it is written, and written the way
// apply writes it: whole, through a temporary file moved into place, 0600.
func (e *Executor) files(h Host, d deployment.Deployment, s SitePlan) error {
	if len(s.Files) == 0 {
		return nil
	}
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
				e.say("  %-9s %s: %s", "removed", s.Site, p)
			}
		case "kept":
			e.Kept = append(e.Kept, fmt.Sprintf("%s: %s, edited on the host since apply wrote it, kept with its manifest entry. Delete it by hand once nothing needs it, and run this again", s.Site, p))
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
	if out, err := h.RunInput(psql(d), DropSQL(db)); err != nil {
		return fmt.Errorf("%s: dropping %s: %w: %s", db.Leader, db.Name, err, strings.TrimSpace(out))
	}
	e.say("  %-9s %s: database and role %s", "dropped", db.Leader, db.Name)
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
		e.say("  %-9s %s: bucket %s", "deleted", st.Anchor, b.Name)
	}
	for _, id := range st.Keys {
		kb, err := garage.ReadKeyBuckets(h, d, id)
		if err != nil {
			return err
		}
		if !kb.Absent {
			if err := garage.Execute(&garage.Plan{Steps: []garage.Step{garage.DeleteKeyStep(d, id)}}, h); err != nil {
				return fmt.Errorf("%s: %w", st.Anchor, err)
			}
		}
		e.say("  %-9s %s: S3 key %s", "deleted", st.Anchor, id)
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

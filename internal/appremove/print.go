package appremove

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// Print writes the plan, one line per thing, each with what happens to it.
func (p *Plan) Print(w io.Writer) {
	line := func(verb, format string, args ...any) {
		fmt.Fprintf(w, "  %-9s %s\n", verb, fmt.Sprintf(format, args...))
	}
	for _, s := range p.Sites {
		if s.Empty() {
			fmt.Fprintf(w, "%s: nothing of %s\n", s.Site, p.App)
			continue
		}
		fmt.Fprintf(w, "%s\n", s.Site)
		if len(s.Containers) > 0 {
			line("stop", "%s: %d container(s), %d running: %s", s.Project, len(s.Containers), s.Running, strings.Join(s.Containers, ", "))
		}
		for _, n := range s.Networks {
			line("remove", "network %s", n)
		}
		for _, f := range s.Files {
			switch f.State {
			case Remove:
				line("remove", "/%s", f.Entry.Path)
			case Gone:
				line("drop", "the manifest entry for /%s, whose file is gone", f.Entry.Path)
			case Edited:
				line("keep", "/%s and its manifest entry: edited on the host since apply wrote it", f.Entry.Path)
			}
		}
		dir := s.Dir
		switch {
		case !dir.Exists:
		case p.DeleteData && s.edited(dir.Path):
			line("keep", "%s and everything in it, because a file in it was edited on the host", dir.Path)
		case p.DeleteData:
			line("delete", "%s and everything in it: %d file(s), %s", dir.Path, dir.Files, size(dir.Bytes))
		case len(s.DataEntries) > 0:
			line("keep", "%s: %s, written by the app rather than by apply (%d file(s) and %s in the directory). --delete-data deletes it", dir.Path, strings.Join(s.DataEntries, ", "), dir.Files, size(dir.Bytes))
		default:
			line("remove", "%s once it is empty", dir.Path)
		}
		for _, v := range s.Volumes {
			if p.DeleteData {
				line("delete", "volume %s", v)
			} else {
				line("keep", "volume %s. --delete-data deletes it", v)
			}
		}
	}
	c := p.Client
	if c.Delete != nil || len(c.Kept) > 0 {
		fmt.Fprintf(w, "Pocket ID (%s on %s)\n", c.Provider, c.Site)
		if c.Delete != nil {
			line("delete", "client %s (id %s): DELETE /api/oidc/clients/%s", c.Delete.Name, c.Delete.ID, c.Delete.ID)
		}
		for _, k := range c.Kept {
			line("keep", "%s", k)
		}
	}
	if db := p.Database; db != nil && (db.DropDatabase || db.DropRole || len(db.Kept) > 0) {
		fmt.Fprintf(w, "Postgres (the leader, %s)\n", db.Leader)
		if db.DropDatabase {
			line("drop", "database %s", db.Name)
		}
		if db.DropRole {
			line("drop", "role %s", db.Name)
		}
		for _, k := range db.Kept {
			line("keep", "%s", k)
		}
	}
	if st := p.Storage; st != nil && (len(st.Buckets) > 0 || len(st.Keys) > 0 || len(st.Kept) > 0) {
		fmt.Fprintf(w, "Garage (on %s)\n", st.Anchor)
		for _, b := range st.Buckets {
			line("delete", "bucket %s and its %d object(s), emptied with key %s first", b.Name, b.Objects, b.KeyID)
		}
		for _, k := range st.Keys {
			line("delete", "S3 key %s", k)
		}
		for _, k := range st.Kept {
			line("keep", "%s", k)
		}
	}
	for _, m := range p.Monitors {
		if !m.Pending() {
			fmt.Fprintf(w, "%s: the monitor's seed already matches the render without %s\n", m.Site, p.App)
			continue
		}
		fmt.Fprintf(w, "%s (the monitor)\n", m.Site)
		for _, s := range m.Steps() {
			line(s.Verb, "%s", s.Text)
		}
	}
}

// Remains is what the operator still has to see to once the plan has run:
// the app's secrets, its DNS records, and what a step kept. Nothing here is
// changed by this command.
func Remains(p *Plan, secrets *config.Secrets, kept []string) []string {
	var out []string
	if secrets != nil {
		var keys []string
		for k := range secrets.Apps[p.App] {
			keys = append(keys, "apps."+p.App+"."+k)
		}
		sort.Strings(keys)
		if _, ok := secrets.OIDCClients[p.App]; ok {
			keys = append(keys, "oidc_clients."+p.App)
		}
		if len(keys) > 0 {
			out = append(out, fmt.Sprintf("secrets: %s are still in the secrets file, which this command never edits. Remove them with sops once nothing needs them", strings.Join(keys, ", ")))
		}
	}
	out = append(out, "DNS: the records dns init made for its hostnames stay until `paisans dns prune --execute` deletes them")
	if !p.DeleteData {
		out = append(out, "data: its database, its Garage bucket and key, its named volumes and what it wrote under its stack directory are kept. `paisans app remove "+p.App+" --delete-data --execute` deletes them")
	}
	out = append(out, p.Client.Kept...)
	if p.Database != nil {
		out = append(out, p.Database.Kept...)
	}
	if p.Storage != nil {
		out = append(out, p.Storage.Kept...)
	}
	return append(out, kept...)
}

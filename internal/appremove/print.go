package appremove

import (
	"fmt"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// Show lists the plan: a section per place it acts, an item per kind of thing
// it does there, and each thing with what happens to it as a detail. Items of
// one kind that follow each other are listed as one.
func (p *Plan) Show(r ui.Reporter) {
	last := ""
	line := func(title, text string, args ...any) {
		if title != last {
			r.Item(title)
			last = title
		}
		r.Detail(text, args...)
	}
	section := func(format string, args ...any) {
		r.Section(fmt.Sprintf(format, args...))
		last = ""
	}
	for _, s := range p.Sites {
		if s.Empty() {
			r.Detail("%s: nothing of %s", s.Site, p.App)
			continue
		}
		section("%s", s.Site)
		if len(s.Containers) > 0 {
			line("remove containers and networks", "stop %s: %d container(s), %d running: %s", s.Project, len(s.Containers), s.Running, strings.Join(s.Containers, ", "))
		}
		for _, n := range s.Networks {
			line("remove containers and networks", "remove network %s", n)
		}
		for _, f := range s.Files {
			switch f.State {
			case Remove:
				line("remove files", "/%s", f.Entry.Path)
			case Gone:
				line("drop manifest entries", "the manifest entry for /%s, whose file is gone", f.Entry.Path)
			case Edited:
				line("keep edited files", "/%s and its manifest entry: edited on the host since apply wrote it", f.Entry.Path)
			}
		}
		dir := s.Dir
		switch {
		case !dir.Exists:
		case p.DeleteData && s.edited(dir.Path):
			line("keep "+dir.Path, "%s and everything in it, because a file in it was edited on the host", dir.Path)
		case p.DeleteData:
			line("delete "+dir.Path, "%s and everything in it: %d file(s), %s", dir.Path, dir.Files, size(dir.Bytes))
		case len(s.DataEntries) > 0:
			line("keep "+dir.Path, "%s: %s, written by the app rather than by apply (%d file(s) and %s in the directory). --delete-data deletes it", dir.Path, strings.Join(s.DataEntries, ", "), dir.Files, size(dir.Bytes))
		default:
			line("remove empty directories", "%s once it is empty", dir.Path)
		}
		for _, v := range s.Volumes {
			if p.DeleteData {
				line("delete volumes", "volume %s", v)
			} else {
				line("keep volumes", "volume %s. --delete-data deletes it", v)
			}
		}
	}
	c := p.Client
	if c.Delete != nil || len(c.Kept) > 0 {
		section("Pocket ID (%s on %s)", c.Provider, c.Site)
		if c.Delete != nil {
			line("delete client "+c.Delete.Name, "client %s (id %s): DELETE /api/oidc/clients/%s", c.Delete.Name, c.Delete.ID, c.Delete.ID)
		}
		for _, k := range c.Kept {
			line("keep client", "%s", k)
		}
	}
	if db := p.Database; db != nil && (db.DropDatabase || db.DropRole || len(db.Kept) > 0) {
		section("Postgres (the leader, %s)", db.Leader)
		if db.DropDatabase {
			line("drop database "+db.Name, "database %s", db.Name)
		}
		if db.DropRole {
			line("drop role "+db.Name, "role %s", db.Name)
		}
		for _, k := range db.Kept {
			line("keep database", "%s", k)
		}
	}
	if st := p.Storage; st != nil && (len(st.Buckets) > 0 || len(st.Keys) > 0 || len(st.Kept) > 0) {
		section("Garage (on %s)", st.Anchor)
		for _, b := range st.Buckets {
			line("delete bucket "+b.Name, "bucket %s and its %d object(s), emptied with key %s first", b.Name, b.Objects, b.KeyID)
		}
		for _, k := range st.Keys {
			line("delete S3 key "+k, "S3 key %s", k)
		}
		for _, k := range st.Kept {
			line("keep buckets and keys", "%s", k)
		}
	}
	for _, m := range p.Monitors {
		if !m.Pending() {
			r.Detail("%s: the monitor's seed already matches the render without %s", m.Site, p.App)
			continue
		}
		section("%s (the monitor)", m.Site)
		for _, s := range m.Steps() {
			line("apply "+m.App+" on "+m.Site, "%s", s.Text)
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
			out = append(out, fmt.Sprintf("secrets: remove %s from the secrets file with sops. This command never edits it, so remove them once nothing needs them", strings.Join(keys, ", ")))
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

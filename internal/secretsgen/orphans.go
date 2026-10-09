package secretsgen

import (
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Orphan is a secret naming something the configuration no longer declares:
// a site or an app taken out of paisans.yaml, or a Pocket ID group no
// signup_default_groups names. Nothing reads it any more. It is reported by
// key and never by value, and only `paisans secrets prune --execute` removes
// it.
type Orphan struct {
	Key string
	// Why is what it names that is gone, for the report.
	Why string
	// Leaves is what removing it leaves elsewhere, empty when nothing.
	Leaves string
}

// Orphans is every orphaned secret, sorted by key.
func Orphans(cfg *config.Config, secrets *config.Secrets) []Orphan {
	var out []Orphan
	for name := range secrets.Sites {
		if _, ok := cfg.Sites[name]; !ok {
			out = append(out, Orphan{Key: "sites." + name, Why: "names a site paisans.yaml does not declare"})
		}
	}
	for name := range secrets.Apps {
		if _, ok := cfg.Apps[name]; !ok {
			out = append(out, Orphan{Key: "apps." + name, Why: "names an app paisans.yaml does not declare"})
		}
	}
	for name := range secrets.OIDCClients {
		if _, ok := cfg.Apps[name]; !ok {
			out = append(out, Orphan{Key: "oidc_clients." + name, Why: "names an app paisans.yaml does not declare",
				Leaves: name + "'s sign-in client still exists at Pocket ID, if that Pocket ID still runs; delete it there"})
		}
	}
	named := map[string]bool{}
	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Kind == config.KindPocketID {
			for _, g := range kinds.PocketIDSignupGroups(cfg.Apps[name].Settings) {
				named[g] = true
			}
		}
	}
	for group := range secrets.PocketIDGroups {
		if !named[group] {
			out = append(out, Orphan{Key: "pocket_id_groups." + group, Why: "names a group no signup_default_groups names"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Prune removes each orphan from secrets. The caller writes the file.
func Prune(secrets *config.Secrets, orphans []Orphan) {
	for _, o := range orphans {
		kind, name, _ := strings.Cut(o.Key, ".")
		switch kind {
		case "sites":
			delete(secrets.Sites, name)
		case "apps":
			delete(secrets.Apps, name)
		case "oidc_clients":
			delete(secrets.OIDCClients, name)
		case "pocket_id_groups":
			delete(secrets.PocketIDGroups, name)
		}
	}
}

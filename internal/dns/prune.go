package dns

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// Prune removes address records the toolkit created and the configuration no
// longer implies, such as the A record for a hostname an app stopped using.
//
// A record is removed only when every one of these holds, and a record the
// toolkit's comment marks but that fails any of them is listed as kept, with
// each reason:
//
//  1. it carries exactly recordComment, so dns init created it;
//  2. it is an A or AAAA record;
//  3. its name is community.domain, a name under it, or a name Desired
//     produces for this configuration;
//  4. its address is one of this deployment's sites' public_address or
//     public_address6, so it pointed at our own host;
//  5. no record of its type is wanted at its name.
//
// The comment alone is not enough because anyone with the zone can write it,
// on a record the toolkit never made. Rules 3 and 4 tie the record to this
// deployment by things a comment cannot forge: where it sits and what it
// points at.
//
// Rule 3 covers every name Desired can produce. Each media hostname, derived
// (<label>-media.<community.domain>) or declared (validate refuses one outside
// community.domain), is under the domain, and so was the single deployment
// wide media hostname that per app media hostnames replaced. App hostnames and
// site endpoints may be declared outside the domain, and those are in scope
// while the configuration still names them. A stale record at a name outside
// the domain that the configuration no longer names is kept: nothing left in
// the configuration says it was ever this deployment's.
//
// Rule 5 compares type and name, not address. A wanted name holding a stale
// address is a conflict for dns init to report, and replacing it would be an
// update, which nothing in this package does.

// PruneAction is what happens to one toolkit record.
type PruneAction string

const (
	// Remove is a record every rule allows deleting.
	Remove PruneAction = "remove"
	// Keep is a record carrying the toolkit's comment that some rule protects.
	Keep PruneAction = "keep"
)

// PruneEntry is one record carrying the toolkit's comment, and its fate.
type PruneEntry struct {
	Record
	Zone   string
	zoneID string
	Action PruneAction
	// Reasons are every rule the record fails, for a kept one.
	Reasons []string
}

// PrunePlan is every record in the deployment's zones that carries the
// toolkit's comment. Records without it are not listed at all: they were
// never the toolkit's to consider.
type PrunePlan struct {
	Provider string
	Entries  []PruneEntry
	zones    []zone
}

// Removes returns the entries an execute will delete.
func (p *PrunePlan) Removes() []PruneEntry {
	var out []PruneEntry
	for _, e := range p.Entries {
		if e.Action == Remove {
			out = append(out, e)
		}
	}
	return out
}

// BuildPrune lists every record in each zone the deployment's names live in
// and decides each toolkit record's fate. It reads and never writes.
//
// wants must be Desired(cfg): it is passed in rather than recomputed so that
// a configuration that cannot name its records is refused before the
// provider is contacted, exactly as for dns init.
func BuildPrune(ctx context.Context, provider Provider, cfg *config.Config, wants []Want) (*PrunePlan, error) {
	domain := normalise(cfg.Community.Domain)
	if domain == "" {
		return nil, fmt.Errorf("dns: community.domain is not set, so there is no subtree to prune within")
	}

	wantedNames := map[string]bool{}
	wanted := map[string][]string{} // type + " " + name -> sources
	for _, w := range wants {
		wantedNames[w.Name] = true
		wanted[w.Type+" "+w.Name] = w.Sources
	}

	addresses := map[string]string{} // canonical address -> site key
	for _, name := range cfg.SiteNames() {
		site := cfg.Sites[name]
		if ip := net.ParseIP(strings.TrimSpace(site.PublicAddress)); ip != nil {
			addresses[ip.String()] = fmt.Sprintf("sites.%s.public_address", name)
		}
		if ip := net.ParseIP(strings.TrimSpace(site.PublicAddress6)); ip != nil {
			addresses[ip.String()] = fmt.Sprintf("sites.%s.public_address6", name)
		}
	}

	// Every zone that holds the domain or a wanted name, once each.
	cache := map[string]zone{}
	seen := map[string]bool{}
	var zones []zone
	names := []string{domain}
	for _, w := range wants {
		names = append(names, w.Name)
	}
	for _, name := range names {
		z, err := FindZone(ctx, provider, name, cache)
		if err != nil {
			return nil, err
		}
		if !seen[z.id] {
			seen[z.id] = true
			zones = append(zones, z)
		}
	}

	plan := &PrunePlan{Provider: provider.Name(), zones: zones}
	for _, z := range zones {
		records, err := provider.AllRecords(ctx, z.id)
		if err != nil {
			return nil, fmt.Errorf("dns: listing zone %s: %w", z.name, err)
		}
		for _, r := range records {
			if r.Comment != recordComment {
				continue // rule 1: not the toolkit's, so not listed
			}
			name := normalise(r.Name)
			typ := strings.ToUpper(r.Type)
			var reasons []string
			if typ != "A" && typ != "AAAA" {
				reasons = append(reasons, fmt.Sprintf("it is a %s record; prune removes only A and AAAA records", typ))
			}
			if name != domain && !strings.HasSuffix(name, "."+domain) && !wantedNames[name] {
				reasons = append(reasons, fmt.Sprintf("%s is outside community.domain (%s) and is not a name this configuration produces, so nothing says it was ever this deployment's", name, domain))
			}
			if ip := net.ParseIP(strings.TrimSpace(r.Content)); ip == nil || addresses[ip.String()] == "" {
				reasons = append(reasons, fmt.Sprintf("%s is not any site's public_address or public_address6, so it does not point at this deployment", r.Content))
			}
			if sources, ok := wanted[typ+" "+name]; ok {
				reasons = append(reasons, fmt.Sprintf("this configuration still wants a record of type %s at this name (%s)", typ, strings.Join(sources, ", ")))
			}
			action := Remove
			if len(reasons) > 0 {
				action = Keep
			}
			plan.Entries = append(plan.Entries, PruneEntry{Record: r, Zone: z.name, zoneID: z.id, Action: action, Reasons: reasons})
		}
	}
	sort.SliceStable(plan.Entries, func(i, j int) bool {
		a, b := plan.Entries[i], plan.Entries[j]
		if a.Action != b.Action {
			return a.Action == Remove
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Type < b.Type
	})
	return plan, nil
}

// ExecutePrune deletes every record the plan removes, then lists each zone
// again and confirms each is gone.
//
// It stops at the first failed delete. Records already deleted stay deleted,
// and a re-run plans from a fresh listing, so a run that failed partway
// resumes rather than starting over.
func ExecutePrune(ctx context.Context, provider Provider, plan *PrunePlan) error {
	removes := plan.Removes()
	for _, e := range removes {
		if e.ID == "" {
			return fmt.Errorf("dns: %s %s has no record id to delete by. Nothing further was deleted", e.Type, e.Name)
		}
		if err := provider.Delete(ctx, e.zoneID, e.ID); err != nil {
			return fmt.Errorf("dns: deleting %s %s (record %s): %w", e.Type, e.Name, e.ID, err)
		}
	}
	remaining := map[string]map[string]bool{} // zone id -> record ids
	for _, e := range removes {
		if _, listed := remaining[e.zoneID]; listed {
			continue
		}
		records, err := provider.AllRecords(ctx, e.zoneID)
		if err != nil {
			return fmt.Errorf("dns: listing zone %s to confirm the deletes: %w", e.Zone, err)
		}
		ids := map[string]bool{}
		for _, r := range records {
			ids[r.ID] = true
		}
		remaining[e.zoneID] = ids
	}
	var still []string
	for _, e := range removes {
		if remaining[e.zoneID][e.ID] {
			still = append(still, fmt.Sprintf("%s %s (record %s)", e.Type, e.Name, e.ID))
		}
	}
	if len(still) > 0 {
		return fmt.Errorf("dns: deleted, but still listed: %s. Check them in the provider's console before re-running", strings.Join(still, ", "))
	}
	return nil
}

// Write prints a prune plan in the shape dns init's plan uses.
func (p *PrunePlan) Write(w io.Writer) {
	fmt.Fprintf(w, "dns prune (%s)\n", p.Provider)
	if len(p.Entries) == 0 {
		fmt.Fprintf(w, "  no record in these zones carries the comment %q\n", recordComment)
		return
	}
	for _, e := range p.Entries {
		fmt.Fprintf(w, "  %-6s %-4s %s -> %s  (zone %s, record %s)\n", e.Action, e.Type, e.Name, e.Content, e.Zone, e.ID)
		for _, reason := range e.Reasons {
			fmt.Fprintf(w, "      %s\n", reason)
		}
	}
}

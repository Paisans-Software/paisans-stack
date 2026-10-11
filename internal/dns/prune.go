package dns

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// Prune removes address records the toolkit created and the configuration no
// longer implies, such as the A record for a hostname an app stopped using.
//
// A record is removed only when every one of these holds, and a record the
// toolkit's comment marks but that fails any of them is listed as kept, with
// each reason:
//
//  1. it carries exactly RecordComment of this deployment, so this
//     deployment's dns init created it;
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
// site remove's DNS stage applies the same rules through BuildSiteRemoval,
// with rule 4's addresses narrowed to the removed site's own and rule 5
// compared against the configuration without it; pruneRules is where the two
// differ, and nowhere else.
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
	// Unscoped is a kept record whose only failed rule is its name's: every
	// other rule would delete it, and only the operator can say it was this
	// deployment's, by vouching for the name or deleting it by hand.
	Unscoped bool
}

// PrunePlan is every record in the deployment's zones that carries the
// toolkit's comment. Records without it are not listed at all: they were
// never the toolkit's to consider.
type PrunePlan struct {
	Provider string
	Entries  []PruneEntry
	zones    []zone
	// comment is this deployment's RecordComment, the one rule 1 matches.
	comment string
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

// pruneRules is what a prune plan is measured against beyond rules 1 and 2,
// which never vary: the comment is this deployment's, and the type is A or
// AAAA. It is the one seam between dns prune and site remove's DNS stage:
// each fills it from its own configurations and hands it to planPrune, and
// both delete through ExecutePrune.
type pruneRules struct {
	// domain is community.domain, normalised: rule 3's subtree.
	domain string
	// names are the names rule 3 admits besides the domain's subtree, and
	// with the domain, the names whose zones are listed.
	names []string
	// vouched are names the operator vouched for with --name, which lift rule
	// 3 for that exact name and nothing else.
	vouched map[string]bool
	// producedBy names the configuration rule 3's names come from.
	producedBy string
	// canVouch says --name exists to lift rule 3, so the reason offers it.
	canVouch bool
	// addresses are rule 4's: canonical address -> where it is declared.
	addresses map[string]string
	// notOurs is rule 4's reason for an address outside the set, after the
	// address itself.
	notOurs string
	// shared are addresses in the set that a remaining site declares too:
	// canonical address -> that site's key. A record at one is kept.
	shared map[string]string
	// wanted are rule 5's: type + " " + name -> the keys that want it.
	wanted map[string][]string
	// wantedBy names the configuration rule 5 compares against.
	wantedBy string
}

// BuildPrune lists every record in each zone the deployment's names live in
// and decides each toolkit record's fate. It reads and never writes.
//
// wants must be Desired(cfg): it is passed in rather than recomputed so that
// a configuration that cannot name its records is refused before the
// provider is contacted, exactly as for dns init.
func BuildPrune(ctx context.Context, provider Provider, cfg *config.Config, wants []Want, vouched ...string) (*PrunePlan, error) {
	// vouched are names the operator named with --name: records at a name the
	// configuration no longer produces and that sits outside
	// community.domain, which only the operator can say were this
	// deployment's (Eg: a media hostname configured explicitly and since
	// dropped). Naming one lifts the scope rule for that exact name and
	// nothing else; the comment, address and not-wanted rules still apply.
	rules := pruneRules{
		domain:     normalise(cfg.Community.Domain),
		vouched:    map[string]bool{},
		producedBy: "this configuration",
		canVouch:   true,
		addresses:  map[string]string{},
		notOurs:    "is not any site's public_address or public_address6, so it does not point at this deployment",
		wanted:     wantedSet(wants),
		wantedBy:   "this configuration",
	}
	if rules.domain == "" {
		return nil, fmt.Errorf("dns: community.domain is not set, so there is no subtree to prune within")
	}
	for _, n := range vouched {
		rules.vouched[normalise(n)] = true
	}
	for _, w := range wants {
		rules.names = append(rules.names, w.Name)
	}
	for _, name := range cfg.SiteNames() {
		for key, ip := range siteAddresses(cfg, name) {
			rules.addresses[ip] = key
		}
	}
	plan, seenVouched, err := planPrune(ctx, provider, cfg, rules)
	if err != nil {
		return nil, err
	}
	for n := range rules.vouched {
		if !seenVouched[n] {
			return nil, fmt.Errorf("dns prune: --name %s matches no record dns init created, so it vouches for nothing; check the spelling", n)
		}
	}
	return plan, nil
}

// BuildSiteRemoval plans site remove's DNS stage: the address records this
// deployment's dns init made for site, which before declares and after, the
// configuration once it is removed, no longer wants. It reads and never
// writes.
//
// It is BuildPrune's planner with three things in place of the
// configuration as it stands: rule 3's names are those Desired produces for
// before; rule 4's addresses are site's own public_address and
// public_address6 in before; and rule 5 compares against Desired(after). An
// address a site in after declares too is kept, since the record may be that
// site's.
//
// When Desired cannot name before's records (Eg: two gateways), rule 3 is the
// domain's subtree alone. When it cannot name after's, nothing says which
// names are still wanted, and it is an error: the caller decides nothing is
// deleted.
func BuildSiteRemoval(ctx context.Context, provider Provider, before *config.Config, site string, after *config.Config) (*PrunePlan, error) {
	rules := pruneRules{
		domain:     normalise(before.Community.Domain),
		producedBy: "the configuration before " + site + " is removed",
		addresses:  siteAddresses(before, site),
		shared:     map[string]string{},
		notOurs:    fmt.Sprintf("is not %s's public_address or public_address6", site),
		wantedBy:   "the configuration without " + site,
	}
	if rules.domain == "" {
		return nil, fmt.Errorf("dns: community.domain is not set, so there is no subtree to remove records within")
	}
	if len(rules.addresses) == 0 {
		return nil, fmt.Errorf("dns: %s has no public address, so no record points at it", site)
	}
	// The addresses are keyed by where they are declared; rule 4 matches the
	// address.
	byAddress := map[string]string{}
	for key, ip := range rules.addresses {
		byAddress[ip] = key
	}
	rules.addresses = byAddress
	for _, name := range after.SiteNames() {
		for key, ip := range siteAddresses(after, name) {
			if _, ours := rules.addresses[ip]; ours {
				rules.shared[ip] = key
			}
		}
	}
	if wants, err := Desired(before); err == nil {
		for _, w := range wants {
			rules.names = append(rules.names, w.Name)
		}
	}
	wants, err := Desired(after)
	if err != nil {
		return nil, err
	}
	rules.wanted = wantedSet(wants)
	plan, _, err := planPrune(ctx, provider, before, rules)
	return plan, err
}

// siteAddresses is a site's declared public addresses, canonical, keyed by
// the configuration key that declares each.
func siteAddresses(cfg *config.Config, name string) map[string]string {
	out := map[string]string{}
	site, ok := cfg.Sites[name]
	if !ok {
		return out
	}
	if ip := net.ParseIP(strings.TrimSpace(site.PublicAddress)); ip != nil {
		out[fmt.Sprintf("sites.%s.public_address", name)] = ip.String()
	}
	if ip := net.ParseIP(strings.TrimSpace(site.PublicAddress6)); ip != nil {
		out[fmt.Sprintf("sites.%s.public_address6", name)] = ip.String()
	}
	return out
}

func wantedSet(wants []Want) map[string][]string {
	out := map[string][]string{} // type + " " + name -> sources
	for _, w := range wants {
		out[w.Type+" "+w.Name] = w.Sources
	}
	return out
}

// planPrune lists every record in each zone rules' names live in and decides
// the fate of each one carrying cfg's deployment's comment. It also reports
// which vouched names a record carrying the comment sat at.
func planPrune(ctx context.Context, provider Provider, cfg *config.Config, rules pruneRules) (*PrunePlan, map[string]bool, error) {
	domain := rules.domain
	inScope := map[string]bool{}
	for _, n := range rules.names {
		inScope[n] = true
	}
	seenVouched := map[string]bool{}

	// Every zone that holds the domain or a name in scope, once each.
	cache := map[string]zone{}
	seen := map[string]bool{}
	var zones []zone
	for _, name := range append([]string{domain}, rules.names...) {
		z, err := FindZone(ctx, provider, name, cache)
		if err != nil {
			return nil, nil, err
		}
		if !seen[z.id] {
			seen[z.id] = true
			zones = append(zones, z)
		}
	}

	plan := &PrunePlan{Provider: provider.Name(), zones: zones, comment: RecordComment(cfg.Deployment())}
	for _, z := range zones {
		records, err := provider.AllRecords(ctx, z.id)
		if err != nil {
			return nil, nil, fmt.Errorf("dns: listing zone %s: %w", z.name, err)
		}
		for _, r := range records {
			if r.Comment != plan.comment {
				continue // rule 1: not this deployment's, so not listed
			}
			name := normalise(r.Name)
			typ := strings.ToUpper(r.Type)
			var reasons []string
			unscoped := false
			if typ != "A" && typ != "AAAA" {
				reasons = append(reasons, fmt.Sprintf("it is a %s record; prune removes only A and AAAA records", typ))
			}
			if rules.vouched[name] {
				seenVouched[name] = true
			}
			if name != domain && !strings.HasSuffix(name, "."+domain) && !inScope[name] && !rules.vouched[name] {
				reason := fmt.Sprintf("%s is outside community.domain (%s) and is not a name %s produces, so nothing says it was ever this deployment's", name, domain, rules.producedBy)
				if rules.canVouch {
					reason += fmt.Sprintf(". If it was, name it with --name %s", name)
				}
				reasons = append(reasons, reason)
				unscoped = len(reasons) == 1
			}
			content := strings.TrimSpace(r.Content)
			ip := net.ParseIP(content)
			// An address only matches after canonicalising, which turns an
			// IPv4-mapped IPv6 address into IPv4: so the record's type must
			// hold the address's own family, as every record dns init made
			// does.
			if ip != nil && (typ == "A" || typ == "AAAA") {
				v4 := ip.To4() != nil && !strings.Contains(content, ":")
				v6 := ip.To4() == nil
				if typ == "A" && !v4 || typ == "AAAA" && !v6 {
					reasons = append(reasons, fmt.Sprintf("%s is not an address of the family a %s record holds", r.Content, typ))
				}
			}
			switch {
			case ip == nil || rules.addresses[ip.String()] == "":
				reasons = append(reasons, fmt.Sprintf("%s %s", r.Content, rules.notOurs))
			case rules.shared[ip.String()] != "":
				key := rules.shared[ip.String()]
				owner := strings.Split(key, ".")[1]
				reasons = append(reasons, fmt.Sprintf("%s is also %s, which stays, so the record may be %s's", r.Content, key, owner))
			}
			if sources, ok := rules.wanted[typ+" "+name]; ok {
				reasons = append(reasons, fmt.Sprintf("%s still wants a record of type %s at this name (%s)", rules.wantedBy, typ, strings.Join(sources, ", ")))
			}
			action := Remove
			if len(reasons) > 0 {
				action = Keep
			}
			plan.Entries = append(plan.Entries, PruneEntry{Record: r, Zone: z.name, zoneID: z.id, Action: action, Reasons: reasons, Unscoped: unscoped && len(reasons) == 1})
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
	return plan, seenVouched, nil
}

// ExecutePrune deletes every record the plan removes, then lists each zone
// again and confirms each is gone.
//
// It stops at the first failed delete. Records already deleted stay deleted,
// and a re-run plans from a fresh listing, so a run that failed partway
// resumes rather than starting over.
func ExecutePrune(ctx context.Context, provider Provider, plan *PrunePlan, r ui.Reporter) error {
	removes := plan.Removes()
	for i, e := range removes {
		step := r.Step(fmt.Sprintf("delete %s %s", e.Type, e.Name))
		step.Detail("%s, zone %s, record %s", e.Content, e.Zone, e.ID)
		if e.ID == "" {
			err := fmt.Errorf("dns: %s %s has no record id to delete by. %s", e.Type, e.Name, partway(removes, i))
			step.Fail(err)
			return err
		}
		if err := provider.Delete(ctx, e.zoneID, e.ID); err != nil {
			err = fmt.Errorf("dns: deleting %s %s (record %s): %w. %s", e.Type, e.Name, e.ID, err, partway(removes, i))
			step.Fail(err)
			return err
		}
		step.Done("")
	}
	if len(removes) == 0 {
		return nil
	}
	confirm := r.Step("confirm deletes")
	remaining := map[string]map[string]bool{} // zone id -> record ids
	for _, e := range removes {
		if _, listed := remaining[e.zoneID]; listed {
			continue
		}
		records, err := provider.AllRecords(ctx, e.zoneID)
		if err != nil {
			err = fmt.Errorf("dns: listing zone %s to confirm the deletes: %w", e.Zone, err)
			confirm.Fail(err)
			return err
		}
		ids := map[string]bool{}
		for _, rec := range records {
			ids[rec.ID] = true
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
		err := fmt.Errorf("dns: deleted, but still listed: %s. Check them in the provider's console before re-running", strings.Join(still, ", "))
		confirm.Fail(err)
		return err
	}
	confirm.Done("")
	return nil
}

// partway says, for a run that stopped at removes[i], what it deleted and
// what it did not, so an operator knows where the zone stands before a
// re-run plans again.
func partway(removes []PruneEntry, i int) string {
	list := func(es []PruneEntry) string {
		if len(es) == 0 {
			return "none"
		}
		out := make([]string, len(es))
		for j, e := range es {
			out[j] = fmt.Sprintf("%s %s (record %s)", e.Type, e.Name, e.ID)
		}
		return strings.Join(out, ", ")
	}
	return fmt.Sprintf("Stopped there; deleted: %s; not deleted: %s. A re-run plans again from a fresh listing", list(removes[:i]), list(removes[i:]))
}

// Show reports a prune plan marked the way dns init marks its plan: each
// record to delete as pending, and each one kept as done under --verbose,
// with the rules it fails as details, since they are what an operator reads
// to decide whether to vouch for a name. A record kept only for its name is
// the operator's to act on, so it is a note with its reason at every
// verbosity. Each line names the record's type, name, address, zone and id.
func (p *PrunePlan) Show(r ui.Reporter) {
	r.Section("dns prune " + p.Provider)
	if len(p.Entries) == 0 {
		r.Detail("no record in these zones carries the comment %q", p.comment)
		return
	}
	for _, e := range p.Entries {
		record := fmt.Sprintf("%s %s -> %s (zone %s, record %s)", e.Type, e.Name, e.Content, e.Zone, e.ID)
		switch {
		case e.Action == Remove:
			r.Step("delete "+record).End(ui.Pending, "")
		case e.Unscoped:
			r.Note("keep "+record, strings.Join(e.Reasons, "; "))
		case r.Verbose():
			s := r.Step("keep " + record)
			for _, reason := range e.Reasons {
				s.Detail("%s", reason)
			}
			s.End(ui.OK, "")
		}
	}
}

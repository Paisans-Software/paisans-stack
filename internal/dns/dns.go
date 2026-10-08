// Package dns works out which public DNS records a deployment needs, compares
// them with what its DNS provider holds, and creates the ones that are
// missing.
//
// The records are derived, never declared. Every app hostname, the media
// hostname and every site endpoint given as a name already appear in
// paisans.yaml, and the address each one points at is a site's
// public_address. A second list of records in the configuration would be a
// second source of truth for the same names, and the one that drifts.
//
// Init only ever creates. It never updates and never deletes, because a
// record it did not create is somebody else's, exactly as a file on a host
// that no apply wrote is somebody else's. A record that disagrees with what
// the deployment needs is a conflict, and any conflict stops the whole run
// before a single record is written.
//
// Prune is the one path that deletes, and only an address record that passes
// every rule in prune.go: the toolkit's comment, a name and an address that
// are this deployment's, and no longer wanted. Nothing in this package
// updates a record.
//
// It reaches no host. The only thing it talks to is the DNS provider's API,
// from the workstation, with the token from the decrypted secrets.
package dns

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// Record is one DNS record as a provider reports it.
type Record struct {
	Type    string
	Name    string
	Content string
	// Proxied is a CDN in front of the address rather than the address itself.
	// Cloudflare calls this proxying. A proxied record that otherwise matches is
	// a conflict, not a match: see the README section on `dns init`.
	Proxied bool
	// ID is the provider's identifier for the record, which is what a delete
	// names. It is empty on a record about to be created.
	ID string
	// Comment is the provider's free text note on the record. Prune reads it
	// as necessary and never as sufficient: anyone with the zone can edit it.
	Comment string
}

// Want is a record the deployment needs.
type Want struct {
	Type    string
	Name    string
	Content string
	// Sources are the configuration keys that require this record, so that a
	// conflict can name the line in paisans.yaml it is about.
	Sources []string
}

// Provider is a DNS provider's API, reduced to the things this package does
// with it. There is no update here on purpose: a method that does not exist
// cannot be called by mistake. Delete exists for prune alone, and the only
// caller is ExecutePrune, which deletes nothing its plan did not pass through
// every prune rule.
type Provider interface {
	// Name is the provider's name as acme.provider spells it, for messages.
	Name() string
	// LookupZone reports the identifier of the zone whose apex is exactly
	// name, if the credential can see one.
	LookupZone(ctx context.Context, name string) (id string, found bool, err error)
	// Records returns every record of every type whose name is exactly name.
	Records(ctx context.Context, zoneID, name string) ([]Record, error)
	// Create adds one record. It must never replace an existing one.
	Create(ctx context.Context, zoneID string, record Record) error
	// AllRecords returns every record in the zone, every page of them. A
	// partial list must be an error, never a short answer.
	AllRecords(ctx context.Context, zoneID string) ([]Record, error)
	// Delete removes the one record with this identifier.
	Delete(ctx context.Context, zoneID, recordID string) error
}

// Desired derives the records a deployment needs from its configuration.
//
// It returns every problem at once rather than the first, for the same reason
// validate does.
func Desired(cfg *config.Config) ([]Want, error) {
	var problems []string
	byKey := map[string]*Want{} // type + " " + name
	var order []string
	add := func(typ, name, content, source string) {
		key := typ + " " + name
		if existing, ok := byKey[key]; ok {
			if existing.Content != content {
				problems = append(problems, fmt.Sprintf(
					"%s needs %s %s to be %s, but %s needs it to be %s. One name can point at one place; give them different names.",
					source, typ, name, content, strings.Join(existing.Sources, ", "), existing.Content))
				return
			}
			existing.Sources = append(existing.Sources, source)
			return
		}
		byKey[key] = &Want{Type: typ, Name: name, Content: content, Sources: []string{source}}
		order = append(order, key)
	}
	addSite := func(site config.Site, name, source string) {
		add("A", name, site.PublicAddress, source)
		if site.PublicAddress6 != "" {
			add("AAAA", name, site.PublicAddress6, source)
		}
	}

	// Every public hostname is served by the gateway, except a monitor's,
	// which the monitor serves from its own address so that it does not go
	// dark with the gateway it reports on.
	type claim struct{ key, name, app string }
	var hostnames []claim
	for _, appName := range cfg.AppNames() {
		app := cfg.Apps[appName]
		if app.Hostname != "" {
			hostnames = append(hostnames, claim{fmt.Sprintf("apps.%s.hostname", appName), app.Hostname, appName})
		}
		roles := make([]string, 0, len(app.Hostnames))
		for role := range app.Hostnames {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		for _, role := range roles {
			hostnames = append(hostnames, claim{fmt.Sprintf("apps.%s.hostnames.%s", appName, role), app.Hostnames[role], appName})
		}
	}
	// Each app that stores objects serves them on a media hostname of its
	// own, derived when the app does not declare one under hostnames.media
	// (a declared one is already in the loop above).
	for _, appName := range cfg.AppNames() {
		app := cfg.Apps[appName]
		if !kinds.MediaHostnameIsDerived(app) {
			continue
		}
		if media := kinds.MediaHostname(app, cfg.Community.Domain); media != "" {
			hostnames = append(hostnames, claim{fmt.Sprintf("apps.%s.hostnames.media (derived)", appName), media, appName})
		}
	}
	var viaGateway []claim
	refusedMonitor := map[string]bool{}
	for _, h := range hostnames {
		monitor, ok := render.ServedBy(cfg, h.app)
		if !ok {
			viaGateway = append(viaGateway, h)
			continue
		}
		site := cfg.Sites[monitor]
		if site.PublicAddress == "" {
			if !refusedMonitor[monitor] {
				refusedMonitor[monitor] = true
				problems = append(problems, fmt.Sprintf(
					"sites.%s.public_address is not set. %s holds the monitor role and serves its own hostnames, so they point at it, and the address the internet reaches it on cannot be guessed from here. Declare it, for example public_address: 203.0.113.20.",
					monitor, monitor))
			}
			continue
		}
		addSite(site, normalise(h.name), h.key)
	}
	if len(viaGateway) > 0 {
		gateways := cfg.GatewaySites()
		switch {
		case len(gateways) == 0:
			problems = append(problems, "no site holds the gateway role, so there is no address for any hostname to point at. Give one site the gateway role and a public_address.")
		case len(gateways) > 1:
			problems = append(problems, fmt.Sprintf(
				"%d sites hold the gateway role (%s). Which of them a hostname should point at is a design that does not exist yet, so this refuses rather than picking one. Publish these records by hand, or give the gateway role to one site.",
				len(gateways), strings.Join(gateways, ", ")))
		default:
			gateway := cfg.Sites[gateways[0]]
			if gateway.PublicAddress == "" {
				problems = append(problems, fmt.Sprintf(
					"sites.%s.public_address is not set. %s holds the gateway role, so every hostname points at it, and the address the internet reaches it on cannot be guessed from here. Declare it, for example public_address: 203.0.113.10.",
					gateways[0], gateways[0]))
			} else {
				for _, h := range viaGateway {
					addSite(gateway, normalise(h.name), h.key)
				}
			}
		}
	}

	// A site endpoint given as a name is what the other sites' WireGuard
	// dials, so it needs a record too. An endpoint given as an address
	// needs nothing.
	for _, siteName := range cfg.SiteNames() {
		site := cfg.Sites[siteName]
		host := endpointHost(site.Endpoint)
		if host == "" || net.ParseIP(host) != nil {
			continue
		}
		if site.PublicAddress == "" {
			problems = append(problems, fmt.Sprintf(
				"sites.%s.public_address is not set, but sites.%s.endpoint names %s, which the other sites dial. Declare the address that name should resolve to, or give the endpoint as an address.",
				siteName, siteName, host))
			continue
		}
		addSite(site, normalise(host), fmt.Sprintf("sites.%s.endpoint", siteName))
	}

	if len(problems) > 0 {
		return nil, &RefusedError{Problems: problems}
	}
	out := make([]Want, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out, nil
}

// RefusedError is every reason the configuration cannot be turned into
// records.
type RefusedError struct {
	Problems []string
}

func (e *RefusedError) Error() string {
	out := "dns: the records this deployment needs cannot be worked out:"
	for _, p := range e.Problems {
		out += "\n  " + p
	}
	return out
}

func endpointHost(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(endpoint); err == nil {
		return host
	}
	return strings.Trim(endpoint, "[]")
}

func normalise(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// Action is what happens to one wanted record.
type Action string

const (
	// Present is a record that already exists exactly as wanted.
	Present Action = "present"
	// Create is a record that does not exist and will be created.
	Create Action = "create"
	// Conflict is a record whose name already holds something else.
	Conflict Action = "conflict"
)

// Entry is one wanted record and what will be done about it.
type Entry struct {
	Want
	Zone   string
	zoneID string
	Action Action
	// Detail says what is in the way, for a conflict.
	Detail string
}

// Plan is every wanted record, compared with the provider.
type Plan struct {
	Provider string
	Entries  []Entry
}

// Conflicts returns the entries that stop an execute.
func (p *Plan) Conflicts() []Entry { return p.only(Conflict) }

// Creates returns the entries an execute will create.
func (p *Plan) Creates() []Entry { return p.only(Create) }

func (p *Plan) only(a Action) []Entry {
	var out []Entry
	for _, e := range p.Entries {
		if e.Action == a {
			out = append(out, e)
		}
	}
	return out
}

// Build compares the wanted records with what the provider holds. It reads
// and never writes.
func Build(ctx context.Context, provider Provider, wants []Want) (*Plan, error) {
	plan := &Plan{Provider: provider.Name()}
	zones := map[string]zone{}
	wantedTypes := map[string]map[string]bool{}
	for _, w := range wants {
		if wantedTypes[w.Name] == nil {
			wantedTypes[w.Name] = map[string]bool{}
		}
		wantedTypes[w.Name][w.Type] = true
	}
	existing := map[string][]Record{}
	for _, w := range wants {
		z, err := FindZone(ctx, provider, w.Name, zones)
		if err != nil {
			return nil, err
		}
		records, ok := existing[w.Name]
		if !ok {
			records, err = provider.Records(ctx, z.id, w.Name)
			if err != nil {
				return nil, err
			}
			existing[w.Name] = records
		}
		action, detail := classify(w, records, wantedTypes[w.Name])
		plan.Entries = append(plan.Entries, Entry{Want: w, Zone: z.name, zoneID: z.id, Action: action, Detail: detail})
	}
	return plan, nil
}

// classify decides what one wanted record needs, given every record already
// at its name.
func classify(w Want, records []Record, wanted map[string]bool) (Action, string) {
	var same []Record
	for _, r := range records {
		switch {
		case strings.EqualFold(r.Type, "CNAME"):
			return Conflict, fmt.Sprintf("a CNAME to %s already exists at this name. A name with a CNAME can hold no other record, and this never removes one it did not create.", r.Content)
		case strings.EqualFold(r.Type, w.Type):
			same = append(same, r)
		}
	}
	// An address record of the other family that nobody asked for splits
	// traffic: clients on that family go somewhere else, silently.
	for _, r := range records {
		other := ""
		switch {
		case w.Type == "A" && strings.EqualFold(r.Type, "AAAA") && !wanted["AAAA"]:
			other = "public_address6"
		case w.Type == "AAAA" && strings.EqualFold(r.Type, "A") && !wanted["A"]:
			other = "public_address"
		}
		if other != "" {
			return Conflict, fmt.Sprintf("a %s record pointing at %s already exists at this name, and this deployment declares no %s. Clients on that address family would reach it instead. Remove it, or declare %s on the site.", r.Type, r.Content, other, other)
		}
	}
	if len(same) == 0 {
		return Create, ""
	}
	for _, r := range same {
		if r.Content != w.Content {
			return Conflict, fmt.Sprintf("a %s record pointing at %s already exists at this name. This never replaces a record it did not create; change or remove it by hand if %s is right.", r.Type, r.Content, w.Content)
		}
	}
	for _, r := range same {
		if r.Proxied {
			return Conflict, "the record exists with this address but is proxied through the provider's CDN. A proxied name does not resolve to the gateway, so WireGuard cannot dial it and the gateway does not see client addresses. Turn proxying off for it."
		}
	}
	return Present, ""
}

// Execute creates every missing record, then reads each back. It refuses to
// write anything if the plan has a conflict.
//
// It stops at the first failure. Records created before it stay, and a re-run
// finds them present, so a run that failed partway resumes rather than
// starting over.
func Execute(ctx context.Context, provider Provider, plan *Plan) error {
	if conflicts := plan.Conflicts(); len(conflicts) > 0 {
		return fmt.Errorf("dns: %d conflicting record(s), listed above. Nothing was created: a partial set of records is a deployment some names reach and others do not", len(conflicts))
	}
	for _, e := range plan.Creates() {
		record := Record{Type: e.Type, Name: e.Name, Content: e.Content}
		if err := provider.Create(ctx, e.zoneID, record); err != nil {
			return fmt.Errorf("dns: creating %s %s: %w", e.Type, e.Name, err)
		}
		back, err := provider.Records(ctx, e.zoneID, e.Name)
		if err != nil {
			return fmt.Errorf("dns: reading back %s %s: %w", e.Type, e.Name, err)
		}
		if action, _ := classify(e.Want, back, map[string]bool{e.Type: true, "A": true, "AAAA": true}); action != Present {
			return fmt.Errorf("dns: %s %s was created but does not read back as %s unproxied. Check it in the provider's console before re-running", e.Type, e.Name, e.Content)
		}
	}
	return nil
}

// Write prints a plan in the shape the other dry-run commands use.
func (p *Plan) Write(w io.Writer) {
	fmt.Fprintf(w, "dns (%s)\n", p.Provider)
	for _, e := range p.Entries {
		fmt.Fprintf(w, "  %-9s %-4s %s -> %s  (zone %s)\n", e.Action, e.Type, e.Name, e.Content, e.Zone)
		if e.Detail != "" {
			fmt.Fprintf(w, "      %s\n", e.Detail)
		}
	}
	if n := len(p.Conflicts()); n > 0 {
		fmt.Fprintf(w, "\n%d record(s) conflict with what the provider already holds. Nothing will be created until that is resolved.\n", n)
	}
}

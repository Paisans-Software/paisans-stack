package siteremove

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/dns"
)

// dnsStage is the stage number of the DNS stage: after the host is cleaned,
// and before paisans.yaml is edited, so that a failure in it is resumed by
// running the same command, which needs the site still declared.
const dnsStage = 4

// dnsTimeout bounds one plan or one run of the DNS stage against the
// provider.
const dnsTimeout = 5 * time.Minute

// buildDNS is the DNS stage: the address records this deployment's dns init
// made for the site, deleted under dns prune's rules with before, the
// configuration as loaded, and after, the configuration once the site is
// removed (dns.BuildSiteRemoval; docs/specs/2026-10-09-site-remove-force.md).
//
// It is skipped, with one short line, when there is nothing it can safely
// do: no provider, no record management for it, no token, no public address
// for the site, or a configuration without the site that cannot name its
// records. Each leaves the removal to go on.
//
// The plan reads the provider and writes nothing. The run plans again from a
// fresh listing, under the same rules, so a record changed meanwhile is
// judged as it is now, and deletes through dns.ExecutePrune, which confirms
// each delete. The gate plans once more and passes only with nothing left.
func (p *Plan) buildDNS(before, after *config.Config) (*Stage, error) {
	st := &Stage{
		Number: dnsStage,
		Name:   "dns",
		Short:  "no record of " + p.Site + "'s is left",
		Gate:   fmt.Sprintf("listing the zones again finds no record this deployment's dns init made for %s that the rules delete", p.Site),
	}
	skip := func(line string) (*Stage, error) {
		st.Skipped, st.SkipLine = line, line
		return st, nil
	}
	name := before.ACME.Provider
	if name == "" {
		return skip("no DNS provider declared")
	}
	site, declared := before.Sites[p.Site]
	if !declared {
		return skip(p.Site + " is not declared; delete its records by hand")
	}
	if net.ParseIP(strings.TrimSpace(site.PublicAddress)) == nil && net.ParseIP(strings.TrimSpace(site.PublicAddress6)) == nil {
		return skip(p.Site + " has no public address")
	}
	if strings.TrimSpace(before.Community.Domain) == "" {
		return skip("community.domain is not set")
	}
	if !contains(dns.Implemented(), name) {
		return skip(name + " record management is not implemented")
	}
	token := ""
	if p.secrets != nil {
		token = p.secrets.External["acme_dns_token"]
	}
	if strings.TrimSpace(token) == "" {
		return skip("the DNS provider's token is not in the secrets")
	}
	if _, err := dns.Desired(after); err != nil {
		return skip("the configuration without " + p.Site + " cannot name its records")
	}
	providerFor := p.DNSProvider
	if providerFor == nil {
		providerFor = dns.For
	}
	provider, err := providerFor(name, token)
	if err != nil {
		return nil, fmt.Errorf("site remove %s: %w", p.Site, err)
	}
	plan := func() (*dns.PrunePlan, error) {
		ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
		defer cancel()
		return dns.BuildSiteRemoval(ctx, provider, before, p.Site, after)
	}
	// A provider that cannot be read skips the stage rather than refusing
	// the removal: a refusal here would also stop a re-run from finishing a
	// removal whose cluster stages have run. The line names the error.
	planned, err := plan()
	if err != nil {
		return skip("the DNS provider could not be read; delete the records by hand or run again: " + err.Error())
	}
	for _, e := range planned.Entries {
		title := fmt.Sprintf("%s %s → %s", e.Type, e.Name, e.Content)
		if e.Action == dns.Remove {
			st.Steps = append(st.Steps, Step{Site: p.Site, Verb: "delete", Title: "delete " + title, Text: fmt.Sprintf("%s, zone %s, record %s", title, e.Zone, e.ID)})
			continue
		}
		st.Kept = append(st.Kept, Kept{Title: "keep " + title, Why: e.Reasons, Shown: pointsAt(before, p.Site, e.Content)})
	}
	st.run = func() error {
		p.idle()
		fresh, err := plan()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
		defer cancel()
		return dns.ExecutePrune(ctx, provider, fresh, p.reporter())
	}
	st.gate = func() error {
		fresh, err := plan()
		if err != nil {
			return err
		}
		if left := fresh.Removes(); len(left) > 0 {
			var names []string
			for _, e := range left {
				names = append(names, fmt.Sprintf("%s %s (record %s)", e.Type, e.Name, e.ID))
			}
			return fmt.Errorf("still to delete: %s", strings.Join(names, ", "))
		}
		return nil
	}
	return st, nil
}

// pointsAt reports whether content is one of site's declared public
// addresses: a record kept although it does, whose reasons an operator
// needs to see.
func pointsAt(cfg *config.Config, site, content string) bool {
	ip := net.ParseIP(strings.TrimSpace(content))
	if ip == nil {
		return false
	}
	s := cfg.Sites[site]
	for _, a := range []string{s.PublicAddress, s.PublicAddress6} {
		if own := net.ParseIP(strings.TrimSpace(a)); own != nil && own.Equal(ip) {
			return true
		}
	}
	return false
}

// DNSNotModified is the warning every --id run gives: with no configuration
// there is no provider and no token, so nothing at the provider can be
// deleted safely. The detail is the comment the deployment's records carry,
// and the address dest names, when it names one.
func DNSNotModified(d deployment.Deployment, dest config.Destination) string {
	out := fmt.Sprintf("DNS records for this deployment, if any exist, were not modified. They carry the comment %q", dns.RecordComment(d))
	if ip := net.ParseIP(strings.Trim(dest.Host, "[]")); ip != nil {
		out += ", and may point at " + ip.String()
	}
	return out
}

// DNSWarning is the --id plan's DNS line, alone, for a run that ends before
// its remains are reported; none for a plan with a configuration.
func (p *Plan) DNSWarning() []string {
	if !p.byID {
		return nil
	}
	return []string{DNSNotModified(p.dep(), p.dest)}
}

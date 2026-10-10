// Package dnstest is a DNS provider held in memory, for tests outside
// internal/dns that need one: site remove's DNS stage and the commands that
// build it. It answers as dns.Provider's contract says a provider must, and
// reaches nothing. No test may use a real provider.
package dnstest

import (
	"context"
	"fmt"
	"sync"

	"github.com/paisans-software/paisans-stack/internal/dns"
)

// Provider is a zone store behind dns.Provider.
type Provider struct {
	mu sync.Mutex
	// Zones maps a zone's apex to its id.
	Zones map[string]string
	// InZone maps a zone id to its records.
	InZone map[string][]dns.Record
	// Deletes are the record ids asked to be deleted, in order.
	Deletes []string
	// Listings counts whole zone listings.
	Listings int
	// FailDelete is a record id whose delete fails, as a provider error
	// would.
	FailDelete string
	// FailList makes every whole zone listing fail.
	FailList bool
	// Token is what the provider was made with, for a test that checks it
	// was handed over and never printed.
	Token string
}

// New is an empty provider.
func New() *Provider {
	return &Provider{Zones: map[string]string{}, InZone: map[string][]dns.Record{}}
}

// For returns a constructor with dns.For's shape that hands back p for name
// and refuses every other provider, recording the token it was given.
func (p *Provider) For(name string) func(string, string) (dns.Provider, error) {
	return func(n, token string) (dns.Provider, error) {
		if n != name {
			return nil, fmt.Errorf("dnstest: asked for provider %q, have %q", n, name)
		}
		p.mu.Lock()
		p.Token = token
		p.mu.Unlock()
		return p, nil
	}
}

func (p *Provider) Name() string { return "cloudflare" }

func (p *Provider) LookupZone(_ context.Context, name string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.Zones[name]
	return id, ok, nil
}

func (p *Provider) Records(_ context.Context, zoneID, name string) ([]dns.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []dns.Record
	for _, r := range p.InZone[zoneID] {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out, nil
}

func (p *Provider) Create(_ context.Context, zoneID string, r dns.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	r.ID = fmt.Sprintf("rec-new-%d", len(p.InZone[zoneID])+1)
	p.InZone[zoneID] = append(p.InZone[zoneID], r)
	return nil
}

func (p *Provider) AllRecords(_ context.Context, zoneID string) ([]dns.Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Listings++
	if p.FailList {
		return nil, fmt.Errorf("dnstest: HTTP 500 listing zone %s", zoneID)
	}
	return append([]dns.Record(nil), p.InZone[zoneID]...), nil
}

func (p *Provider) Delete(_ context.Context, zoneID, recordID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if recordID == p.FailDelete {
		return fmt.Errorf("dnstest: HTTP 500 deleting record %s", recordID)
	}
	p.Deletes = append(p.Deletes, recordID)
	kept := p.InZone[zoneID][:0:0]
	found := false
	for _, r := range p.InZone[zoneID] {
		if r.ID == recordID {
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return fmt.Errorf("dnstest: HTTP 404: record %s does not exist", recordID)
	}
	p.InZone[zoneID] = kept
	return nil
}

// IDs are the ids of every record left in the zone, in order.
func (p *Provider) IDs(zoneID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.InZone[zoneID] {
		out = append(out, r.ID)
	}
	return out
}

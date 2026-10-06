package dns

import (
	"context"
	"fmt"
	"strings"
)

type zone struct{ id, name string }

// FindZone finds the zone a name belongs to by asking the provider about each
// suffix of it, longest first, and taking the first one the credential can
// see.
//
// Longest first is what makes a delegated subzone come out right: with zones
// for both example.org and staging.example.org, talk.staging.example.org
// belongs to the second, and a record created in the first would never be
// served. The bare top level label is never asked about.
//
// The zone is discovered rather than declared because community.domain is
// not necessarily a zone apex, and an operator who has to type the zone too
// has one more value to get wrong. cache may be nil.
func FindZone(ctx context.Context, provider Provider, name string, cache map[string]zone) (zone, error) {
	labels := strings.Split(name, ".")
	var tried []string
	for i := 0; i < len(labels)-1; i++ {
		candidate := strings.Join(labels[i:], ".")
		if z, ok := cache[candidate]; ok {
			if z.id != "" {
				return z, nil
			}
			tried = append(tried, candidate)
			continue
		}
		id, found, err := provider.LookupZone(ctx, candidate)
		if err != nil {
			return zone{}, fmt.Errorf("dns: looking up zone %s: %w", candidate, err)
		}
		z := zone{}
		if found {
			z = zone{id: id, name: candidate}
		}
		if cache != nil {
			cache[candidate] = z
		}
		if found {
			return z, nil
		}
		tried = append(tried, candidate)
	}
	return zone{}, fmt.Errorf("dns: no zone that %s can see holds %s (tried %s). Either the zone is not at this provider, or the token cannot read it: it needs read access to the zone as well as permission to edit its records", provider.Name(), name, strings.Join(tried, ", "))
}

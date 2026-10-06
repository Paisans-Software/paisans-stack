package dns

import (
	"fmt"
	"sort"
	"strings"
)

// implemented is every provider whose record management exists. It is a
// subset of the providers internal/acme knows: answering an ACME challenge
// and managing ordinary records are different code, and a provider can have
// the first without the second.
var implemented = map[string]func(token string) Provider{
	"cloudflare": func(token string) Provider { return newCloudflare(token) },
}

// Implemented returns the providers record management exists for, sorted.
func Implemented() []string {
	out := make([]string, 0, len(implemented))
	for name := range implemented {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// For returns the provider named by acme.provider, holding the token.
//
// The same token that answers ACME challenges creates the records. That is
// deliberate: it is already scoped to the zone and already in the secrets, and
// a second credential with the same reach would be one more thing to rotate
// and to leak.
func For(name, token string) (Provider, error) {
	if name == "" {
		return nil, fmt.Errorf("dns: acme.provider is not set, so there is no DNS provider to create records at")
	}
	constructor, ok := implemented[name]
	if !ok {
		return nil, fmt.Errorf("dns: %s record management is not implemented yet. Implemented: %s. Until it is, create the records at %s by hand, unproxied, as the README section on `dns init` describes", name, strings.Join(Implemented(), ", "), name)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("dns: external.acme_dns_token is not set in the secrets. It is issued by %s, scoped to the zone, and pasted in; the toolkit cannot generate it", name)
	}
	return constructor(token), nil
}

package validate_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/validate"
)

// The cases are built from the valid fixture in code rather than as one
// fixture file each, because the thing varied is a single string and a file
// per address would be a dozen copies of the same deployment.
func TestPublicAddress(t *testing.T) {
	cases := []struct {
		name    string
		v4, v6  string
		rule    string // empty: no finding expected
		mention string // a word the message must contain
	}{
		{name: "documentation range is accepted", v4: "203.0.113.10"},
		{name: "ipv6 documentation range is accepted", v4: "203.0.113.10", v6: "2001:db8::10"},
		{name: "not an address", v4: "vm.example.org", rule: "public-address-is-not-ipv4", mention: "not an IPv4"},
		{name: "ipv6 in the ipv4 field", v4: "2001:db8::10", rule: "public-address-is-not-ipv4", mention: "public_address6"},
		{name: "rfc1918", v4: "192.168.1.20", rule: "public-address-is-not-public", mention: "RFC 1918"},
		{name: "cgnat", v4: "100.72.3.4", rule: "public-address-is-not-public", mention: "carrier grade NAT"},
		{name: "loopback", v4: "127.0.0.1", rule: "public-address-is-not-public", mention: "loopback"},
		{name: "link local", v4: "169.254.1.1", rule: "public-address-is-not-public", mention: "link local"},
		{name: "inside the mesh", v4: "10.44.0.3", rule: "public-address-is-not-public", mention: "mesh.subnet"},
		{name: "ipv4 in the ipv6 field", v4: "203.0.113.10", v6: "203.0.113.11", rule: "public-address-is-not-ipv6", mention: "not an IPv6"},
		{name: "ipv6 unique local", v4: "203.0.113.10", v6: "fd00::1", rule: "public-address-is-not-public", mention: "RFC 4193"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := load(t, "valid")
			site := cfg.GatewaySites()[0]
			declared := cfg.Sites[site]
			declared.PublicAddress = tc.v4
			declared.PublicAddress6 = tc.v6
			cfg.Sites[site] = declared

			result := validate.Check(cfg)
			if tc.rule == "" {
				if len(result.Findings) != 0 {
					t.Fatalf("expected no findings, got %v", result.Findings)
				}
				return
			}
			if !result.Has(tc.rule) {
				t.Fatalf("rule %s did not fire. findings: %v", tc.rule, result.Findings)
			}
			for _, f := range result.Findings {
				if f.Rule != tc.rule {
					continue
				}
				if f.Level != validate.Refuse {
					t.Fatalf("rule %s fired at %s, want REFUSE", tc.rule, f.Level)
				}
				if !strings.Contains(f.Message, tc.mention) {
					t.Fatalf("message should mention %q: %s", tc.mention, f.Message)
				}
			}
		})
	}
}

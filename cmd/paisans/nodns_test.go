package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/dns"
)

// TestMain stands a refusal in for the DNS provider site remove builds, so
// that no test in this package can reach a real one, whatever provider its
// fixture names. A test that needs one replaces it with a fake.
func TestMain(m *testing.M) {
	dnsProviderFor = func(name, _ string) (dns.Provider, error) {
		return nil, fmt.Errorf("test: no DNS provider may be built in tests, asked for %s", name)
	}
	os.Exit(m.Run())
}

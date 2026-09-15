package acme_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Every provider the toolkit claims to support needs both halves: a module
// identifier to check for at apply time, and an image that actually has it.
// Half a provider is worse than none, because it validates and then fails on
// the host.
func TestSupportedProvidersAreComplete(t *testing.T) {
	providers := acme.Providers()
	if len(providers) < 2 {
		t.Fatalf("expected at least cloudflare and desec, got %v", providers)
	}
	for _, provider := range providers {
		if module := acme.Module(provider); !strings.HasPrefix(module, "dns.providers.") {
			t.Errorf("%s has module %q, which is not a Caddy DNS module identifier", provider, module)
		}
		image, ok := acme.Image(provider)
		if !ok {
			t.Errorf("%s is supported but has no published image", provider)
			continue
		}
		if !kinds.ParseReference(image).Pinned() {
			t.Errorf("%s image %q does not name one build", provider, image)
		}
		if !strings.Contains(image, "@sha256:") {
			t.Errorf("%s image %q is not pinned by digest, and every tag we publish moves", provider, image)
		}
	}
}

// A provider nobody published an image for is not an error here: the
// configuration may declare its own image. This package reports what it knows,
// and validate decides what that means.
func TestUnknownProviderIsReportedNotRefused(t *testing.T) {
	if _, ok := acme.Image("route53"); ok {
		t.Error("an image was claimed for a provider this toolkit does not publish")
	}
}

// An unknown provider still gets a module identifier, and this is load
// bearing: apply only checks the gateway's binary when it has one to check
// for, so an empty answer here would skip the check for exactly the provider
// an adopter brought their own image for, which is the one nobody has tested.
func TestUnknownProviderStillHasAModuleToCheckFor(t *testing.T) {
	module := acme.Module("route53")
	if module == "" {
		t.Fatal("an unknown provider yields no module, so apply would skip the check that exists for it")
	}
	if module != "dns.providers.route53" {
		t.Errorf("module is %q, want the caddy-dns convention dns.providers.route53", module)
	}
	if !strings.HasPrefix(module, "dns.providers.") {
		t.Errorf("module %q is not shaped like a Caddy DNS module identifier", module)
	}
	// A deployment with no gateway declares no provider, and there is then
	// nothing to check for.
	if module := acme.Module(""); module != "" {
		t.Errorf("no provider yielded module %q, so an apply with no gateway would check for it", module)
	}
}

// Upstream's own image carries no DNS module at all, so declaring it as an
// override is the one case that can be judged from the reference alone.
func TestStockCaddyIsRecognised(t *testing.T) {
	stock := []string{
		"caddy:2-alpine",
		"caddy:latest",
		"docker.io/library/caddy:2.11.4",
		"library/caddy:2",
	}
	for _, reference := range stock {
		if !acme.IsStockCaddy(reference) {
			t.Errorf("%q is upstream's image and was not recognised as one", reference)
		}
	}
	notStock := []string{
		"ghcr.io/paisans-software/caddy:2.11.4@sha256:abc",
		"ghcr.io/them/caddy-route53:2.9.0",
		"example.org/caddy-with-modules:1",
	}
	for _, reference := range notStock {
		if acme.IsStockCaddy(reference) {
			t.Errorf("%q was wrongly called upstream's image", reference)
		}
	}
}

// The two published providers do not share one Caddyfile syntax: cloudflare
// takes its token as a bare argument, deSEC requires a block and rejects a
// bare argument. A directive that used one shape for both would validate and
// then fail on the host for whichever provider does not accept it, which is
// exactly the failure this package exists to remove.
func TestDirectiveMatchesEachProvidersOwnSyntax(t *testing.T) {
	desec := acme.Directive("desec")
	if !strings.Contains(desec, "token") {
		t.Errorf("desec directive %q has no token subdirective, but desec rejects a bare argument", desec)
	}

	cloudflare := acme.Directive("cloudflare")
	if strings.Contains(cloudflare, "token") {
		t.Errorf("cloudflare directive %q has a token subdirective, but cloudflare takes a bare argument", cloudflare)
	}

	// A provider this toolkit does not publish still needs something usable:
	// the bare argument form, using the provider's own name, since that is the
	// common shape.
	unknown := acme.Directive("route53")
	if !strings.Contains(unknown, "route53") {
		t.Errorf("unknown provider directive %q does not name the provider", unknown)
	}
	if strings.TrimSpace(unknown) == "" {
		t.Error("unknown provider directive is empty")
	}
}

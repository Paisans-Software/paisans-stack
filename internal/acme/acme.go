// Package acme records what the toolkit knows about ACME DNS providers: the
// Caddy module each one needs, the image that carries it, and the Caddyfile
// directive that configures it.
//
// It is its own package because two callers need the same answer and neither
// should own it. internal/validate refuses a provider with no image, and
// internal/render picks the image and writes the provider into the Caddyfile.
// A second copy would drift into a configuration that validates and then fails
// on the host, which is the failure this whole change exists to remove.
package acme

import (
	"fmt"
	"sort"
	"strings"
)

// Provider is what the toolkit knows about one DNS provider.
type Provider struct {
	// Module is the Caddy module identifier, as `caddy list-modules` prints it.
	// apply greps for exactly this string, so it is the identifier rather than
	// the Go import path.
	Module string
	// Image is the reference a deployment using this provider runs.
	//
	// Pinned by digest, not by tag. Every tag ghcr.io/paisans-software/caddy
	// publishes moves, mirroring upstream Caddy, which rebuilds even a patch
	// tag when its base image gets a security fix. A digest is the only
	// reference that makes two runs of apply deploy the same bytes.
	//
	// Moved forward by a pull request that Paisans-Software/caddy-dns opens
	// after a successful build. Do not edit it by hand except to correct it.
	Image string
	// directive is the Caddyfile lines that configure this provider inside
	// Caddy's global options block, indented for that block (one tab). The
	// providers do not share one syntax:
	//
	//   - cloudflare takes the token as a bare argument:
	//     `acme_dns cloudflare {env.ACME_DNS_TOKEN}`
	//     (github.com/caddy-dns/cloudflare, README "Caddyfile" section)
	//   - deSEC requires a block and rejects a bare argument:
	//     `acme_dns desec { token {env.ACME_DNS_TOKEN} }`
	//     (github.com/caddy-dns/desec, README "Caddyfile" section)
	//
	// A single shared form would validate and then fail on the host for
	// whichever provider does not accept it, which is exactly the failure
	// this package exists to remove.
	directive string
}

// supported is every provider this toolkit publishes an image for. A provider
// absent here is not refused outright: a deployment may declare its own image,
// and the module it claims is verified at apply time rather than assumed.
var supported = map[string]Provider{
	"cloudflare": {
		Module:    "dns.providers.cloudflare",
		Image:     "ghcr.io/paisans-software/caddy:2.11.4@sha256:afd8356c2b12dc8a7473ed10a99e1e575e453482e2530582bbb32de8d0be47a6",
		directive: "\tacme_dns cloudflare {env.ACME_DNS_TOKEN}",
	},
	"desec": {
		Module: "dns.providers.desec",
		Image:  "ghcr.io/paisans-software/caddy:2.11.4@sha256:afd8356c2b12dc8a7473ed10a99e1e575e453482e2530582bbb32de8d0be47a6",
		directive: "\tacme_dns desec {\n" +
			"\t\ttoken {env.ACME_DNS_TOKEN}\n" +
			"\t}",
	},
}

// Providers returns the supported provider names, sorted, for a message that
// has to list them.
func Providers() []string {
	out := make([]string, 0, len(supported))
	for name := range supported {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Module returns the Caddy module identifier for a provider.
//
// For a provider this toolkit publishes an image for, it is the identifier
// recorded above. For any other, it is `dns.providers.<name>`, which is the
// convention every caddy-dns module follows, and which is the whole point: the
// provider an adopter brought their own image for is the one most likely to be
// wrong, so it is the one the apply time check matters most for. Returning
// nothing there would silently skip the check on exactly that path.
//
// Empty for an empty name, which is a deployment with no gateway anywhere and
// so nothing to check.
func Module(name string) string {
	if name == "" {
		return ""
	}
	if provider, ok := supported[name]; ok {
		return provider.Module
	}
	return "dns.providers." + name
}

// Image returns the published image for a provider, and whether one exists.
func Image(name string) (string, bool) {
	provider, ok := supported[name]
	return provider.Image, ok
}

// Directive returns the Caddyfile lines that configure a provider inside
// Caddy's global options block, indented one tab for that block.
//
// For a provider this toolkit does not publish an image for, it returns the
// bare argument form using the provider's own name, since that is the shape
// most caddy-dns modules use (cloudflare's included); a provider that needs
// the block form instead belongs in supported above, with its own directive.
func Directive(name string) string {
	if provider, ok := supported[name]; ok {
		return provider.directive
	}
	return fmt.Sprintf("\tacme_dns %s {env.ACME_DNS_TOKEN}", name)
}

// IsStockCaddy reports whether a reference names upstream's own Caddy image.
//
// This is the one thing about an image that can be judged from its reference:
// upstream's image certainly carries no DNS provider module. Every other
// reference is accepted here and verified at apply time instead, because
// whether a module is compiled into a binary is a property of the binary.
func IsStockCaddy(reference string) bool {
	name := reference
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	slash := strings.LastIndex(name, "/")
	if colon := strings.LastIndex(name, ":"); colon > slash {
		name = name[:colon]
	}
	switch name {
	case "caddy", "library/caddy", "docker.io/library/caddy", "docker.io/caddy":
		return true
	}
	return false
}

package kinds

import (
	"sort"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// PrimaryRole is the hostname every app has, held in App.Hostname rather than
// in the Hostnames map.
const PrimaryRole = "primary"

// WellknownRole is the hostname a homeserver's delegation documents are served
// on. It is named here rather than spelled as a literal in three packages
// because it decides more than routing: the name in every user identifier is
// the name the delegation is served on, so it is also what server_name must
// be.
const WellknownRole = "wellknown"

// extraRoles is the additional hostname roles each kind understands, and it is
// deliberately a closed set. A role is not a label: it selects which Caddy
// snippet that hostname gets, so a role the kind ships no snippet for would
// render a host block importing a file that does not exist.
var extraRoles = map[config.Kind][]string{
	// A homeserver answers its API on the primary name and its delegation
	// documents on whatever name appears in user identifiers, which is usually
	// the apex. Routing them identically would publish the whole API on the
	// apex as well.
	config.KindSynapse: {WellknownRole},
}

// HostnameRoles returns the roles a kind understands, sorted, always including
// the primary.
func HostnameRoles(kind config.Kind) []string {
	out := append([]string{PrimaryRole}, extraRoles[kind]...)
	sort.Strings(out)
	return out
}

// HasHostnameRole reports whether a kind understands a role.
func HasHostnameRole(kind config.Kind, role string) bool {
	if role == PrimaryRole {
		return true
	}
	for _, known := range extraRoles[kind] {
		if known == role {
			return true
		}
	}
	return false
}

// SnippetFor returns the template filename a role's routing lives in.
func SnippetFor(role string) string {
	if role == PrimaryRole {
		return "caddy.snippet.tmpl"
	}
	return "caddy.snippet." + role + ".tmpl"
}
